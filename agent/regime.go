package agent

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/eino-multi-etf-strategy/datasource"
	"github.com/eino-multi-etf-strategy/indicator"
	"github.com/eino-multi-etf-strategy/types"
)

// RegimeAgent 宏观环境过滤：基于沪深300ETF（510300）的中长期趋势 + 回撤，
// 推导整体风险偏好状态，作为 FinalAgent 的硬性前置过滤。
//
// 设计原则（资深交易员经验）：
//   - 价格 > MA60 > MA120 → 长牛，position_cap=1.0
//   - 价格 < MA60 但 > MA120 → 中性，position_cap=0.6
//   - 价格 < MA120 且 60 日回撤 > 8% → risk_off，position_cap=0.0
//   - 不依赖 LLM，纯规则推导，结果可重复
type RegimeAgent struct {
	DS         datasource.ETFDataSource
	Benchmark  string // 默认 510300
	HistoryDay int    // 默认 130
	AsOf       time.Time
}

func NewRegimeAgent(ds datasource.ETFDataSource) *RegimeAgent {
	return &RegimeAgent{
		DS:         ds,
		Benchmark:  "510300",
		HistoryDay: 130,
	}
}

func (a *RegimeAgent) Run(ctx context.Context) (*types.RegimeAnalysis, error) {
	klines, err := a.DS.GetKLineAsOf(a.Benchmark, a.HistoryDay, a.AsOf)
	if err != nil {
		return nil, fmt.Errorf("regime fetch %s: %w", a.Benchmark, err)
	}
	if len(klines) < 60 {
		return nil, fmt.Errorf("regime %s real klines insufficient: got %d need >=60", a.Benchmark, len(klines))
	}

	last := klines[len(klines)-1].Close
	ma20 := indicator.MA(klines, 20)
	ma60 := indicator.MA(klines, 60)
	ma120 := indicator.MA(klines, 120)
	dd := drawdown(klines, 60)

	res := &types.RegimeAnalysis{
		Benchmark:    a.Benchmark,
		PriceVsMA20:  diffPct(last, ma20),
		PriceVsMA60:  diffPct(last, ma60),
		PriceVsMA120: diffPct(last, ma120),
		DrawDown60:   dd,
	}
	res.Trend, res.Score, res.PositionCap = classifyRegime(last, ma20, ma60, ma120, dd)
	res.Summary = composeRegimeSummary(res)
	return res, nil
}

// classifyRegime 输出 (trend, score, positionCap)。
//
// PositionCap 设计原则（已对齐聚宽实战 + 保留风控旁路）：
//   - bull / neutral_up / neutral：保持高仓位（1.0/0.95/0.85），让动量信号充分发挥；
//   - bear：降仓 0.4，但仍允许部分动量反弹机会；
//   - risk_off：仅在 MA120 跌破 + 60 日回撤 ≥ 8% 这类系统性风险时强制空仓。
//
// 旧版本（0.5/0.7/1.0）对 bull 没问题，但 neutral/neutral_up 机械打 50%/70% 折扣，
// 在 A 股波段上经常把上涨初期识别成 neutral_up，损失明显。
func classifyRegime(price, ma20, ma60, ma120, dd float64) (string, float64, float64) {
	// risk_off：价格跌破 MA120 且 60 日回撤 ≥ 8%
	if ma120 > 0 && price < ma120 && dd >= 0.08 {
		score := math.Max(0, 30-dd*100) // 回撤越大分越低
		return "risk_off", score, 0.0
	}
	// bear：价格跌破 MA60 且 MA60 < MA120（中期下降）
	if ma60 > 0 && ma120 > 0 && price < ma60 && ma60 < ma120 {
		return "bear", 35, 0.4
	}
	// bull：价格 > MA20 > MA60 > MA120（多头排列）
	if ma20 > 0 && ma60 > 0 && ma120 > 0 && price > ma20 && ma20 > ma60 && ma60 > ma120 {
		return "bull", 85, 1.0
	}
	// neutral_up：价格 > MA60 但未形成完整多头排列
	if ma60 > 0 && price > ma60 {
		return "neutral_up", 65, 0.95
	}
	// neutral：其他
	return "neutral", 50, 0.85
}

func composeRegimeSummary(r *types.RegimeAnalysis) string {
	label := map[string]string{
		"bull":       "强势多头",
		"neutral_up": "中性偏多",
		"neutral":    "震荡整理",
		"bear":       "弱势空头",
		"risk_off":   "系统性风险",
	}[r.Trend]
	if label == "" {
		label = r.Trend
	}
	return fmt.Sprintf(
		"基准 %s：%s（评分 %.0f）。价格相对 MA20 %+.2f%%、MA60 %+.2f%%、MA120 %+.2f%%；近 60 日最大回撤 %.2f%%。建议最大仓位 %.0f%%。",
		r.Benchmark, label, r.Score,
		r.PriceVsMA20*100, r.PriceVsMA60*100, r.PriceVsMA120*100,
		r.DrawDown60*100, r.PositionCap*100,
	)
}

func diffPct(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return (a - b) / b
}

// drawdown 计算最近 n 日 close 的最大回撤（正值）。
func drawdown(klines []types.KLine, n int) float64 {
	if len(klines) < 2 {
		return 0
	}
	if n > len(klines) {
		n = len(klines)
	}
	sub := klines[len(klines)-n:]
	peak := sub[0].Close
	maxDD := 0.0
	for _, k := range sub {
		if k.Close > peak {
			peak = k.Close
		}
		if peak > 0 {
			dd := (peak - k.Close) / peak
			if dd > maxDD {
				maxDD = dd
			}
		}
	}
	return maxDD
}

// CapRecommendation 在 risk_off 环境下，将 FinalAgent 的 recommendation 强制 cap。
// strong_buy/buy → hold（risk_off 时进一步降为 avoid）。
func CapRecommendation(reco string, regime *types.RegimeAnalysis) string {
	if regime == nil {
		return reco
	}
	switch regime.Trend {
	case "risk_off":
		return "avoid"
	case "bear":
		// bear 时降一档
		switch reco {
		case "strong_buy", "buy":
			return "hold"
		}
	}
	return reco
}

// 溢价率风险阈值：高溢价说明场内追捧 / 套利空间被透支，追高风险大。
const (
	PremiumWarnThreshold     = 0.015 // ≥ +1.5% 提示风险，strong_buy → buy
	PremiumDowngradeThreshold = 0.03  // ≥ +3.0% 强制降档，buy → hold；strong_buy → hold
)

// CapByPremium 根据溢价率对 recommendation 做风险降档。
// 仅对正溢价（场内追高）生效；折价（IOPV 高于 Price）不降档。
// 返回降档后的 recommendation 与人话说明（无降档时 note 为空）。
func CapByPremium(reco string, premiumPct float64) (string, string) {
	if premiumPct >= PremiumDowngradeThreshold {
		switch reco {
		case "strong_buy", "buy":
			return "hold", fmt.Sprintf("溢价率 +%.2f%% ≥ %.1f%%，追高风险显著，强制降档为 hold", premiumPct*100, PremiumDowngradeThreshold*100)
		}
	}
	if premiumPct >= PremiumWarnThreshold {
		if reco == "strong_buy" {
			return "buy", fmt.Sprintf("溢价率 +%.2f%% ≥ %.1f%%，追高风险偏高，由 strong_buy 降为 buy", premiumPct*100, PremiumWarnThreshold*100)
		}
	}
	return reco, ""
}

// PremiumRiskLabel 给定溢价率返回风险等级标签，用于报告 / reasoning 展示。
//
//	+3% 以上 → high
//	+1.5%~+3% → elevated
//	-0.5%~+1.5% → normal
//	-0.5% 以下 → discount（折价，可关注套利）
func PremiumRiskLabel(premiumPct float64) string {
	switch {
	case premiumPct >= PremiumDowngradeThreshold:
		return "high"
	case premiumPct >= PremiumWarnThreshold:
		return "elevated"
	case premiumPct <= -0.005:
		return "discount"
	default:
		return "normal"
	}
}
