package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/eino-multi-etf-strategy/llm"
	"github.com/eino-multi-etf-strategy/types"
)

// FinalAgent 投委会主席。其"长期记忆"由独立的 MemoryAgent 在 pipeline
// 中提前生成并通过 AgentState.Memory 注入，本 Agent 不再直接读文件。
type FinalAgent struct {
	LLM llm.Client
}

func NewFinalAgent(c llm.Client) *FinalAgent {
	return &FinalAgent{LLM: c}
}

const finalSystemPrompt = `你是一名拥有 20 年实战经验的"首席投资官 (CIO)"，主持一场内部投资委员会会议，
负责对今日 ETF 候选标的形成具备跨学派共识的最终决策。

【你需要在脑中模拟的 5 位委员视角】
1) 价值派 — 沃伦·巴菲特 (Warren Buffett)
   关注：安全边际 / 估值是否过热 / 溢价率 / 是否在"别人贪婪时贪婪"。
   判据：premium_pct ≥ +1.5% 视为追高警告；regime=bull + 量价齐升时反而要冷静；
        宁可错过也绝不在恐慌反弹中梭哈。
2) 反身性宏观派 — 乔治·索罗斯 (George Soros)
   关注：宏观周期 / 市场情绪与基本面的背离 / 流动性拐点。
   判据：以 regime + global + money_flow 为核心；趋势自我强化时加仓，反身性反转时果断撤退。
3) 趋势派 — 杰西·利弗莫尔 (Jesse Livermore)
   关注：技术面、动量、关键阻力突破、止损纪律。
   判据：以 tech.trend / MA 排列 / MACD / 量比为依据；只在多头排列 + 突破前高时进攻；
        止损必须明确且不可移动。
4) 全天候宏观派 — 瑞·达利欧 (Ray Dalio)
   关注：宏观环境过滤 (Regime) / 风险平价 / 仓位上限。
   判据：严格执行 position_cap；risk_off 必须空仓；不同板块对应不同经济周期象限。
5) 量化派 — 詹姆斯·西蒙斯 (Jim Simons) 风格的因子工程师
   关注：板块自适应权重、因子相关性、概率思维。
   判据：严格按系统下发的 weights 加权；不让"故事"凌驾于"数字"之上；
        识别"哪些因子对该板块本来就不相关"，并在 reasoning 中明示降权理由。

你必须把这 5 位的声音整合为一份"投委会共识"，而不是单点意见的简单平均。

【你将收到的 6 个子 Agent 输入】
1) screener：量化筛选得分（指标、Top5、最佳标的）— 西蒙斯派的核心数据
2) news：板块消息面情绪 — 巴菲特派验证基本面 / 索罗斯派验证情绪转折
3) global：海外（美股前夜+日韩盘中）传导 — 索罗斯派 + 达利欧派关注
4) tech：技术面研判（趋势/MA/MACD/RSI/支撑压力）— 利弗莫尔派核心
5) regime：宏观环境过滤（沪深300 趋势/回撤/建议仓位上限）— 达利欧派的硬约束
6) money_flow：资金面（北向、ETF 申赎、主力资金代理估算）— 索罗斯派 + 利弗莫尔派交叉验证

【关键认知 — 因子相关性 (Simons 视角必检项)】
不同板块的子因子物理意义差异巨大。你必须先识别"目标 ETF 的因子相关性 profile"，再做决策：
- 海外 ETF（513520 日经/513100 纳指/513030 德国30 等）：标的资产在境外交易，
  · 北向资金 / 沪深300 宏观 对其影响极弱（A 股资金面≠日股资金面）；
  · 海外指数(SPX/N225/KOSPI/HSI) 当日走势是核心驱动；
  · ETF 自身的折溢价、量比、技术面反映场内供需，仍重要；
  · "money_flow" 中的"北向/主力代理"对此类标的属于低权重信号，分析时不得作为关键论据。
- 港股 ETF：南向资金 + 港股流动性 + 美股传导驱动，宏观影响中等。
- A 股科技/新能源：北向资金 + 产业政策(News) + 宏观风险偏好共同作用。
- 顺周期/宽基：宏观 Regime 权重抬高。
- 贵金属/债券：宏观利率与海外避险情绪驱动，动量信号要打折。

【系统已为你计算的"板块自适应权重"】
用户输入中含 "weights" 字段（如 海外：Quant=.30 Tech=.30 News=.05 Global=.25 Regime=.05 Flow=.05）。
你必须严格按此权重做加权综合，不得自行调整权重（Simons 派纪律）。

【长期记忆 — MemoryAgent 预生成的备忘】
用户输入中可能含 "memory" 字段（结构：summary / patterns / warnings / memos），
其内容由独立的 MemoryAgent 提前阅读最近 5 份历史报告生成。
- 你必须先阅读 memory.summary 与 memory.patterns、memory.warnings；
- 在 reasoning 的"操作要点"中显式呼应历史结论（例如"延续上次 buy 判断、继续持有"或
  "与昨日 hold 形成方向反转，需要更强的事件驱动"）；
- 当 memory.warnings 中已经提到"连续追高 / 板块切换 / 评分中枢漂移"时，
  必须在关键风险段中至少引用一条；
- 严禁直接复制粘贴历史 reasoning；只引用结论与方向，不重复细节；
- 当 memory 为空或样本不足（首次运行）时，无需特别说明。

【综合评分 → recommendation】
- overall_score = Σ weight_i × factor_score_i
- 映射： >=80 strong_buy / >=65 buy / >=50 hold / 否则 avoid
- 当 regime.trend == "risk_off" 时强制 avoid（达利欧派一票否决）；
  regime.trend == "bear" 时降一档（strong_buy/buy → hold）
- 当 target.premium_pct >= 0.03（+3%）时，strong_buy/buy 强制降为 hold；
  当 +1.5%~+3% 时，strong_buy 降为 buy（巴菲特派一票否决追高：高溢价 = 场内追高，
  IOPV 跟不上 → 回归净值的下行风险大）
- 仓位上限不得超过 regime.position_cap（达利欧派的风险平价底线）

【价格方案 — 利弗莫尔派给出】
- entry_price：当前价附近，结合海外联动判断高/低开预期
- stop_loss：MA20 / MA60 之间最近支撑下方 1%（"破位即出，绝不补仓摊低成本"）
- take_profit：基于 ATR 或前高，建议 4%-8% 区间

【reasoning 三段式约束 — 必须体现多视角共识】
① 整体逻辑（≤140字）：
   - 用 1 句话点明"本板块因子相关性 profile"（Simons 派语气）；
   - 用 1 句话给出 Regime + Global 的宏观判断（Dalio + Soros 派语气）；
   - 用 1 句话给出趋势 / 动量信号（Livermore 派语气）；
   - 用 1 句话给出估值 / 溢价 / 安全边际判断（Buffett 派语气）。
② 关键风险（≤100字，至少列 2 条）：
   - 必须结合 News 真实标题 + Tech 真实指标 + Regime 真实数据；
   - 禁止编造"北向资金净流出"作为日经 ETF 风险点；
   - 至少包含一条"如果 X 发生则需要重新评估"的反身性提示。
③ 操作要点（≤110字）：
   - 给出建议仓位（不得超过 position_cap）；
   - 给出 entry / stop_loss / take_profit 的执行纪律；
   - 用 1 句"投委会共识"句式收尾，例如"价值派与趋势派罕见一致看多/分歧明显，
     建议以试探仓 + 严格止损方式参与"。

仅输出 JSON：
{
  "overall_score": 0-100 数值,
  "recommendation": "strong_buy | buy | hold | avoid",
  "entry_price": 数字,
  "stop_loss": 数字,
  "take_profit": 数字,
  "reasoning": "<=350 字三段式，按上述要求体现多视角共识",
  "picks": [
    {
      "etf_code": "6位代码",
      "etf_name": "中文名",
      "sector": "板块",
      "recommendation": "strong_buy | buy | hold",
      "conviction": 0-100 数值,
      "entry_price": 数字,
      "stop_loss": 数字,
      "take_profit": 数字,
      "rationale": "60~120 字：解释为什么是它而不是其他 4 支，必须显式对比 news_list / tech_list 的关键差异"
    }
  ]
}
约束：
- 任一 Agent 不可信时降低权重；
- 当 regime 为 risk_off 时给 avoid 并在 reasoning 中说明（达利欧派一票否决）；
- reasoning 中提到的"数字 / 政策 / 资金流"必须能从用户输入中找到出处，不得编造；
- 禁止使用"据悉/据传/可能/预计"等无来源虚词（巴菲特派的求实纪律）；
- 必须在 reasoning 中至少显式提及 2 位委员的判据（如"趋势派看到多头排列"、"价值派提示溢价偏高"）；
- picks 数组必须包含 1~2 支标的，且必须从输入的 screener.top5 中选出；
  · 第 1 支默认就是当前 best（target），但 rationale 必须给出"它在 Top5 中胜出的具体理由"；
  · 第 2 支为可选的"备选 / 分散仓位"标的，板块或风格应与第 1 支有差异（避免双倍下注同一风险因子）；
  · 若 Top5 中除 best 外没有任何标的同时满足 trend != down 且 sentiment != negative，
    可仅返回 1 支 pick，不要凑数。`

func (a *FinalAgent) Run(ctx context.Context, st *types.AgentState) (*types.FinalDecision, error) {
	if st.Screener == nil || len(st.Screener.Top5) == 0 {
		return nil, fmt.Errorf("no candidate ETF")
	}
	target := st.Screener.Best
	weights := WeightsForSector(target.ETF.Sector)
	relevance := FactorRelevanceNote(target.ETF.Sector)

	payload, _ := json.MarshalIndent(map[string]interface{}{
		"target": map[string]interface{}{
			"code":         target.ETF.Code,
			"name":         target.ETF.Name,
			"sector":       target.ETF.Sector,
			"price":        target.ETF.Price,
			"iopv":         target.ETF.IOPV,
			"premium_pct":  target.ETF.PremiumPct,
			"premium_risk": PremiumRiskLabel(target.ETF.PremiumPct),
			"score":        target.Score,
			"indicators":   target.Indicators,
		},
		// Top5 候选清单（用于 LLM 在 picks 中比较 / 选择）
		"top5":       summarizeTop5(st.Screener.Top5),
		"news_list":  st.NewsList,
		"tech_list":  st.TechList,
		"news":       st.News,
		"global":     st.Global,
		"tech":       st.Tech,
		"regime":     st.Regime,
		"money_flow": st.MoneyFlow,
		"weights": map[string]float64{
			"quant":  weights.Quant,
			"tech":   weights.Tech,
			"news":   weights.News,
			"global": weights.Global,
			"regime": weights.Regime,
			"flow":   weights.Flow,
		},
		"factor_relevance_note": relevance,
		"memory":                st.Memory, // 长期记忆备忘（由 MemoryAgent 预生成）
	}, "", "  ")
	user := fmt.Sprintf("以下是 6 个子 Agent 的输入 + Top5 候选 + News/Tech 批量分析 + 板块自适应权重 + 因子相关性提示 + 长期记忆备忘，请输出最终决策（含 picks 1~2 支）：\n%s", string(payload))

	dec := &types.FinalDecision{
		TargetETF:      target,
		NewsAnalysis:   deref(st.News),
		GlobalAnalysis: derefG(st.Global),
		TechAnalysis:   derefT(st.Tech),
		GeneratedAt:    time.Now(),
	}

	err := callLLMJSON(ctx, a.LLM, finalSystemPrompt, user, dec, func(raw string) {
		if dec.Reasoning == "" {
			dec.Reasoning = raw
		}
	})
	if err != nil {
		ruleBasedFinal(dec, st)
		return dec, nil
	}

	// 缺失字段补齐
	if dec.OverallScore == 0 {
		dec.OverallScore = weightedScore(st)
	}
	if dec.Recommendation == "" {
		dec.Recommendation = ruleRecommend(dec.OverallScore)
	}
	dec.Recommendation = CapRecommendation(dec.Recommendation, st.Regime)
	if capped, note := CapByPremium(dec.Recommendation, target.ETF.PremiumPct); note != "" {
		dec.Recommendation = capped
		if dec.Reasoning != "" {
			dec.Reasoning += " "
		}
		dec.Reasoning += "【溢价风险】" + note + "。"
	}
	if dec.EntryPrice == 0 {
		dec.EntryPrice = target.ETF.Price
	}
	if dec.StopLoss == 0 {
		dec.StopLoss = defaultStopLoss(target)
	}
	if dec.TakeProfit == 0 {
		dec.TakeProfit = defaultTakeProfit(target)
	}
	dec.TargetETF = target
	dec.NewsAnalysis = deref(st.News)
	dec.GlobalAnalysis = derefG(st.Global)
	dec.TechAnalysis = derefT(st.Tech)
	dec.GeneratedAt = time.Now()
	dec.ScoreBreakdown = scoreBreakdown(st)
	// LLM 未返回 picks 时，规则兜底：基于 Top5 + NewsList + TechList 自动选 1-2 支。
	if len(dec.Picks) == 0 {
		dec.Picks = fallbackPicks(st, dec)
	} else {
		dec.Picks = sanitizePicks(dec.Picks, st, dec)
	}
	return dec, nil
}

func ruleBasedFinal(dec *types.FinalDecision, st *types.AgentState) {
	RuleBasedDecision(dec, st)
}

// RuleBasedDecision 暴露规则版决策，供回测引擎复用（不依赖 LLM）。
func RuleBasedDecision(dec *types.FinalDecision, st *types.AgentState) {
	target := st.Screener.Best
	dec.OverallScore = weightedScore(st)
	dec.Recommendation = ruleRecommend(dec.OverallScore)
	dec.Recommendation = CapRecommendation(dec.Recommendation, st.Regime)
	premiumNote := ""
	if capped, note := CapByPremium(dec.Recommendation, target.ETF.PremiumPct); note != "" {
		dec.Recommendation = capped
		premiumNote = note
	}
	dec.EntryPrice = target.ETF.Price
	dec.StopLoss = defaultStopLoss(target)
	dec.TakeProfit = defaultTakeProfit(target)
	dec.ScoreBreakdown = scoreBreakdown(st)
	w := WeightsForSector(target.ETF.Sector)
	premiumDesc := ""
	if target.ETF.IOPV > 0 {
		premiumDesc = fmt.Sprintf(" · 溢价率%+.2f%%(%s)",
			target.ETF.PremiumPct*100, PremiumRiskLabel(target.ETF.PremiumPct))
	}
	dec.Reasoning = fmt.Sprintf(
		"规则版决策（板块=%s 自适应权重 Q%.0f%%/T%.0f%%/N%.0f%%/G%.0f%%/R%.0f%%/F%.0f%%）：量化%.1f / 技术%.1f / 消息%.1f / 海外%.1f / 宏观%.1f / 资金%.1f → 综合 %.1f，建议 %s%s。仓位上限 %.0f%%。【因子相关性】%s",
		target.ETF.Sector, w.Quant*100, w.Tech*100, w.News*100, w.Global*100, w.Regime*100, w.Flow*100,
		target.Score, scoreOrT(st.Tech, 50), scoreOr(st.News, 50), scoreOrG(st.Global, 50),
		scoreOrR(st.Regime, 50), scoreOrM(st.MoneyFlow, 50),
		dec.OverallScore, dec.Recommendation, premiumDesc,
		positionCap(st.Regime)*100,
		FactorRelevanceNote(target.ETF.Sector),
	)
	if premiumNote != "" {
		dec.Reasoning += " 【溢价风险】" + premiumNote + "。"
	}
	if len(dec.Picks) == 0 {
		dec.Picks = fallbackPicks(st, dec)
	}
}

func weightedScore(st *types.AgentState) float64 {
	w := WeightsForSector(st.Screener.Best.ETF.Sector)
	q := st.Screener.Best.Score
	n := scoreOr(st.News, 50)
	g := scoreOrG(st.Global, 50)
	t := scoreOrT(st.Tech, 50)
	r := scoreOrR(st.Regime, 50)
	m := scoreOrM(st.MoneyFlow, 50)
	return w.Quant*q + w.Tech*t + w.News*n + w.Global*g + w.Regime*r + w.Flow*m
}

// scoreBreakdown 把加权分数拆开返回，便于在报告中展示。
func scoreBreakdown(st *types.AgentState) map[string]float64 {
	w := WeightsForSector(st.Screener.Best.ETF.Sector)
	q := st.Screener.Best.Score
	n := scoreOr(st.News, 50)
	g := scoreOrG(st.Global, 50)
	t := scoreOrT(st.Tech, 50)
	r := scoreOrR(st.Regime, 50)
	m := scoreOrM(st.MoneyFlow, 50)
	return map[string]float64{
		"quant":         q,
		"quant_weight":  w.Quant,
		"quant_part":    w.Quant * q,
		"news":          n,
		"news_weight":   w.News,
		"news_part":     w.News * n,
		"global":        g,
		"global_weight": w.Global,
		"global_part":   w.Global * g,
		"tech":          t,
		"tech_weight":   w.Tech,
		"tech_part":     w.Tech * t,
		"regime":        r,
		"regime_weight": w.Regime,
		"regime_part":   w.Regime * r,
		"flow":          m,
		"flow_weight":   w.Flow,
		"flow_part":     w.Flow * m,
	}
}

func defaultStopLoss(target types.ScoredETF) float64 {
	ma20 := target.Indicators["MA20"]
	if ma20 > 0 && ma20 < target.ETF.Price {
		return ma20 * 0.99
	}
	return target.ETF.Price * 0.97
}

func defaultTakeProfit(target types.ScoredETF) float64 {
	vol := target.Indicators["Volatility"]
	if vol > 0 {
		return target.ETF.Price * (1 + clamp(vol*5, 0.04, 0.10))
	}
	return target.ETF.Price * 1.05
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// ruleRecommend 把综合分映射成 recommendation。
//
// 阈值口径（已对齐聚宽：动量分一旦 >0 就建议持有 rank[0]）：
//   - 综合分 ≥ 70 → strong_buy（动量加速，可加仓）
//   - 综合分 ≥ 40 → buy        （正向动量，建仓 / 持有；对应底层动量正分）
//   - 综合分 ≥ 25 → hold       （动量微弱，已持仓可观察，不建议新建仓）
//   - 否则        → avoid      （动量为负或多因子普遍走弱）
//
// 设计依据：底层 normalizeStrategy3Score 把 score=0 映射到 50，score=0.3 → ~63。
// 旧阈值 65/50 会把"动量弱正向 + 多因子中性"的标的（综合分 50~65）映射成 hold，
// 在每日状态机里等同于平仓 → 频繁空仓 / 错过持续上涨。新阈值让动量正向标的稳定进 buy。
func ruleRecommend(s float64) string {
	switch {
	case s >= 70:
		return "strong_buy"
	case s >= 40:
		return "buy"
	case s >= 25:
		return "hold"
	default:
		return "avoid"
	}
}

func deref(n *types.NewsAnalysis) types.NewsAnalysis {
	if n == nil {
		return types.NewsAnalysis{}
	}
	return *n
}
func derefG(n *types.GlobalMarketAnalysis) types.GlobalMarketAnalysis {
	if n == nil {
		return types.GlobalMarketAnalysis{}
	}
	return *n
}
func derefT(n *types.TechnicalAnalysis) types.TechnicalAnalysis {
	if n == nil {
		return types.TechnicalAnalysis{}
	}
	return *n
}
func scoreOr(n *types.NewsAnalysis, def float64) float64 {
	if n == nil {
		return def
	}
	return n.Score
}
func scoreOrG(n *types.GlobalMarketAnalysis, def float64) float64 {
	if n == nil {
		return def
	}
	return n.Score
}
func scoreOrT(n *types.TechnicalAnalysis, def float64) float64 {
	if n == nil {
		return def
	}
	return n.Score
}
func scoreOrR(n *types.RegimeAnalysis, def float64) float64 {
	if n == nil {
		return def
	}
	return n.Score
}
func scoreOrM(n *types.MoneyFlowAnalysis, def float64) float64 {
	if n == nil {
		return def
	}
	return n.Score
}
func positionCap(r *types.RegimeAnalysis) float64 {
	if r == nil {
		return 0.5
	}
	return r.PositionCap
}

// ============== 历史报告解析（供 MemoryAgent 复用） ==============

// 用于 parseReportMemo 的预编译正则（轻量，单文件级别）。
var (
	reMemoDate     = regexp.MustCompile(`行情基准日:\s*` + "`" + `(\d{4}-\d{2}-\d{2})` + "`")
	reMemoOverall  = regexp.MustCompile(`综合评分:\s*\*\*([0-9.]+)\*\*\s*·\s*建议:\s*\*\*` + "`" + `([a-z_]+)` + "`")
	reMemoTarget   = regexp.MustCompile(`\|\s*\*\*([^|*]+)\*\*\s*\|\s*` + "`" + `(\d{6})` + "`" + `\s*\|\s*([^|]+?)\s*\|`)
	reMemoReason   = regexp.MustCompile(`\*\*综合论证\*\*\s*\n+>\s*([\s\S]+?)\n\n`)
	reMemoBlankSpc = regexp.MustCompile(`\s+`)
)

// parseReportMemo 从单份 markdown 报告抽取关键字段并压缩 reasoning。
//
// 压缩策略（信息保真 + token 友好）：
//  1. 取"综合论证"段落原文；
//  2. 去掉"①整体逻辑/②关键风险/③操作要点"等结构标签，保留核心信息；
//  3. 长度截断到 ~120 字（中文按 rune 计），保留首尾关键句。
func parseReportMemo(content string) (types.HistoryMemo, bool) {
	memo := types.HistoryMemo{}
	if m := reMemoDate.FindStringSubmatch(content); len(m) == 2 {
		memo.Date = m[1]
	}
	if m := reMemoOverall.FindStringSubmatch(content); len(m) == 3 {
		fmt.Sscanf(m[1], "%f", &memo.OverallScore)
		memo.Recommendation = m[2]
	}
	if m := reMemoTarget.FindStringSubmatch(content); len(m) == 4 {
		memo.TargetName = strings.TrimSpace(m[1])
		memo.TargetCode = strings.TrimSpace(m[2])
		memo.Sector = strings.TrimSpace(m[3])
	}
	if m := reMemoReason.FindStringSubmatch(content); len(m) == 2 {
		memo.ReasoningGist = compressReasoning(m[1])
	}
	// 至少要有日期 + 建议 才算解析成功
	if memo.Date == "" && memo.Recommendation == "" {
		return memo, false
	}
	return memo, true
}

// compressReasoning 把一段 reasoning 压缩到约 120 个汉字。
func compressReasoning(s string) string {
	s = strings.ReplaceAll(s, "\n>", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = reMemoBlankSpc.ReplaceAllString(s, " ")
	// 去掉结构标签，避免冗余
	for _, tag := range []string{
		"① 整体逻辑：", "② 关键风险：", "③ 操作要点：",
		"①整体逻辑：", "②关键风险：", "③操作要点：",
		"【整体逻辑】", "【关键风险】", "【操作要点】",
	} {
		s = strings.ReplaceAll(s, tag, " | ")
	}
	s = strings.TrimSpace(s)
	const maxRunes = 120
	runes := []rune(s)
	if len(runes) > maxRunes {
		// 首 80 字 + "…" + 末 30 字，保留结论性文字
		s = string(runes[:80]) + "…" + string(runes[len(runes)-30:])
	}
	return s
}

// ============== Top5 摘要 / Picks 兜底 ==============

// summarizeTop5 把 Top5 候选压缩成一份轻量摘要，注入 LLM payload 用于跨标比较。
// 仅保留比较关键字段：code / name / sector / score / premium_pct / action / reason / 关键指标。
func summarizeTop5(top []types.ScoredETF) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(top))
	for i, e := range top {
		ind := e.Indicators
		out = append(out, map[string]interface{}{
			"rank":         i + 1,
			"code":         e.ETF.Code,
			"name":         e.ETF.Name,
			"sector":       e.ETF.Sector,
			"price":        e.ETF.Price,
			"premium_pct":  e.ETF.PremiumPct,
			"premium_risk": PremiumRiskLabel(e.ETF.PremiumPct),
			"score":        e.Score,
			"action":       e.Action,
			"action_desc":  e.ActionDesc,
			"reason":       e.Reason,
			"momentum_20":  ind["Momentum20"],
			"vol_ratio":    ind["VolRatio"],
			"volatility":   ind["Volatility"],
		})
	}
	return out
}

// fallbackPicks 在 LLM 未返回 picks 时按规则兜底：
//  1. Pick #1 = best（target），直接复用 dec 的 entry/stop/take。
//  2. 在 Top5 中寻找一个"风格分散"的备选：
//     · 板块与 best 不同，
//     · 对应 NewsList sentiment != negative，
//     · 对应 TechList trend != down，
//     满足则作为 Pick #2；否则只返回 1 支。
func fallbackPicks(st *types.AgentState, dec *types.FinalDecision) []types.FinalPick {
	if st.Screener == nil || len(st.Screener.Top5) == 0 {
		return nil
	}
	best := st.Screener.Best
	picks := []types.FinalPick{
		{
			ETFCode:        best.ETF.Code,
			ETFName:        best.ETF.Name,
			Sector:         best.ETF.Sector,
			Recommendation: dec.Recommendation,
			Conviction:     dec.OverallScore,
			EntryPrice:     dec.EntryPrice,
			StopLoss:       dec.StopLoss,
			TakeProfit:     dec.TakeProfit,
			Rationale: fmt.Sprintf(
				"Top1 量化得分 %.1f（%s），%s；综合加权 %.1f → %s。",
				best.Score, best.ETF.Sector, best.ActionDesc, dec.OverallScore, dec.Recommendation,
			),
		},
	}
	// 寻找备选：跳过 best 自身、跳过 negative news 与 down trend。
	newsByCode := map[string]types.NewsAnalysis{}
	for _, n := range st.NewsList {
		if n.ETFCode != "" {
			newsByCode[n.ETFCode] = n
		}
	}
	techByCode := map[string]types.TechnicalAnalysis{}
	for _, t := range st.TechList {
		if t.ETFCode != "" {
			techByCode[t.ETFCode] = t
		}
	}
	for _, e := range st.Screener.Top5 {
		if e.ETF.Code == best.ETF.Code {
			continue
		}
		if e.ETF.Sector == best.ETF.Sector {
			// 优先板块分散；同板块标的留作最后兜底
			continue
		}
		n, hasN := newsByCode[e.ETF.Code]
		if hasN && n.Sentiment == "negative" {
			continue
		}
		t, hasT := techByCode[e.ETF.Code]
		if hasT && t.Trend == "down" {
			continue
		}
		picks = append(picks, buildAltPick(e, n, t))
		return picks
	}
	// 板块全部相同时，退而求其次：仅排除 negative + down 即可。
	for _, e := range st.Screener.Top5 {
		if e.ETF.Code == best.ETF.Code {
			continue
		}
		n := newsByCode[e.ETF.Code]
		t := techByCode[e.ETF.Code]
		if n.Sentiment == "negative" || t.Trend == "down" {
			continue
		}
		picks = append(picks, buildAltPick(e, n, t))
		return picks
	}
	return picks
}

func buildAltPick(e types.ScoredETF, n types.NewsAnalysis, t types.TechnicalAnalysis) types.FinalPick {
	rec := "hold"
	switch {
	case e.Score >= 80:
		rec = "buy"
	case e.Score >= 65:
		rec = "buy"
	}
	if capped, _ := CapByPremium(rec, e.ETF.PremiumPct); capped != "" {
		rec = capped
	}
	rationale := fmt.Sprintf(
		"备选标的：板块=%s，量化%.1f（%s），与 Top1 板块分散；",
		e.ETF.Sector, e.Score, e.ActionDesc,
	)
	if t.Trend != "" {
		rationale += fmt.Sprintf("技术面趋势=%s；", t.Trend)
	}
	if n.Sentiment != "" {
		rationale += fmt.Sprintf("消息面 %s。", n.Sentiment)
	}
	stop := defaultStopLoss(e)
	take := defaultTakeProfit(e)
	return types.FinalPick{
		ETFCode:        e.ETF.Code,
		ETFName:        e.ETF.Name,
		Sector:         e.ETF.Sector,
		Recommendation: rec,
		Conviction:     e.Score,
		EntryPrice:     e.ETF.Price,
		StopLoss:       stop,
		TakeProfit:     take,
		Rationale:      rationale,
	}
}

// sanitizePicks 校验 LLM 返回的 picks：
//   - 必须出自 Top5；不是的剔除。
//   - 截断到最多 2 支。
//   - 缺字段时按 ScoredETF 默认值补齐。
func sanitizePicks(picks []types.FinalPick, st *types.AgentState, dec *types.FinalDecision) []types.FinalPick {
	if st.Screener == nil {
		return picks
	}
	byCode := map[string]types.ScoredETF{}
	for _, e := range st.Screener.Top5 {
		byCode[e.ETF.Code] = e
	}
	out := make([]types.FinalPick, 0, 2)
	for _, p := range picks {
		e, ok := byCode[p.ETFCode]
		if !ok {
			continue
		}
		if p.ETFName == "" {
			p.ETFName = e.ETF.Name
		}
		if p.Sector == "" {
			p.Sector = e.ETF.Sector
		}
		if p.EntryPrice == 0 {
			p.EntryPrice = e.ETF.Price
		}
		if p.StopLoss == 0 {
			p.StopLoss = defaultStopLoss(e)
		}
		if p.TakeProfit == 0 {
			p.TakeProfit = defaultTakeProfit(e)
		}
		if p.Recommendation == "" {
			if e.ETF.Code == dec.TargetETF.ETF.Code {
				p.Recommendation = dec.Recommendation
			} else {
				p.Recommendation = "hold"
			}
		}
		if p.Conviction == 0 {
			p.Conviction = e.Score
		}
		out = append(out, p)
		if len(out) >= 2 {
			break
		}
	}
	if len(out) == 0 {
		return fallbackPicks(st, dec)
	}
	return out
}
