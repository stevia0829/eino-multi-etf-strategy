package datasource

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// MoneyFlowDay 单日资金流向记录（来自 EastMoney push2/qt/stock/fflow/kline）。
//
// 单位说明：
//   - EastMoney 原始返回为「元」，本结构已统一换算为「亿元」（除以 1e8），
//     与项目其他模块（types.MoneyFlowAnalysis）口径一致，便于直接相加做汇总。
//   - 净流入正负号：正值表示净流入，负值表示净流出。
type MoneyFlowDay struct {
	Date           time.Time `json:"date"`
	MainNet        float64   `json:"main_net"`         // 主力净流入（亿元）= 超大单 + 大单
	SuperLargeNet  float64   `json:"super_large_net"`  // 超大单净流入（亿元）
	LargeNet       float64   `json:"large_net"`        // 大单
	MediumNet      float64   `json:"medium_net"`       // 中单
	SmallNet       float64   `json:"small_net"`        // 小单
	PriceChangePct float64   `json:"price_change_pct"` // 当日涨跌幅（%）
}

// NorthboundDay 沪深港通北向资金单日记录（整体口径，含沪股通+深股通）。
//
//   - Net 单位为「亿元」，正值表示净流入。
//   - 注意：北向是大盘整体口径，无法精确归因到单只 ETF；
//     在 MoneyFlowAgent 中作为「市场情绪」加成因子使用。
type NorthboundDay struct {
	Date time.Time `json:"date"`
	Net  float64   `json:"net"` // 北向当日净买入（亿元）
}

// MoneyFlowFetcher 是可选能力接口：数据源若实现，则可用于补全真实资金流向。
// 通过类型断言在 MoneyFlowAgent 中按需调用，避免破坏 ETFDataSource 主接口
// （与 RealtimeQuoter 同样的模式）。
type MoneyFlowFetcher interface {
	FetchETFMoneyFlow(code string, days int) ([]MoneyFlowDay, error)
	FetchNorthboundFlow(days int) ([]NorthboundDay, error)
}

// ─── 内存缓存（5 分钟） ─────────────────────────────────────────────────
//
// 资金流接口对回测不友好（同一日期会被反复请求），用进程内缓存 + TTL 兜底。
// 不引入外部依赖（redis/file），保证 datasource 包零外部副作用。

type mfCacheEntry struct {
	at   time.Time
	data interface{}
}

var (
	mfCache    = map[string]mfCacheEntry{}
	mfCacheMu  sync.RWMutex
	mfCacheTTL = 5 * time.Minute
)

func mfCacheGet(key string) (interface{}, bool) {
	mfCacheMu.RLock()
	defer mfCacheMu.RUnlock()
	e, ok := mfCache[key]
	if !ok || time.Since(e.at) > mfCacheTTL {
		return nil, false
	}
	return e.data, true
}

func mfCacheSet(key string, v interface{}) {
	mfCacheMu.Lock()
	defer mfCacheMu.Unlock()
	mfCache[key] = mfCacheEntry{at: time.Now(), data: v}
}

// ─── EastMoneyDataSource 实现 ────────────────────────────────────────────

// FetchETFMoneyFlow 拉取 ETF 自身主力资金流向日序列（最近 N 个交易日）。
//
// 接口示例（公开 push2，无需鉴权）：
//
//	https://push2.eastmoney.com/api/qt/stock/fflow/kline/get
//	  ?secid=0.159915          (深 0. / 沪 1.)
//	  &klt=101                  日 K
//	  &fields1=f1,f2,f3,f7
//	  &fields2=f51,f52,f53,f54,f55,f56,f57,f58,f59,f60,f61,f62,f63,f64,f65
//	  &lmt=0                    全量
//
// 返回字段（fields2 解码后逗号分隔）：
//
//	f51=日期 f52=主力净额 f53=小单净额 f54=中单净额 f55=大单净额 f56=超大单净额
//	f57=主力占比% f58=小单占比% f59=中单占比% f60=大单占比% f61=超大单占比%
//	f62=收盘价 f63=涨跌幅% f64,f65=保留
//
// 注意：
//   - ETF 资金流数据在 EastMoney 的 quote.eastmoney.com/sz159915.html 页面"资金流向"tab 已稳定使用，
//     未授权但是公开接口；少数迷你 ETF 可能返回空 klines。
//   - 若 days <= 0 取全部；返回顺序为时间正序（旧→新）。
func (e *EastMoneyDataSource) FetchETFMoneyFlow(code string, days int) ([]MoneyFlowDay, error) {
	cacheKey := fmt.Sprintf("mf:%s:%d", code, days)
	if v, ok := mfCacheGet(cacheKey); ok {
		return v.([]MoneyFlowDay), nil
	}

	secid := etfSecid(code)
	lmt := days
	if lmt <= 0 {
		lmt = 0
	}
	url := fmt.Sprintf(
		"https://push2.eastmoney.com/api/qt/stock/fflow/kline/get?secid=%s&klt=101&fields1=f1,f2,f3,f7&fields2=f51,f52,f53,f54,f55,f56,f57,f58,f59,f60,f61,f62,f63,f64,f65&lmt=%d",
		secid, lmt,
	)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")
	req.Header.Set("Referer", "https://quote.eastmoney.com/")
	resp, err := e.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch etf moneyflow %s: %w", code, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var raw struct {
		Data struct {
			Klines []string `json:"klines"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("parse etf moneyflow %s: %w", code, err)
	}
	if len(raw.Data.Klines) == 0 {
		return nil, fmt.Errorf("etf moneyflow %s empty: %w", code, ErrNoRealData)
	}
	out := make([]MoneyFlowDay, 0, len(raw.Data.Klines))
	for _, line := range raw.Data.Klines {
		parts := strings.Split(line, ",")
		if len(parts) < 13 {
			continue
		}
		t, err := time.Parse("2006-01-02", parts[0])
		if err != nil {
			continue
		}
		out = append(out, MoneyFlowDay{
			Date:           t,
			MainNet:        parseF(parts[1]) / 1e8,
			SmallNet:       parseF(parts[2]) / 1e8,
			MediumNet:      parseF(parts[3]) / 1e8,
			LargeNet:       parseF(parts[4]) / 1e8,
			SuperLargeNet:  parseF(parts[5]) / 1e8,
			PriceChangePct: parseF(parts[12]),
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("etf moneyflow %s parsed empty: %w", code, ErrNoRealData)
	}
	mfCacheSet(cacheKey, out)
	return out, nil
}

// FetchNorthboundFlow 拉取沪深港通北向资金日级净流入序列。
//
// 接口（公开 push2his）：
//
//	https://push2his.eastmoney.com/api/qt/kamt.kline/get
//	  ?fields1=f1,f3,f5
//	  &fields2=f51,f54,f52     f54=沪股通+深股通净流入(元)，f52=保留
//	  &klt=101&lmt=300
//
// 字段说明：
//   - 北向 = 沪股通(SH→HK→SH) + 深股通(SZ→HK→SZ) 当日净买入；EastMoney 已合并；
//   - 单位为「元」，本函数统一换算为「亿元」；
//   - 是大盘整体口径，无法分配到单只 ETF（在 Agent 层用作市场情绪因子）。
//
// 返回顺序为时间正序（旧→新）。
func (e *EastMoneyDataSource) FetchNorthboundFlow(days int) ([]NorthboundDay, error) {
	cacheKey := fmt.Sprintf("nb:%d", days)
	if v, ok := mfCacheGet(cacheKey); ok {
		return v.([]NorthboundDay), nil
	}
	if days <= 0 {
		days = 60
	}
	// 多拉一些以防停牌日；调用方自行截尾。
	url := fmt.Sprintf(
		"https://push2his.eastmoney.com/api/qt/kamt.kline/get?fields1=f1,f3,f5&fields2=f51,f54,f52&klt=101&lmt=%d",
		days+30,
	)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")
	req.Header.Set("Referer", "https://data.eastmoney.com/hsgt/index.html")
	resp, err := e.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch northbound: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var raw struct {
		Data struct {
			S2N []string `json:"s2n"` // 实测 EastMoney 历史返回的字段名
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw); err == nil && len(raw.Data.S2N) > 0 {
		return parseNorthboundLines(cacheKey, raw.Data.S2N, days)
	}
	// 兼容备用字段名（kline）。
	var raw2 struct {
		Data struct {
			Kline []string `json:"klines"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &raw2); err != nil {
		return nil, fmt.Errorf("parse northbound: %w", err)
	}
	if len(raw2.Data.Kline) == 0 {
		return nil, fmt.Errorf("northbound empty: %w", ErrNoRealData)
	}
	return parseNorthboundLines(cacheKey, raw2.Data.Kline, days)
}

func parseNorthboundLines(cacheKey string, lines []string, want int) ([]NorthboundDay, error) {
	out := make([]NorthboundDay, 0, len(lines))
	for _, line := range lines {
		parts := strings.Split(line, ",")
		if len(parts) < 2 {
			continue
		}
		t, err := time.Parse("2006-01-02", parts[0])
		if err != nil {
			continue
		}
		out = append(out, NorthboundDay{
			Date: t,
			Net:  parseF(parts[1]) / 1e8,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("northbound parsed empty: %w", ErrNoRealData)
	}
	if want > 0 && len(out) > want {
		out = out[len(out)-want:]
	}
	mfCacheSet(cacheKey, out)
	return out, nil
}

// etfSecid 把 ETF 6 位代码映射成 EastMoney push2 接口的 secid 形式（市场前缀 + 代码）。
//   - 沪市 (5/6 开头) → "1.xxxxxx"
//   - 深市 (1/0/3 开头) → "0.xxxxxx"
func etfSecid(code string) string {
	if strings.HasPrefix(code, "159") || strings.HasPrefix(code, "15") ||
		strings.HasPrefix(code, "0") || strings.HasPrefix(code, "30") {
		return "0." + code
	}
	return "1." + code
}

// SumLastN 返回最近 n 日的某字段累计（亿元）。pick 用于选取字段。
// 若 days 大于切片长度则按全长累计。
func SumLastN(days []MoneyFlowDay, n int, pick func(MoneyFlowDay) float64) float64 {
	if len(days) == 0 || n <= 0 {
		return 0
	}
	start := len(days) - n
	if start < 0 {
		start = 0
	}
	total := 0.0
	for _, d := range days[start:] {
		total += pick(d)
	}
	return total
}

// SumNorthboundLastN 返回最近 n 个交易日北向净流入累计（亿元）。
func SumNorthboundLastN(days []NorthboundDay, n int) float64 {
	if len(days) == 0 || n <= 0 {
		return 0
	}
	start := len(days) - n
	if start < 0 {
		start = 0
	}
	total := 0.0
	for _, d := range days[start:] {
		total += d.Net
	}
	return total
}
