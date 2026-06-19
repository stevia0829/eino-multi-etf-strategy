package agent

import (
	"context"
	"fmt"

	"github.com/multi-agents-etf-trade-strategy/indicator"
	"github.com/multi-agents-etf-trade-strategy/llm"
	"github.com/multi-agents-etf-trade-strategy/types"
)

type TechnicalAgent struct {
	LLM llm.Client
}

func NewTechnicalAgent(c llm.Client) *TechnicalAgent {
	return &TechnicalAgent{LLM: c}
}

const techSystemPrompt = `你是一名 A 股 ETF 技术面分析师。请基于给定的 K 线指标，做一次开盘前技术面体检。

分析步骤：
1) 趋势判断：MA5/MA20/MA60 排列状态、价格相对均线位置。
2) 动量判断：RSI(14)、20 日动量、量比。
3) 信号判断：MACD 金叉/死叉、是否处于零轴上方。
4) 波动判断：20 日波动率，是否处于压缩末端。
5) 综合给出 trend / score / 100 字 summary，并指出"明日开盘可能的支撑/压力位 (基于 MA5/MA20)"。

只输出 JSON：
{
  "etf_code":"代码",
  "trend":"up | down | sideways",
  "score": 0-100,
  "summary":"<=120 字，包含支撑位/压力位的数字"
}
注意：不要重复 indicators 字段，indicators 与 signals 由系统填充。`

func (a *TechnicalAgent) Run(ctx context.Context, etf types.ScoredETF) (*types.TechnicalAnalysis, error) {
	klines := etf.ETF.History
	ma5 := indicator.MA(klines, 5)
	ma20 := indicator.MA(klines, 20)
	ma60 := indicator.MA(klines, 60)
	rsi := indicator.RSI(klines, 14)
	dif, dea, hist := indicator.MACD(klines)
	mom := indicator.Momentum(klines, 20)
	vr := indicator.VolumeRatio(klines, 5)
	vol := indicator.Volatility(klines, 20)

	signals := computeSignals(ma5, ma20, ma60, rsi, dif, dea, hist)
	indicators := map[string]float64{
		"MA5": ma5, "MA20": ma20, "MA60": ma60,
		"RSI": rsi, "DIF": dif, "DEA": dea, "HIST": hist,
		"Momentum20": mom, "VolRatio": vr, "Volatility": vol,
	}

	user := fmt.Sprintf(
		"ETF: %s(%s)\n当前价: %.3f\n指标: %+v\n信号: %+v\n请输出技术面研判（按 JSON Schema）。",
		etf.ETF.Name, etf.ETF.Code, etf.ETF.Price, indicators, signals,
	)

	res := &types.TechnicalAnalysis{
		ETFCode:    etf.ETF.Code,
		Signals:    signals,
		Indicators: indicators,
	}
	fillTechnicalLevels(res, etf, ma5, ma20, ma60)

	err := callLLMJSON(ctx, a.LLM, techSystemPrompt, user, res, func(raw string) {
		if res.Summary == "" {
			res.Summary = raw
		}
	})
	if err != nil || res.Trend == "" {
		fillRuleTechnical(res, etf, ma5, ma20, ma60)
		return res, nil
	}
	if res.ETFCode == "" {
		res.ETFCode = etf.ETF.Code
	}
	if res.Score == 0 {
		res.Score = etf.Score
	}
	res.Signals = signals
	res.Indicators = indicators
	fillTechnicalLevels(res, etf, ma5, ma20, ma60)
	return res, nil
}

// fillTechnicalLevels 由技术指标推导支撑/阻力以及建议持有区间。
// 持有区间下界 = 一线支撑下方 0.5%；上界 = 阻力上方 1%。
func fillTechnicalLevels(res *types.TechnicalAnalysis, etf types.ScoredETF, ma5, ma20, ma60 float64) {
	res.Support1 = ma20
	res.Support2 = ma60
	res.Resistance = priorHigh(etf.ETF.History, 20)
	if res.Resistance == 0 || res.Resistance < etf.ETF.Price {
		// 若前期高点不可用或已破，取 MA5 与现价较高者再上浮 2%
		base := ma5
		if etf.ETF.Price > base {
			base = etf.ETF.Price
		}
		res.Resistance = base * 1.02
	}
	low := res.Support1 * 0.995
	high := res.Resistance * 1.01
	if low > 0 && high > low {
		res.HoldRange = fmt.Sprintf("%.3f - %.3f", low, high)
	}
}

func priorHigh(klines []types.KLine, n int) float64 {
	if len(klines) == 0 {
		return 0
	}
	start := len(klines) - n
	if start < 0 {
		start = 0
	}
	high := 0.0
	for i := start; i < len(klines); i++ {
		if klines[i].High > high {
			high = klines[i].High
		}
	}
	return high
}

func computeSignals(ma5, ma20, ma60, rsi, dif, dea, hist float64) map[string]string {
	s := map[string]string{}
	switch {
	case ma5 > ma20 && ma20 > ma60:
		s["MA"] = "多头排列"
	case ma5 < ma20 && ma20 < ma60:
		s["MA"] = "空头排列"
	default:
		s["MA"] = "中性纠缠"
	}
	switch {
	case rsi > 70:
		s["RSI"] = "超买"
	case rsi < 30:
		s["RSI"] = "超卖"
	case rsi > 50:
		s["RSI"] = "中性偏强"
	default:
		s["RSI"] = "中性偏弱"
	}
	switch {
	case dif > dea && hist > 0:
		s["MACD"] = "金叉/红柱"
	case dif < dea && hist < 0:
		s["MACD"] = "死叉/绿柱"
	default:
		s["MACD"] = "震荡"
	}
	if dif > 0 && dea > 0 {
		s["MACD零轴"] = "零轴上方"
	} else if dif < 0 && dea < 0 {
		s["MACD零轴"] = "零轴下方"
	} else {
		s["MACD零轴"] = "零轴附近"
	}
	return s
}

func fillRuleTechnical(res *types.TechnicalAnalysis, etf types.ScoredETF, ma5, ma20, ma60 float64) {
	res.ETFCode = etf.ETF.Code
	res.Trend = inferTrend(ma5, ma20, ma60)
	if res.Score == 0 {
		res.Score = etf.Score
	}
	res.Summary = fmt.Sprintf(
		"技术面规则推断：%s。支撑参考 MA20=%.3f / MA60=%.3f；压力参考 MA5=%.3f。",
		res.Trend, ma20, ma60, ma5,
	)
}

func inferTrend(ma5, ma20, ma60 float64) string {
	if ma5 > ma20 && ma20 > ma60 {
		return "up"
	}
	if ma5 < ma20 && ma20 < ma60 {
		return "down"
	}
	return "sideways"
}
