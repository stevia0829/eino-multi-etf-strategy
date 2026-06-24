package agent

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/multi-agents-etf-trade-strategy/datasource"
	"github.com/multi-agents-etf-trade-strategy/indicator"
	"github.com/multi-agents-etf-trade-strategy/types"
)

// ScreenerAgent 现在以策略 3（ETF 轮动）作为底层评分器：
//  1. RotationAgent 拉取 etf_pool_3，按 21 日加权对数回归动量打分；
//  2. 对每只候选 ETF 补充 MA/RSI/MACD/VolRatio 等技术指标，方便后续 TechnicalAgent 复用；
//  3. 将策略 3 的原始 score 归一化到 0~100 区间，以兼容 FinalAgent 的加权融合。
type ScreenerAgent struct {
	DS         datasource.ETFDataSource
	HistoryDay int
	MinScore   float64 // 归一化后的最低分阈值（0~100）
	TopN       int
	// AsOf 指定基准日期；零值表示当天最新行情，用于回测 / 复盘。
	AsOf time.Time
	// DedupBySector 是否对 Top5 做同板块去重；默认 false（对齐聚宽 get_etf_rank 不去重）。
	// 开启后每个 sector 仅保留分数最高的一只，旧版本默认行为。
	DedupBySector bool
	// CurrentHolds 用户当前持仓 ETF 代码列表（advice 模式注入；回测/聚宽模式不传）。
	// 持仓在排名中享有"豁免追加位"：分数本就在 TopN 内不影响，落在 TopN 外则按客观分数追加在末尾。
	// 不参与 Score 修正、不抬分、不绕过过滤。
	CurrentHolds []string

	Rotation *RotationAgent
}

func NewScreenerAgent(ds datasource.ETFDataSource) *ScreenerAgent {
	return &ScreenerAgent{
		DS:            ds,
		HistoryDay:    60,
		MinScore:      0,
		TopN:          5,
		DedupBySector: false, // 默认关闭，对齐聚宽
		Rotation:      NewRotationAgent(ds),
	}
}

// Run 工作流：
//  1. 通过 RotationAgent 跑策略 3 评分（含日间动量阈值过滤）
//  2. 为每个候选拉取 60 日 K 线 → 计算 MA/RSI/MACD/动量/量比/波动率
//  3. 将原始 score 归一化为 0~100 综合分
//  4. 取 TopN 并标注最佳标的
func (a *ScreenerAgent) Run(ctx context.Context) (*types.ScreenerResult, error) {
	// 同步 AsOf / 持仓豁免给 RotationAgent
	a.Rotation.AsOf = a.AsOf
	a.Rotation.Params.MustIncludeCodes = a.CurrentHolds

	cands, err := a.Rotation.Rank(ctx)
	if err != nil {
		return nil, err
	}
	if len(cands) == 0 {
		return &types.ScreenerResult{AsOfDate: asOfOrNow(a.AsOf)}, nil
	}

	holdSet := make(map[string]struct{}, len(a.CurrentHolds))
	for _, h := range a.CurrentHolds {
		if h = strings.TrimSpace(h); h != "" {
			holdSet[h] = struct{}{}
		}
	}

	scored := make([]types.ScoredETF, 0, len(cands))
	for _, c := range cands {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		etf := c.ETF
		// 补足 60 日 K 线，便于 TechnicalAgent 算 MA60 / 波动率
		klines, err := a.DS.GetKLineAsOf(etf.Code, a.HistoryDay, a.AsOf)
		if err != nil || len(klines) < 30 {
			klines = c.Klines
		}
		etf.History = klines
		if !a.AsOf.IsZero() && len(klines) > 0 {
			etf.Price = klines[len(klines)-1].Close
		}

		ma5 := indicator.MA(klines, 5)
		ma20 := indicator.MA(klines, 20)
		ma60 := indicator.MA(klines, 60)
		rsi := indicator.RSI(klines, 14)
		dif, dea, hist := indicator.MACD(klines)
		mom20 := indicator.Momentum(klines, 20)
		volRatio := indicator.VolumeRatio(klines, 5)
		vol := indicator.Volatility(klines, 20)

		normalized := normalizeStrategy3Score(c.Score, c.R2)
		if normalized < a.MinScore {
			continue
		}

		action := c.Action()
		ind := map[string]float64{
			"MA5": ma5, "MA20": ma20, "MA60": ma60,
			"RSI": rsi, "DIF": dif, "DEA": dea, "HIST": hist,
			"Momentum20": mom20, "VolRatio": volRatio, "Volatility": vol,
			// 策略 3 原始量纲，便于报告侧展示
			"Strategy3Score":     c.Score,
			"AnnualizedReturn":   c.Annualized,
			"WeightedR2":         c.R2,
			"PrevStrategy3Score": c.PrevScore,
		}

		scored = append(scored, types.ScoredETF{
			ETF:           etf,
			Score:         normalized,
			Indicators:    ind,
			Reason:        buildRotationReason(c, action),
			Action:        string(action),
			ActionDesc:    action.Label(),
			IsCurrentHold: isInSet(holdSet, c.ETF.Code),
		})
	}

	// 同板块去重（可选）：每个 sector 仅保留分数最高的一只，避免 Top5 在同一风险因子上双倍下注。
	// 默认关闭，对齐聚宽 get_etf_rank（不去重）。如需保留多 Agent 风险分散，把 DedupBySector
	// 显式设为 true。
	// 注意：dedupBySector 会保留每个板块第一个出现（即最高分）的标的，可能把"板块同行的持仓"丢掉；
	// advice 模式持仓应单独保留，所以下面再单独追加被去重掉的持仓（保持其原 Score 与排序）。
	if a.DedupBySector {
		scored = dedupBySectorPreserveHolds(scored, holdSet)
	}

	top := scored
	if a.TopN > 0 && len(top) > a.TopN {
		// TopN 截断后，若有持仓被截到外面但仍通过过滤，则按客观分数追加在末尾（不插队、不抬分）
		head := top[:a.TopN]
		tail := top[a.TopN:]
		top = appendHoldsBeyondTop(head, tail, holdSet)
	}

	// 仅在"实时模式"（AsOf 为零值，即跑当天最新行情）下补全 IOPV / 溢价率，
	// 历史回测时拉实时报价没意义且会拖慢速度。
	if a.AsOf.IsZero() {
		if rq, ok := a.DS.(datasource.RealtimeQuoter); ok {
			for i := range top {
				q, err := rq.FetchRealtimeQuote(top[i].ETF.Code)
				if err != nil || q.IOPV <= 0 {
					continue
				}
				top[i].ETF.IOPV = q.IOPV
				top[i].ETF.PremiumPct = q.PremiumPct()
				if top[i].Indicators == nil {
					top[i].Indicators = map[string]float64{}
				}
				top[i].Indicators["IOPV"] = q.IOPV
				top[i].Indicators["PremiumPct"] = top[i].ETF.PremiumPct
			}
			// ── P3-3 折溢价反向因子（仅实时模式，记录 PenaltyMult 指标供 FinalAgent 参考） ─────
			// 注意：不再对 top 做 sortScoredDesc 重排——保证线上排名与聚宽裸分排名完全一致。
			// applyPremiumPenalty 仅修改 Score 供下游加权参考，不改变 Screener 的排名顺序。
			applyPremiumPenalty(top)
		}
	}

	result := &types.ScreenerResult{
		Top5:     top,
		AsOfDate: asOfOrNow(a.AsOf),
	}
	if len(top) > 0 {
		result.Best = top[0]
	}
	return result, nil
}

// normalizeStrategy3Score 把策略 3 原始 score (= 年化收益 * R²)
// 归一化到 0~100，便于 FinalAgent 加权融合。
//
// 经验上 score ∈ [-1, 6]，常见区间 (-0.3, 1.5)。这里采用 sigmoid 平滑：
//
//	100 * (1 / (1 + exp(-2.5 * score)))
//
// score=0 → 50；score=1 → ~92；score=-0.5 → ~22。
//
// 重要：sigmoid 是 score 的严格单调递增函数，因此归一化后的排名与裸分完全一致
// （= 聚宽 get_etf_rank 的排名）。不再叠加 R² 置信度乘子——裸分本身已含 R²
// （score = annualized × R²），二次乘入会破坏单调性、扭曲排名。
func normalizeStrategy3Score(score, r2 float64) float64 {
	if math.IsNaN(score) || math.IsInf(score, 0) {
		return 0
	}
	v := 100.0 / (1.0 + math.Exp(-2.5*score))
	if v < 0 {
		v = 0
	}
	if v > 100 {
		v = 100
	}
	return v
}

// dedupBySector 在保持原有顺序（按分数从高到低）的前提下，对同一 sector 仅保留首个出现的标的。
// 没有 sector 字段的标的（Sector 为空）视为独立类别，全部保留。
func dedupBySector(in []types.ScoredETF) []types.ScoredETF {
	if len(in) == 0 {
		return in
	}
	out := make([]types.ScoredETF, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		sector := s.ETF.Sector
		if sector != "" {
			if _, ok := seen[sector]; ok {
				continue
			}
			seen[sector] = struct{}{}
		}
		out = append(out, s)
	}
	return out
}

// dedupBySectorPreserveHolds 在 dedupBySector 基础上保留所有命中持仓的标的：
//   - 持仓与同板块的非持仓共存：先按"板块首个最高分"去重，再把被丢掉的持仓追加回结果末尾；
//   - 持仓本身就是板块首个最高分：天然保留；
//   - 多只同板块持仓：全部保留（分散风险纪律由用户自负）。
func dedupBySectorPreserveHolds(in []types.ScoredETF, holds map[string]struct{}) []types.ScoredETF {
	if len(in) == 0 {
		return in
	}
	if len(holds) == 0 {
		return dedupBySector(in)
	}
	dedup := dedupBySector(in)
	already := make(map[string]struct{}, len(dedup))
	for _, s := range dedup {
		already[s.ETF.Code] = struct{}{}
	}
	for _, s := range in {
		if _, isHold := holds[s.ETF.Code]; !isHold {
			continue
		}
		if _, dup := already[s.ETF.Code]; dup {
			continue
		}
		dedup = append(dedup, s)
		already[s.ETF.Code] = struct{}{}
	}
	return dedup
}

// appendHoldsBeyondTop 当 head 已截断到 TopN 时，把 tail 中命中持仓的标的按客观分数追加进 head。
// 行为约束：不插队、不重排、不抬分。
func appendHoldsBeyondTop(head, tail []types.ScoredETF, holds map[string]struct{}) []types.ScoredETF {
	if len(holds) == 0 || len(tail) == 0 {
		return head
	}
	already := make(map[string]struct{}, len(head))
	for _, s := range head {
		already[s.ETF.Code] = struct{}{}
	}
	for _, s := range tail {
		if _, isHold := holds[s.ETF.Code]; !isHold {
			continue
		}
		if _, dup := already[s.ETF.Code]; dup {
			continue
		}
		head = append(head, s)
		already[s.ETF.Code] = struct{}{}
	}
	return head
}

func isInSet(set map[string]struct{}, key string) bool {
	if len(set) == 0 {
		return false
	}
	_, ok := set[key]
	return ok
}

// 折溢价反向因子阈值（P3-3）：
//   - PremiumPct ≥ +1.5%：追高警告，Score × 0.95
//   - PremiumPct ≥ +3.0%：严重过热，Score × 0.85
//
// 折价 / 正常溢价不做任何调整（不放大已经折价的标的，避免双重激励）。
const (
	premiumPenaltyWarn     = 0.015
	premiumPenaltyHigh     = 0.030
	premiumPenaltyMultWarn = 0.95
	premiumPenaltyMultHigh = 0.85
)

// applyPremiumPenalty 对 top 列表按 PremiumPct 做 Score 反向调整（in-place）。
// 设计原则：
//  1. 仅在 IOPV > 0 时生效（拿到了真实溢价才校准）；
//  2. 与 CapByPremium 解耦：CapByPremium 是 final 决策层降档，不影响排名；
//     这里直接修正排名，避免"top1 严重溢价、top2 折价"时仍买 top1。
//  3. 调整幅度温和：-5% / -15%，不会让一个明显折价的弱动量标的反超强动量标的。
func applyPremiumPenalty(top []types.ScoredETF) {
	for i := range top {
		if top[i].ETF.IOPV <= 0 {
			continue
		}
		prem := top[i].ETF.PremiumPct
		mult := 1.0
		switch {
		case prem >= premiumPenaltyHigh:
			mult = premiumPenaltyMultHigh
		case prem >= premiumPenaltyWarn:
			mult = premiumPenaltyMultWarn
		}
		if mult < 1.0 {
			top[i].Score *= mult
			if top[i].Indicators == nil {
				top[i].Indicators = map[string]float64{}
			}
			top[i].Indicators["PremiumPenaltyMult"] = mult
		}
	}
}

// sortScoredDesc 按 Score 降序对 in-place 排序，相等时保持原相对顺序（stable）。
func sortScoredDesc(in []types.ScoredETF) {
	sort.SliceStable(in, func(i, j int) bool {
		return in[i].Score > in[j].Score
	})
}

func buildRotationReason(c RotationCandidate, action RotationAction) string {
	return fmt.Sprintf("策略3 score=%.3f (年化%.2f%% × R²%.2f) · %s · 动作=%s",
		c.Score, c.Annualized*100, c.R2, c.BuildReason(), action)
}

func asOfOrNow(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}
