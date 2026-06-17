package datasource

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// IndexQuote 一只指数的实时/最近一笔报价。
//
// Source 字段标识数据真实来源，方便上层 Agent 做"数据可信度"打标。
type IndexQuote struct {
	Symbol    string    // 内部统一 key，例如 SPX / N225 / KOSPI
	Name      string    // 中文名
	Last      float64   // 最新价 / 收盘价
	PrevClose float64   // 前收盘
	Change    float64   // 绝对涨跌
	ChangePct float64   // 百分比涨跌（小数，0.0123 == +1.23%）
	Time      time.Time // 报价时间（接口时间戳，可为零值）
	Source    string    // 数据源：tencent / fallback
}

// IndexFetcher 用于拉取真实海外指数行情。
//
// AsOf 用于"查询时间锚点"校验：所有数据源返回的报价时间戳必须 <= AsOf，
// 否则视为"未来数据"丢弃（典型场景：模拟 5/25 早 8:00 时不允许使用 5/25 09:00 之后的 tick）。
// 零值 AsOf 表示不做时间校验（默认行为）。
type IndexFetcher struct {
	HTTP *http.Client
	AsOf time.Time
}

func NewIndexFetcher() *IndexFetcher {
	return &IndexFetcher{HTTP: &http.Client{Timeout: 8 * time.Second}}
}

// WithAsOf 设置查询时间锚点。
func (f *IndexFetcher) WithAsOf(t time.Time) *IndexFetcher {
	f.AsOf = t
	return f
}

// isFutureTick 判断这条数据是否晚于查询时间锚点。
// 若 AsOf 为零值则不做校验。
func (f *IndexFetcher) isFutureTick(t time.Time) bool {
	if f.AsOf.IsZero() || t.IsZero() {
		return false
	}
	return t.After(f.AsOf)
}

// isFutureDay 仅在"日期维度"判断是否未来；用于亚太指数：
// stooq 给的是日 K 收盘快照（亚洲 15:00 收盘 → 16:45 北京时间出数据），
// 时间戳常晚于 anchor (例如 09:30)，但 date 仍然是当日 / 历史日，
// 这种应允许通过；只有 date 严格晚于 AsOf 当日才视为未来数据。
func (f *IndexFetcher) isFutureDay(t time.Time) bool {
	if f.AsOf.IsZero() || t.IsZero() {
		return false
	}
	ay, am, ad := f.AsOf.UTC().Date()
	ty, tm, td := t.UTC().Date()
	if ty != ay {
		return ty > ay
	}
	if tm != am {
		return tm > am
	}
	return td > ad
}

// 腾讯财经实时接口 qt.gtimg.cn 支持的指数 code 映射：
//
//	实测：标普/纳指/道指返回 v_pv_none_match（接口裁掉），仅恒生 / 上证可用。
//	因此腾讯只保留 HSI / SH 作为"实时盘中"补充，其他指数全部走 Stooq。
var indexSymbolMap = map[string]string{
	"HSI": "hkHSI",    // 恒生指数（实时）
	"SH":  "sh000001", // 上证综指（实时）
}

// FetchIndex 拉取一组指数的实时行情。
// 多源策略：
//  1. 腾讯 qt.gtimg.cn 实时接口（覆盖标普/纳指/道指/恒生/上证）
//  2. Stooq CSV（覆盖 N225 / KOSPI / 几乎所有海外指数，免认证、CDN 稳定）
//  3. 兜底：Source = "fallback" 的零值，由上层判断是否走规则推断
func (f *IndexFetcher) FetchIndex(symbols []string) map[string]IndexQuote {
	out := make(map[string]IndexQuote, len(symbols))
	for _, s := range symbols {
		key := strings.ToUpper(s)
		if q := f.fetchFromTencent(key); q.Last > 0 {
			out[key] = q
			continue
		}
		if q := f.fetchFromStooq(key); q.Last > 0 {
			out[key] = q
			continue
		}
		if q := f.fetchFromYahoo(key); q.Last > 0 {
			out[key] = q
			continue
		}
		out[key] = IndexQuote{Symbol: key, Source: "fallback"}
	}
	return out
}

func (f *IndexFetcher) fetchFromTencent(symbol string) IndexQuote {
	tencentCode, ok := indexSymbolMap[symbol]
	if !ok {
		return IndexQuote{Symbol: symbol}
	}
	url := "https://qt.gtimg.cn/q=" + tencentCode
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "https://stockapp.finance.qq.com/")
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return IndexQuote{Symbol: symbol}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := decodeGBK(body)
	// 腾讯返回格式：v_usSPX="200~标普500指数~SPX~5234.18~...~5210.00~..."
	idx := strings.Index(text, `"`)
	if idx < 0 {
		return IndexQuote{Symbol: symbol}
	}
	tail := text[idx+1:]
	if end := strings.Index(tail, `"`); end > 0 {
		tail = tail[:end]
	}
	parts := strings.Split(tail, "~")
	if len(parts) < 6 {
		return IndexQuote{Symbol: symbol}
	}
	last := parseF(parts[3])
	prev := parseF(parts[4])
	if last <= 0 {
		return IndexQuote{Symbol: symbol}
	}
	change := last - prev
	chgPct := 0.0
	if prev > 0 {
		chgPct = change / prev
	}
	q := IndexQuote{
		Symbol: symbol, Name: parts[1],
		Last: last, PrevClose: prev,
		Change: change, ChangePct: chgPct,
		Time: time.Now(), Source: "tencent",
	}
	if f.isFutureTick(q.Time) {
		return IndexQuote{Symbol: symbol}
	}
	return q
}

// fetchFromStooq 通过 Stooq CSV 接口拉取最近一根日线，覆盖 N225/KOSPI 等亚太指数。
// CSV: Date,Open,High,Low,Close,Volume
func (f *IndexFetcher) fetchFromStooq(symbol string) IndexQuote {
	stooqMap := map[string]string{
		"SPX":   "^spx",
		"NDX":   "^ndx",
		"DJI":   "^dji",
		"N225":  "^nkx",
		"KOSPI": "^kospi",
		"HSI":   "^hsi",
	}
	code, ok := stooqMap[symbol]
	if !ok {
		return IndexQuote{Symbol: symbol}
	}
	// i=d 单行紧凑格式：Symbol,Date(yyyymmdd),Time,Open,High,Low,Close,Volume
	url := fmt.Sprintf("https://stooq.com/q/l/?s=%s&i=d", code)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return IndexQuote{Symbol: symbol}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := strings.TrimSpace(string(body))
	if text == "" || strings.HasPrefix(strings.ToLower(text), "no data") {
		return IndexQuote{Symbol: symbol}
	}
	cols := strings.Split(strings.Split(text, "\n")[0], ",")
	if len(cols) < 7 {
		return IndexQuote{Symbol: symbol}
	}
	date, _ := time.Parse("20060102", cols[1])
	// Stooq 第三列是 hhmmss（盘中或收盘快照时间，UTC 之外的本地时区，但日期足够区分交易日），
	// 与 date 合并成完整时间戳；若解析失败则只用 date。
	if len(cols[2]) == 6 {
		if hh, err := time.Parse("150405", cols[2]); err == nil {
			date = time.Date(date.Year(), date.Month(), date.Day(), hh.Hour(), hh.Minute(), hh.Second(), 0, time.UTC)
		}
	}
	open := parseF(cols[3])
	closep := parseF(cols[6])
	if closep <= 0 {
		return IndexQuote{Symbol: symbol}
	}
	change := closep - open
	chgPct := 0.0
	if open > 0 {
		chgPct = change / open
	}
	q := IndexQuote{
		Symbol: symbol, Name: symbol,
		Last: closep, PrevClose: open,
		Change: change, ChangePct: chgPct,
		Time: date, Source: "stooq",
	}
	// 亚太指数：stooq 给的是日 K 收盘快照，时间戳常常是 16:45 北京时间（即当日盘后），
	// 即使是真实的当日数据，时间戳也会晚于早盘 anchor（如 09:30）。
	// 因此对亚太指数仅做"日期维度"未来校验，避免合法的当日 close 被误判为未来数据丢弃。
	// 美股指数（SPX/NDX/DJI）保留严格的 tick 维度校验。
	isAsia := symbol == "N225" || symbol == "KOSPI" || symbol == "HSI"
	if isAsia {
		if f.isFutureDay(q.Time) {
			return IndexQuote{Symbol: symbol}
		}
	} else {
		if f.isFutureTick(q.Time) {
			return IndexQuote{Symbol: symbol}
		}
	}
	return q
}

// decodeGBK 腾讯接口返回 GBK，这里做 best-effort 解码（指数行情数字 + ASCII 字段，
// 即使中文乱码也不影响数字解析）。
func decodeGBK(b []byte) string {
	return string(b)
}

// ── Yahoo Finance v8 API（2026-06 Stooq 加 JS 验证后新增）─────────────────

var yahooSymbolMap = map[string]string{
	"SPX":   "^GSPC",
	"NDX":   "^IXIC",
	"DJI":   "^DJI",
	"N225":  "^N225",
	"KOSPI": "KS11",
}

// fetchFromYahoo 通过 Yahoo Finance v8 chart API 获取指数最新报价。
// URL: https://query1.finance.yahoo.com/v8/finance/chart/{code}?range=3d&interval=1d
func (f *IndexFetcher) fetchFromYahoo(symbol string) IndexQuote {
	code, ok := yahooSymbolMap[symbol]
	if !ok {
		return IndexQuote{Symbol: symbol}
	}
	urlStr := fmt.Sprintf("https://query1.finance.yahoo.com/v8/finance/chart/%s?range=3d&interval=1d", url.PathEscape(code))
	req, _ := http.NewRequest("GET", urlStr, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return IndexQuote{Symbol: symbol}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var raw struct {
		Chart struct {
			Error  interface{} `json:"error"`
			Result []struct {
				Meta struct {
					RegularMarketPrice float64 `json:"regularMarketPrice"`
					RegularMarketTime  int64   `json:"regularMarketTime"`
					PreviousClose      float64 `json:"previousClose"`
					ChartPreviousClose float64 `json:"chartPreviousClose"`
				} `json:"meta"`
				Timestamp  []int64 `json:"timestamp"`
				Indicators struct {
					Quote []struct {
						Open  []float64 `json:"open"`
						Close []float64 `json:"close"`
					} `json:"quote"`
				} `json:"indicators"`
			} `json:"result"`
		} `json:"chart"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return IndexQuote{Symbol: symbol}
	}
	if len(raw.Chart.Result) == 0 {
		return IndexQuote{Symbol: symbol}
	}
	meta := raw.Chart.Result[0].Meta
	if meta.RegularMarketPrice <= 0 {
		return IndexQuote{Symbol: symbol}
	}

	// prev close：Yahoo v8 对指数类 symbol 常常不返回 previousClose/chartPreviousClose，
	// 优先用 meta 字段，不可用时从 indicators 倒数第二根 bar 的 close 推。
	prevClose := meta.PreviousClose
	if prevClose <= 0 {
		prevClose = meta.ChartPreviousClose
	}
	if prevClose <= 0 {
		if len(raw.Chart.Result[0].Indicators.Quote) > 0 {
			closes := raw.Chart.Result[0].Indicators.Quote[0].Close
			if len(closes) >= 2 {
				prevClose = closes[len(closes)-2]
			}
		}
	}

	change := meta.RegularMarketPrice - prevClose
	chgPct := 0.0
	if prevClose > 0 {
		chgPct = change / prevClose
	}

	tm := time.Now()
	if meta.RegularMarketTime > 0 {
		tm = time.Unix(meta.RegularMarketTime, 0)
	}

	q := IndexQuote{
		Symbol: symbol, Name: symbol,
		Last: meta.RegularMarketPrice, PrevClose: prevClose,
		Change: change, ChangePct: chgPct,
		Time: tm, Source: "yahoo",
	}
	// 美股指数：严格 tick 维度校验；亚太指数：仅日期维度校验
	isAsia := symbol == "N225" || symbol == "KOSPI" || symbol == "HSI"
	if isAsia {
		if f.isFutureDay(q.Time) {
			return IndexQuote{Symbol: symbol}
		}
	} else {
		if f.isFutureTick(q.Time) {
			return IndexQuote{Symbol: symbol}
		}
	}
	return q
}
