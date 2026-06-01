package types

import "time"

type ETF struct {
	Code      string  `json:"code"`
	Name      string  `json:"name"`
	Sector    string  `json:"sector"`
	Price     float64 `json:"price"`
	Volume    float64 `json:"volume"`
	MarketCap float64 `json:"market_cap"`
	PE        float64 `json:"pe"`
	PB        float64 `json:"pb"`
	History   []KLine `json:"history"`
	// IOPV 单位净值估值（盘中实时跟踪标的指数推算的净值），单位元。
	// 数据源：腾讯实时报价 [78] 字段；为 0 时表示未拉取到。
	IOPV float64 `json:"iopv,omitempty"`
	// PremiumPct 溢价率 = (Price - IOPV) / IOPV，单位为小数（0.01 表示 +1%）；
	// 正值表示市价高于净值（场内资金追捧 → 追高风险），
	// 负值表示折价（场内冷淡 / 套利可能）。
	PremiumPct float64 `json:"premium_pct,omitempty"`
}

type KLine struct {
	Date   time.Time `json:"date"`
	Open   float64   `json:"open"`
	High   float64   `json:"high"`
	Low    float64   `json:"low"`
	Close  float64   `json:"close"`
	Volume float64   `json:"volume"`
}

type ScoredETF struct {
	ETF        ETF                `json:"etf"`
	Score      float64            `json:"score"`
	Indicators map[string]float64 `json:"indicators"`
	Reason     string             `json:"reason"`
	// Action 是策略 3 推导的"持仓动作语义"：strong_buy / buy / hold_only / avoid。
	// 完全无状态，由 T 日 / T-1 日 score + R² 决定。
	Action string `json:"action,omitempty"`
	// ActionDesc 为 Action 的中文人话描述，方便报告/CLI 展示。
	ActionDesc string `json:"action_desc,omitempty"`
}

type ScreenerResult struct {
	Top5     []ScoredETF `json:"top5"`
	Best     ScoredETF   `json:"best"`
	AsOfDate time.Time   `json:"as_of_date"`
}

type NewsAnalysis struct {
	// ETFCode 关联的 ETF 代码，便于在批量分析（NewsList）中按代码匹配。
	ETFCode   string   `json:"etf_code,omitempty"`
	Sector    string   `json:"sector"`
	Sentiment string   `json:"sentiment"`
	Score     float64  `json:"score"`
	Highlight []string `json:"highlight"`
	Summary   string   `json:"summary"`
}

type GlobalMarketAnalysis struct {
	USPrev    MarketSnapshot `json:"us_prev"`
	JPToday   MarketSnapshot `json:"jp_today"`
	KRToday   MarketSnapshot `json:"kr_today"`
	Sentiment string         `json:"sentiment"`
	Score     float64        `json:"score"`
	Summary   string         `json:"summary"`
}

type MarketSnapshot struct {
	Index    string  `json:"index"`
	Change   float64 `json:"change"`
	ChangePc float64 `json:"change_pct"`
	Note     string  `json:"note"`
}

type TechnicalAnalysis struct {
	ETFCode    string             `json:"etf_code"`
	Trend      string             `json:"trend"`
	Signals    map[string]string  `json:"signals"`
	Score      float64            `json:"score"`
	Indicators map[string]float64 `json:"indicators"`
	Summary    string             `json:"summary"`
	// 价格关键位 —— 由技术指标推导
	Support1   float64 `json:"support_1"`  // 一线支撑（一般 MA20）
	Support2   float64 `json:"support_2"`  // 二线支撑（一般 MA60）
	Resistance float64 `json:"resistance"` // 阻力位（前期高点 / MA5 上方）
	HoldRange  string  `json:"hold_range"` // 建议持有区间，如 "1.230 - 1.310"
}

type FinalDecision struct {
	TargetETF      ScoredETF            `json:"target_etf"`
	NewsAnalysis   NewsAnalysis         `json:"news_analysis"`
	GlobalAnalysis GlobalMarketAnalysis `json:"global_analysis"`
	TechAnalysis   TechnicalAnalysis    `json:"tech_analysis"`
	OverallScore   float64              `json:"overall_score"`
	Recommendation string               `json:"recommendation"`
	EntryPrice     float64              `json:"entry_price"`
	StopLoss       float64              `json:"stop_loss"`
	TakeProfit     float64              `json:"take_profit"`
	Reasoning      string               `json:"reasoning"`
	GeneratedAt    time.Time            `json:"generated_at"`
	// 加权分数明细
	ScoreBreakdown map[string]float64 `json:"score_breakdown"`
	// Picks 投委会精选 1~2 支推荐买入标的（来自 Top5）。
	Picks []FinalPick `json:"picks,omitempty"`
}

// FinalPick 投委会精选标的，对应 FinalDecision.Picks 数组中的一项。
type FinalPick struct {
	ETFCode        string  `json:"etf_code"`
	ETFName        string  `json:"etf_name"`
	Sector         string  `json:"sector"`
	Recommendation string  `json:"recommendation"`
	Conviction     float64 `json:"conviction"`
	EntryPrice     float64 `json:"entry_price"`
	StopLoss       float64 `json:"stop_loss"`
	TakeProfit     float64 `json:"take_profit"`
	Rationale      string  `json:"rationale"`
}

type AgentState struct {
	Screener  *ScreenerResult       `json:"screener,omitempty"`
	News      *NewsAnalysis         `json:"news,omitempty"`
	Global    *GlobalMarketAnalysis `json:"global,omitempty"`
	Tech      *TechnicalAnalysis    `json:"tech,omitempty"`
	Regime    *RegimeAnalysis       `json:"regime,omitempty"`
	MoneyFlow *MoneyFlowAnalysis    `json:"money_flow,omitempty"`
	Final     *FinalDecision        `json:"final,omitempty"`
	// NewsList 对 Top5 的逐只批量消息面分析。
	NewsList []NewsAnalysis `json:"news_list,omitempty"`
	// TechList 对 Top5 的逐只批量技术面分析。
	TechList []TechnicalAnalysis `json:"tech_list,omitempty"`
	// Memory 由 MemoryAgent 预生成的长期记忆备忘。
	Memory *MemorySummary `json:"memory,omitempty"`
	// CurrentHold 用户当前持仓 ETF 代码，可选；为空时报告中跳过"持仓对照"章节。
	// 仅本次会话使用，系统不做任何本地持久化。
	CurrentHold string `json:"current_hold,omitempty"`
	// HoldAdvice 在 CurrentHold 非空时由 BuildHoldAdvice 计算，包含命中情况与建议。
	HoldAdvice *HoldAdvice `json:"hold_advice,omitempty"`
}

// MemorySummary 由 MemoryAgent 输出，注入 FinalAgent 的长期记忆备忘。
type MemorySummary struct {
	Summary  string        `json:"summary"`
	Patterns []string      `json:"patterns,omitempty"`
	Warnings []string      `json:"warnings,omitempty"`
	Memos    []HistoryMemo `json:"memos,omitempty"`
}

// HistoryMemo 单份历史报告压缩后的关键纪要。
type HistoryMemo struct {
	Date           string  `json:"date"`
	TargetCode     string  `json:"target_code"`
	TargetName     string  `json:"target_name"`
	Sector         string  `json:"sector"`
	OverallScore   float64 `json:"overall_score"`
	Recommendation string  `json:"recommendation"`
	ReasoningGist  string  `json:"reasoning_gist"`
}

// RegimeAnalysis 由 RegimeAgent 输出，用作 FinalAgent 的"宏观环境硬性过滤"。
// 当 Trend == "risk_off" 或 Score < 30 时，FinalAgent 应将 recommendation 强制 cap 到 hold/avoid。
type RegimeAnalysis struct {
	Benchmark    string  `json:"benchmark"`     // 基准代码（默认 510300 沪深300ETF）
	Trend        string  `json:"trend"`         // bull / neutral_up / neutral / bear / risk_off
	Score        float64 `json:"score"`         // 0~100
	PositionCap  float64 `json:"position_cap"`  // 建议最大仓位 0~1
	PriceVsMA20  float64 `json:"price_vs_ma20"` // (price-ma20)/ma20，单位为小数，如 0.02 表示 +2%
	PriceVsMA60  float64 `json:"price_vs_ma60"`
	PriceVsMA120 float64 `json:"price_vs_ma120"`
	DrawDown60   float64 `json:"drawdown_60"` // 60 日最大回撤（正数表示回撤幅度）
	Summary      string  `json:"summary"`
}

// MoneyFlowAnalysis 由 MoneyFlowAgent 输出，反映目标 ETF 的"真实资金行为"。
type MoneyFlowAnalysis struct {
	ETFCode         string  `json:"etf_code"`
	NorthCapital5d  float64 `json:"north_capital_5d"`   // 北向 5 日累计净流入（亿元，估算）
	NorthCapital20d float64 `json:"north_capital_20d"`  // 北向 20 日累计
	ETFNetSubscribe float64 `json:"etf_net_subscribe"`  // ETF 近 5 日净申购（亿元）
	MainNetInflow3d float64 `json:"main_net_inflow_3d"` // 主力 3 日净流入（亿元）
	Score           float64 `json:"score"`              // 0~100
	Sentiment       string  `json:"sentiment"`          // positive / neutral / negative
	Summary         string  `json:"summary"`
}

// HoldAdvice 持仓对照建议（无状态推导）。
type HoldAdvice struct {
	CurrentHold string `json:"current_hold"`
	HitTop      bool   `json:"hit_top"`
	Rank        int    `json:"rank"`        // 1-based；未命中时为 0
	HitName     string `json:"hit_name"`    // 命中时的 ETF 名称
	Action      string `json:"action"`      // 命中时取该 ETF 的 RotationAction；未命中给 "rotate"
	ActionDesc  string `json:"action_desc"` // 中文人话
	BestCode    string `json:"best_code"`   // Top1 代码（轮动目标）
	BestName    string `json:"best_name"`   // Top1 名称
	Suggestion  string `json:"suggestion"`  // 一句话建议
}
