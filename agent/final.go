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

【你将收到的 6 个子 Agent 输入 + Top5 横向对比数据】
1) screener：量化筛选得分 + Top5 候选清单（每只含完整技术指标：MA5/MA20/MA60/RSI/MACD/动量/波动率/量比/策略3量化分/年化/R²）— 西蒙斯派的核心数据
2) news_list[0..N]：逐只 Top5 的消息面情绪分析（sentiment/score/highlight/summary）— 巴菲特派验证每只 ETF 的基本面
3) global：海外（美股前夜+日韩盘中）传导 — 索罗斯派 + 达利欧派关注
4) tech_list[0..N]：逐只 Top5 的技术面研判（趋势/MA/MACD/RSI/支撑压力/建议持有区间）— 利弗莫尔派对每只 ETF 的独立判断
5) regime：宏观环境过滤（沪深300 趋势/回撤/建议仓位上限）— 达利欧派的硬约束
6) money_flow：资金面（北向、ETF 申赎、主力资金代理估算）— 索罗斯派 + 利弗莫尔派交叉验证

【关键：你现在拥有所有候选 ETF（含持仓）的完整数据】
- top5 数组中每一只 ETF 都包含完整的 indicators（MA/RSI/MACD/动量/波动率/量比/策略3原始分/年化/R²），与 target 同口径；
- 注意：top5 可能超过 5 只（用户持仓通过豁免机制追加在末尾），每个 is_current_hold=true 的条目就是用户当前持有的 ETF；
- news_list 和 tech_list 分别提供了每只 ETF 的独立 LLM 分析（按 etf_code 匹配），覆盖所有 top5 候选（含持仓）；
- 你必须横向对比所有候选（含持仓）的技术面强弱、消息面优劣、动量评分高低、溢价风险，而不是只盯着 target。
- 做 picks 选择时必须对每只候选给出"为什么选它而不选其他候选"的具体技术面/消息面对比理由；不得仅凭 score 排序敷衍。

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
- 消息面 / 技术面的 factor_score 必须做**跨标校准**：
  先看 news_list / tech_list 中其他候选的 Score 均值，再决定 target 的两因子贡献：
  · target 的 news/tech score 显著高于 peers（高出 15%+）→ 该因子贡献 +5%~+10%
  · target 显著低于 peers（低出 15%+）→ 该因子贡献 -5%~-10%
  · 无显著差异（±15% 以内）→ 不做调整
  这是 Simons 派的"因子相对强度"逻辑：孤立看 70 分没有意义，关键看同类是 50 还是 75。
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

【reasoning 三段式约束 — 必须体现多视角共识与跨标对比】
① 整体逻辑（≤180字）：
   - 用 1 句话点明"本板块因子相关性 profile"（Simons 派语气）；
   - 用 1 句话给出 Regime + Global 的宏观判断（Dalio + Soros 派语气）；
   - 用 1~2 句话横向对比 Top5 中前 2~3 名的技术面差异（如：谁 MA 多头排列、谁 RSI 过热/超卖、谁 MACD 金叉/死叉），并明确说明 target 相比其他候选的技术面优劣（Livermore 派语气）；
   - 用 1~2 句话横向对比 Top5 各标的消息面（news_list 中各标的 sentiment/score 对比），并给出估值/溢价/安全边际判断（Buffett 派语气）。
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
  "reasoning": "<=420 字三段式，按上述要求体现多视角共识与跨标技术面/消息面对比",
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
      "rationale": "80~160 字：必须横向对比此标的技术面（MA/RSI/MACD）+ 消息面（news_list 匹配项的 sentiment/highlight）与其他 Top5 候选的关键差异，明确解释为什么是它而不是其他候选"
    }
  ],
  "hold_reviews": [
    {
      "etf_code": "6位代码（必须出自用户输入的 current_holds）",
      "etf_name": "中文名",
      "sector": "板块",
      "in_top": true/false,
      "rank": 1-based 在合并候选列表的名次,
      "score": 候选量化分,
      "action": "keep | trim | rotate",
      "action_desc": "中文人话（如：继续持有 / 减仓观察 / 平仓切换）",
      "news_bias": "positive | neutral | negative | unknown",
      "tech_trend": "up | flat | down | unknown",
      "rationale": "60~140 字：结合 top5 中该 ETF 的完整技术指标（MA/RSI/MACD/动量）+ news_list 匹配项的 sentiment/score + tech_list 匹配项的 trend/summary + 与 best 的 score 差距，给出客观评审结论"
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
    可仅返回 1 支 pick，不要凑数。
- hold_reviews 仅在用户输入的 current_holds 非空时返回；为空时输出 [] 即可。
  · 必须为 current_holds 中"在 screener.top5 中能匹配到候选的"每只持仓输出一项；
  · 不得把 hold_reviews 与 picks 混淆：hold_reviews 是"对持仓的客观评审"，与是否买入新标的无关；
  · keep = 信号还在 + 趋势/消息至少不背离，建议继续持有；
    trim = 信号弱化或消息中性偏弱，建议减仓观望，保留少量底仓；
    rotate = 信号反转 / 趋势 down / 消息 negative / 量化分明显落后 best，建议平仓切换；
  · rationale 必须显式引用：候选量化分（绝对 + 与 best 的差距）+ news_list 中匹配项的 sentiment + tech_list 中匹配项的 trend；
  · 严禁让 hold_reviews 改变 recommendation / picks / 价格方案。`

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
		// 持仓评审输入（advice 模式注入；为空时 LLM 应输出 hold_reviews=[]）
		"current_holds":   collectHoldsList(st),
		"hold_candidates": holdCandidatesPayload(st),
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
		dec.Recommendation = RuleRecommend(dec.OverallScore)
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
		dec.StopLoss = DefaultStopLoss(target)
	}
	if dec.TakeProfit == 0 {
		dec.TakeProfit = DefaultTakeProfit(target)
	}
	if capped, note := CapByPullbackCooldownForState(dec.Recommendation, target, st); note != "" {
		dec.Recommendation = capped
		if dec.Reasoning != "" {
			dec.Reasoning += " "
		}
		dec.Reasoning += "【回撤冷却】" + note + "。"
	}
	if _, note := CapByRiskReward(dec.Recommendation, dec.EntryPrice, dec.StopLoss, dec.TakeProfit); note != "" {
		if dec.Reasoning != "" {
			dec.Reasoning += " "
		}
		dec.Reasoning += "【盈亏比提示】" + note + "。"
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
	// HoldReviews：LLM 未返回 / 缺字段时按规则兜底；有返回则做 sanitize 校验。
	if len(dec.HoldReviews) == 0 {
		dec.HoldReviews = buildHoldReviewsFallback(st)
	} else {
		dec.HoldReviews = sanitizeHoldReviews(dec.HoldReviews, st)
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
	dec.Recommendation = RuleRecommend(dec.OverallScore)
	dec.Recommendation = CapRecommendation(dec.Recommendation, st.Regime)
	premiumNote := ""
	if capped, note := CapByPremium(dec.Recommendation, target.ETF.PremiumPct); note != "" {
		dec.Recommendation = capped
		premiumNote = note
	}
	cooldownNote := ""
	if capped, note := CapByPullbackCooldownForState(dec.Recommendation, target, st); note != "" {
		dec.Recommendation = capped
		cooldownNote = note
	}
	dec.EntryPrice = target.ETF.Price
	dec.StopLoss = DefaultStopLoss(target)
	dec.TakeProfit = DefaultTakeProfit(target)
	rrNote := ""
	if _, note := CapByRiskReward(dec.Recommendation, dec.EntryPrice, dec.StopLoss, dec.TakeProfit); note != "" {
		rrNote = note
	}
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
	if cooldownNote != "" {
		dec.Reasoning += " 【回撤冷却】" + cooldownNote + "。"
	}
	if rrNote != "" {
		dec.Reasoning += " 【盈亏比提示】" + rrNote + "。"
	}
	if len(dec.Picks) == 0 {
		dec.Picks = fallbackPicks(st, dec)
	}
	if len(dec.HoldReviews) == 0 {
		dec.HoldReviews = buildHoldReviewsFallback(st)
	}
}

func weightedScore(st *types.AgentState) float64 {
	w := WeightsForSector(st.Screener.Best.ETF.Sector)
	bestCode := st.Screener.Best.ETF.Code
	// 用量化裸分（Strategy3Score 线性映射到 0~100），而非 sigmoid+R² 归一化分，
	// 保证 ranking 和聚宽裸动量一致。sigmoid 归一化仅在报告展示用。
	q := rawQuantScore(st.Screener.Best)
	n := scoreOr(st.News, 50)
	g := scoreOrG(st.Global, 50)
	t := scoreOrT(st.Tech, 50)
	r := scoreOrR(st.Regime, 50)
	m := scoreOrM(st.MoneyFlow, 50)

	// 跨标的相对比较：best 的消息面/技术面 vs 同类均值的偏离 → ±10% 因子调权
	nAdj, tAdj := 1.0, 1.0
	if peerNewsAvg, ok := peerAvgNews(st.NewsList, bestCode); ok {
		nAdj = relativeAdjust(n, peerNewsAvg)
	}
	if peerTechAvg, ok := peerAvgTech(st.TechList, bestCode); ok {
		tAdj = relativeAdjust(t, peerTechAvg)
	}

	return w.Quant*q + w.Tech*t*tAdj + w.News*n*nAdj + w.Global*g + w.Regime*r + w.Flow*m
}

// rawQuantScore 从 ScoredETF 取出策略 3 裸分（Strategy3Score），
// 线性映射到 0~100，与聚宽裸动量排序一致。
// 裸分 0 → 50，裸分 0.5 → 75，裸分 1.0 → 100，裸分 <0 → clamp 到 0~50。
func rawQuantScore(e types.ScoredETF) float64 {
	raw := e.Indicators["Strategy3Score"]
	if raw == 0 {
		// 退化：没有 Strategy3Score 时用归一化分反推
		return e.Score
	}
	mapped := 50 + raw*50
	if mapped < 0 {
		mapped = 0
	}
	if mapped > 100 {
		mapped = 100
	}
	return mapped
}

// peerAvgNews 计算 NewsList 中除 bestCode 外所有 ETF 的 Score 均值。
func peerAvgNews(list []types.NewsAnalysis, bestCode string) (float64, bool) {
	sum, cnt := 0.0, 0
	for _, n := range list {
		if n.ETFCode == bestCode || n.ETFCode == "" || n.Sentiment == "" {
			continue
		}
		if n.Score > 0 {
			sum += n.Score
			cnt++
		}
	}
	if cnt == 0 {
		return 0, false
	}
	return sum / float64(cnt), true
}

// peerAvgTech 计算 TechList 中除 bestCode 外所有 ETF 的 Score 均值。
func peerAvgTech(list []types.TechnicalAnalysis, bestCode string) (float64, bool) {
	sum, cnt := 0.0, 0
	for _, t := range list {
		if t.ETFCode == bestCode || t.ETFCode == "" || t.Trend == "" {
			continue
		}
		if t.Score > 0 {
			sum += t.Score
			cnt++
		}
	}
	if cnt == 0 {
		return 0, false
	}
	return sum / float64(cnt), true
}

// relativeAdjust 计算 best 相对 peerAvg 的偏离，返回 [0.90, 1.10] 的因子调权系数。
// 逻辑：同板块新闻/技术面互为参照，best 与 peers 无显著差异时不调权（1.0）；
// best 显著优于 peers → 加大消息面/技术面因子贡献；best 显著弱于 peers → 减小贡献。
func relativeAdjust(bestScore, peerAvg float64) float64 {
	if peerAvg <= 0 {
		return 1.0
	}
	diff := (bestScore - peerAvg) / peerAvg // 相对偏离
	factor := 1.0 + clamp(diff*0.5, -0.10, 0.10)
	return factor
}

// scoreBreakdown 把加权分数拆开返回，便于在报告中展示。
func scoreBreakdown(st *types.AgentState) map[string]float64 {
	w := WeightsForSector(st.Screener.Best.ETF.Sector)
	bestCode := st.Screener.Best.ETF.Code
	q := rawQuantScore(st.Screener.Best)
	n := scoreOr(st.News, 50)
	g := scoreOrG(st.Global, 50)
	t := scoreOrT(st.Tech, 50)
	r := scoreOrR(st.Regime, 50)
	m := scoreOrM(st.MoneyFlow, 50)

	nAdj, tAdj := 1.0, 1.0
	if peerAvg, ok := peerAvgNews(st.NewsList, bestCode); ok {
		nAdj = relativeAdjust(n, peerAvg)
	}
	if peerAvg, ok := peerAvgTech(st.TechList, bestCode); ok {
		tAdj = relativeAdjust(t, peerAvg)
	}

	return map[string]float64{
		"quant":         q,
		"quant_weight":  w.Quant,
		"quant_part":    w.Quant * q,
		"news":          n,
		"news_weight":   w.News,
		"news_adj":      nAdj,
		"news_part":     w.News * n * nAdj,
		"global":        g,
		"global_weight": w.Global,
		"global_part":   w.Global * g,
		"tech":          t,
		"tech_weight":   w.Tech,
		"tech_adj":      tAdj,
		"tech_part":     w.Tech * t * tAdj,
		"regime":        r,
		"regime_weight": w.Regime,
		"regime_part":   w.Regime * r,
		"flow":          m,
		"flow_weight":   w.Flow,
		"flow_part":     w.Flow * m,
	}
}

// DefaultStopLoss 返回基于 MA20 的默认止损价。
func DefaultStopLoss(target types.ScoredETF) float64 {
	ma20 := target.Indicators["MA20"]
	if ma20 > 0 && ma20 < target.ETF.Price {
		return ma20 * 0.99
	}
	return target.ETF.Price * 0.97
}

func DefaultTakeProfit(target types.ScoredETF) float64 {
	vol := target.Indicators["Volatility"]
	if vol > 0 {
		return target.ETF.Price * (1 + clamp(vol*5, 0.04, 0.10))
	}
	return target.ETF.Price * 1.05
}

const minExecutableRiskReward = 1.4

// CapByRiskReward 检查交易计划的最小盈亏比，只返回风险提示，不改变 recommendation。
// 亏损端用 entry-stop，收益端用 take-entry；低于阈值说明即便方向看对，
// 单笔交易也没有足够安全边际。是否拦截追入/加仓由 PreOpenAgent 在集合竞价阶段决定。
func CapByRiskReward(reco string, entry, stop, take float64) (string, string) {
	if reco != "strong_buy" && reco != "buy" {
		return reco, ""
	}
	if entry <= 0 || stop <= 0 || take <= 0 || stop >= entry || take <= entry {
		return reco, "入场/止损/止盈结构无效，需等待更好价格或重新计算交易计划"
	}
	rr := (take - entry) / (entry - stop)
	if rr < minExecutableRiskReward {
		return reco, fmt.Sprintf("当前盈亏比 %.2f < %.2f，安全边际不足，集合竞价追入/加仓需等待更好价格",
			rr, minExecutableRiskReward)
	}
	return reco, ""
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

// RuleRecommend 把综合分映射成 recommendation。
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
func RuleRecommend(s float64) string {
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

const (
	pullbackCooldownLookback = 5
	pullbackCooldownTrigger  = 0.05
)

// CapByPullbackCooldown 在强动量标的短线急跌且尚未修复时，把新开仓建议降为 hold。
// 规则：
//   - 最近 5 日高点回撤 ≥ 5%；
//   - 当前价尚未重新站上 MA5；
//   - 若最近一根 K 线是阴线，当前价也未收复该阴线实体的一半。
//
// 这条规则主要约束"空仓新追"，防止 advice 连续几天吃到强动量尾巴后仍反复提示买入。
func CapByPullbackCooldown(reco string, target types.ScoredETF) (string, string) {
	if reco != "strong_buy" && reco != "buy" {
		return reco, ""
	}
	note := PullbackCooldownNote(target)
	if note == "" {
		return reco, ""
	}
	return "hold", fmt.Sprintf("%s，新开仓由 %s 降为 hold", note, reco)
}

// CapByPullbackCooldownForState 按持仓状态输出短线回撤提示，但不硬降档。
// 主策略保持信号在线即持有/换仓，避免回撤冷却误伤收益；加仓拦截交给 PreOpenAgent。
// 多持仓场景下，命中"换仓目标本身就是当前持仓之一"会按"已持有同一标的"处理。
func CapByPullbackCooldownForState(reco string, target types.ScoredETF, st *types.AgentState) (string, string) {
	if reco != "strong_buy" && reco != "buy" {
		return reco, ""
	}
	note := PullbackCooldownNote(target)
	if note == "" {
		return reco, ""
	}
	holds := collectHolds(st)
	if len(holds) == 0 {
		return reco, note + "，未提供当前持仓，不降档，仅提示谨慎追高"
	}
	if _, hit := holds[target.ETF.Code]; hit {
		return reco, note + "，当前已持有同一标的，不降档，仅提示谨慎加仓"
	}
	return reco, note + "，换仓目标短线未修复，不降档，仅提示降低追入优先级"
}

// collectHolds 把 AgentState 中的多持仓 + 兼容的单字段持仓合并成 set。
func collectHolds(st *types.AgentState) map[string]struct{} {
	if st == nil {
		return nil
	}
	out := make(map[string]struct{}, len(st.CurrentHolds)+1)
	for _, h := range st.CurrentHolds {
		h = strings.TrimSpace(h)
		if h != "" {
			out[h] = struct{}{}
		}
	}
	if h := strings.TrimSpace(st.CurrentHold); h != "" {
		out[h] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func PullbackCooldownNote(target types.ScoredETF) string {
	klines := target.ETF.History
	if len(klines) < pullbackCooldownLookback {
		return ""
	}
	price := target.ETF.Price
	if price <= 0 {
		price = klines[len(klines)-1].Close
	}
	if price <= 0 {
		return ""
	}

	start := len(klines) - pullbackCooldownLookback
	high := 0.0
	for _, k := range klines[start:] {
		if k.High > high {
			high = k.High
		}
		if k.Close > high {
			high = k.Close
		}
	}
	if high <= 0 {
		return ""
	}
	pullback := (high - price) / high
	if pullback < pullbackCooldownTrigger {
		return ""
	}

	ma5 := target.Indicators["MA5"]
	if ma5 <= 0 {
		ma5 = avgClose(klines, 5)
	}
	reclaimedMA5 := ma5 > 0 && price >= ma5
	reclaimedPrevHalf := reclaimedLastBearHalf(klines, price)
	if reclaimedMA5 || reclaimedPrevHalf {
		return ""
	}
	return fmt.Sprintf("近%d日高点回撤 %.2f%%，且未收复 MA5 / 最近阴线半分位",
		pullbackCooldownLookback, pullback*100)
}

func avgClose(klines []types.KLine, n int) float64 {
	if n <= 0 || len(klines) < n {
		return 0
	}
	start := len(klines) - n
	sum := 0.0
	for _, k := range klines[start:] {
		sum += k.Close
	}
	return sum / float64(n)
}

func reclaimedLastBearHalf(klines []types.KLine, price float64) bool {
	if len(klines) < 1 {
		return false
	}
	last := klines[len(klines)-1]
	if last.Open <= last.Close {
		return false
	}
	half := last.Close + (last.Open-last.Close)*0.5
	return price >= half
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
	reMemoNewsSent = regexp.MustCompile(`消息面研判[\s\S]*?\|\s*\*\*[^|]+\*\*\s*\|\s*[⚪🟢🔴]\s*\*\*(\w+)\*\*\s*\|\s*\*\*([0-9.]+)\*\*`)
	reMemoNewsGist = regexp.MustCompile(`消息面速览\s*\n\n([\s\S]{1,200}?)\n\n`)
	reMemoBlankSpc = regexp.MustCompile(`\s+`)
)

// parseReportMemo 从单份 markdown 报告抽取关键字段并压缩 reasoning。
//
// 压缩策略（信息保真 + token 友好）：
//  1. 取"综合论证"段落原文；
//  2. 去掉"①整体逻辑/②关键风险/③操作要点"等结构标签，保留核心信息；
//  3. 长度截断到 ~120 字（中文按 rune 计），保留首尾关键句。
//  4. 同时提取消息面情绪/评分/速览，用于跨日情绪趋势识别。
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
	// 提取消息面摘要
	if m := reMemoNewsSent.FindStringSubmatch(content); len(m) == 3 {
		memo.NewsSentiment = m[1]
		fmt.Sscanf(m[2], "%f", &memo.NewsScore)
	}
	if m := reMemoNewsGist.FindStringSubmatch(content); len(m) == 2 {
		gist := strings.TrimSpace(m[1])
		if len([]rune(gist)) > 60 {
			gist = string([]rune(gist)[:60]) + "…"
		}
		memo.NewsGist = gist
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

// summarizeTop5 把 Top5 候选压缩成一份摘要，注入 LLM payload 用于跨标比较。
// 包含完整技术指标（MA/RSI/MACD/动量/波动率/量比）+ 策略3量化分/年化/R²，供 LLM 做多维度横向对比。
func summarizeTop5(top []types.ScoredETF) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(top))
	for i, e := range top {
		ind := e.Indicators
		entry := map[string]interface{}{
			"rank":            i + 1,
			"code":            e.ETF.Code,
			"name":            e.ETF.Name,
			"sector":          e.ETF.Sector,
			"price":           e.ETF.Price,
			"premium_pct":     e.ETF.PremiumPct,
			"premium_risk":    PremiumRiskLabel(e.ETF.PremiumPct),
			"score":           e.Score,
			"action":          e.Action,
			"action_desc":     e.ActionDesc,
			"reason":          e.Reason,
			"is_current_hold": e.IsCurrentHold,
			"indicators":      ind,
			// 以下为 LLM 易读的扁平化关键指标（同时保留完整 indicators map）
			"ma5":                  ind["MA5"],
			"ma20":                 ind["MA20"],
			"ma60":                 ind["MA60"],
			"rsi":                  ind["RSI"],
			"macd_dif":             ind["DIF"],
			"macd_dea":             ind["DEA"],
			"macd_hist":            ind["HIST"],
			"momentum_20":          ind["Momentum20"],
			"vol_ratio":            ind["VolRatio"],
			"volatility":           ind["Volatility"],
			"strategy3_score":      ind["Strategy3Score"],
			"annualized_return":    ind["AnnualizedReturn"],
			"weighted_r2":          ind["WeightedR2"],
			"prev_strategy3_score": ind["PrevStrategy3Score"],
			"iopv":                 ind["IOPV"],
			"premium_penalty_mult": ind["PremiumPenaltyMult"],
		}
		out = append(out, entry)
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
	riskNotes := []string{}
	if capped, note := CapByPullbackCooldown(rec, e); note != "" {
		rec = capped
		riskNotes = append(riskNotes, "回撤冷却："+note)
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
	stop := DefaultStopLoss(e)
	take := DefaultTakeProfit(e)
	if _, note := CapByRiskReward(rec, e.ETF.Price, stop, take); note != "" {
		riskNotes = append(riskNotes, "盈亏比提示："+note)
	}
	if len(riskNotes) > 0 {
		rationale += strings.Join(riskNotes, "；") + "。"
	}
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
			p.StopLoss = DefaultStopLoss(e)
		}
		if p.TakeProfit == 0 {
			p.TakeProfit = DefaultTakeProfit(e)
		}
		if e.ETF.Code == dec.TargetETF.ETF.Code {
			if p.Recommendation != dec.Recommendation {
				p.Recommendation = dec.Recommendation
				p.Rationale = appendPickRiskNote(p.Rationale, "与最终建议同步为 "+dec.Recommendation)
			}
		} else {
			if p.Recommendation == "" {
				p.Recommendation = "hold"
			}
			if capped, note := CapByPremium(p.Recommendation, e.ETF.PremiumPct); note != "" {
				p.Recommendation = capped
				p.Rationale = appendPickRiskNote(p.Rationale, "溢价风险："+note)
			}
			if capped, note := CapByPullbackCooldown(p.Recommendation, e); note != "" {
				p.Recommendation = capped
				p.Rationale = appendPickRiskNote(p.Rationale, "回撤冷却："+note)
			}
			if _, note := CapByRiskReward(p.Recommendation, p.EntryPrice, p.StopLoss, p.TakeProfit); note != "" {
				p.Rationale = appendPickRiskNote(p.Rationale, "盈亏比提示："+note)
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

func appendPickRiskNote(rationale, note string) string {
	if note == "" {
		return rationale
	}
	if rationale != "" && !strings.HasSuffix(rationale, "。") && !strings.HasSuffix(rationale, "；") {
		rationale += "。"
	}
	return rationale + "【风险过滤】" + note + "。"
}

// ============== 持仓评审 (HoldReviews) ==============

// collectHoldsList 把 collectHolds 的 set 还原为去重保序的 slice，便于注入 LLM payload。
func collectHoldsList(st *types.AgentState) []string {
	if st == nil {
		return nil
	}
	out := make([]string, 0, len(st.CurrentHolds)+1)
	seen := make(map[string]struct{}, len(st.CurrentHolds)+1)
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
	for _, h := range st.CurrentHolds {
		push(h)
	}
	push(st.CurrentHold)
	if len(out) == 0 {
		return nil
	}
	return out
}

// holdCandidatesPayload 构造每只持仓在合并候选列表中的对应记录，供 LLM 在 hold_reviews 中
// 引用客观分数 / 名次 / news_list / tech_list。命中 Top5 / 持仓豁免位的标的才会出现；
// 完全在候选外的持仓（连 Strategy3Pool 都未命中或被过滤）按用户决策"忽略，不强制评估"处理。
func holdCandidatesPayload(st *types.AgentState) []map[string]interface{} {
	if st == nil || st.Screener == nil {
		return nil
	}
	holds := collectHolds(st)
	if len(holds) == 0 {
		return nil
	}
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
	bestScore := 0.0
	if len(st.Screener.Top5) > 0 {
		bestScore = st.Screener.Top5[0].Score
	}
	out := make([]map[string]interface{}, 0, len(holds))
	for i, e := range st.Screener.Top5 {
		if _, ok := holds[e.ETF.Code]; !ok {
			continue
		}
		row := map[string]interface{}{
			"etf_code":     e.ETF.Code,
			"etf_name":     e.ETF.Name,
			"sector":       e.ETF.Sector,
			"rank":         i + 1,
			"score":        e.Score,
			"score_gap":    bestScore - e.Score,
			"action":       e.Action,
			"action_desc":  e.ActionDesc,
			"is_top5":      i < 5,
			"premium_pct":  e.ETF.PremiumPct,
			"premium_risk": PremiumRiskLabel(e.ETF.PremiumPct),
			"indicators":   e.Indicators,
		}
		if n, ok := newsByCode[e.ETF.Code]; ok {
			row["news_sentiment"] = n.Sentiment
			row["news_score"] = n.Score
			row["news_summary"] = n.Summary
		}
		if t, ok := techByCode[e.ETF.Code]; ok {
			row["tech_trend"] = t.Trend
			row["tech_score"] = t.Score
			row["tech_summary"] = t.Summary
		}
		out = append(out, row)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// buildHoldReviewsFallback 规则版持仓评审：基于排名命中 + news/tech 客观信号映射 keep/trim/rotate。
//   - 未命中候选（持仓不在 Top5 / 豁免位）：忽略，不出条目（与"忽略池外标的"决策一致）；
//   - rotate：动作 = avoid 或 趋势=down 或 消息=negative 或 (与 best 分差 ≥ 15 且不在 Top3)；
//   - trim：动作 = hold_only / 趋势=flat / 消息=neutral；
//   - keep：动作 ∈ {strong_buy, buy} 且无负向信号。
func buildHoldReviewsFallback(st *types.AgentState) []types.HoldReview {
	if st == nil || st.Screener == nil || len(st.Screener.Top5) == 0 {
		return nil
	}
	holds := collectHolds(st)
	if len(holds) == 0 {
		return nil
	}
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
	bestScore := st.Screener.Top5[0].Score
	out := make([]types.HoldReview, 0, len(holds))
	for i, e := range st.Screener.Top5 {
		if _, ok := holds[e.ETF.Code]; !ok {
			continue
		}
		n := newsByCode[e.ETF.Code]
		t := techByCode[e.ETF.Code]
		newsBias := classifyNewsBias(n)
		techTrend := classifyTechTrend(t)
		gap := bestScore - e.Score
		action, actionDesc := decideHoldAction(e, newsBias, techTrend, gap, i+1)
		out = append(out, types.HoldReview{
			ETFCode:    e.ETF.Code,
			ETFName:    e.ETF.Name,
			Sector:     e.ETF.Sector,
			InTop:      i < 5,
			Rank:       i + 1,
			Score:      e.Score,
			Action:     action,
			ActionDesc: actionDesc,
			NewsBias:   newsBias,
			TechTrend:  techTrend,
			Rationale: fmt.Sprintf(
				"量化分%.2f（落后 best %.2f）+ 动量动作=%s + 消息面=%s + 技术趋势=%s → %s。",
				e.Score, gap, e.ActionDesc, newsBias, techTrend, actionDesc,
			),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// sanitizeHoldReviews 校验 LLM 返回的 HoldReviews：
//   - 必须出自 CurrentHolds 集合；
//   - 必须能在 Top5/豁免位中匹配到候选（否则丢弃）；
//   - 缺字段时用规则兜底回填；
//   - action 不在 keep/trim/rotate 中时按规则重判。
func sanitizeHoldReviews(reviews []types.HoldReview, st *types.AgentState) []types.HoldReview {
	if len(reviews) == 0 || st == nil || st.Screener == nil {
		return buildHoldReviewsFallback(st)
	}
	holds := collectHolds(st)
	if len(holds) == 0 {
		return nil
	}
	candidateByCode := map[string]types.ScoredETF{}
	rankByCode := map[string]int{}
	for i, e := range st.Screener.Top5 {
		candidateByCode[e.ETF.Code] = e
		rankByCode[e.ETF.Code] = i + 1
	}
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
	bestScore := 0.0
	if len(st.Screener.Top5) > 0 {
		bestScore = st.Screener.Top5[0].Score
	}
	seen := make(map[string]struct{}, len(reviews))
	out := make([]types.HoldReview, 0, len(reviews))
	for _, r := range reviews {
		code := strings.TrimSpace(r.ETFCode)
		if code == "" {
			continue
		}
		if _, ok := holds[code]; !ok {
			continue
		}
		e, hit := candidateByCode[code]
		if !hit {
			continue
		}
		if _, dup := seen[code]; dup {
			continue
		}
		seen[code] = struct{}{}
		if r.ETFName == "" {
			r.ETFName = e.ETF.Name
		}
		if r.Sector == "" {
			r.Sector = e.ETF.Sector
		}
		if r.Score == 0 {
			r.Score = e.Score
		}
		if r.Rank == 0 {
			r.Rank = rankByCode[code]
		}
		r.InTop = r.Rank > 0 && r.Rank <= 5
		newsBias := classifyNewsBias(newsByCode[code])
		techTrend := classifyTechTrend(techByCode[code])
		if r.NewsBias == "" || !validNewsBias(r.NewsBias) {
			r.NewsBias = newsBias
		}
		if r.TechTrend == "" || !validTechTrend(r.TechTrend) {
			r.TechTrend = techTrend
		}
		if !validHoldAction(r.Action) {
			r.Action, r.ActionDesc = decideHoldAction(e, newsBias, techTrend, bestScore-e.Score, r.Rank)
		}
		if r.ActionDesc == "" {
			r.ActionDesc = holdActionDesc(r.Action)
		}
		if strings.TrimSpace(r.Rationale) == "" {
			r.Rationale = fmt.Sprintf(
				"量化分%.2f（落后 best %.2f）+ 动量动作=%s + 消息面=%s + 技术趋势=%s → %s。",
				e.Score, bestScore-e.Score, e.ActionDesc, r.NewsBias, r.TechTrend, r.ActionDesc,
			)
		}
		out = append(out, r)
	}
	// LLM 漏掉某些持仓时，用规则兜底补齐
	if len(out) < len(holds) {
		fallback := buildHoldReviewsFallback(st)
		for _, r := range fallback {
			if _, dup := seen[r.ETFCode]; dup {
				continue
			}
			out = append(out, r)
			seen[r.ETFCode] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func decideHoldAction(e types.ScoredETF, newsBias, techTrend string, gap float64, rank int) (string, string) {
	switch {
	case e.Action == string(ActionAvoid),
		techTrend == "down",
		newsBias == "negative",
		gap >= 15 && rank > 3:
		return "rotate", holdActionDesc("rotate")
	case e.Action == string(ActionHoldOnly),
		techTrend == "flat",
		newsBias == "neutral" && gap >= 8:
		return "trim", holdActionDesc("trim")
	default:
		return "keep", holdActionDesc("keep")
	}
}

func holdActionDesc(action string) string {
	switch action {
	case "keep":
		return "继续持有"
	case "trim":
		return "减仓观察"
	case "rotate":
		return "平仓切换"
	}
	return action
}

func validHoldAction(a string) bool {
	return a == "keep" || a == "trim" || a == "rotate"
}

func validNewsBias(s string) bool {
	return s == "positive" || s == "neutral" || s == "negative" || s == "unknown"
}

func validTechTrend(s string) bool {
	return s == "up" || s == "flat" || s == "down" || s == "unknown"
}

// classifyNewsBias 把 NewsAnalysis 的 sentiment/score 折成 4 档；缺数据走 unknown。
func classifyNewsBias(n types.NewsAnalysis) string {
	s := strings.ToLower(strings.TrimSpace(n.Sentiment))
	switch s {
	case "positive", "negative", "neutral":
		return s
	}
	if n.Score > 0 {
		switch {
		case n.Score >= 65:
			return "positive"
		case n.Score <= 40:
			return "negative"
		default:
			return "neutral"
		}
	}
	return "unknown"
}

// classifyTechTrend 把 TechnicalAnalysis.Trend 折成 up / flat / down / unknown。
func classifyTechTrend(t types.TechnicalAnalysis) string {
	tr := strings.ToLower(strings.TrimSpace(t.Trend))
	switch tr {
	case "up", "uptrend", "bull", "bullish":
		return "up"
	case "down", "downtrend", "bear", "bearish":
		return "down"
	case "flat", "sideways", "neutral", "range":
		return "flat"
	}
	if tr != "" {
		return tr
	}
	return "unknown"
}
