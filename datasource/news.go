package datasource

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// NewsItem 一条板块/标的相关新闻摘要。
//
//	Date  原始日期文本（如 "2026-05-25 19:27:02"），保留作展示
//	Time  统一解析后的时间戳，用于跨源排序与时效过滤
//	Title 标题（已剥 <em>/</em> 高亮）
type NewsItem struct {
	Date    string    `json:"date"`
	Time    time.Time `json:"-"`
	Title   string    `json:"title"`
	Source  string    `json:"source"`
	Content string    `json:"content"`
	URL     string    `json:"url"`
}

// NewsFetcher 通过 EastMoney / 新浪财经 / 财联社 / Wind 等开放接口抓取真实新闻列表。
//
// 设计：
//   - 多源扇出 → 合并 → 时效过滤 → 关键词强相关过滤 → 时间倒序 → 截 limit
//   - 单源失败不影响其他源
//   - LLM 仅做摘要，禁止凭空生成
type NewsFetcher struct {
	HTTP *http.Client
	// FreshWithin 仅保留 now-FreshWithin 内的新闻；零值默认 24h。
	FreshWithin time.Duration
}

func NewNewsFetcher() *NewsFetcher {
	return &NewsFetcher{
		HTTP:        &http.Client{Timeout: 8 * time.Second},
		FreshWithin: 24 * time.Hour,
	}
}

// FetchSectorNews 单关键词抓取（兼容旧调用）。
func (f *NewsFetcher) FetchSectorNews(keyword string, limit int) []NewsItem {
	return f.FetchMulti([]string{keyword}, limit)
}

// FetchMulti 多关键词扇出抓取，去重 + 时效/关联度过滤 + 按时间倒序，返回最近 limit 条。
//
// 多源策略：
//  1. EastMoney 站内搜索（cmsArticleWebOld，按 time 排序）
//  2. 新浪财经财经滚动（feed.mix.sina.com.cn，按时间倒序）
//  3. 财联社电报（cls.cn nodeapi，A 股最快源）
//  4. Wind 财经资讯（wind.com.cn search api）
//
// 任一源失败自动跳过，不抛错。
func (f *NewsFetcher) FetchMulti(keywords []string, limit int) []NewsItem {
	if limit <= 0 {
		return nil
	}
	if limit > 30 {
		limit = 30
	}
	bag := make(map[string]NewsItem)
	for _, kw := range keywords {
		kw = strings.TrimSpace(kw)
		if kw == "" {
			continue
		}
		f.mergeBag(bag, f.fromEastMoney(kw, limit))
		f.mergeBag(bag, f.fromSina(kw, limit))
		f.mergeBag(bag, f.fromCailianpress(kw, limit))
		f.mergeBag(bag, f.fromWind(kw, limit))
	}

	out := make([]NewsItem, 0, len(bag))
	for _, v := range bag {
		out = append(out, v)
	}

	// 时效过滤：仅保留 now-FreshWithin 内的（Time 零值的样本保留，避免误删字典序还能用的 EastMoney 样本）
	out = f.filterFresh(out)

	// 关键词强相关过滤：标题/内容必须命中任意 keyword（去除"芯片"搜出来的"芯片烘焙"等噪音）
	out = filterByKeywords(out, keywords)

	// 跨源统一排序：优先按 Time 倒序，Time 零值的退化为 Date 字典序
	sort.Slice(out, func(i, j int) bool {
		ti, tj := out[i].Time, out[j].Time
		if !ti.IsZero() && !tj.IsZero() {
			return ti.After(tj)
		}
		if ti.IsZero() && tj.IsZero() {
			return out[i].Date > out[j].Date
		}
		// 有 Time 的优先排前
		return !ti.IsZero()
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// mergeBag 合并新闻到去重 bag（key = URL，URL 为空时退化为 Title）。
func (f *NewsFetcher) mergeBag(bag map[string]NewsItem, items []NewsItem) {
	for _, it := range items {
		key := it.URL
		if key == "" {
			key = it.Title
		}
		if key == "" {
			continue
		}
		if _, ok := bag[key]; !ok {
			bag[key] = it
		}
	}
}

// filterFresh 仅保留时间在 now-FreshWithin 内的新闻；Time 零值视为放过。
func (f *NewsFetcher) filterFresh(items []NewsItem) []NewsItem {
	if f.FreshWithin <= 0 {
		return items
	}
	cutoff := time.Now().Add(-f.FreshWithin)
	out := items[:0]
	for _, it := range items {
		if it.Time.IsZero() || !it.Time.Before(cutoff) {
			out = append(out, it)
		}
	}
	return out
}

// filterByKeywords 关联度过滤：title/content 必须包含至少一个 keyword 才保留。
// keyword 列表为空时跳过此过滤。
func filterByKeywords(items []NewsItem, keywords []string) []NewsItem {
	clean := make([]string, 0, len(keywords))
	for _, k := range keywords {
		if s := strings.TrimSpace(k); s != "" {
			clean = append(clean, s)
		}
	}
	if len(clean) == 0 {
		return items
	}
	out := items[:0]
	for _, it := range items {
		text := it.Title + it.Content
		matched := false
		for _, kw := range clean {
			if strings.Contains(text, kw) {
				matched = true
				break
			}
		}
		if matched {
			out = append(out, it)
		}
	}
	return out
}

// fromEastMoney 调用 EastMoney 站内搜索 JSONP 接口。
func (f *NewsFetcher) fromEastMoney(keyword string, limit int) []NewsItem {
	payload := fmt.Sprintf(
		`{"uid":"","keyword":%q,"type":["cmsArticleWebOld"],"client":"web","clientType":"web","clientVersion":"curr","param":{"cmsArticleWebOld":{"searchScope":"default","sort":"time","pageIndex":1,"pageSize":%d,"preTag":"<em>","postTag":"</em>"}}}`,
		keyword, limit,
	)
	u := "https://search-api-web.eastmoney.com/search/jsonp?cb=jQuery&param=" + url.QueryEscape(payload)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")
	req.Header.Set("Referer", "https://so.eastmoney.com/")
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if i := strings.Index(text, "("); i >= 0 {
		if j := strings.LastIndex(text, ")"); j > i {
			text = text[i+1 : j]
		}
	}
	var raw struct {
		Result struct {
			Articles []struct {
				Date      string `json:"date"`
				Title     string `json:"title"`
				Content   string `json:"content"`
				MediaName string `json:"mediaName"`
				URL       string `json:"url"`
			} `json:"cmsArticleWebOld"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil
	}
	items := make([]NewsItem, 0, len(raw.Result.Articles))
	for _, a := range raw.Result.Articles {
		t, _ := time.ParseInLocation("2006-01-02 15:04:05", a.Date, time.Local)
		items = append(items, NewsItem{
			Date: a.Date, Time: t,
			Title: stripHL(a.Title), Content: stripHL(a.Content),
			Source: nonEmpty(a.MediaName, "东方财富"), URL: a.URL,
		})
	}
	return items
}

// fromSina 调用新浪财经财经滚动 JSON 接口。
//
// 接口（公开 mix.sina）：
//
//	https://feed.mix.sina.com.cn/api/roll/get
//	  ?pageid=153&lid=2516&num=20&page=1
//	  &k=<keyword>           （传 q 参数实测部分场景被忽略，作 fallback）
//
// 真实使用：先按 lid=2516（A股财经滚动）拉最近 num 条，再用 keyword 在 title/content 里
// 二次过滤（filterByKeywords 在 FetchMulti 末尾兜一次，保证关联度）。
func (f *NewsFetcher) fromSina(keyword string, limit int) []NewsItem {
	if limit > 30 {
		limit = 30
	}
	u := fmt.Sprintf(
		"https://feed.mix.sina.com.cn/api/roll/get?pageid=153&lid=2516&num=%d&page=1&k=%s",
		limit, url.QueryEscape(keyword),
	)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")
	req.Header.Set("Referer", "https://finance.sina.com.cn/")
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var raw struct {
		Result struct {
			Data []struct {
				Title    string `json:"title"`
				URL      string `json:"url"`
				Intro    string `json:"intro"`
				Media    string `json:"media_name"`
				CtimeStr string `json:"create_time"` // unix 秒字符串
			} `json:"data"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	items := make([]NewsItem, 0, len(raw.Result.Data))
	for _, a := range raw.Result.Data {
		var t time.Time
		var sec int64
		fmt.Sscanf(a.CtimeStr, "%d", &sec)
		if sec > 0 {
			t = time.Unix(sec, 0).In(time.Local)
		}
		dateStr := a.CtimeStr
		if !t.IsZero() {
			dateStr = t.Format("2006-01-02 15:04:05")
		}
		items = append(items, NewsItem{
			Date: dateStr, Time: t,
			Title: stripHL(a.Title), Content: stripHL(a.Intro),
			Source: nonEmpty(a.Media, "新浪财经"), URL: a.URL,
		})
	}
	return items
}

// fromCailianpress 调用财联社电报接口。
//
// 接口（公开 nodeapi）：
//
//	https://www.cls.cn/nodeapi/telegraphList?app=CailianpressWeb&category=&os=web&refresh_type=1&rn=20&subscribedColumnIds=
//
// 财联社的电报通常没有结构化 keyword 搜索，本函数拉最近 rn 条，由 filterByKeywords 在
// FetchMulti 末尾做关联度过滤；电报时效极短，对消息面打分非常关键。
func (f *NewsFetcher) fromCailianpress(keyword string, limit int) []NewsItem {
	if limit > 30 {
		limit = 30
	}
	u := fmt.Sprintf(
		"https://www.cls.cn/nodeapi/telegraphList?app=CailianpressWeb&category=&os=web&refresh_type=1&rn=%d&subscribedColumnIds=",
		limit,
	)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")
	req.Header.Set("Referer", "https://www.cls.cn/telegraph")
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var raw struct {
		Data struct {
			RollData []struct {
				ID       int64  `json:"id"`
				Title    string `json:"title"`
				Content  string `json:"content"`
				CtimeSec int64  `json:"ctime"`
				ShareURL string `json:"shareurl"`
				IsAd     int    `json:"is_ad"`
			} `json:"roll_data"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	items := make([]NewsItem, 0, len(raw.Data.RollData))
	for _, a := range raw.Data.RollData {
		// 黑名单：广告类电报跳过
		if a.IsAd == 1 {
			continue
		}
		// keyword 在标题/正文中无命中则跳过（财联社没有原生搜索）
		if keyword != "" && !strings.Contains(a.Title, keyword) && !strings.Contains(a.Content, keyword) {
			continue
		}
		t := time.Unix(a.CtimeSec, 0).In(time.Local)
		items = append(items, NewsItem{
			Date:    t.Format("2006-01-02 15:04:05"),
			Time:    t,
			Title:   stripHL(a.Title),
			Content: stripHL(a.Content),
			Source:  "财联社",
			URL:     a.ShareURL,
		})
	}
	return items
}

// fromWind 调用 Wind 财经站内搜索接口。
//
// Wind 的 web 资讯检索接口（无需登录的免费部分）：
//
//	https://www.wind.com.cn/portal/zh/api/portal/news/search
//	  ?keyword=<kw>&page=1&pageSize=20
//
// Wind 接口可能不稳定 / 被风控，失败时直接返回 nil 不阻断。
func (f *NewsFetcher) fromWind(keyword string, limit int) []NewsItem {
	if limit > 30 {
		limit = 30
	}
	u := fmt.Sprintf(
		"https://www.wind.com.cn/portal/zh/api/portal/news/search?keyword=%s&page=1&pageSize=%d",
		url.QueryEscape(keyword), limit,
	)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")
	req.Header.Set("Referer", "https://www.wind.com.cn/")
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var raw struct {
		Data struct {
			List []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Summary     string `json:"summary"`
				Source      string `json:"source"`
				PublishTime string `json:"publishTime"` // "2026-05-25 19:27:02"
			} `json:"list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	items := make([]NewsItem, 0, len(raw.Data.List))
	for _, a := range raw.Data.List {
		t, _ := time.ParseInLocation("2006-01-02 15:04:05", a.PublishTime, time.Local)
		items = append(items, NewsItem{
			Date: a.PublishTime, Time: t,
			Title: stripHL(a.Title), Content: stripHL(a.Summary),
			Source: nonEmpty(a.Source, "Wind"), URL: a.URL,
		})
	}
	return items
}

func stripHL(s string) string {
	s = strings.ReplaceAll(s, "<em>", "")
	s = strings.ReplaceAll(s, "</em>", "")
	return strings.TrimSpace(s)
}

func nonEmpty(a, b string) string {
	if a == "" {
		return b
	}
	return a
}
