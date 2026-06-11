package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/eino-multi-etf-strategy/datasource"
	"github.com/eino-multi-etf-strategy/llm"
	"github.com/eino-multi-etf-strategy/types"
)

// PreOpenAgent 9:24 集合竞价复核 Agent。
// 输入：8:50 主 agent 的 AgentState（含 Final 推荐 + Picks + 入场/止损/止盈）
// 输出：基于集合竞价撮合数据（虚拟开盘价 / IOPV / 大盘 510300）的 PreOpenAnalysis。
type PreOpenAgent struct {
	LLM llm.Client
	DS  datasource.ETFDataSource
}

func NewPreOpenAgent(c llm.Client, ds datasource.ETFDataSource) *PreOpenAgent {
	return &PreOpenAgent{LLM: c, DS: ds}
}

const preOpenSystemPrompt = `你是一名 A 股盘前撮合复核员。8:50 主分析已给出推荐 ETF 与入场/止损/止盈，
你的任务是结合 9:20-9:25 集合竞价（不可撤单期）撮合数据做"开盘前最后一公里复核"，
对每只标的判定 verdict 并给出调整后的入场/止损/止盈。

【输入】
- market: 510300 大盘集合竞价快照（auction_price / iopv / premium_pct / gap_pct）
- snapshots: 待复核 ETF 列表，每只含 prev_close / auction_price / iopv / premium_pct / gap_pct / entry_price / entry_gap_pct + 规则版初判 verdict + 调整后价位
- current_holds: 当前已持有 ETF 代码列表；若 snapshots 命中其中任一代码，低开只能提示观察/不加仓，不能直接 chase

【判定规则（你必须遵守）】
- premium_pct ≥ +2%   → verdict=abandon （高溢价，回归净值风险大，无视跳空方向）
- entry_gap_pct > +1.5% → verdict=wait_pullback （高开追入风险大，建议等回踩，adj_entry 上抬到 entry × 1.005）
- entry_gap_pct < -1% 且命中 current_holds → verdict=wait_pullback（已有持仓低开不加仓，adj_entry=0）
- entry_gap_pct < -1% 且 market.gap_pct >= -0.3%、premium_pct <= +0.5%、gap_pct > -0.8% → verdict=chase（空仓折价机会）
- entry_gap_pct < -1% 但大盘/个股弱开 → verdict=wait_pullback（低开可能是弱势延续，不追）
- 其余                  → verdict=on_target （维持原入场价/止损/止盈）
- 大盘 market.gap_pct < -0.8% 时，对所有标的 verdict 至少不弱于 wait_pullback（不主动追涨）；
  大盘 market.gap_pct > +0.8% 时，可对 on_target 标的略微上抬 adj_entry（不超过 entry × 1.003）。

【输出】
- 严格 JSON，禁止 markdown 围栏，禁止解释；
- 仅修正 verdict / adj_entry / adj_stop_loss / adj_take_profit / note / market_bias / summary / final_action；
- 字段未变化的也必须原样回填；
- summary 限 200 字以内，三段式：大盘集合竞价情绪 / 标的逐只复核 / 建议优先级排序；
- final_action 限 50 字以内，例："建议追入 588080（折价+0.3%），放弃 159928（溢价 2.4%）"；
- note 单只标的限 30 字以内。

JSON Schema:
{
  "market_bias": "strong_up | weak_up | flat | weak_down | strong_down",
  "snapshots": [{
    "etf_code": "...",
    "verdict": "chase | wait_pullback | abandon | on_target",
    "adj_entry": 0.0,
    "adj_stop_loss": 0.0,
    "adj_take_profit": 0.0,
    "note": "<=30 字"
  }],
  "summary": "<=200 字",
  "final_action": "<=50 字"
}`

// Run 主流程。
func (a *PreOpenAgent) Run(ctx context.Context, state *types.AgentState) (*types.PreOpenAnalysis, error) {
	if state == nil || state.Final == nil {
		return nil, fmt.Errorf("preopen: nil state or final")
	}

	rq, ok := a.DS.(datasource.RealtimeQuoter)
	if !ok {
		return nil, fmt.Errorf("preopen: datasource does not implement RealtimeQuoter")
	}

	out := &types.PreOpenAnalysis{GeneratedAt: time.Now()}
	holds := normalizePreOpenHolds(state)
	out.CurrentHolds = holds
	if len(holds) > 0 {
		out.CurrentHold = holds[0]
	}
	holdSet := make(map[string]struct{}, len(holds))
	for _, h := range holds {
		holdSet[h] = struct{}{}
	}

	// 1) 大盘 510300 撮合
	out.Market = fetchPreOpenSnapshot(rq, "510300", "沪深300ETF", 0)
	out.MarketBias = classifyMarketBias(out.Market.GapPct)

	// 2) 待复核标的：Final 推荐 + Picks（去重，≤3 支）
	codes := []string{}
	names := map[string]string{}
	entries := map[string]float64{}
	stops := map[string]float64{}
	takes := map[string]float64{}

	addCandidate := func(code, name string, entry, stop, take float64) {
		if code == "" {
			return
		}
		if _, dup := names[code]; dup {
			return
		}
		codes = append(codes, code)
		names[code] = name
		entries[code] = entry
		stops[code] = stop
		takes[code] = take
	}

	tgt := state.Final.TargetETF
	addCandidate(tgt.ETF.Code, tgt.ETF.Name, state.Final.EntryPrice, state.Final.StopLoss, state.Final.TakeProfit)
	for _, p := range state.Final.Picks {
		addCandidate(p.ETFCode, p.ETFName, p.EntryPrice, p.StopLoss, p.TakeProfit)
		if len(codes) >= 3 {
			break
		}
	}

	// 3) 逐只拉撮合数据 + 规则初判
	for _, code := range codes {
		snap := fetchPreOpenSnapshot(rq, code, names[code], entries[code])
		snap.AdjStopLoss = stops[code]
		snap.AdjTakeProf = takes[code]
		snap.AdjEntry = entries[code]
		applyVerdictRule(&snap, out.Market.GapPct, holdSet)
		out.Snapshots = append(out.Snapshots, snap)
	}

	// 4) 规则版兜底摘要
	out.Summary = ruleBasedPreOpenSummary(out)
	out.FinalAction = ruleBasedFinalAction(out)

	// 5) LLM 综合论证（失败则用规则版结果）
	if a.LLM != nil {
		_ = enrichPreOpenWithLLM(ctx, a.LLM, out)
	}

	return out, nil
}

// normalizePreOpenHolds 把 state.CurrentHolds 与旧 CurrentHold 字段合并、去空白、去重保序。
// 兼容老报告 JSON sidecar 仅写了 CurrentHold 单值字段的场景。
func normalizePreOpenHolds(state *types.AgentState) []string {
	if state == nil {
		return nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, len(state.CurrentHolds)+1)
	push := func(c string) {
		c = strings.TrimSpace(c)
		if c == "" {
			return
		}
		if _, dup := seen[c]; dup {
			return
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	for _, h := range state.CurrentHolds {
		push(h)
	}
	push(state.CurrentHold)
	if len(out) == 0 {
		return nil
	}
	return out
}

// fetchPreOpenSnapshot 拉单只标的的集合竞价快照；失败时仍返回 ETFCode/Name 占位。
func fetchPreOpenSnapshot(rq datasource.RealtimeQuoter, code, name string, entry float64) types.PreOpenSnapshot {
	s := types.PreOpenSnapshot{ETFCode: code, ETFName: name, EntryPrice: entry}
	q, err := rq.FetchRealtimeQuote(code)
	if err != nil {
		s.Note = "撮合数据获取失败"
		return s
	}
	s.AuctionPrice = q.Price
	s.PrevClose = q.PrevClose
	s.IOPV = q.IOPV
	s.PremiumPct = q.PremiumPct()
	if q.PrevClose > 0 {
		s.GapPct = (q.Price - q.PrevClose) / q.PrevClose
	}
	if entry > 0 {
		s.EntryGapPct = (q.Price - entry) / entry
	}
	if name == "" && q.Name != "" {
		s.ETFName = q.Name
	}
	return s
}

// classifyMarketBias 用大盘跳空幅度划分情绪带。
func classifyMarketBias(gap float64) string {
	switch {
	case gap >= 0.008:
		return "strong_up"
	case gap >= 0.003:
		return "weak_up"
	case gap <= -0.008:
		return "strong_down"
	case gap <= -0.003:
		return "weak_down"
	default:
		return "flat"
	}
}

// applyVerdictRule 规则版初判 + 调整后入场价。
// holds 为当前持仓代码集合（多持仓）；命中其一则按"已持仓"路径处理。
func applyVerdictRule(s *types.PreOpenSnapshot, marketGap float64, holds map[string]struct{}) {
	isHolding := false
	if len(holds) > 0 {
		_, isHolding = holds[s.ETFCode]
	}
	switch {
	case s.PremiumPct >= 0.02:
		s.Verdict = "abandon"
		s.AdjEntry = 0
		s.Note = strings.TrimSpace(fmt.Sprintf("溢价%+.2f%%, 高溢价警告", s.PremiumPct*100))
	case s.EntryGapPct > 0.015:
		s.Verdict = "wait_pullback"
		if s.EntryPrice > 0 {
			s.AdjEntry = s.EntryPrice * 1.005
		}
		s.Note = fmt.Sprintf("跳空%+.2f%%, 等回踩", s.EntryGapPct*100)
	case s.EntryGapPct < -0.01 && isHolding:
		s.Verdict = "wait_pullback"
		s.AdjEntry = 0
		s.Note = fmt.Sprintf("已持有低开%+.2f%%, 不加仓", s.EntryGapPct*100)
	case s.EntryGapPct < -0.01:
		if marketGap >= -0.003 && s.PremiumPct <= 0.005 && s.GapPct > -0.008 {
			s.Verdict = "chase"
			s.AdjEntry = s.AuctionPrice
			s.Note = fmt.Sprintf("低开%+.2f%%, 折价介入", s.EntryGapPct*100)
		} else {
			s.Verdict = "wait_pullback"
			s.AdjEntry = 0
			s.Note = fmt.Sprintf("低开%+.2f%%, 弱势不追", s.EntryGapPct*100)
		}
	default:
		s.Verdict = "on_target"
		s.Note = "符合 8:50 入场区间"
	}

	// 大盘弱势时，on_target 标的不再主动追，至少 wait_pullback
	if marketGap <= -0.008 && (s.Verdict == "on_target" || s.Verdict == "chase") {
		s.Verdict = "wait_pullback"
		if s.EntryPrice > 0 {
			s.AdjEntry = s.EntryPrice * 0.998
		}
		if isHolding {
			s.AdjEntry = 0
		}
		s.Note = "大盘弱势, 等回落"
	}

	// 盈亏比只在集合竞价执行层做硬拦截：不影响主信号是否持有，只影响是否追入/加仓。
	if s.Verdict == "chase" || s.Verdict == "on_target" {
		if _, note := CapByRiskReward("buy", s.AdjEntry, s.AdjStopLoss, s.AdjTakeProf); note != "" {
			s.Verdict = "wait_pullback"
			if isHolding {
				s.AdjEntry = 0
				s.Note = "盈亏比不足, 不加仓"
			} else {
				s.AdjEntry = 0
				s.Note = "盈亏比不足, 暂不追"
			}
		}
	}
}

func ruleBasedPreOpenSummary(a *types.PreOpenAnalysis) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("大盘 510300 集合竞价 %+.2f%% (%s); ",
		a.Market.GapPct*100, a.MarketBias))
	for _, s := range a.Snapshots {
		sb.WriteString(fmt.Sprintf("%s(%s) 跳空%+.2f%%/溢价%+.2f%% → %s; ",
			s.ETFName, s.ETFCode, s.GapPct*100, s.PremiumPct*100, s.Verdict))
	}
	return strings.TrimSuffix(strings.TrimSpace(sb.String()), ";")
}

func ruleBasedFinalAction(a *types.PreOpenAnalysis) string {
	var chase, wait, abandon []string
	for _, s := range a.Snapshots {
		switch s.Verdict {
		case "chase", "on_target":
			chase = append(chase, fmt.Sprintf("%s(%s)", s.ETFName, s.ETFCode))
		case "wait_pullback":
			wait = append(wait, fmt.Sprintf("%s(%s)", s.ETFName, s.ETFCode))
		case "abandon":
			abandon = append(abandon, fmt.Sprintf("%s(%s)", s.ETFName, s.ETFCode))
		}
	}
	parts := []string{}
	if len(chase) > 0 {
		parts = append(parts, "建议追入 "+strings.Join(chase, "/"))
	}
	if len(wait) > 0 {
		parts = append(parts, "等回踩 "+strings.Join(wait, "/"))
	}
	if len(abandon) > 0 {
		parts = append(parts, "放弃 "+strings.Join(abandon, "/"))
	}
	if len(parts) == 0 {
		return "无可执行标的"
	}
	return strings.Join(parts, "; ")
}

// llmPreOpenResponse LLM 返回的 patch 结构。
type llmPreOpenResponse struct {
	MarketBias string `json:"market_bias"`
	Snapshots  []struct {
		ETFCode     string  `json:"etf_code"`
		Verdict     string  `json:"verdict"`
		AdjEntry    float64 `json:"adj_entry"`
		AdjStopLoss float64 `json:"adj_stop_loss"`
		AdjTakeProf float64 `json:"adj_take_profit"`
		Note        string  `json:"note"`
	} `json:"snapshots"`
	Summary     string `json:"summary"`
	FinalAction string `json:"final_action"`
}

func enrichPreOpenWithLLM(ctx context.Context, c llm.Client, a *types.PreOpenAnalysis) error {
	user := buildPreOpenUserPrompt(a)
	res := &llmPreOpenResponse{}
	err := callLLMJSON(ctx, c, preOpenSystemPrompt, user, res, nil)
	if err != nil {
		return err
	}
	if res.MarketBias != "" {
		a.MarketBias = res.MarketBias
	}
	if res.Summary != "" {
		a.Summary = res.Summary
	}
	if res.FinalAction != "" {
		a.FinalAction = res.FinalAction
	}
	// 按 etf_code 反查 patch
	patch := map[string]int{}
	for i, s := range res.Snapshots {
		patch[s.ETFCode] = i
	}
	for i := range a.Snapshots {
		idx, ok := patch[a.Snapshots[i].ETFCode]
		if !ok {
			continue
		}
		p := res.Snapshots[idx]
		if p.Verdict != "" {
			a.Snapshots[i].Verdict = p.Verdict
		}
		if p.AdjEntry > 0 {
			a.Snapshots[i].AdjEntry = p.AdjEntry
		}
		if p.AdjStopLoss > 0 {
			a.Snapshots[i].AdjStopLoss = p.AdjStopLoss
		}
		if p.AdjTakeProf > 0 {
			a.Snapshots[i].AdjTakeProf = p.AdjTakeProf
		}
		if p.Note != "" {
			a.Snapshots[i].Note = p.Note
		}
	}
	return nil
}

func buildPreOpenUserPrompt(a *types.PreOpenAnalysis) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("市场基准 510300: prev_close=%.4f auction=%.4f iopv=%.4f premium=%+.2f%% gap=%+.2f%% (规则判定 market_bias=%s)\n\n",
		a.Market.PrevClose, a.Market.AuctionPrice, a.Market.IOPV,
		a.Market.PremiumPct*100, a.Market.GapPct*100, a.MarketBias))
	if len(a.CurrentHolds) > 0 {
		sb.WriteString("当前持仓 (current_holds): " + strings.Join(a.CurrentHolds, ",") + "\n\n")
	}
	sb.WriteString("[待复核标的]\n")
	for _, s := range a.Snapshots {
		sb.WriteString(fmt.Sprintf(
			"- %s(%s): prev_close=%.4f auction=%.4f iopv=%.4f premium=%+.2f%% gap=%+.2f%% | 8:50 入场=%.4f 止损=%.4f 止盈=%.4f | entry_gap=%+.2f%% | 规则初判 verdict=%s adj_entry=%.4f note=%q\n",
			s.ETFName, s.ETFCode, s.PrevClose, s.AuctionPrice, s.IOPV,
			s.PremiumPct*100, s.GapPct*100,
			s.EntryPrice, s.AdjStopLoss, s.AdjTakeProf,
			s.EntryGapPct*100, s.Verdict, s.AdjEntry, s.Note,
		))
	}
	sb.WriteString("\n请按 JSON Schema 输出复核结果。")
	return sb.String()
}
