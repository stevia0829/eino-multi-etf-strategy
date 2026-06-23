package datasource

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/multi-agents-etf-trade-strategy/types"
)

type ETFDataSource interface {
	ListAllETFs() ([]types.ETF, error)
	GetKLine(code string, days int) ([]types.KLine, error)
	// GetKLineAsOf 拉取以 asOf 日期（含）为终点的最近 N 条日 K 线。
	// 当 asOf 为零值时等价于 GetKLine（拉取至最新）。
	GetKLineAsOf(code string, days int, asOf time.Time) ([]types.KLine, error)
}

// RealtimeQuote 来自交易所撮合的盘中实时报价，主要用于补充 IOPV / 溢价率。
type RealtimeQuote struct {
	Code      string
	Name      string
	Price     float64   // 最新价
	PrevClose float64   // 昨收价
	IOPV      float64   // 单位净值估值（盘中跟踪标的指数推算）
	ChangePct float64   // 当日涨跌幅 %（场内价口径）
	Time      time.Time // 报价时间
}

// PremiumPct 计算溢价率（Price 相对 IOPV，单位为小数；IOPV<=0 时返回 0）。
func (q RealtimeQuote) PremiumPct() float64 {
	if q.IOPV <= 0 {
		return 0
	}
	return (q.Price - q.IOPV) / q.IOPV
}

// RealtimeQuoter 是可选能力接口：数据源若实现，则可用于补全 IOPV / 溢价率。
// 通过类型断言在 ScreenerAgent 中按需调用，避免破坏 ETFDataSource 主接口。
type RealtimeQuoter interface {
	FetchRealtimeQuote(code string) (RealtimeQuote, error)
}

type EastMoneyDataSource struct {
	HTTPClient *http.Client

	mu      sync.RWMutex
	klCache map[string]klCacheEntry
}

type klCacheEntry struct {
	klines []types.KLine
	at     time.Time
}

var klCacheTTL = 5 * time.Minute

func NewEastMoneyDataSource() *EastMoneyDataSource {
	return &EastMoneyDataSource{
		HTTPClient: &http.Client{Timeout: 15 * time.Second},
		klCache:    make(map[string]klCacheEntry),
	}
}

// ErrNoRealData 在所有真实数据源都失败时返回。
// 上游 Agent 必须显式处理（跳过 / 返回 error），严禁用 mock 兜底。
var ErrNoRealData = errors.New("no real data available from any data source")

func (e *EastMoneyDataSource) ListAllETFs() ([]types.ETF, error) {
	url := "https://push2.eastmoney.com/api/qt/clist/get?pn=1&pz=2000&po=1&np=1&fltt=2&invt=2&fid=f3&fs=b:MK0021,b:MK0022,b:MK0023,b:MK0024&fields=f12,f14,f2,f3,f5,f6,f9,f23,f100"

	resp, err := e.HTTPClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("listAllETFs: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("listAllETFs read body: %w", err)
	}

	var raw struct {
		Data struct {
			Diff []map[string]interface{} `json:"diff"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("listAllETFs unmarshal: %w", err)
	}

	etfs := make([]types.ETF, 0, len(raw.Data.Diff))
	for _, item := range raw.Data.Diff {
		etf := types.ETF{
			Code:   toStr(item["f12"]),
			Name:   toStr(item["f14"]),
			Price:  toFloat(item["f2"]),
			Volume: toFloat(item["f6"]),
			PE:     toFloat(item["f9"]),
			Sector: inferSector(toStr(item["f14"])),
		}
		if etf.Code != "" {
			etfs = append(etfs, etf)
		}
	}

	if len(etfs) == 0 {
		return nil, ErrNoRealData
	}
	return etfs, nil
}

func (e *EastMoneyDataSource) GetKLine(code string, days int) ([]types.KLine, error) {
	return e.GetKLineAsOf(code, days, time.Time{})
}

// GetKLineAsOf 严格只走真实数据源：腾讯前复权 → EastMoney 历史 K 线。
// 全部失败时直接返回 ErrNoRealData，绝不再返回伪造（mock）K 线。
// 内置进程级缓存：同 code+asOf 在 TTL 内直接命中，大幅降低回测 API 调用量。
func (e *EastMoneyDataSource) GetKLineAsOf(code string, days int, asOf time.Time) ([]types.KLine, error) {
	cacheKey := fmt.Sprintf("kl:%s:%s:%d", code, asOf.Format("2006-01-02"), days)

	// 读缓存
	e.mu.RLock()
	if entry, ok := e.klCache[cacheKey]; ok && time.Since(entry.at) < klCacheTTL {
		e.mu.RUnlock()
		return entry.klines, nil
	}
	e.mu.RUnlock()

	var klines []types.KLine
	if klines = e.fetchTencentKLine(code, days, asOf); len(klines) == 0 {
		if klines = e.fetchEastMoneyKLine(code, days, asOf); len(klines) == 0 {
			return nil, fmt.Errorf("getKLineAsOf %s days=%d asOf=%s: %w",
				code, days, asOf.Format("2006-01-02"), ErrNoRealData)
		}
	}

	// 写缓存
	e.mu.Lock()
	e.klCache[cacheKey] = klCacheEntry{klines: klines, at: time.Now()}
	e.mu.Unlock()

	return klines, nil
}

// FetchRealtimeQuote 拉取腾讯实时报价（qt.gtimg.cn），用于补全 IOPV / 溢价率。
//
// 返回字段以 ~ 分隔，关键索引：
//
//	[1]=name (GBK 编码) [3]=最新价 [4]=昨收 [30]=报价时间
//	[32]=涨跌幅%        [78]=IOPV 单位净值估值
//
// 经实测：
//
//	sz159732 → [3]=1.628 [78]=1.6245 → premium=+0.215%
//	sh510300 → [3]=4.972 [78]=4.9628 → premium=+0.186%
//
// 字段不齐 / IOPV=0 / 网络失败均返回 error，由调用方决定是否退化为忽略。
func (e *EastMoneyDataSource) FetchRealtimeQuote(code string) (RealtimeQuote, error) {
	prefix := "sh"
	if strings.HasPrefix(code, "159") || strings.HasPrefix(code, "15") || strings.HasPrefix(code, "0") || strings.HasPrefix(code, "30") {
		prefix = "sz"
	}
	url := fmt.Sprintf("https://qt.gtimg.cn/q=%s%s", prefix, code)
	resp, err := e.HTTPClient.Get(url)
	if err != nil {
		return RealtimeQuote{}, fmt.Errorf("realtime %s: %w", code, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return RealtimeQuote{}, fmt.Errorf("realtime %s read: %w", code, err)
	}
	text := string(body)
	// 形如：v_sh510300="1~沪深300ETF~510300~4.972~...";
	idx := strings.Index(text, "\"")
	if idx < 0 {
		return RealtimeQuote{}, fmt.Errorf("realtime %s: empty payload", code)
	}
	end := strings.LastIndex(text, "\"")
	if end <= idx {
		return RealtimeQuote{}, fmt.Errorf("realtime %s: malformed payload", code)
	}
	parts := strings.Split(text[idx+1:end], "~")
	if len(parts) < 79 {
		return RealtimeQuote{}, fmt.Errorf("realtime %s: too few fields (%d)", code, len(parts))
	}
	q := RealtimeQuote{
		Code:      code,
		Name:      parts[1],
		Price:     parseF(parts[3]),
		PrevClose: parseF(parts[4]),
		ChangePct: parseF(parts[32]),
		IOPV:      parseF(parts[78]),
	}
	// 字段 30 是 yyyyMMddHHmmss
	if t, err := time.ParseInLocation("20060102150405", parts[30], time.Local); err == nil {
		q.Time = t
	}
	if q.Price <= 0 {
		return q, fmt.Errorf("realtime %s: invalid price", code)
	}
	return q, nil
}

// fetchTencentKLine 调用腾讯财经接口拉取前复权日 K 线。
// param 形如：sh518880,day,,2026-04-22,30,qfq
//
// 注意：data.<sec> 字段同时含 day(数组) / qt(对象) / market(数组) 等异构子字段，
// 因此必须先用 json.RawMessage 解到 secKey 一级，再针对 day/qfqday 单独解析，
// 否则刚性类型会因为 qt 是 object 而整体 Unmarshal 失败 → fallthrough 到 mock。
func (e *EastMoneyDataSource) fetchTencentKLine(code string, days int, asOf time.Time) []types.KLine {
	prefix := "sh"
	if strings.HasPrefix(code, "159") || strings.HasPrefix(code, "15") || strings.HasPrefix(code, "0") || strings.HasPrefix(code, "30") {
		prefix = "sz"
	}
	endStr := ""
	if !asOf.IsZero() {
		endStr = asOf.Format("2006-01-02")
	}
	url := fmt.Sprintf("https://web.ifzq.gtimg.cn/appstock/app/fqkline/get?param=%s%s,day,,%s,%d,qfq", prefix, code, endStr, days)
	resp, err := e.HTTPClient.Get(url)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var raw struct {
		Code int                        `json:"code"`
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil || raw.Code != 0 {
		return nil
	}
	secKey := prefix + code
	secRaw, ok := raw.Data[secKey]
	if !ok || len(secRaw) == 0 {
		return nil
	}
	var sec struct {
		Day    [][]interface{} `json:"day"`
		QFQDay [][]interface{} `json:"qfqday"`
	}
	if err := json.Unmarshal(secRaw, &sec); err != nil {
		return nil
	}
	rows := sec.QFQDay
	if len(rows) == 0 {
		rows = sec.Day
	}
	klines := make([]types.KLine, 0, len(rows))
	for _, row := range rows {
		if len(row) < 6 {
			continue
		}
		dateStr, _ := row[0].(string)
		t, _ := time.Parse("2006-01-02", dateStr)
		klines = append(klines, types.KLine{
			Date:   t,
			Open:   parseF(toStrAny(row[1])),
			Close:  parseF(toStrAny(row[2])),
			High:   parseF(toStrAny(row[3])),
			Low:    parseF(toStrAny(row[4])),
			Volume: parseF(toStrAny(row[5])),
		})
	}
	return klines
}

// fetchEastMoneyKLine 备用源：EastMoney 历史 K 线接口（部分网络环境会返回空 body）
func (e *EastMoneyDataSource) fetchEastMoneyKLine(code string, days int, asOf time.Time) []types.KLine {
	secid := "1." + code
	if strings.HasPrefix(code, "159") || strings.HasPrefix(code, "15") {
		secid = "0." + code
	}
	endStr := "20500101"
	if !asOf.IsZero() {
		endStr = asOf.Format("20060102")
	}
	url := fmt.Sprintf("https://push2his.eastmoney.com/api/qt/stock/kline/get?secid=%s&fields1=f1,f2,f3,f4,f5,f6&fields2=f51,f52,f53,f54,f55,f56,f57,f58,f59,f60,f61&klt=101&fqt=1&end=%s&lmt=%d", secid, endStr, days)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")
	req.Header.Set("Referer", "https://quote.eastmoney.com/")
	resp, err := e.HTTPClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var raw struct {
		Data struct {
			Klines []string `json:"klines"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	klines := make([]types.KLine, 0, len(raw.Data.Klines))
	for _, line := range raw.Data.Klines {
		parts := strings.Split(line, ",")
		if len(parts) < 6 {
			continue
		}
		t, _ := time.Parse("2006-01-02", parts[0])
		klines = append(klines, types.KLine{
			Date:   t,
			Open:   parseF(parts[1]),
			Close:  parseF(parts[2]),
			High:   parseF(parts[3]),
			Low:    parseF(parts[4]),
			Volume: parseF(parts[5]),
		})
	}
	return klines
}

func toStrAny(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return fmt.Sprintf("%f", x)
	}
	return ""
}

func inferSector(name string) string {
	switch {
	case strings.Contains(name, "医") || strings.Contains(name, "药") || strings.Contains(name, "生物"):
		return "医药"
	case strings.Contains(name, "芯片") || strings.Contains(name, "半导体") || strings.Contains(name, "科技") || strings.Contains(name, "信息"):
		return "科技"
	case strings.Contains(name, "新能源") || strings.Contains(name, "光伏") || strings.Contains(name, "电池"):
		return "新能源"
	case strings.Contains(name, "证券") || strings.Contains(name, "金融") || strings.Contains(name, "银行") || strings.Contains(name, "保险"):
		return "金融"
	case strings.Contains(name, "酒") || strings.Contains(name, "消费") || strings.Contains(name, "食品"):
		return "消费"
	case strings.Contains(name, "军工") || strings.Contains(name, "国防"):
		return "军工"
	case strings.Contains(name, "地产") || strings.Contains(name, "房"):
		return "地产"
	default:
		return "宽基"
	}
}

func toStr(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func toFloat(v interface{}) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

func parseF(s string) float64 {
	var f float64
	fmt.Sscanf(s, "%f", &f)
	return f
}

func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
