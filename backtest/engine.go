package backtest

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/multi-agents-etf-trade-strategy/agent"
	"github.com/multi-agents-etf-trade-strategy/datasource"
	"github.com/multi-agents-etf-trade-strategy/types"
)

// Trade 单次回测交易记录（状态化每日回测：一笔从 EntryDate 入场到 ExitDate 平仓的完整交易）。
type Trade struct {
	AsOf           time.Time `json:"as_of"`      // 信号日（= EntryDate）
	EntryDate      time.Time `json:"entry_date"` // 入场日（信号产生当日收盘）
	ExitDate       time.Time `json:"exit_date"`  // 平仓日（信号反转或区间结束）
	BestCode       string    `json:"best_code"`
	BestName       string    `json:"best_name"`
	Sector         string    `json:"sector"`
	QuantScore     float64   `json:"quant_score"`
	Recommendation string    `json:"recommendation"`
	RegimeTrend    string    `json:"regime_trend"`
	PositionCap    float64   `json:"position_cap"`
	EntryPrice     float64   `json:"entry_price"`
	ExitPrice      float64   `json:"exit_price"`
	HoldDays       int       `json:"hold_days"`       // 实际持有交易日数
	RawReturnPct   float64   `json:"raw_return_pct"`  // 原始收益（不含仓位、不扣费）
	WeightedReturn float64   `json:"weighted_return"` // 仓位加权收益 = RawReturn × PositionCap
	Win            bool      `json:"win"`             // 净收益（已扣双边费率）> 0
	V2Switched     bool      `json:"v2_switched"`
	V2RejectedTop1 string    `json:"v2_rejected_top1"`
	ExitReason     string    `json:"exit_reason"` // signal_change / regime_off / end_of_range
}

// EquityPoint 权益曲线单点（连续复利累计净值，初始 1.0）。
type EquityPoint struct {
	Date   time.Time `json:"date"`
	Equity float64   `json:"equity"`
}

// Result 回测汇总。
type Result struct {
	StartDate time.Time         `json:"start_date"`
	EndDate   time.Time         `json:"end_date"`
	HoldDays  int               `json:"hold_days"`
	Total     int               `json:"total"`
	Wins      int               `json:"wins"`
	Losses    int               `json:"losses"`
	WinRate   float64           `json:"win_rate"`
	AvgReturn float64           `json:"avg_return"`
	MaxReturn float64           `json:"max_return"`
	MinReturn float64           `json:"min_return"`
	StdReturn float64           `json:"std_return"`
	Sharpe    float64           `json:"sharpe"`
	ByReco    map[string]Bucket `json:"by_recommendation"`
	ByRegime  map[string]Bucket `json:"by_regime"`
	BySector  map[string]Bucket `json:"by_sector"`
	Trades    []Trade           `json:"trades"`

	// ── 新增：风险与基准指标 ─────────────────────────────────────────
	EquityCurve     []EquityPoint `json:"equity_curve"`
	FinalEquity     float64       `json:"final_equity"`  // 最终累计净值
	TotalReturn     float64       `json:"total_return"`  // 累计收益率 = FinalEquity-1
	AnnualReturn    float64       `json:"annual_return"` // 年化收益（按交易日跨度推算）
	MaxDrawdown     float64       `json:"max_drawdown"`  // 最大回撤（正数表示回撤幅度）
	MaxDrawdownDate time.Time     `json:"max_drawdown_date"`
	Calmar          float64       `json:"calmar"`           // 年化 / |MDD|
	Sortino         float64       `json:"sortino"`          // 平均收益 / 下行波动 × sqrt(N)
	ProfitFactor    float64       `json:"profit_factor"`    // 总盈利 / |总亏损|
	AvgWin          float64       `json:"avg_win"`          // 平均盈利
	AvgLoss         float64       `json:"avg_loss"`         // 平均亏损（负数）
	WinLossRatio    float64       `json:"win_loss_ratio"`   // 平均盈利 / |平均亏损|
	BenchmarkCode   string        `json:"benchmark_code"`   // 基准 ETF 代码
	BenchmarkReturn float64       `json:"benchmark_return"` // 基准 buy & hold 收益
	Alpha           float64       `json:"alpha"`            // 策略 - 基准
	CostPerSide     float64       `json:"cost_per_side"`    // 单边成本（已扣除）
}

type Bucket struct {
	Count   int     `json:"count"`
	WinRate float64 `json:"win_rate"`
	AvgRet  float64 `json:"avg_return"`
}

// Engine 回测引擎。
//
// 设计要点：
//   - 不调用任何 LLM Agent（News/Global/Technical/Final-LLM），只用 Screener + Regime + MoneyFlow + 规则版 Final。
//     这样保证回测可重复、零成本、纯量化。
//   - 仓位加权：实际收益 = 原始 5 日收益 × regime.PositionCap。
//   - 仅当 recommendation ∈ {strong_buy, buy} 时才记入交易；hold/avoid 视为空仓（不计 Win/Loss）。
//   - Variant 决定使用纯 V3 评分，还是在 V3 之上叠加 V2 4 道闸门过滤。
type Engine struct {
	DS         datasource.ETFDataSource
	Screener   *agent.ScreenerAgent
	Regime     *agent.RegimeAgent
	MoneyFlow  *agent.MoneyFlowAgent
	HoldDays   int    // 持有 N 个交易日后看收益，默认 5
	MaxSamples int    // 最大样本数，默认 60
	Variant    string // "v3" (默认) 或 "v3v2"
	V2Config   agent.V2FilterConfig
	state      *agent.V2State // V2 模式的跨样本状态
	// CostPerSide 单边交易成本（手续费 + 滑点），默认 0.0008（手续费 0.03% + 滑点 0.05%）。
	// 进出双边各扣一次：net = raw*posCap - 2*CostPerSide。
	CostPerSide float64
	// Benchmark 基准 ETF 代码，默认 510300（沪深300ETF），用于 buy&hold 对比 + Alpha。
	Benchmark string
	// EntryNextOpen 入场时机开关（消除前视偏差）：
	//   false（默认）= 信号日收盘价入场（历史行为，保留以复现旧回测）；
	//   true         = 次日开盘价入场（信号 T 日收盘产生 → T+1 开盘执行，无 lookahead）。
	// 机理：GetKLineAsOf 的 asOf 含当日，Screener/Rotation 在 AsOf=d 时已看到 d 的收盘；
	//       若又用 indexOnOrAfter(d).Close 入场，等于"看了今日收盘才在今日收盘买入"。
	// 开关开时改用 indexAfter(d).Open；末日（最后样本）不开新仓，因其次日开盘已落在回测窗外、
	// 同迭代 force-exit 会在入场前平仓。force-exit 本身不动（只对末日之前入场的仓位生效，时序自洽）。
	EntryNextOpen bool
}

func NewEngine(ds datasource.ETFDataSource) *Engine {
	return &Engine{
		DS:          ds,
		Screener:    agent.NewScreenerAgent(ds),
		Regime:      agent.NewRegimeAgent(ds),
		MoneyFlow:   agent.NewMoneyFlowAgent(ds),
		HoldDays:    5,
		Variant:     "v3",
		V2Config:    agent.DefaultV2FilterConfig(),
		state:       agent.NewV2State(),
		CostPerSide: 0.0008,
		Benchmark:   "510300",
	}
}

// Run 在 [start, end] 区间内**每个交易日**逐日跑回测（状态化）。
//
// 算法（每日 mark-to-market 持仓状态机）：
//
//  1. 用基准 510300 的 K 线确定有效交易日序列；
//
//  2. 维护 currentHold（当前持仓 ETF 代码）+ holdEntry（入场日 K 线）+ entryPrice + posCap；
//
//  3. 每个交易日 d：
//     a) 跑 Screener（asOf=d）→ 当日 best；
//     b) 跑 Regime（asOf=d）→ 当日 PositionCap 与 trend；
//     c) 决策（规则版）→ recommendation；
//     d) 比较：
//     - 若 best.Code == currentHold AND recommendation ∈ {buy,strong_buy}：持有不动
//     - 若 best.Code != currentHold OR recommendation ∈ {hold,avoid}：
//     · 有持仓 → 当日收盘平仓（扣双边费率），登记一笔 Trade；
//     · 若新信号是 buy/strong_buy → 当日收盘按新 best 入场（扣双边费率），更新持仓状态。
//
//  4. 区间末（最后一个交易日）：若仍有持仓 → 强制平仓，ExitReason=end_of_range。
//
// 区别于原"每 N 日采样 + HoldDays 固定持有"模式：
//   - 不采样、按每个交易日跑信号
//   - 持有/平仓由信号一致性驱动，而非固定 5 日
//   - HoldDays 字段保留语义改为"平均持仓天数"，由回测推导
//   - step 参数被忽略
func (e *Engine) Run(ctx context.Context, start, end time.Time, step int) (*Result, error) {
	_ = step // 保留参数兼容，状态化每日回测忽略 step

	// Variant 注入：v3p1 在 v3 主流程之上启用 P1-1（双周期动量）+ P1-2（凸性调整）
	// 默认开启 RegimeAwareP1：仅 bear/risk_off 时真正生效，bull/neutral 时回退到 P0 行为。
	// 注意：MaxScore 由 Rank 内部根据 useConvexity 自动联动（true→30, false→6），
	// 这里不要再改 p.MaxScore，否则会让 bull/neutral 期的过热门槛被无差别放宽，
	// 导致 v3p1 ≠ v3，v3p1 反而比 v3 差（已踩过的坑，见 docs/CHANGELOG-strategy.md）。
	if e.Variant == "v3p1" && e.Screener != nil && e.Screener.Rotation != nil {
		p := e.Screener.Rotation.Params
		p.EnableDualMomentum = true
		p.LongLookback = 252
		p.LongMinAnnualized = 0.0
		p.EnableConvexity = true
		p.ConvexityLookback = 21
		p.ConvexitySigmaFloor = 0.05
		p.RegimeAwareP1 = true
		e.Screener.Rotation.Params = p
	}

	// v3opt：多窗口动量融合 + 滚动分位数入场阈值（P0+P1 优化变体）
	// 注：v3opt 走 JoinQuant 同路径（直接 RotationAgent），不走 Screener
	if e.Variant == "v3opt" {
		// 配置在 joinquant 分支内注入
	}

	// 用基准 ETF（510300）的历史 K 线来确定有效交易日序列。
	// 按区间日数 × 1.5 推算所需 days（日历→交易日冗余充足，不再固定 1500 被打爆）。
	needDays := int(end.Sub(start).Hours()/24)*3/2 + 200
	if needDays < 200 {
		needDays = 200
	}
	if needDays > 1500 {
		needDays = 1500
	}
	baseKlines, err := e.DS.GetKLineAsOf("510300", needDays, end)
	if err != nil || len(baseKlines) == 0 {
		return nil, fmt.Errorf("load benchmark klines: %v", err)
	}

	dates := make([]time.Time, 0)
	for _, k := range baseKlines {
		if !k.Date.Before(start) && !k.Date.After(end) {
			dates = append(dates, k.Date)
		}
	}
	if len(dates) < 2 {
		return nil, fmt.Errorf("not enough trading days: %d", len(dates))
	}
	// 应用 MaxSamples（可选，默认 0 = 不限）；从尾部保留最新的 N 个交易日
	if e.MaxSamples > 0 && len(dates) > e.MaxSamples {
		dates = dates[len(dates)-e.MaxSamples:]
	}

	res := &Result{
		StartDate:     dates[0],
		EndDate:       dates[len(dates)-1],
		HoldDays:      0, // 状态化模式下记平均持仓天数，summarize 阶段补
		ByReco:        map[string]Bucket{},
		ByRegime:      map[string]Bucket{},
		BySector:      map[string]Bucket{},
		CostPerSide:   e.CostPerSide,
		BenchmarkCode: e.Benchmark,
	}

	// 持仓状态
	type holdState struct {
		code        string
		name        string
		sector      string
		quantScore  float64
		regimeTrend string
		posCap      float64
		entryDate   time.Time
		entryPrice  float64
		klineCache  []types.KLine // 入场后预拉的 K 线，用于 mark-to-market 与平仓
		v2Switched  bool
		v2Rejected  string
		recommend   string
	}
	var hold *holdState
	exit := func(d time.Time, exitPrice float64, reason string) {
		if hold == nil || hold.entryPrice <= 0 || exitPrice <= 0 {
			hold = nil
			return
		}
		holdDays := 0
		for _, k := range hold.klineCache {
			if !k.Date.Before(hold.entryDate) && !k.Date.After(d) {
				holdDays++
			}
		}
		if holdDays < 1 {
			holdDays = 1
		}
		raw := (exitPrice - hold.entryPrice) / hold.entryPrice
		weighted := raw * hold.posCap
		// 净收益扣双边费率（用于 Win 标记）
		net := weighted - 2*e.CostPerSide
		res.Trades = append(res.Trades, Trade{
			AsOf:           hold.entryDate,
			EntryDate:      hold.entryDate,
			ExitDate:       d,
			BestCode:       hold.code,
			BestName:       hold.name,
			Sector:         hold.sector,
			QuantScore:     hold.quantScore,
			Recommendation: hold.recommend,
			RegimeTrend:    hold.regimeTrend,
			PositionCap:    hold.posCap,
			EntryPrice:     hold.entryPrice,
			ExitPrice:      exitPrice,
			HoldDays:       holdDays,
			RawReturnPct:   raw,
			WeightedReturn: weighted,
			Win:            net > 0,
			V2Switched:     hold.v2Switched,
			V2RejectedTop1: hold.v2Rejected,
			ExitReason:     reason,
		})
		hold = nil
	}

	// 取某只 ETF 在某交易日的收盘价（用入场后预拉的 cache，避免反复请求）。
	priceOnDate := func(klines []types.KLine, d time.Time) float64 {
		for _, k := range klines {
			if sameDay(k.Date, d) {
				return k.Close
			}
		}
		// 兜底：取最后一根 <= d 的
		var last float64
		for _, k := range klines {
			if !k.Date.After(d) {
				last = k.Close
			}
		}
		return last
	}

	// P0: 滚动分数历史（仅 v3opt 生效），用于计算分位数入场阈值
	scoreHistory := make([]float64, 0, 64)
	const percentileLookback = 60   // 回看窗口：最近 60 个交易日
	const percentileMinSamples = 20 // 冷启动期最少样本（< 20 退化到 rawScore > 0）
	const entryPercentile = 40      // P40：只有分数高于近 60 日 P40 分位才入场

	for di, d := range dates {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		// ── 聚宽对齐模式：直接调 RotationAgent，按聚宽口径满仓持有 rank[0] ──────
		// 旁路 Screener 的所有外围装饰（dedupBySector / RuleBasedDecision / PositionCap）。
		// 聚宽口径：MinScore=0（剔除负分）、MaxScore=6 + 1.1 倍过热门槛、不做板块去重、满仓 100%。
		if e.Variant == "joinquant" || e.Variant == "v3opt" {
			rot := agent.NewRotationAgent(e.DS)
			rot.AsOf = d
			// 关键：MinScore 对齐聚宽 = 0（不允许负分进 rank）
			rot.Params.MinScore = 0
			rot.Params.MaxScore = 6
			rot.Params.ScoreThresholdMultiplier = 1.1
			rot.Params.MDays = 21
			rot.Params.TopN = 0 // 0 = 不截断，保留全部排名

			// P1: 多窗口动量融合（仅 v3opt）
			if e.Variant == "v3opt" {
				rot.Params.MultiWindowWindows = []int{10, 21, 40}
			}

			ranked, rerr := rot.Rank(ctx)
			if rerr != nil || len(ranked) == 0 {
				// rank 为空 → 聚宽口径：清仓
				if hold != nil {
					px := priceOnDate(hold.klineCache, d)
					if px <= 0 {
						px = hold.entryPrice
					}
					exit(d, px, "rank_empty")
				}
				if di == len(dates)-1 && hold != nil {
					px := priceOnDate(hold.klineCache, d)
					if px <= 0 {
						px = hold.entryPrice
					}
					exit(d, px, "end_of_range")
				}
				continue
			}
			top := ranked[0]

			// P0: 滚动分位数入场过滤（仅 v3opt）
			// 累积裸分历史，当样本够时用 P40 分位做门槛
			if e.Variant == "v3opt" {
				scoreHistory = append(scoreHistory, top.Score)
				if len(scoreHistory) > percentileLookback {
					scoreHistory = scoreHistory[1:]
				}
				if len(scoreHistory) >= percentileMinSamples {
					threshold := percentile(scoreHistory, entryPercentile)
					if threshold < 0 {
						threshold = 0
					}
					if top.Score < threshold {
						// 裸分低于近 60 日 P40 → 视为弱信号，不入场
						// 但若已持仓且 rank[0] 仍是持仓标的 → 继续持有（不因门槛平仓）
						if hold != nil && hold.code == top.ETF.Code {
							if di == len(dates)-1 {
								px := priceOnDate(hold.klineCache, d)
								if px <= 0 {
									px = hold.entryPrice
								}
								exit(d, px, "end_of_range")
							}
							continue
						}
						// 弱信号 + 非持仓 → 若有持仓则平仓，否则空仓等待
						if hold != nil {
							px := priceOnDate(hold.klineCache, d)
							if px <= 0 {
								px = hold.entryPrice
							}
							exit(d, px, "weak_signal")
						}
						if di == len(dates)-1 && hold != nil {
							px := priceOnDate(hold.klineCache, d)
							if px <= 0 {
								px = hold.entryPrice
							}
							exit(d, px, "end_of_range")
						}
						continue
					}
				}
			}

			// 满仓 100%
			if hold != nil {
				if hold.code == top.ETF.Code {
					if di == len(dates)-1 {
						px := priceOnDate(hold.klineCache, d)
						if px <= 0 {
							px = hold.entryPrice
						}
						exit(d, px, "end_of_range")
					}
					continue
				}
				// 换仓：先平
				px := priceOnDate(hold.klineCache, d)
				if px <= 0 {
					px = hold.entryPrice
				}
				exit(d, px, "rotation")
			}
			// 入场 top
			tailEnd := dates[len(dates)-1].AddDate(0, 0, 5)
			span := int(tailEnd.Sub(d).Hours()/24) + 30
			if span < 30 {
				span = 30
			}
			klines, kerr := e.DS.GetKLineAsOf(top.ETF.Code, span, tailEnd)
			if kerr != nil || len(klines) == 0 {
				continue
			}
			entryIdx, entryPx := e.entryIndexPrice(klines, d, di == len(dates)-1)
			if entryIdx < 0 || entryPx <= 0 {
				continue
			}
			hold = &holdState{
				code:        top.ETF.Code,
				name:        top.ETF.Name,
				sector:      top.ETF.Sector,
				quantScore:  top.Score,
				regimeTrend: "bull",
				posCap:      1.0, // 满仓
				entryDate:   klines[entryIdx].Date,
				entryPrice:  entryPx,
				klineCache:  klines,
				v2Switched:  false,
				v2Rejected:  "",
				recommend:   "buy",
			}
			if di == len(dates)-1 && hold != nil {
				px := priceOnDate(hold.klineCache, d)
				if px <= 0 {
					px = hold.entryPrice
				}
				exit(d, px, "end_of_range")
			}
			continue
		}

		// 跑 Screener / Regime
		e.Screener.AsOf = d
		e.Regime.AsOf = d
		// ── Regime-aware P1：先跑 Regime，把 trend 注入 Rotation，再跑 Screener ──
		// 这样 P1-1/P1-2 才能根据"当日 trend"决定是否生效（v3p1 才需要，但写在通用路径里没副作用）。
		regime, _ := e.Regime.Run(ctx)
		if e.Screener.Rotation != nil {
			if regime != nil {
				e.Screener.Rotation.Params.RegimeTrend = regime.Trend
			} else {
				e.Screener.Rotation.Params.RegimeTrend = ""
			}
		}
		scr, err := e.Screener.Run(ctx)
		if err != nil || scr == nil || len(scr.Top5) == 0 {
			// 无信号：若有持仓继续持有，下一日再判
			continue
		}
		target := scr.Best
		v2Switched := false
		v2RejectedTop1 := ""

		if e.Variant == "v3v2" {
			if e.state == nil {
				e.state = agent.NewV2State()
			}
			e.state.ResetBanToday()
			e.state.CleanupCooldown(d)
			allowed, decisions := agent.ApplyV2Filter(scr.Top5, e.V2Config, e.state, d)
			if len(decisions) > 0 && !decisions[0].Allowed {
				v2RejectedTop1 = decisions[0].Reason
			}
			if len(allowed) == 0 {
				// V2 全拒：若有持仓按"信号反转"平仓
				if hold != nil {
					px := priceOnDate(hold.klineCache, d)
					if px > 0 {
						exit(d, px, "v2_reject_all")
					}
				}
				continue
			}
			newTarget := allowed[0]
			if newTarget.ETF.Code != scr.Best.ETF.Code {
				v2Switched = true
			}
			target = newTarget
		}

		// ── 对齐聚宽：用 RotationAgent 裸分（Strategy3Score），跳过 sigmoid+R² 归一化 ──
		// Sigmoid 归一化 + R² 置信度乘子会改变排名，导致与聚宽裸分不一致。
		// 权重融合（weightedScore）在实盘 advice 模式才生效，回测只靠裸动量。
		rawScore := target.Indicators["Strategy3Score"]
		if rawScore == 0 {
			rawScore = target.Score / 100 * 6 // 退化：从归一化分反推
		}
		dec := &types.FinalDecision{TargetETF: target}
		dec.OverallScore = target.Score // 报告展示用归一化分
		// 裸分 > 0 即买入（对齐聚宽 MinScore=0 语义）
		if rawScore > 0 {
			dec.Recommendation = "buy"
			if rawScore >= 1.0 {
				dec.Recommendation = "strong_buy"
			}
		} else {
			dec.Recommendation = "hold"
		}
		// regime 仅在 risk_off 时强制清仓，其余满仓（对齐聚宽不降仓）
		dec.Recommendation = agent.CapRecommendation(dec.Recommendation, regime)
		if dec.EntryPrice == 0 {
			dec.EntryPrice = target.ETF.Price
		}
		if dec.StopLoss == 0 {
			dec.StopLoss = agent.DefaultStopLoss(target)
		}
		if dec.TakeProfit == 0 {
			dec.TakeProfit = agent.DefaultTakeProfit(target)
		}

		// 仓位：risk_off → 0，其余满仓 100%（对齐聚宽）
		posCap := 1.0
		regimeTrend := "neutral"
		if regime != nil {
			regimeTrend = regime.Trend
			if regime.Trend == "risk_off" {
				posCap = 0
			}
		}

		// 决策映射：裸分 > 0 即可入场；持仓时只看 rank[0] 是否换标（对齐聚宽）
		shouldEnter := rawScore > 0
		if hold != nil {
			// 已持仓：只有 rank[0] 换标才平仓（对齐聚宽）
			if target.ETF.Code == hold.code {
				continue // 同一标的，无论分数涨跌都不动
			}
			// 换标了 → 平旧仓
			px := priceOnDate(hold.klineCache, d)
			if px > 0 {
				exit(d, px, "rotation")
			} else {
				exit(d, hold.entryPrice, "no_price")
			}
		}

		// 空仓 → 裸分 > 0 时建仓
		if shouldEnter {
			// 拉入场后到区间末的 K 线
			tailEnd := dates[len(dates)-1].AddDate(0, 0, 5)
			span := int(tailEnd.Sub(d).Hours()/24) + 30
			if span < 30 {
				span = 30
			}
			klines, kerr := e.DS.GetKLineAsOf(target.ETF.Code, span, tailEnd)
			if kerr != nil || len(klines) == 0 {
				continue
			}
			entryIdx, entryPx := e.entryIndexPrice(klines, d, di == len(dates)-1)
			if entryIdx < 0 || entryPx <= 0 {
				continue
			}
			hold = &holdState{
				code:        target.ETF.Code,
				name:        target.ETF.Name,
				sector:      target.ETF.Sector,
				quantScore:  target.Score,
				regimeTrend: regimeTrend,
				posCap:      posCap,
				entryDate:   klines[entryIdx].Date,
				entryPrice:  entryPx,
				klineCache:  klines,
				v2Switched:  v2Switched,
				v2Rejected:  v2RejectedTop1,
				recommend:   dec.Recommendation,
			}
		}

		// 最后一个交易日：强制平仓
		if di == len(dates)-1 && hold != nil {
			px := priceOnDate(hold.klineCache, d)
			if px <= 0 {
				px = hold.entryPrice
			}
			exit(d, px, "end_of_range")
		}
	}

	// 区间末再保险一次：若上面的循环因为最后一日有新入场而没平仓
	if hold != nil {
		px := priceOnDate(hold.klineCache, dates[len(dates)-1])
		if px <= 0 {
			px = hold.entryPrice
		}
		exit(dates[len(dates)-1], px, "end_of_range")
	}

	// 基准 buy & hold 收益（同区间的 510300 默认）
	res.BenchmarkReturn = e.computeBenchmarkReturn(res.StartDate, res.EndDate)

	summarize(res, e.CostPerSide)
	return res, nil
}

// computeBenchmarkReturn 计算基准 ETF 在 [start, end] 区间的简单收益率。
// 若拉取失败返回 0（不阻断回测）。
func (e *Engine) computeBenchmarkReturn(start, end time.Time) float64 {
	if e.Benchmark == "" {
		return 0
	}
	// 多拉一些避免端点停牌
	span := int(end.Sub(start).Hours()/24) + 30
	if span < 60 {
		span = 60
	}
	klines, err := e.DS.GetKLineAsOf(e.Benchmark, span, end)
	if err != nil || len(klines) < 2 {
		return 0
	}
	startIdx := indexOnOrAfter(klines, start)
	if startIdx < 0 {
		return 0
	}
	endIdx := -1
	for i := len(klines) - 1; i >= 0; i-- {
		if !klines[i].Date.After(end) {
			endIdx = i
			break
		}
	}
	if endIdx <= startIdx {
		return 0
	}
	entry := klines[startIdx].Close
	exit := klines[endIdx].Close
	if entry <= 0 {
		return 0
	}
	return (exit - entry) / entry
}

// percentile 计算数据集的 p 百分位值（线性插值法）。
// 输入会被排序，p 范围 [0, 100]。
func percentile(data []float64, p float64) float64 {
	if len(data) == 0 {
		return 0
	}
	if len(data) == 1 {
		return data[0]
	}
	sorted := make([]float64, len(data))
	copy(sorted, data)
	sort.Float64s(sorted)
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[len(sorted)-1]
	}
	idx := p / 100 * float64(len(sorted)-1)
	lower := int(math.Floor(idx))
	upper := int(math.Ceil(idx))
	if lower == upper {
		return sorted[lower]
	}
	frac := idx - float64(lower)
	return sorted[lower]*(1-frac) + sorted[upper]*frac
}

// indexOnOrAfter 返回 K 线序列中第一根 Date >= asOf 的索引；找不到返回 -1。
func indexOnOrAfter(klines []types.KLine, asOf time.Time) int {
	for i, k := range klines {
		if k.Date.After(asOf) || sameDay(k.Date, asOf) {
			return i
		}
	}
	return -1
}

// indexAfter 返回 K 线序列中第一根 Date 严格 > asOf 的索引；找不到返回 -1。
// 用于 EntryNextOpen：信号在 asOf 当日收盘产生，次日（严格 > asOf 的第一根）开盘执行。
func indexAfter(klines []types.KLine, asOf time.Time) int {
	for i, k := range klines {
		if k.Date.After(asOf) && !sameDay(k.Date, asOf) {
			return i
		}
	}
	return -1
}

// entryIndexPrice 按 EntryNextOpen 决定入场 K 线索引与执行价。
//
//	EntryNextOpen=false：indexOnOrAfter(d) 当日收盘（历史行为）；
//	EntryNextOpen=true ：indexAfter(d) 次日开盘；末日（isLastSample）不开仓返回 -1，
//	                     因其次日开盘落在回测窗外、同迭代 force-exit 会在入场前平仓。
//
// 找不到合适 K 线或价格无效时返回 (-1, 0)。
func (e *Engine) entryIndexPrice(klines []types.KLine, d time.Time, isLastSample bool) (int, float64) {
	if e.EntryNextOpen {
		if isLastSample {
			return -1, 0
		}
		idx := indexAfter(klines, d)
		if idx < 0 {
			return -1, 0
		}
		return idx, klines[idx].Open
	}
	idx := indexOnOrAfter(klines, d)
	if idx < 0 {
		return -1, 0
	}
	return idx, klines[idx].Close
}

func sameDay(a, b time.Time) bool {
	return a.Year() == b.Year() && a.Month() == b.Month() && a.Day() == b.Day()
}

func summarize(r *Result, costPerSide float64) {
	if len(r.Trades) == 0 {
		return
	}
	executed := 0 // 实际建仓次数（hold/avoid 不计）
	sumRet := 0.0
	maxRet, minRet := -1e9, 1e9
	rets := make([]float64, 0, len(r.Trades))
	netRets := make([]float64, 0, len(r.Trades))    // 已扣手续费/滑点的净收益（用于权益曲线/PF）
	netDates := make([]time.Time, 0, len(r.Trades)) // 与 netRets 对齐的入场日
	sumWin, sumLoss := 0.0, 0.0
	winCount, lossCount := 0, 0
	sumHoldDays := 0 // 实际持仓天数累计（用于推导年化频率）
	r.Total = len(r.Trades)
	for _, t := range r.Trades {
		if t.Recommendation == "hold" || t.Recommendation == "avoid" {
			// 空仓样本不计入收益统计，但仍参与"信号正确率"
			continue
		}
		executed++
		if t.HoldDays > 0 {
			sumHoldDays += t.HoldDays
		}
		if t.Win {
			r.Wins++
		} else {
			r.Losses++
		}
		sumRet += t.WeightedReturn
		rets = append(rets, t.WeightedReturn)
		if t.WeightedReturn > maxRet {
			maxRet = t.WeightedReturn
		}
		if t.WeightedReturn < minRet {
			minRet = t.WeightedReturn
		}
		// 净收益：扣双边成本（进 + 出 各一次）
		net := t.WeightedReturn - 2*costPerSide
		netRets = append(netRets, net)
		netDates = append(netDates, t.AsOf)
		if net > 0 {
			sumWin += net
			winCount++
		} else if net < 0 {
			sumLoss += net // 累加为负
			lossCount++
		}
	}
	if executed > 0 {
		r.WinRate = float64(r.Wins) / float64(executed)
		r.AvgReturn = sumRet / float64(executed)
		r.MaxReturn = maxRet
		r.MinReturn = minRet
		// 标准差
		var sumSq float64
		for _, x := range rets {
			sumSq += (x - r.AvgReturn) * (x - r.AvgReturn)
		}
		r.StdReturn = math.Sqrt(sumSq / float64(executed))
		// 年化频率 = 252 交易日 / 实际平均持仓天数（信号驱动，非固定 5 日）
		avgHold := 5.0
		if executed > 0 && sumHoldDays > 0 {
			avgHold = float64(sumHoldDays) / float64(executed)
		}
		annualFreq := 252.0 / avgHold
		if r.StdReturn > 0 {
			// 简易 Sharpe：年化收益 / 年化波动
			r.Sharpe = (r.AvgReturn * annualFreq) / (r.StdReturn * math.Sqrt(annualFreq))
		}
		r.HoldDays = int(avgHold) // 记录实际平均持仓天数

		// ── 权益曲线（连续复利，初始 1.0）+ MDD ──────────────────────
		equity := 1.0
		curve := make([]EquityPoint, 0, len(netRets)+1)
		curve = append(curve, EquityPoint{Date: r.StartDate, Equity: 1.0})
		peak := 1.0
		mdd := 0.0
		var mddDate time.Time
		for i, ret := range netRets {
			equity *= 1 + ret
			curve = append(curve, EquityPoint{Date: netDates[i], Equity: equity})
			if equity > peak {
				peak = equity
			}
			if peak > 0 {
				dd := (peak - equity) / peak
				if dd > mdd {
					mdd = dd
					mddDate = netDates[i]
				}
			}
		}
		r.EquityCurve = curve
		r.FinalEquity = equity
		r.TotalReturn = equity - 1
		r.MaxDrawdown = mdd
		r.MaxDrawdownDate = mddDate

		// ── 年化收益（按交易日跨度 252 天/年） ─────────────────────────
		days := r.EndDate.Sub(r.StartDate).Hours() / 24
		if days > 0 && equity > 0 {
			years := days / 365.0
			if years > 0 {
				r.AnnualReturn = math.Pow(equity, 1/years) - 1
			}
		}
		if r.MaxDrawdown > 0 {
			r.Calmar = r.AnnualReturn / r.MaxDrawdown
		}

		// ── Sortino：仅下行波动 ─────────────────────────────────────
		var downSumSq float64
		downCount := 0
		netAvg := 0.0
		for _, x := range netRets {
			netAvg += x
		}
		if len(netRets) > 0 {
			netAvg /= float64(len(netRets))
		}
		for _, x := range netRets {
			if x < 0 {
				downSumSq += x * x
				downCount++
			}
		}
		if downCount > 0 {
			downStd := math.Sqrt(downSumSq / float64(downCount))
			if downStd > 0 {
				r.Sortino = (netAvg * annualFreq) / (downStd * math.Sqrt(annualFreq))
			}
		}

		// ── 盈亏比 + 平均盈亏 + Profit Factor ───────────────────────
		if winCount > 0 {
			r.AvgWin = sumWin / float64(winCount)
		}
		if lossCount > 0 {
			r.AvgLoss = sumLoss / float64(lossCount) // 负数
			if r.AvgWin > 0 {
				r.WinLossRatio = r.AvgWin / math.Abs(r.AvgLoss)
			}
		}
		if sumLoss < 0 {
			r.ProfitFactor = sumWin / math.Abs(sumLoss)
		} else if sumWin > 0 {
			r.ProfitFactor = math.Inf(1) // 全胜时返回 +Inf，渲染时显示 ∞
		}

		// ── Alpha = 累计净收益 - 基准 buy&hold ─────────────────────
		r.Alpha = r.TotalReturn - r.BenchmarkReturn
	}

	r.ByReco = bucketize(r.Trades, func(t Trade) string { return t.Recommendation })
	r.ByRegime = bucketize(r.Trades, func(t Trade) string { return t.RegimeTrend })
	r.BySector = bucketize(r.Trades, func(t Trade) string { return t.Sector })
}

func bucketize(trades []Trade, key func(Trade) string) map[string]Bucket {
	tmp := map[string]*[]Trade{}
	for i := range trades {
		k := key(trades[i])
		if k == "" {
			k = "未知"
		}
		if _, ok := tmp[k]; !ok {
			s := make([]Trade, 0)
			tmp[k] = &s
		}
		*tmp[k] = append(*tmp[k], trades[i])
	}
	res := map[string]Bucket{}
	for k, v := range tmp {
		if v == nil || len(*v) == 0 {
			continue
		}
		var wins int
		var sum float64
		exec := 0
		for _, t := range *v {
			if t.Recommendation == "hold" || t.Recommendation == "avoid" {
				continue
			}
			exec++
			if t.Win {
				wins++
			}
			sum += t.WeightedReturn
		}
		b := Bucket{Count: len(*v)}
		if exec > 0 {
			b.WinRate = float64(wins) / float64(exec)
			b.AvgRet = sum / float64(exec)
		}
		res[k] = b
	}
	return res
}

// BuildMarkdown 把 Result 渲染成 markdown 报告。
func BuildMarkdown(r *Result) string {
	var b strings.Builder
	b.WriteString("# 多 Agent 策略历史回测报告\n\n")
	b.WriteString(fmt.Sprintf("- 回测区间: `%s` ~ `%s`\n", r.StartDate.Format("2006-01-02"), r.EndDate.Format("2006-01-02")))
	b.WriteString(fmt.Sprintf("- 持有期: **%d** 个交易日\n", r.HoldDays))
	b.WriteString(fmt.Sprintf("- 样本数: %d（其中实际建仓：%d）\n", r.Total, r.Wins+r.Losses))
	b.WriteString(fmt.Sprintf("- 单边成本: %.4f（已扣除手续费+滑点）；基准: `%s`\n",
		r.CostPerSide, defaultStr(r.BenchmarkCode, "510300")))
	b.WriteString("\n## 一、整体表现\n\n")
	b.WriteString("| 指标 | 数值 |\n|---|---|\n")
	b.WriteString(fmt.Sprintf("| 胜率 | %.2f%% |\n", r.WinRate*100))
	b.WriteString(fmt.Sprintf("| 平均收益（仓位加权）| %.2f%% |\n", r.AvgReturn*100))
	b.WriteString(fmt.Sprintf("| 最大单笔收益 | %.2f%% |\n", r.MaxReturn*100))
	b.WriteString(fmt.Sprintf("| 最大单笔亏损 | %.2f%% |\n", r.MinReturn*100))
	b.WriteString(fmt.Sprintf("| 收益标准差 | %.2f%% |\n", r.StdReturn*100))
	b.WriteString(fmt.Sprintf("| 简易 Sharpe | %.2f |\n\n", r.Sharpe))

	b.WriteString("## 二、风险与基准指标（已扣交易成本）\n\n")
	b.WriteString("| 指标 | 数值 |\n|---|---|\n")
	b.WriteString(fmt.Sprintf("| 累计净值 | %.4f |\n", r.FinalEquity))
	b.WriteString(fmt.Sprintf("| 累计净收益 | %+.2f%% |\n", r.TotalReturn*100))
	b.WriteString(fmt.Sprintf("| 年化收益 | %+.2f%% |\n", r.AnnualReturn*100))
	b.WriteString(fmt.Sprintf("| 最大回撤 | %.2f%% （%s） |\n", r.MaxDrawdown*100, dateOrEmpty(r.MaxDrawdownDate)))
	b.WriteString(fmt.Sprintf("| Calmar | %s |\n", fmtFinite(r.Calmar)))
	b.WriteString(fmt.Sprintf("| Sortino | %s |\n", fmtFinite(r.Sortino)))
	b.WriteString(fmt.Sprintf("| Profit Factor | %s |\n", fmtFinite(r.ProfitFactor)))
	b.WriteString(fmt.Sprintf("| 平均盈利 | %+.2f%% |\n", r.AvgWin*100))
	b.WriteString(fmt.Sprintf("| 平均亏损 | %+.2f%% |\n", r.AvgLoss*100))
	b.WriteString(fmt.Sprintf("| 盈亏比 | %s |\n", fmtFinite(r.WinLossRatio)))
	b.WriteString(fmt.Sprintf("| 基准 buy & hold | %+.2f%% |\n", r.BenchmarkReturn*100))
	b.WriteString(fmt.Sprintf("| Alpha（策略 - 基准） | %+.2f%% |\n\n", r.Alpha*100))

	b.WriteString("## 三、按 Recommendation 分类\n\n")
	writeBucket(&b, r.ByReco)
	b.WriteString("\n## 四、按宏观环境分类\n\n")
	writeBucket(&b, r.ByRegime)
	b.WriteString("\n## 五、按板块分类\n\n")
	writeBucket(&b, r.BySector)

	b.WriteString("\n## 六、近 20 笔交易明细\n\n")
	b.WriteString("| 日期 | 标的 | 板块 | 综合分 | 建议 | 宏观 | 仓位 | 5日收益 | 加权 | 胜? |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")
	tail := r.Trades
	if len(tail) > 20 {
		tail = tail[len(tail)-20:]
	}
	for _, t := range tail {
		win := "❌"
		if t.Win {
			win = "✅"
		}
		b.WriteString(fmt.Sprintf("| %s | %s(%s) | %s | %.2f | %s | %s | %.0f%% | %+.2f%% | %+.2f%% | %s |\n",
			t.AsOf.Format("2006-01-02"), t.BestName, t.BestCode, t.Sector,
			t.QuantScore, t.Recommendation, t.RegimeTrend, t.PositionCap*100,
			t.RawReturnPct*100, t.WeightedReturn*100, win))
	}
	return b.String()
}

func writeBucket(b *strings.Builder, m map[string]Bucket) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b.WriteString("| 分类 | 样本 | 胜率 | 平均加权收益 |\n|---|---|---|---|\n")
	for _, k := range keys {
		v := m[k]
		b.WriteString(fmt.Sprintf("| %s | %d | %.2f%% | %+.2f%% |\n",
			k, v.Count, v.WinRate*100, v.AvgRet*100))
	}
}

// fmtFinite 渲染可能为 +Inf 的指标（如全胜时的 Profit Factor）。
func fmtFinite(v float64) string {
	if math.IsInf(v, 1) {
		return "∞"
	}
	if math.IsInf(v, -1) {
		return "-∞"
	}
	if math.IsNaN(v) {
		return "—"
	}
	return fmt.Sprintf("%.2f", v)
}

func defaultStr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func dateOrEmpty(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Format("2006-01-02")
}

// BuildCompareMarkdown 渲染 V3 vs V3+V2 的 A/B 对比报告。
func BuildCompareMarkdown(v3, v3v2 *Result) string {
	var b strings.Builder
	b.WriteString("# 多 Agent 策略 A/B 对比回测：V3 vs V3+V2\n\n")
	b.WriteString(fmt.Sprintf("- 回测区间: `%s` ~ `%s`\n", v3.StartDate.Format("2006-01-02"), v3.EndDate.Format("2006-01-02")))
	b.WriteString(fmt.Sprintf("- 持有期: **%d** 个交易日\n", v3.HoldDays))
	b.WriteString(fmt.Sprintf("- 单边成本: %.4f；基准: `%s` (buy & hold %+.2f%%)\n\n",
		v3.CostPerSide, defaultStr(v3.BenchmarkCode, "510300"), v3.BenchmarkReturn*100))

	b.WriteString("## 一、整体对比\n\n")
	b.WriteString("| 指标 | V3（纯量化动量） | V3+V2（叠加 4 道闸门） | Δ |\n|---|---|---|---|\n")
	row := func(label string, av, bv float64, fmtPct bool) {
		var sa, sb, sd string
		if fmtPct {
			sa = fmt.Sprintf("%.2f%%", av*100)
			sb = fmt.Sprintf("%.2f%%", bv*100)
			sd = fmt.Sprintf("%+.2f pp", (bv-av)*100)
		} else {
			sa = fmtFinite(av)
			sb = fmtFinite(bv)
			sd = fmt.Sprintf("%+.2f", bv-av)
		}
		b.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n", label, sa, sb, sd))
	}
	rowInt := func(label string, av, bv int) {
		b.WriteString(fmt.Sprintf("| %s | %d | %d | %+d |\n", label, av, bv, bv-av))
	}
	rowInt("样本数", v3.Total, v3v2.Total)
	rowInt("实际建仓数", v3.Wins+v3.Losses, v3v2.Wins+v3v2.Losses)
	rowInt("胜次", v3.Wins, v3v2.Wins)
	rowInt("败次", v3.Losses, v3v2.Losses)
	row("胜率", v3.WinRate, v3v2.WinRate, true)
	row("平均加权收益", v3.AvgReturn, v3v2.AvgReturn, true)
	row("最大单笔收益", v3.MaxReturn, v3v2.MaxReturn, true)
	row("最大单笔亏损", v3.MinReturn, v3v2.MinReturn, true)
	row("收益标准差", v3.StdReturn, v3v2.StdReturn, true)
	row("简易 Sharpe", v3.Sharpe, v3v2.Sharpe, false)
	// 新增：风险与基准指标对比
	row("累计净收益", v3.TotalReturn, v3v2.TotalReturn, true)
	row("年化收益", v3.AnnualReturn, v3v2.AnnualReturn, true)
	row("最大回撤", v3.MaxDrawdown, v3v2.MaxDrawdown, true)
	row("Calmar", v3.Calmar, v3v2.Calmar, false)
	row("Sortino", v3.Sortino, v3v2.Sortino, false)
	row("Profit Factor", v3.ProfitFactor, v3v2.ProfitFactor, false)
	row("盈亏比", v3.WinLossRatio, v3v2.WinLossRatio, false)
	row("Alpha vs 基准", v3.Alpha, v3v2.Alpha, true)

	b.WriteString("\n## 二、按 Recommendation 分桶对比\n\n")
	writeBucketCompare(&b, v3.ByReco, v3v2.ByReco)
	b.WriteString("\n## 三、按宏观环境分桶对比\n\n")
	writeBucketCompare(&b, v3.ByRegime, v3v2.ByRegime)
	b.WriteString("\n## 四、按板块分桶对比\n\n")
	writeBucketCompare(&b, v3.BySector, v3v2.BySector)

	b.WriteString("\n## 五、V3 近 20 笔交易明细\n\n")
	writeTradeTail(&b, v3.Trades, 20)
	b.WriteString("\n## 六、V3+V2 近 20 笔交易明细\n\n")
	writeTradeTail(&b, v3v2.Trades, 20)

	b.WriteString("\n## 七、结论速览\n\n")
	b.WriteString(deriveCompareConclusion(v3, v3v2))
	return b.String()
}

func writeBucketCompare(b *strings.Builder, av, bv map[string]Bucket) {
	keys := map[string]bool{}
	for k := range av {
		keys[k] = true
	}
	for k := range bv {
		keys[k] = true
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)
	b.WriteString("| 分类 | V3 样本 | V3 胜率 | V3 平均收益 | V3+V2 样本 | V3+V2 胜率 | V3+V2 平均收益 |\n")
	b.WriteString("|---|---|---|---|---|---|---|\n")
	for _, k := range ordered {
		x := av[k]
		y := bv[k]
		b.WriteString(fmt.Sprintf("| %s | %d | %.2f%% | %+.2f%% | %d | %.2f%% | %+.2f%% |\n",
			k, x.Count, x.WinRate*100, x.AvgRet*100, y.Count, y.WinRate*100, y.AvgRet*100))
	}
}

// BuildOptCompareMarkdown 对比 v3（基线）与 v3opt（多窗口动量 + 分位数入场阈值）的回测结果。
// BuildLookaheadCompareMarkdown 对比"信号日收盘价入场（现状，含前视偏差）" vs
// "次日开盘价入场（EntryNextOpen，无前视）"，量化前视偏差对回测指标的高估程度。
// close = EntryNextOpen=false（现状），nextOpen = EntryNextOpen=true。
func BuildLookaheadCompareMarkdown(close, nextOpen *Result) string {
	var b strings.Builder
	b.WriteString("# 回测对比：入场时机（前视偏差 A/B）\n\n")
	b.WriteString(fmt.Sprintf("- 回测区间: **%s ~ %s**\n",
		close.StartDate.Format("2006-01-02"), close.EndDate.Format("2006-01-02")))
	b.WriteString(fmt.Sprintf("- 基准: `%s` (buy & hold %+.2f%%)\n\n",
		defaultStr(close.BenchmarkCode, "510300"), close.BenchmarkReturn*100))
	b.WriteString("> **A = 信号日收盘价入场**（历史行为，含前视偏差：用了当日收盘这个事后价）\n")
	b.WriteString("> **B = 次日开盘价入场**（EntryNextOpen，无前视，更贴近真实可执行）\n")
	b.WriteString("> 预期 B 的收益/Sharpe 通常低于 A；B 与 A 的差距 ≈ 前视偏差的高估幅度。\n\n")

	b.WriteString("## 一、整体对比\n\n")
	b.WriteString("| 指标 | A 信号日收盘 | B 次日开盘 | Δ(B-A) |\n|---|---|---|---|\n")
	row := func(label string, av, bv float64, fmtPct bool) {
		var sa, sb, sd string
		if fmtPct {
			sa, sb = fmt.Sprintf("%.2f%%", av*100), fmt.Sprintf("%.2f%%", bv*100)
			sd = fmt.Sprintf("%+.2f pp", (bv-av)*100)
		} else {
			sa, sb = fmtFinite(av), fmtFinite(bv)
			sd = fmt.Sprintf("%+.2f", bv-av)
		}
		b.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n", label, sa, sb, sd))
	}
	rowInt := func(label string, av, bv int) {
		b.WriteString(fmt.Sprintf("| %s | %d | %d | %+d |\n", label, av, bv, bv-av))
	}
	rowInt("实际建仓数", close.Wins+close.Losses, nextOpen.Wins+nextOpen.Losses)
	row("胜率", close.WinRate, nextOpen.WinRate, true)
	row("平均加权收益", close.AvgReturn, nextOpen.AvgReturn, true)
	row("简易 Sharpe", close.Sharpe, nextOpen.Sharpe, false)
	row("累计净收益", close.TotalReturn, nextOpen.TotalReturn, true)
	row("年化收益", close.AnnualReturn, nextOpen.AnnualReturn, true)
	row("最大回撤", close.MaxDrawdown, nextOpen.MaxDrawdown, true)
	row("Calmar", close.Calmar, nextOpen.Calmar, false)
	row("Alpha vs 基准", close.Alpha, nextOpen.Alpha, true)

	b.WriteString("\n## 二、按宏观环境分桶对比\n\n")
	writeBucketCompareLabeled(&b, close.ByRegime, nextOpen.ByRegime, "A收盘", "B次日开盘")
	b.WriteString("\n## 三、按板块分桶对比\n\n")
	writeBucketCompareLabeled(&b, close.BySector, nextOpen.BySector, "A收盘", "B次日开盘")

	b.WriteString("\n---\n")
	b.WriteString("> ⚠️ 若 B（无前视）的 Sharpe/Calmar 仍显著为正且回撤可控，说明策略在可执行口径下依然有效；\n")
	b.WriteString("> 若 B 大幅劣化，则历史回测收益有相当部分来自前视偏差，需对 A 的结论打折扣。\n")
	return b.String()
}

func BuildOptCompareMarkdown(v3, v3opt *Result) string {
	var b strings.Builder
	b.WriteString("# 回测对比：v3 基线 vs v3opt（P0+P1 优化）\n\n")
	b.WriteString(fmt.Sprintf("- 回测区间: **%s ~ %s** (%d 交易日)\n",
		v3.StartDate.Format("2006-01-02"), v3.EndDate.Format("2006-01-02"), v3.Total))
	b.WriteString(fmt.Sprintf("- 基准: `%s` (buy & hold %+.2f%%)\n\n",
		defaultStr(v3.BenchmarkCode, "510300"), v3.BenchmarkReturn*100))

	b.WriteString("## 优化说明\n\n")
	b.WriteString("- **P1-a 多窗口动量融合**: 对 {10, 21, 40} 三个回归窗口分别计算加权对数回归动量分，按各自 R² 加权融合\n")
	b.WriteString("  - 短窗口(10日)捕获快速趋势，长窗口(40日)过滤噪音，融合后跨行情节奏更鲁棒\n")
	b.WriteString("  - 融合公式: `fused = Σ max(R²_m, 0) × score_m / Σ max(R²_m, 0)`\n")
	b.WriteString("- **P0 滚动分位数入场阈值**: 用近 60 日裸分 P40 分位作为入场门槛替代固定 0\n")
	b.WriteString("  - 牛市门槛自动提高（过滤弱信号），熊市退化到 >0（不买负动量）\n")
	b.WriteString("  - 冷启动期（< 20 日）退化到原始 `rawScore > 0` 行为\n\n")

	b.WriteString("## 一、整体对比\n\n")
	b.WriteString("| 指标 | v3 基线 | v3opt 优化 | Δ |\n|---|---|---|---|\n")
	row := func(label string, av, bv float64, fmtPct bool) {
		var sa, sb, sd string
		if fmtPct {
			sa = fmt.Sprintf("%.2f%%", av*100)
			sb = fmt.Sprintf("%.2f%%", bv*100)
			sd = fmt.Sprintf("%+.2f pp", (bv-av)*100)
		} else {
			sa = fmtFinite(av)
			sb = fmtFinite(bv)
			sd = fmt.Sprintf("%+.2f", bv-av)
		}
		b.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n", label, sa, sb, sd))
	}
	rowInt := func(label string, av, bv int) {
		b.WriteString(fmt.Sprintf("| %s | %d | %d | %+d |\n", label, av, bv, bv-av))
	}
	rowInt("样本数", v3.Total, v3opt.Total)
	rowInt("实际建仓数", v3.Wins+v3.Losses, v3opt.Wins+v3opt.Losses)
	rowInt("胜次", v3.Wins, v3opt.Wins)
	rowInt("败次", v3.Losses, v3opt.Losses)
	row("胜率", v3.WinRate, v3opt.WinRate, true)
	row("平均加权收益", v3.AvgReturn, v3opt.AvgReturn, true)
	row("最大单笔收益", v3.MaxReturn, v3opt.MaxReturn, true)
	row("最大单笔亏损", v3.MinReturn, v3opt.MinReturn, true)
	row("收益标准差", v3.StdReturn, v3opt.StdReturn, true)
	row("简易 Sharpe", v3.Sharpe, v3opt.Sharpe, false)
	row("累计净收益", v3.TotalReturn, v3opt.TotalReturn, true)
	row("年化收益", v3.AnnualReturn, v3opt.AnnualReturn, true)
	row("最大回撤", v3.MaxDrawdown, v3opt.MaxDrawdown, true)
	row("Calmar", v3.Calmar, v3opt.Calmar, false)
	row("Sortino", v3.Sortino, v3opt.Sortino, false)
	row("Profit Factor", v3.ProfitFactor, v3opt.ProfitFactor, false)
	row("盈亏比", v3.WinLossRatio, v3opt.WinLossRatio, false)
	row("Alpha vs 基准", v3.Alpha, v3opt.Alpha, true)

	b.WriteString("\n## 二、按 Recommendation 分桶对比\n\n")
	writeBucketCompareLabeled(&b, v3.ByReco, v3opt.ByReco, "v3", "v3opt")
	b.WriteString("\n## 三、按宏观环境分桶对比\n\n")
	writeBucketCompareLabeled(&b, v3.ByRegime, v3opt.ByRegime, "v3", "v3opt")
	b.WriteString("\n## 四、按板块分桶对比\n\n")
	writeBucketCompareLabeled(&b, v3.BySector, v3opt.BySector, "v3", "v3opt")

	b.WriteString("\n## 五、v3 基线 近 20 笔交易明细\n\n")
	writeTradeTail(&b, v3.Trades, 20)
	b.WriteString("\n## 六、v3opt 优化 近 20 笔交易明细\n\n")
	writeTradeTail(&b, v3opt.Trades, 20)

	b.WriteString("\n## 七、结论速览\n\n")
	dSharpe := v3opt.Sharpe - v3.Sharpe
	dRet := (v3opt.TotalReturn - v3.TotalReturn) * 100
	dMDD := (v3opt.MaxDrawdown - v3.MaxDrawdown) * 100
	dCalmar := v3opt.Calmar - v3.Calmar
	b.WriteString(fmt.Sprintf("- Sharpe 变化：%+.4f（v3 %.4f → v3opt %.4f）\n", dSharpe, v3.Sharpe, v3opt.Sharpe))
	b.WriteString(fmt.Sprintf("- 累计净收益变化：%+.2f pp（v3 %+.2f%% → v3opt %+.2f%%）\n", dRet, v3.TotalReturn*100, v3opt.TotalReturn*100))
	b.WriteString(fmt.Sprintf("- 最大回撤变化：%+.2f pp（v3 %.2f%% → v3opt %.2f%%）\n", dMDD, v3.MaxDrawdown*100, v3opt.MaxDrawdown*100))
	b.WriteString(fmt.Sprintf("- Calmar 变化：%+.2f（v3 %s → v3opt %s）\n", dCalmar, fmtFinite(v3.Calmar), fmtFinite(v3opt.Calmar)))
	switch {
	case dSharpe > 0.05 && dMDD <= 0:
		b.WriteString("- 综合判断：**v3opt 显著提升 Sharpe 且不放大回撤，建议合入主流程**。\n")
	case dSharpe > 0.05 && dMDD > 0:
		b.WriteString("- 综合判断：v3opt 提升 Sharpe 但回撤略放大，建议保留为可选开关。\n")
	case dSharpe > 0 && dSharpe <= 0.05:
		b.WriteString("- 综合判断：v3opt 有微弱改善，收益增量不显著，建议扩大样本再做判断。\n")
	default:
		b.WriteString("- 综合判断：v3opt 在该区间未带来改善，甚至可能过拟合，建议换区间验证。\n")
	}
	return b.String()
}

func writeTradeTail(b *strings.Builder, trades []Trade, n int) {
	b.WriteString("| 日期 | 标的 | 板块 | 综合分 | 建议 | 宏观 | 仓位 | 5日收益 | 加权 | 胜? |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")
	tail := trades
	if len(tail) > n {
		tail = tail[len(tail)-n:]
	}
	for _, t := range tail {
		win := "❌"
		if t.Win {
			win = "✅"
		}
		b.WriteString(fmt.Sprintf("| %s | %s(%s) | %s | %.2f | %s | %s | %.0f%% | %+.2f%% | %+.2f%% | %s |\n",
			t.AsOf.Format("2006-01-02"), t.BestName, t.BestCode, t.Sector,
			t.QuantScore, t.Recommendation, t.RegimeTrend, t.PositionCap*100,
			t.RawReturnPct*100, t.WeightedReturn*100, win))
	}
}

func deriveCompareConclusion(v3, v3v2 *Result) string {
	dWin := (v3v2.WinRate - v3.WinRate) * 100
	dRet := (v3v2.AvgReturn - v3.AvgReturn) * 100
	dStd := (v3v2.StdReturn - v3.StdReturn) * 100
	dSharpe := v3v2.Sharpe - v3.Sharpe
	var b strings.Builder
	b.WriteString(fmt.Sprintf("- 胜率变化：%+.2f pp（V3 %.2f%% → V3+V2 %.2f%%）\n", dWin, v3.WinRate*100, v3v2.WinRate*100))
	b.WriteString(fmt.Sprintf("- 平均加权收益变化：%+.2f pp（V3 %+.2f%% → V3+V2 %+.2f%%）\n", dRet, v3.AvgReturn*100, v3v2.AvgReturn*100))
	b.WriteString(fmt.Sprintf("- 收益波动变化：%+.2f pp（V3 %.2f%% → V3+V2 %.2f%%）\n", dStd, v3.StdReturn*100, v3v2.StdReturn*100))
	b.WriteString(fmt.Sprintf("- Sharpe 变化：%+.2f\n", dSharpe))
	switch {
	case dSharpe > 0.1 && dWin >= 0:
		b.WriteString("- 综合判断：V2 4 道闸门在该区间显著提升风险调整后收益，建议保留启用。\n")
	case dSharpe > 0 && dWin >= -2:
		b.WriteString("- 综合判断：V2 4 道闸门带来温和改善，建议在波动放大时保留。\n")
	case dSharpe < -0.1:
		b.WriteString("- 综合判断：V2 4 道闸门在该区间反而拖累收益，建议放宽参数或仅在熊市启用。\n")
	default:
		b.WriteString("- 综合判断：两者表现接近，需要扩大样本/区间再做结论。\n")
	}
	return b.String()
}

// BuildP1CompareMarkdown 渲染 P0(v3) vs P0+P1(v3p1) 的 A/B 对比报告。
func BuildP1CompareMarkdown(v3, v3p1 *Result) string {
	var b strings.Builder
	b.WriteString("# P0 vs P0+P1 对比回测：双周期动量 + 凸性调整\n\n")
	b.WriteString(fmt.Sprintf("- 回测区间: `%s` ~ `%s`\n", v3.StartDate.Format("2006-01-02"), v3.EndDate.Format("2006-01-02")))
	b.WriteString(fmt.Sprintf("- 持有期: **%d** 个交易日\n", v3.HoldDays))
	b.WriteString(fmt.Sprintf("- 单边成本: %.4f；基准: `%s` (buy & hold %+.2f%%)\n",
		v3.CostPerSide, defaultStr(v3.BenchmarkCode, "510300"), v3.BenchmarkReturn*100))
	b.WriteString("- P1 改造：① 252 日年化 ≥ 0 的双周期动量过滤（Antonacci 2014）；② 凸性调整 score := score / max(σ_21, 0.05)（Daniel & Moskowitz 2016）\n\n")

	b.WriteString("## 一、整体对比\n\n")
	b.WriteString("| 指标 | P0（v3） | P0+P1（v3p1） | Δ |\n|---|---|---|---|\n")
	row := func(label string, av, bv float64, fmtPct bool) {
		var sa, sb, sd string
		if fmtPct {
			sa = fmt.Sprintf("%.2f%%", av*100)
			sb = fmt.Sprintf("%.2f%%", bv*100)
			sd = fmt.Sprintf("%+.2f pp", (bv-av)*100)
		} else {
			sa = fmtFinite(av)
			sb = fmtFinite(bv)
			sd = fmt.Sprintf("%+.2f", bv-av)
		}
		b.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n", label, sa, sb, sd))
	}
	rowInt := func(label string, av, bv int) {
		b.WriteString(fmt.Sprintf("| %s | %d | %d | %+d |\n", label, av, bv, bv-av))
	}
	rowInt("样本数", v3.Total, v3p1.Total)
	rowInt("实际建仓数", v3.Wins+v3.Losses, v3p1.Wins+v3p1.Losses)
	rowInt("胜次", v3.Wins, v3p1.Wins)
	rowInt("败次", v3.Losses, v3p1.Losses)
	row("胜率", v3.WinRate, v3p1.WinRate, true)
	row("平均加权收益", v3.AvgReturn, v3p1.AvgReturn, true)
	row("最大单笔收益", v3.MaxReturn, v3p1.MaxReturn, true)
	row("最大单笔亏损", v3.MinReturn, v3p1.MinReturn, true)
	row("收益标准差", v3.StdReturn, v3p1.StdReturn, true)
	row("简易 Sharpe", v3.Sharpe, v3p1.Sharpe, false)
	row("累计净收益", v3.TotalReturn, v3p1.TotalReturn, true)
	row("年化收益", v3.AnnualReturn, v3p1.AnnualReturn, true)
	row("最大回撤", v3.MaxDrawdown, v3p1.MaxDrawdown, true)
	row("Calmar", v3.Calmar, v3p1.Calmar, false)
	row("Sortino", v3.Sortino, v3p1.Sortino, false)
	row("Profit Factor", v3.ProfitFactor, v3p1.ProfitFactor, false)
	row("盈亏比", v3.WinLossRatio, v3p1.WinLossRatio, false)
	row("Alpha vs 基准", v3.Alpha, v3p1.Alpha, true)

	b.WriteString("\n## 二、按 Recommendation 分桶对比\n\n")
	writeBucketCompareLabeled(&b, v3.ByReco, v3p1.ByReco, "P0", "P0+P1")
	b.WriteString("\n## 三、按宏观环境分桶对比\n\n")
	writeBucketCompareLabeled(&b, v3.ByRegime, v3p1.ByRegime, "P0", "P0+P1")
	b.WriteString("\n## 四、按板块分桶对比\n\n")
	writeBucketCompareLabeled(&b, v3.BySector, v3p1.BySector, "P0", "P0+P1")

	b.WriteString("\n## 五、P0 近 20 笔交易明细\n\n")
	writeTradeTail(&b, v3.Trades, 20)
	b.WriteString("\n## 六、P0+P1 近 20 笔交易明细\n\n")
	writeTradeTail(&b, v3p1.Trades, 20)

	b.WriteString("\n## 七、结论速览\n\n")
	dRet := (v3p1.TotalReturn - v3.TotalReturn) * 100
	dMDD := (v3p1.MaxDrawdown - v3.MaxDrawdown) * 100
	dAlpha := (v3p1.Alpha - v3.Alpha) * 100
	dCalmar := v3p1.Calmar - v3.Calmar
	b.WriteString(fmt.Sprintf("- 累计净收益变化：%+.2f pp（P0 %+.2f%% → P0+P1 %+.2f%%）\n", dRet, v3.TotalReturn*100, v3p1.TotalReturn*100))
	b.WriteString(fmt.Sprintf("- 最大回撤变化：%+.2f pp（P0 %.2f%% → P0+P1 %.2f%%）\n", dMDD, v3.MaxDrawdown*100, v3p1.MaxDrawdown*100))
	b.WriteString(fmt.Sprintf("- Calmar 变化：%+.2f（P0 %s → P0+P1 %s）\n", dCalmar, fmtFinite(v3.Calmar), fmtFinite(v3p1.Calmar)))
	b.WriteString(fmt.Sprintf("- Alpha 变化：%+.2f pp（P0 %+.2f%% → P0+P1 %+.2f%%）\n", dAlpha, v3.Alpha*100, v3p1.Alpha*100))
	switch {
	case dRet > 0 && dMDD <= 0:
		b.WriteString("- 综合判断：P1 双动量 + 凸性调整 同时改善收益与回撤，建议合入主流程默认开启。\n")
	case dRet > 0 && dMDD > 0:
		b.WriteString("- 综合判断：P1 提升收益但回撤略放大，建议保留为可选开关。\n")
	case dRet <= 0 && dMDD < 0:
		b.WriteString("- 综合判断：P1 牺牲一定收益换取回撤控制，适合熊市/高波动期启用。\n")
	default:
		b.WriteString("- 综合判断：P1 在该区间未带来明显改善，需要扩大样本/区间再做结论。\n")
	}
	return b.String()
}

// writeBucketCompareLabeled 与 writeBucketCompare 同语义，但表头使用自定义 label。
func writeBucketCompareLabeled(b *strings.Builder, av, bv map[string]Bucket, labelA, labelB string) {
	keys := map[string]bool{}
	for k := range av {
		keys[k] = true
	}
	for k := range bv {
		keys[k] = true
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)
	b.WriteString(fmt.Sprintf("| 分类 | %s 样本 | %s 胜率 | %s 平均收益 | %s 样本 | %s 胜率 | %s 平均收益 |\n",
		labelA, labelA, labelA, labelB, labelB, labelB))
	b.WriteString("|---|---|---|---|---|---|---|\n")
	for _, k := range ordered {
		x := av[k]
		y := bv[k]
		b.WriteString(fmt.Sprintf("| %s | %d | %.2f%% | %+.2f%% | %d | %.2f%% | %+.2f%% |\n",
			k, x.Count, x.WinRate*100, x.AvgRet*100, y.Count, y.WinRate*100, y.AvgRet*100))
	}
}
