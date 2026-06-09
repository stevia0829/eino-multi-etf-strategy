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
	// 同步 AsOf 给 RotationAgent
	a.Rotation.AsOf = a.AsOf

	cands, err := a.Rotation.Rank(ctx)
	if err != nil {
		return nil, err
	}
	if len(cands) == 0 {
		return &types.ScreenerResult{AsOfDate: asOfOrNow(a.AsOf)}, nil
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
			ETF:        etf,
			Score:      normalized,
			Indicators: ind,
			Reason:     buildRotationReason(c, action),
			Action:     string(action),
			ActionDesc: action.Label(),
		})
	}

	// 同板块去重（可选）：每个 sector 仅保留分数最高的一只，避免 Top5 在同一风险因子上双倍下注。
	// 默认关闭，对齐聚宽 get_etf_rank（不去重）。如需保留多 Agent 风险分散，把 DedupBySector
	// 显式设为 true。
	if a.DedupBySector {
		scored = dedupBySector(scored)
	}

	top := scored
	if a.TopN > 0 && len(top) > a.TopN {
		top = top[:a.TopN]
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
// 同时引入 R² 作为置信度乘子，避免 R² 极低的伪趋势拿高分。
func normalizeStrategy3Score(score, r2 float64) float64 {
	if math.IsNaN(score) || math.IsInf(score, 0) {
		return 0
	}
	base := 100.0 / (1.0 + math.Exp(-2.5*score))
	confidence := 0.5 + 0.5*r2 // R²=0 → 0.5, R²=1 → 1.0
	v := base * confidence
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
