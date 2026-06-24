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
	// IsCurrentHold 标记该候选当前是否为用户持仓（advice 模式下由 Screener 注入）。
	// 仅作为标记用，不参与 Score 计算 / 排序，目的：
	//   - 让 FinalAgent / 报告能在同一份候选列表里高亮"我持有"的标的；
	//   - 让 PreOpen / Cooldown 等执行层逻辑能区分加仓与新开仓。
	IsCurrentHold bool `json:"is_current_hold,omitempty"`
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
	// HoldReviews 针对用户当前持仓的"逐只持仓评审"（advice 模式且 CurrentHolds 非空时输出）。
	// 客观结合候选评分 + 消息面 + 技术面，给出"继续持有 / 减仓观察 / 平仓切换"三档建议，
	// 不影响 Recommendation / Picks（避免持仓主观偏差污染主决策）。
	HoldReviews []HoldReview `json:"hold_reviews,omitempty"`
}

// HoldReview 单只持仓的客观评审结果（FinalAgent 输出 / 规则版兜底）。
type HoldReview struct {
	ETFCode    string  `json:"etf_code"`
	ETFName    string  `json:"etf_name"`
	Sector     string  `json:"sector"`
	InTop      bool    `json:"in_top"`      // 是否进入当日 Top5
	Rank       int     `json:"rank"`        // 在合并候选列表中的名次（1-based）
	Score      float64 `json:"score"`       // 候选量化分（与 Top5 同一量纲）
	Action     string  `json:"action"`      // 客观建议: keep / trim / rotate
	ActionDesc string  `json:"action_desc"` // 中文人话
	NewsBias   string  `json:"news_bias"`   // positive / neutral / negative / unknown
	TechTrend  string  `json:"tech_trend"`  // up / flat / down / unknown
	Rationale  string  `json:"rationale"`   // 1~3 句客观依据
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
	// CurrentHold 用户当前持仓 ETF 代码（取 CurrentHolds[0]，向后兼容旧字段；
	// 已废弃直接读取，新代码请用 CurrentHolds）。
	// 为空时报告中跳过"持仓对照"章节。仅本次会话使用，系统不做任何本地持久化。
	CurrentHold string `json:"current_hold,omitempty"`
	// CurrentHolds 用户当前持仓 ETF 代码列表（advice 模式可填多个），上游通过 --current-hold a,b,c 传入。
	// 仅本次会话使用，系统不做任何本地持久化；为空切片时与未提供持仓等价。
	CurrentHolds []string `json:"current_holds,omitempty"`
	// HoldAdvice 在 CurrentHold 非空时由 BuildHoldAdvice 计算（取首个持仓），向后兼容旧报告。
	HoldAdvice *HoldAdvice `json:"hold_advice,omitempty"`
	// HoldAdvices 多持仓对应的逐只对照建议；CurrentHolds 为空时为 nil。
	HoldAdvices []HoldAdvice `json:"hold_advices,omitempty"`
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
	// 消息面摘要（用于长期记忆中的跨日情绪趋势识别）
	NewsSentiment string  `json:"news_sentiment,omitempty"` // positive/neutral/negative
	NewsScore     float64 `json:"news_score,omitempty"`     // 0-100
	NewsGist      string  `json:"news_gist,omitempty"`      // ≤40字消息面要点
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

// PreOpenSnapshot 单只标的在 9:24 集合竞价时刻的快照与复核结论。
type PreOpenSnapshot struct {
	ETFCode      string  `json:"etf_code"`
	ETFName      string  `json:"etf_name"`
	PrevClose    float64 `json:"prev_close"`
	AuctionPrice float64 `json:"auction_price"` // 虚拟开盘价（撮合价）
	IOPV         float64 `json:"iopv"`
	PremiumPct   float64 `json:"premium_pct"` // (auction-iopv)/iopv
	GapPct       float64 `json:"gap_pct"`     // (auction-prevClose)/prevClose
	EntryPrice   float64 `json:"entry_price"` // 来自 8:50 报告
	EntryGapPct  float64 `json:"entry_gap_pct"`
	Verdict      string  `json:"verdict"` // chase / wait_pullback / abandon / on_target / reduce
	AdjEntry     float64 `json:"adj_entry"`
	AdjStopLoss  float64 `json:"adj_stop_loss"`
	AdjTakeProf  float64 `json:"adj_take_profit"`
	Note         string  `json:"note"`
	// 卖出侧字段（仅 verdict==reduce 时有意义）：来自 FinalAgent.HoldReviews 的 trim/rotate 信号。
	HoldAction   string  `json:"hold_action,omitempty"`    // keep / trim / rotate（来自 HoldReview）
	SellRatio    float64 `json:"sell_ratio,omitempty"`     // 建议减仓比例 0~1（trim=0.5, rotate=1.0）
	SellType     string  `json:"sell_type,omitempty"`      // open_market / limit（开盘卖 / 挂限价单）
	AdjSellPrice float64 `json:"adj_sell_price,omitempty"` // 建议卖出价
}

// PreOpenAnalysis 9:24 PreOpenAgent 综合输出。
type PreOpenAnalysis struct {
	BaseReportPath string            `json:"base_report_path"`
	GeneratedAt    time.Time         `json:"generated_at"`
	CurrentHold    string            `json:"current_hold,omitempty"`  // 兼容旧字段，等价于 CurrentHolds[0]
	CurrentHolds   []string          `json:"current_holds,omitempty"` // 当前已持有 ETF 列表；用于区分加仓与新开仓
	Market         PreOpenSnapshot   `json:"market"`                  // 510300 大盘
	MarketBias     string            `json:"market_bias"`             // strong_up / weak_up / flat / weak_down / strong_down
	Snapshots      []PreOpenSnapshot `json:"snapshots"`               // Final + Picks 合并去重
	Summary        string            `json:"summary"`
	FinalAction    string            `json:"final_action"`
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
