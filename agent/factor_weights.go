package agent

import (
	"strings"

	"github.com/multi-agents-etf-trade-strategy/types"
)

// FactorWeights 描述六个因子在最终评分中的权重轮廓。
// 权重之和 = 1.0；按板块"因子相关性"自适应。
type FactorWeights struct {
	Quant  float64 // 量化动量
	Tech   float64 // 技术面
	News   float64 // 消息面
	Global float64 // 海外联动
	Regime float64 // 宏观环境（沪深300）
	Flow   float64 // 资金面（北向 + ETF 申赎 + 主力代理）
}

// SectorWeightProfile 按"因子-板块相关性"给六类板块分别定制权重。
//
// 设计依据（基于因子本身的物理含义而非主观偏好）：
//  1. 海外 ETF（513520 日经/513100 纳指/513030 德国30 等）：
//     - 标的资产在境外交易，A 股北向资金、沪深300 宏观对其影响极弱；
//     - 直接驱动力是"映射的海外指数当日走势"+"该 ETF 自身的折溢价/流动性技术面"；
//     - 因此 Global 权重抬高至 0.25，Tech 抬到 0.30，Flow 压到 0.05，Regime 压到 0.05。
//  2. 港股 ETF（513130 恒生科技 等）：
//     - 受南向资金 + 港股流动性 + 美股纳指联动驱动；
//     - Global 0.20，Flow 0.10（南向资金代理仍有意义），Regime 0.10。
//  3. A 股科技/新能源（159732/512480 等）：
//     - 受北向资金、沪深300 宏观、消息面（产业政策）影响显著；
//     - 维持原 0.30 量化主导，News/Flow 各 0.15。
//  4. A 股周期（材料/能源/金融/军工/消费/医药/贵金属/农产品/宽基/债券）：
//     - 北向资金 + 宏观 + 消息面共同作用；按子类微调。
//
// 任意 sector 找不到 profile 时退化为 DefaultWeights。
var SectorWeights = map[string]FactorWeights{
	"海外":  {Quant: 0.30, Tech: 0.30, News: 0.05, Global: 0.25, Regime: 0.05, Flow: 0.05},
	"港股":  {Quant: 0.30, Tech: 0.25, News: 0.05, Global: 0.20, Regime: 0.10, Flow: 0.10},
	"科技":  {Quant: 0.30, Tech: 0.25, News: 0.10, Global: 0.10, Regime: 0.10, Flow: 0.15},
	"新能源": {Quant: 0.30, Tech: 0.25, News: 0.10, Global: 0.10, Regime: 0.10, Flow: 0.15},
	"军工":  {Quant: 0.30, Tech: 0.25, News: 0.15, Global: 0.05, Regime: 0.10, Flow: 0.15},
	"医药":  {Quant: 0.30, Tech: 0.25, News: 0.15, Global: 0.05, Regime: 0.10, Flow: 0.15},
	"消费":  {Quant: 0.30, Tech: 0.25, News: 0.10, Global: 0.05, Regime: 0.15, Flow: 0.15},
	"金融":  {Quant: 0.30, Tech: 0.25, News: 0.10, Global: 0.05, Regime: 0.15, Flow: 0.15},
	"地产":  {Quant: 0.30, Tech: 0.20, News: 0.15, Global: 0.05, Regime: 0.20, Flow: 0.10},
	"传媒":  {Quant: 0.30, Tech: 0.25, News: 0.15, Global: 0.05, Regime: 0.10, Flow: 0.15},
	"材料":  {Quant: 0.30, Tech: 0.25, News: 0.10, Global: 0.10, Regime: 0.10, Flow: 0.15},
	"能源":  {Quant: 0.30, Tech: 0.25, News: 0.10, Global: 0.15, Regime: 0.05, Flow: 0.15},
	"贵金属": {Quant: 0.30, Tech: 0.25, News: 0.05, Global: 0.20, Regime: 0.05, Flow: 0.15},
	"农产品": {Quant: 0.30, Tech: 0.25, News: 0.10, Global: 0.10, Regime: 0.10, Flow: 0.15},
	"汽车":  {Quant: 0.30, Tech: 0.25, News: 0.10, Global: 0.05, Regime: 0.15, Flow: 0.15},
	"公用":  {Quant: 0.30, Tech: 0.25, News: 0.05, Global: 0.05, Regime: 0.20, Flow: 0.15},
	"宽基":  {Quant: 0.30, Tech: 0.25, News: 0.05, Global: 0.10, Regime: 0.20, Flow: 0.10},
	"债券":  {Quant: 0.20, Tech: 0.20, News: 0.05, Global: 0.10, Regime: 0.30, Flow: 0.15},
}

// DefaultWeights 当板块未在 SectorWeights 中显式定义时使用的回退权重。
var DefaultWeights = FactorWeights{
	Quant: 0.30, Tech: 0.25, News: 0.10, Global: 0.10, Regime: 0.10, Flow: 0.15,
}

// WeightsForSector 返回某板块的权重轮廓；未知板块走 DefaultWeights。
func WeightsForSector(sector string) FactorWeights {
	if w, ok := SectorWeights[strings.TrimSpace(sector)]; ok {
		return w
	}
	return DefaultWeights
}

// FactorRelevanceNote 为某板块给出"哪些因子强相关 / 哪些弱相关"的可读注释，
// 用于注入到 FinalAgent 的 Prompt，让 LLM 明白"为什么这个权重"。
func FactorRelevanceNote(sector string) string {
	switch strings.TrimSpace(sector) {
	case "海外":
		return "标的为海外指数 ETF：海外联动(Global)是核心驱动；北向资金/沪深300 宏观对其影响极弱，需大幅降权；技术面(Tech)反映 ETF 自身溢价/流动性，仍重要。"
	case "港股":
		return "港股 ETF：南向资金 + 港股流动性 + 美股传导共同驱动；A 股宏观/北向影响中等。"
	case "科技", "新能源":
		return "成长性板块：北向资金 + 产业政策(News) + 宏观风险偏好共同作用；技术面与量化动量为核心。"
	case "金融", "消费", "宽基":
		return "顺周期/大盘板块：宏观环境(Regime)权重抬高；北向资金有效；消息面影响中等。"
	case "贵金属":
		return "避险资产：与美元/美债/海外避险情绪强相关；A 股北向资金影响弱。"
	case "债券":
		return "利率类资产：宏观环境(利率/流动性)是主要驱动；动量与技术面在债券上敏感度低。"
	}
	return "标准 A 股板块权重：量化动量 + 技术面为主，辅以北向资金、宏观与消息面。"
}

// WeightedScoreBySector 按板块自适应权重计算综合分。
func WeightedScoreBySector(st *types.AgentState) (float64, FactorWeights) {
	w := WeightsForSector(st.Screener.Best.ETF.Sector)
	q := st.Screener.Best.Score
	n := scoreOr(st.News, 50)
	g := scoreOrG(st.Global, 50)
	t := scoreOrT(st.Tech, 50)
	r := scoreOrR(st.Regime, 50)
	m := scoreOrM(st.MoneyFlow, 50)
	score := w.Quant*q + w.Tech*t + w.News*n + w.Global*g + w.Regime*r + w.Flow*m
	return score, w
}
