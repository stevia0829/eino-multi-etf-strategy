package backtest

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/eino-multi-etf-strategy/agent"
	"github.com/eino-multi-etf-strategy/datasource"
	"github.com/eino-multi-etf-strategy/types"
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

	// 用基准 ETF（510300）的历史 K 线来确定有效交易日序列
	baseKlines, err := e.DS.GetKLineAsOf("510300", 1500, end)
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

	for di, d := range dates {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		// ── 聚宽对齐模式：直接调 RotationAgent，按聚宽口径满仓持有 rank[0] ──────
		// 旁路 Screener 的所有外围装饰（dedupBySector / RuleBasedDecision / PositionCap）。
		// 聚宽口径：MinScore=0（剔除负分）、MaxScore=6 + 1.1 倍过热门槛、不做板块去重、满仓 100%。
		if e.Variant == "joinquant" {
			rot := agent.NewRotationAgent(e.DS)
			rot.AsOf = d
			// 关键：MinScore 对齐聚宽 = 0（不允许负分进 rank）
			rot.Params.MinScore = 0
			rot.Params.MaxScore = 6
			rot.Params.ScoreThresholdMultiplier = 1.1
			rot.Params.MDays = 21
			rot.Params.TopN = 0 // 0 = 不截断，保留全部排名

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
			entryIdx := indexOnOrAfter(klines, d)
			if entryIdx < 0 {
				continue
			}
			entryPx := klines[entryIdx].Close
			if entryPx <= 0 {
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

		st := &types.AgentState{Screener: scr, Regime: regime}
		if hold != nil {
			st.CurrentHold = hold.code
		}
		dec := &types.FinalDecision{TargetETF: target}
		agent.RuleBasedDecision(dec, st)

		posCap := 0.5
		regimeTrend := "neutral"
		if regime != nil {
			posCap = regime.PositionCap
			regimeTrend = regime.Trend
		}

		// 决策映射：是否应该持有 best
		shouldHold := dec.Recommendation == "buy" || dec.Recommendation == "strong_buy"

		// ── 持仓状态机 ─────────────────────────────────────────────
		if hold != nil {
			// 当前已有持仓
			if shouldHold && target.ETF.Code == hold.code {
				// 信号一致 → 继续持有
				continue
			}
			// 信号反转或换标的 → 当日收盘平仓
			px := priceOnDate(hold.klineCache, d)
			if px > 0 {
				reason := "signal_change"
				if !shouldHold {
					reason = "no_buy_signal"
				}
				exit(d, px, reason)
			} else {
				// 拿不到当日价（停牌等），强行用入场价平 0 收益
				exit(d, hold.entryPrice, "no_price")
			}
		}

		// 平仓后（hold == nil），若新信号是 buy/strong_buy → 当日收盘入场
		if shouldHold {
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
			entryIdx := indexOnOrAfter(klines, d)
			if entryIdx < 0 {
				continue
			}
			entryPx := klines[entryIdx].Close
			if entryPx <= 0 {
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

// indexOnOrAfter 返回 K 线序列中第一根 Date >= asOf 的索引；找不到返回 -1。
func indexOnOrAfter(klines []types.KLine, asOf time.Time) int {
	for i, k := range klines {
		if k.Date.After(asOf) || sameDay(k.Date, asOf) {
			return i
		}
	}
	return -1
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
	r.Total = len(r.Trades)
	for _, t := range r.Trades {
		if t.Recommendation == "hold" || t.Recommendation == "avoid" {
			// 空仓样本不计入收益统计，但仍参与"信号正确率"
			continue
		}
		executed++
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
		if r.StdReturn > 0 {
			// 简易 Sharpe：年化收益 / 年化波动（假设每次持有 5 日，一年约 50 个非重叠样本）
			r.Sharpe = (r.AvgReturn * 50) / (r.StdReturn * math.Sqrt(50))
		}

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
				r.Sortino = (netAvg * 50) / (downStd * math.Sqrt(50))
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
