package agent

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/eino-multi-etf-strategy/datasource"
	"github.com/eino-multi-etf-strategy/types"
)

// MoneyFlowAgent 资金流向 Agent。
//
// 设计说明（双轨）：
//
//	① 真实数据轨道（优先）：
//	   通过 datasource.MoneyFlowFetcher 接口拉取
//	   - ETF 自身主力/超大单/大单 资金流（EastMoney push2 fflow.kline）
//	   - 沪深港通北向资金（整体口径，作为市场情绪因子）
//	   ETF 净申赎用主力净流入近似（A 股 ETF 没有公开的份额日变动接口）。
//
//	② 估算回退轨道（fallback）：
//	   当真实接口失败 / 数据缺失时，自动回退到原有的 estimate* 函数，
//	   完全基于 K 线 + 量比 + BOP 推导，保证 pipeline 不阻断。
//
//	最终 Summary 中带 [real] 或 [estimate] 标识，便于使用者识别数据来源。
type MoneyFlowAgent struct {
	DS         datasource.ETFDataSource
	HistoryDay int // 默认 30
	AsOf       time.Time
	// UseRealData 控制是否优先使用真实接口；默认 true，失败自动回退。
	UseRealData bool
}

func NewMoneyFlowAgent(ds datasource.ETFDataSource) *MoneyFlowAgent {
	return &MoneyFlowAgent{DS: ds, HistoryDay: 30, UseRealData: true}
}

func (a *MoneyFlowAgent) Run(ctx context.Context, etf types.ScoredETF) (*types.MoneyFlowAnalysis, error) {
	klines := etf.ETF.History
	if len(klines) < 20 {
		k, err := a.DS.GetKLineAsOf(etf.ETF.Code, a.HistoryDay, a.AsOf)
		if err != nil {
			return nil, fmt.Errorf("moneyflow fetch %s: %w", etf.ETF.Code, err)
		}
		klines = k
	}
	if len(klines) < 5 {
		return nil, fmt.Errorf("moneyflow %s real klines insufficient: got %d need >=5", etf.ETF.Code, len(klines))
	}

	res := &types.MoneyFlowAnalysis{ETFCode: etf.ETF.Code}
	dataReal := false

	// ── 真实数据轨道 ──────────────────────────────────────────────────
	if a.UseRealData {
		if mf, ok := a.DS.(datasource.MoneyFlowFetcher); ok {
			realOK := a.fillFromRealAPI(res, mf, etf.ETF.Code)
			dataReal = realOK
		}
	}

	// ── 估算回退（任一字段没填上就走 estimate；保证向下兼容） ────────
	if !dataReal {
		res.NorthCapital5d = estimateNorthCapital(klines, 5)
		res.NorthCapital20d = estimateNorthCapital(klines, 20)
		res.ETFNetSubscribe = estimateETFSubscribe(klines, 5)
		res.MainNetInflow3d = estimateMainInflow(klines, 3)
	}

	res.Score = scoreMoneyFlow(res)
	res.Sentiment = sentimentFromScore(res.Score)
	res.Summary = composeMoneyFlowSummary(res, dataReal)
	return res, nil
}

// fillFromRealAPI 用真实接口数据填充 MoneyFlowAnalysis。
//
// 字段映射：
//   - MainNetInflow3d ← ETF 主力近 3 日净流入累计（亿元，按 asOf 截尾）
//   - ETFNetSubscribe ← ETF 主力近 5 日净流入累计（A 股没公开 ETF 份额日变动，
//                       用主力流入作为"场内申赎"代理；仍比纯估算可靠得多）
//   - NorthCapital5d  ← 北向 5 日整体净流入（亿元，市场情绪因子）
//   - NorthCapital20d ← 北向 20 日整体净流入
//
// 任一关键字段拉取失败即返回 false，由 Run 决定整体回退。
func (a *MoneyFlowAgent) fillFromRealAPI(
	res *types.MoneyFlowAnalysis,
	mf datasource.MoneyFlowFetcher,
	code string,
) bool {
	// 主力流：拉 30 个交易日，按 asOf 截尾
	flow, err := mf.FetchETFMoneyFlow(code, 30)
	if err != nil || len(flow) < 3 {
		return false
	}
	flow = truncateFlowByAsOf(flow, a.AsOf)
	if len(flow) < 3 {
		return false
	}

	// 北向：拉 30 个交易日，按 asOf 截尾
	north, err := mf.FetchNorthboundFlow(60)
	if err != nil || len(north) < 5 {
		return false
	}
	north = truncateNorthByAsOf(north, a.AsOf)
	if len(north) < 5 {
		return false
	}

	res.MainNetInflow3d = roundN(datasource.SumLastN(flow, 3, func(d datasource.MoneyFlowDay) float64 {
		return d.MainNet
	}), 2)
	res.ETFNetSubscribe = roundN(datasource.SumLastN(flow, 5, func(d datasource.MoneyFlowDay) float64 {
		return d.MainNet
	}), 2)
	res.NorthCapital5d = roundN(datasource.SumNorthboundLastN(north, 5), 2)
	res.NorthCapital20d = roundN(datasource.SumNorthboundLastN(north, 20), 2)
	return true
}

// truncateFlowByAsOf 仅保留 Date <= asOf 的数据；asOf 零值时不截。
func truncateFlowByAsOf(days []datasource.MoneyFlowDay, asOf time.Time) []datasource.MoneyFlowDay {
	if asOf.IsZero() {
		return days
	}
	for i := len(days) - 1; i >= 0; i-- {
		if !days[i].Date.After(asOf) {
			return days[:i+1]
		}
	}
	return nil
}

func truncateNorthByAsOf(days []datasource.NorthboundDay, asOf time.Time) []datasource.NorthboundDay {
	if asOf.IsZero() {
		return days
	}
	for i := len(days) - 1; i >= 0; i-- {
		if !days[i].Date.After(asOf) {
			return days[:i+1]
		}
	}
	return nil
}

// estimateNorthCapital 用价格涨幅 × 成交额 × 系数估算"北向资金倾向"。
// 这只是行为代理：真实北向数据应替换此函数。
func estimateNorthCapital(klines []types.KLine, days int) float64 {
	if len(klines) <= days {
		days = len(klines) - 1
	}
	if days <= 0 {
		return 0
	}
	sub := klines[len(klines)-days:]
	total := 0.0
	for i := 0; i < len(sub); i++ {
		change := 0.0
		if i > 0 {
			prev := sub[i-1].Close
			if prev > 0 {
				change = (sub[i].Close - prev) / prev
			}
		}
		// 用涨跌幅 × 成交额作为代理（亿元单位 ≈ volume * close / 1e8）
		amount := sub[i].Volume * sub[i].Close / 1e8
		total += change * amount * 0.05 // 0.05 是经验系数（北向占 ETF 成交比）
	}
	return roundN(total, 2)
}

// estimateETFSubscribe 用量比 × 涨幅估算 ETF 净申购量级（亿元）。
func estimateETFSubscribe(klines []types.KLine, days int) float64 {
	if len(klines) <= days {
		days = len(klines) - 1
	}
	if days <= 0 {
		return 0
	}
	avgVol := 0.0
	baseStart := len(klines) - days - 10
	if baseStart < 0 {
		baseStart = 0
	}
	for i := baseStart; i < len(klines)-days; i++ {
		avgVol += klines[i].Volume
	}
	if div := len(klines) - days - baseStart; div > 0 {
		avgVol /= float64(div)
	}
	if avgVol <= 0 {
		return 0
	}

	total := 0.0
	for i := len(klines) - days; i < len(klines); i++ {
		ratio := klines[i].Volume / avgVol
		change := 0.0
		if i > 0 && klines[i-1].Close > 0 {
			change = (klines[i].Close - klines[i-1].Close) / klines[i-1].Close
		}
		amount := klines[i].Volume * klines[i].Close / 1e8
		total += (ratio - 1) * sign(change) * amount * 0.1
	}
	return roundN(total, 2)
}

// estimateMainInflow 用 (收-开)/(高-低) × 成交额 估算主力净流入。
// 经典 BOP（Balance of Power）变体。
func estimateMainInflow(klines []types.KLine, days int) float64 {
	if len(klines) <= days {
		days = len(klines) - 1
	}
	if days <= 0 {
		return 0
	}
	total := 0.0
	for i := len(klines) - days; i < len(klines); i++ {
		k := klines[i]
		hl := k.High - k.Low
		if hl <= 0 {
			continue
		}
		bop := (k.Close - k.Open) / hl // -1 ~ +1
		amount := k.Volume * k.Close / 1e8
		total += bop * amount * 0.3
	}
	return roundN(total, 2)
}

func scoreMoneyFlow(r *types.MoneyFlowAnalysis) float64 {
	score := 50.0
	score += clampMF(r.NorthCapital5d*5, -15, 15)
	score += clampMF(r.NorthCapital20d*1.5, -10, 10)
	score += clampMF(r.ETFNetSubscribe*8, -10, 10)
	score += clampMF(r.MainNetInflow3d*10, -15, 15)
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	return roundN(score, 2)
}

func sentimentFromScore(score float64) string {
	switch {
	case score >= 65:
		return "positive"
	case score <= 35:
		return "negative"
	default:
		return "neutral"
	}
}

func composeMoneyFlowSummary(r *types.MoneyFlowAnalysis, dataReal bool) string {
	tag := "中性"
	switch r.Sentiment {
	case "positive":
		tag = "净流入"
	case "negative":
		tag = "净流出"
	}
	source := "[estimate]"
	if dataReal {
		source = "[real]"
	}
	return fmt.Sprintf(
		"%s %s：北向 5 日 %.2f / 20 日 %.2f；ETF 5 日净申购 %.2f；主力 3 日净流入 %.2f（亿元）。资金面评分 %.0f。",
		source, tag,
		r.NorthCapital5d, r.NorthCapital20d,
		r.ETFNetSubscribe, r.MainNetInflow3d,
		r.Score,
	)
}

func clampMF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func sign(v float64) float64 {
	if v > 0 {
		return 1
	}
	if v < 0 {
		return -1
	}
	return 0
}

func roundN(v float64, n int) float64 {
	p := math.Pow(10, float64(n))
	return math.Round(v*p) / p
}
