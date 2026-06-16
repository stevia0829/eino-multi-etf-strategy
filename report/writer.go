package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/eino-multi-etf-strategy/agent"
	"github.com/eino-multi-etf-strategy/types"
)

type Writer struct {
	Dir string
}

func NewWriter(dir string) *Writer {
	if dir == "" {
		dir = "report"
	}
	return &Writer{Dir: dir}
}

// Save 生成 markdown 报告。文件名 etf-report-YYYYMMDD-HHmmss.md。
func (w *Writer) Save(state *types.AgentState) (string, error) {
	if err := os.MkdirAll(w.Dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", w.Dir, err)
	}
	now := time.Now()
	filename := fmt.Sprintf("etf-report-%s.md", now.Format("20060102-150405"))
	path := filepath.Join(w.Dir, filename)

	content := BuildMarkdown(state, now)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	// 同步落 JSON sidecar，供下游 9:24 PreOpenAgent 直接 Unmarshal 使用。
	jsonPath := filepath.Join(w.Dir, fmt.Sprintf("etf-report-%s.json", now.Format("20060102-150405")))
	if buf, err := json.MarshalIndent(state, "", "  "); err == nil {
		_ = os.WriteFile(jsonPath, buf, 0o644)
	}
	abs, _ := filepath.Abs(path)
	return abs, nil
}

// BuildMarkdown 暴露给单测使用：纯函数，无 IO。
func BuildMarkdown(s *types.AgentState, now time.Time) string {
	var b strings.Builder
	b.WriteString("# A 股 ETF 开盘前多 Agent 策略报告\n\n")
	b.WriteString(fmt.Sprintf("- 生成时间: `%s`\n", now.Format("2006-01-02 15:04:05")))
	if s.Screener != nil && !s.Screener.AsOfDate.IsZero() {
		b.WriteString(fmt.Sprintf("- 行情基准日: `%s`\n", s.Screener.AsOfDate.Format("2006-01-02")))
	}
	if s.Final != nil {
		b.WriteString(fmt.Sprintf("- 综合评分: **%.2f**  ·  建议: **`%s`**\n",
			s.Final.OverallScore, s.Final.Recommendation))
	}
	b.WriteString("\n---\n\n")

	writeTargetSummary(&b, s)  // §1 目标 ETF
	writeTechnical(&b, s.Tech) // §2 技术面持有区间/阻力位
	writeNews(&b, s.News)      // §3 大面消息摘要
	writeGlobal(&b, s.Global)
	writeRegime(&b, s.Regime)       // §x 宏观环境过滤
	writeMoneyFlow(&b, s.MoneyFlow) // §x 资金流向
	writeScreener(&b, s.Screener)
	writeFinal(&b, s.Final) // §n 综合加权评分
	writeHoldAdvice(&b, s)  // §last-1 持仓对照（CurrentHolds 为空时跳过）
	writeHoldReviews(&b, s) // §last 持仓评审（HoldReviews 为空时跳过）

	b.WriteString("\n---\n")
	b.WriteString("> ⚠️ 本报告由多 Agent + LLM 自动生成，仅供研究参考，不构成投资建议。\n")
	return b.String()
}

func writeTargetSummary(b *strings.Builder, s *types.AgentState) {
	if s.Screener == nil {
		return
	}
	best := s.Screener.Best
	b.WriteString("## 一、目标 ETF\n\n")
	hasIOPV := best.ETF.IOPV > 0
	if hasIOPV {
		b.WriteString("| 名称 | 代码 | 板块 | 现价 | IOPV | 溢价率 | 量化分 |\n|---|---|---|---|---|---|---|\n")
		b.WriteString(fmt.Sprintf("| **%s** | `%s` | %s | %.3f | %.4f | %s | %.2f |\n\n",
			best.ETF.Name, best.ETF.Code, best.ETF.Sector, best.ETF.Price,
			best.ETF.IOPV, formatPremium(best.ETF.PremiumPct), best.Score))
		if note := premiumCallout(best.ETF.PremiumPct); note != "" {
			b.WriteString("> " + note + "\n\n")
		}
	} else {
		b.WriteString("| 名称 | 代码 | 板块 | 现价 | 量化分 |\n|---|---|---|---|---|\n")
		b.WriteString(fmt.Sprintf("| **%s** | `%s` | %s | %.3f | %.2f |\n\n",
			best.ETF.Name, best.ETF.Code, best.ETF.Sector, best.ETF.Price, best.Score))
	}
	if best.Reason != "" {
		b.WriteString("**入选理由**：" + best.Reason + "\n\n")
	}
}

// formatPremium 把溢价率渲染为带风险图标的文本，例如 "+1.62% ⚠️"。
func formatPremium(p float64) string {
	icon := ""
	switch agent.PremiumRiskLabel(p) {
	case "high":
		icon = " 🚨高溢价"
	case "elevated":
		icon = " ⚠️偏高"
	case "discount":
		icon = " 💎折价"
	}
	return fmt.Sprintf("%+.2f%%%s", p*100, icon)
}

// premiumCallout 返回溢价率风险提示行；正常 / 折价时返回空字符串。
func premiumCallout(p float64) string {
	switch agent.PremiumRiskLabel(p) {
	case "high":
		return fmt.Sprintf("🚨 **高溢价警告**：当前溢价率 %+.2f%% ≥ %.1f%%，场内资金对该 ETF 追捧严重，回归净值的下行风险显著。recommendation 已强制降档至 hold；如确需介入，建议等待溢价回落至 +1.5%% 以下再入场。",
			p*100, agent.PremiumDowngradeThreshold*100)
	case "elevated":
		return fmt.Sprintf("⚠️ **溢价偏高**：当前溢价率 %+.2f%% ≥ %.1f%%，存在追高风险；建议轻仓 / 等回调，避免开盘冲高介入。",
			p*100, agent.PremiumWarnThreshold*100)
	case "discount":
		return fmt.Sprintf("💎 **折价**：当前溢价率 %+.2f%%，场内价低于 IOPV，理论上有套利空间。",
			p*100)
	}
	return ""
}

func writeTechnical(b *strings.Builder, t *types.TechnicalAnalysis) {
	if t == nil {
		return
	}
	b.WriteString("## 二、技术面研判 (TechnicalAgent)\n\n")
	b.WriteString(fmt.Sprintf("- 趋势: **%s** · 评分: **%.2f**\n", t.Trend, t.Score))
	if t.HoldRange != "" {
		b.WriteString(fmt.Sprintf("- **建议持有区间**: `%s`\n", t.HoldRange))
	}
	b.WriteString(fmt.Sprintf("- **关键价位**：一线支撑 `%.3f` · 二线支撑 `%.3f` · 阻力位 `%.3f`\n\n",
		t.Support1, t.Support2, t.Resistance))
	if len(t.Signals) > 0 {
		b.WriteString("**技术信号**\n\n| 维度 | 状态 |\n|---|---|\n")
		for k, v := range t.Signals {
			b.WriteString(fmt.Sprintf("| %s | %s |\n", k, v))
		}
		b.WriteString("\n")
	}
	if len(t.Indicators) > 0 {
		b.WriteString("**核心指标**\n\n| 指标 | 值 |\n|---|---|\n")
		keys := []string{"MA5", "MA20", "MA60", "RSI", "DIF", "DEA", "HIST", "Momentum20", "VolRatio", "Volatility"}
		for _, k := range keys {
			if v, ok := t.Indicators[k]; ok {
				b.WriteString(fmt.Sprintf("| %s | %.4f |\n", k, v))
			}
		}
		b.WriteString("\n")
	}
	if t.Summary != "" {
		b.WriteString("**研判**\n\n> " + indentQuote(t.Summary) + "\n\n")
	}
}

func writeNews(b *strings.Builder, n *types.NewsAnalysis) {
	if n == nil {
		return
	}
	// 情绪标记
	sentimentIcon := map[string]string{
		"positive": "🟢", "negative": "🔴", "neutral": "⚪",
	}[n.Sentiment]
	if sentimentIcon == "" {
		sentimentIcon = "⚪"
	}

	b.WriteString("## 三、消息面研判 (NewsAgent)\n\n")
	b.WriteString(fmt.Sprintf("| 板块 | 情绪 | 评分 |\n|---|---|---|\n"))
	b.WriteString(fmt.Sprintf("| **%s** | %s **%s** | **%.0f**/100 |\n\n", n.Sector, sentimentIcon, n.Sentiment, n.Score))

	// 关键要点（信息密度优先）
	if len(n.Highlight) > 0 {
		b.WriteString("### 关键信号\n\n")
		for _, h := range n.Highlight {
			b.WriteString("- 💬 " + h + "\n")
		}
		b.WriteString("\n")
	}

	// 研判摘要（4段式结构化输出）
	if n.Summary != "" {
		b.WriteString("### 消息面速览\n\n")
		// 不加引用块了，缩进引用块不利于长期记忆提取
		b.WriteString(n.Summary + "\n\n")
	}
}

func writeGlobal(b *strings.Builder, g *types.GlobalMarketAnalysis) {
	if g == nil {
		return
	}
	b.WriteString("## 四、跨境市场联动 (GlobalMarketAgent)\n\n")
	b.WriteString(fmt.Sprintf("- 整体情绪: **%s** · 评分: **%.2f**\n\n", g.Sentiment, g.Score))
	b.WriteString("| 市场 | 指数 | 涨跌 | 涨跌幅 | 备注 |\n|---|---|---|---|---|\n")
	rows := []types.MarketSnapshot{g.USPrev, g.JPToday, g.KRToday}
	labels := []string{"美股(前夜)", "日本(今日)", "韩国(今日)"}
	for i, r := range rows {
		b.WriteString(fmt.Sprintf("| %s | %s | %.2f | %.2f%% | %s |\n",
			labels[i], r.Index, r.Change, r.ChangePc, r.Note))
	}
	if g.Summary != "" {
		b.WriteString("\n**传导研判**\n\n> " + indentQuote(g.Summary) + "\n\n")
	}
}

func writeScreener(b *strings.Builder, sc *types.ScreenerResult) {
	if sc == nil {
		return
	}
	hasIOPV := false
	for _, e := range sc.Top5 {
		if e.ETF.IOPV > 0 {
			hasIOPV = true
			break
		}
	}
	b.WriteString("## 七、量化筛选 Top5 (ScreenerAgent)\n\n")
	if hasIOPV {
		b.WriteString("| 排名 | 名称 | 代码 | 板块 | 现价 | 溢价率 | 综合分 | 动量动作 | 入选理由 |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
		for i, e := range sc.Top5 {
			action := e.ActionDesc
			if action == "" {
				action = e.Action
			}
			premium := "—"
			if e.ETF.IOPV > 0 {
				premium = formatPremium(e.ETF.PremiumPct)
			}
			name := e.ETF.Name
			if e.IsCurrentHold {
				name = "🟦 " + name
			}
			b.WriteString(fmt.Sprintf("| %d | %s | %s | %s | %.3f | %s | %.2f | %s | %s |\n",
				i+1, name, e.ETF.Code, e.ETF.Sector, e.ETF.Price, premium, e.Score, action, e.Reason))
		}
	} else {
		b.WriteString("| 排名 | 名称 | 代码 | 板块 | 现价 | 综合分 | 动量动作 | 入选理由 |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|\n")
		for i, e := range sc.Top5 {
			action := e.ActionDesc
			if action == "" {
				action = e.Action
			}
			name := e.ETF.Name
			if e.IsCurrentHold {
				name = "🟦 " + name
			}
			b.WriteString(fmt.Sprintf("| %d | %s | %s | %s | %.3f | %.2f | %s | %s |\n",
				i+1, name, e.ETF.Code, e.ETF.Sector, e.ETF.Price, e.Score, action, e.Reason))
		}
	}
	b.WriteString("\n")
}

func writeFinal(b *strings.Builder, f *types.FinalDecision) {
	if f == nil {
		return
	}
	b.WriteString("## 八、加权综合评分与交易决策 (FinalAgent)\n\n")
	b.WriteString("**多 Agent 加权打分**\n\n")
	b.WriteString("| 维度 | 子分数 | 权重 | 跨标调整 | 贡献分 |\n|---|---|---|---|---|\n")
	if f.ScoreBreakdown != nil {
		row := func(label, sk, wk, skAdj, pk string) {
			adj := f.ScoreBreakdown[skAdj]
			adjStr := ""
			if adj > 0 && (adj < 0.995 || adj > 1.005) {
				adjStr = fmt.Sprintf("%+.1f%%", (adj-1)*100)
			}
			b.WriteString(fmt.Sprintf("| %s | %.2f | %.0f%% | %s | %.2f |\n",
				label, f.ScoreBreakdown[sk], f.ScoreBreakdown[wk]*100, adjStr, f.ScoreBreakdown[pk]))
		}
		row("量化动量", "quant", "quant_weight", "quant", "quant_part")
		row("消息面", "news", "news_weight", "news_adj", "news_part")
		row("跨境联动", "global", "global_weight", "global", "global_part")
		row("技术面", "tech", "tech_weight", "tech_adj", "tech_part")
		if _, ok := f.ScoreBreakdown["regime"]; ok {
			row("宏观环境", "regime", "regime_weight", "regime", "regime_part")
		}
		if _, ok := f.ScoreBreakdown["flow"]; ok {
			row("资金面", "flow", "flow_weight", "flow", "flow_part")
		}
	}
	b.WriteString(fmt.Sprintf("| **综合** |  |  |  | **%.2f** |\n\n", f.OverallScore))

	b.WriteString("**操作建议**\n\n")
	b.WriteString("| 项目 | 数值 |\n|---|---|\n")
	b.WriteString(fmt.Sprintf("| 建议 | `%s` |\n", f.Recommendation))
	b.WriteString(fmt.Sprintf("| 入场 | %.3f |\n", f.EntryPrice))
	b.WriteString(fmt.Sprintf("| 止损 | %.3f |\n", f.StopLoss))
	b.WriteString(fmt.Sprintf("| 止盈 | %.3f |\n", f.TakeProfit))
	if f.EntryPrice > 0 {
		risk := f.EntryPrice - f.StopLoss
		if risk <= 0 {
			risk = 1e-6
		}
		b.WriteString(fmt.Sprintf("| 盈亏比 | 1 : %.2f |\n", (f.TakeProfit-f.EntryPrice)/risk))
	}
	b.WriteString("\n")
	if f.Reasoning != "" {
		b.WriteString("**综合论证**\n\n> " + indentQuote(f.Reasoning) + "\n\n")
	}
}

func indentQuote(s string) string {
	return strings.ReplaceAll(s, "\n", "\n> ")
}

// writeRegime 输出宏观环境过滤结果。
func writeRegime(b *strings.Builder, r *types.RegimeAnalysis) {
	if r == nil {
		return
	}
	b.WriteString("## 五、宏观环境过滤 (RegimeAgent)\n\n")
	b.WriteString(fmt.Sprintf("- 基准: `%s` · 趋势: **%s** · 评分: **%.2f** · 建议最大仓位: **%.0f%%**\n",
		r.Benchmark, r.Trend, r.Score, r.PositionCap*100))
	b.WriteString("\n| 指标 | 值 |\n|---|---|\n")
	b.WriteString(fmt.Sprintf("| 价格 vs MA20 | %+.2f%% |\n", r.PriceVsMA20*100))
	b.WriteString(fmt.Sprintf("| 价格 vs MA60 | %+.2f%% |\n", r.PriceVsMA60*100))
	b.WriteString(fmt.Sprintf("| 价格 vs MA120 | %+.2f%% |\n", r.PriceVsMA120*100))
	b.WriteString(fmt.Sprintf("| 60 日最大回撤 | %.2f%% |\n\n", r.DrawDown60*100))
	if r.Summary != "" {
		b.WriteString("**研判**\n\n> " + indentQuote(r.Summary) + "\n\n")
	}
}

// writeMoneyFlow 输出资金流向（北向 / ETF 申赎 / 主力）。
func writeMoneyFlow(b *strings.Builder, m *types.MoneyFlowAnalysis) {
	if m == nil {
		return
	}
	b.WriteString("## 六、资金流向 (MoneyFlowAgent)\n\n")
	b.WriteString(fmt.Sprintf("- ETF 代码: `%s` · 情绪: **%s** · 评分: **%.2f**\n", m.ETFCode, m.Sentiment, m.Score))
	b.WriteString("\n| 维度 | 数值（亿元，估算） |\n|---|---|\n")
	b.WriteString(fmt.Sprintf("| 北向资金 5 日累计 | %+.2f |\n", m.NorthCapital5d))
	b.WriteString(fmt.Sprintf("| 北向资金 20 日累计 | %+.2f |\n", m.NorthCapital20d))
	b.WriteString(fmt.Sprintf("| ETF 5 日净申购 | %+.2f |\n", m.ETFNetSubscribe))
	b.WriteString(fmt.Sprintf("| 主力 3 日净流入 | %+.2f |\n\n", m.MainNetInflow3d))
	if m.Summary != "" {
		b.WriteString("**研判**\n\n> " + indentQuote(m.Summary) + "\n\n")
	}
	b.WriteString("> 备注：本节为基于量价行为推导的代理估算，非真实北向 / 申赎数据，仅作辅助参考。\n\n")
}

// writeHoldAdvice 当用户提供 CurrentHold(s) 时输出持仓对照章节；为空则整段跳过。
// 多持仓模式：依次列出每只持仓的对照建议；兼容旧 HoldAdvice 单值字段。
func writeHoldAdvice(b *strings.Builder, s *types.AgentState) {
	if s == nil {
		return
	}
	advices := s.HoldAdvices
	if len(advices) == 0 && s.HoldAdvice != nil {
		advices = []types.HoldAdvice{*s.HoldAdvice}
	}
	if len(advices) == 0 {
		return
	}
	b.WriteString("## 九、与您当前持仓的对照\n\n")
	bestCode, bestName := "", ""
	for i, a := range advices {
		if i == 0 {
			bestCode, bestName = a.BestCode, a.BestName
		}
		b.WriteString(fmt.Sprintf("### 持仓 %d：`%s`\n\n", i+1, a.CurrentHold))
		if a.HitTop {
			b.WriteString(fmt.Sprintf("- 命中 Top5：✅ 第 **%d** 名（%s）\n", a.Rank, a.HitName))
			if a.ActionDesc != "" {
				b.WriteString(fmt.Sprintf("- 动量动作：**%s**\n", a.ActionDesc))
			} else if a.Action != "" {
				b.WriteString(fmt.Sprintf("- 动量动作：**%s**\n", a.Action))
			}
		} else {
			b.WriteString("- 命中 Top5：❌ 未进入候选\n")
		}
		if a.Suggestion != "" {
			b.WriteString("\n> " + indentQuote(a.Suggestion) + "\n\n")
		}
	}
	if bestCode != "" {
		b.WriteString(fmt.Sprintf("- 当日策略 Top1：%s(`%s`)\n\n", bestName, bestCode))
	}
	b.WriteString("> 提示：本系统不会保存您的任何持仓信息，`--current-hold` 仅在本次会话使用。\n\n")
}

// writeHoldReviews 输出 FinalAgent 给出的"逐只持仓客观评审"（keep / trim / rotate）。
// 与 writeHoldAdvice 的差异：HoldAdvice 仅基于动量名次给出；HoldReview 结合 score / news / tech
// 给出更立体的评审，是"持仓维度的最终决策"，但不影响主决策的 recommendation / picks。
func writeHoldReviews(b *strings.Builder, s *types.AgentState) {
	if s == nil || s.Final == nil || len(s.Final.HoldReviews) == 0 {
		return
	}
	b.WriteString("## 十、当前持仓客观评审 (FinalAgent · HoldReviews)\n\n")
	b.WriteString("| 名称 | 代码 | 板块 | 名次 | 量化分 | 消息面 | 技术趋势 | 建议 |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|\n")
	for _, r := range s.Final.HoldReviews {
		rank := "外"
		if r.InTop {
			rank = fmt.Sprintf("Top%d", r.Rank)
		} else if r.Rank > 0 {
			rank = fmt.Sprintf("第%d", r.Rank)
		}
		desc := r.ActionDesc
		if desc == "" {
			desc = r.Action
		}
		b.WriteString(fmt.Sprintf("| %s | `%s` | %s | %s | %.2f | %s | %s | **`%s`**（%s） |\n",
			r.ETFName, r.ETFCode, r.Sector, rank, r.Score, r.NewsBias, r.TechTrend, r.Action, desc))
	}
	b.WriteString("\n")
	for _, r := range s.Final.HoldReviews {
		if strings.TrimSpace(r.Rationale) == "" {
			continue
		}
		b.WriteString(fmt.Sprintf("- **%s(`%s`)**：%s\n", r.ETFName, r.ETFCode, r.Rationale))
	}
	b.WriteString("\n> 备注：HoldReviews 仅评审您当前持仓，不影响上方的 recommendation / picks / 价格方案；")
	b.WriteString("是否调仓需结合您自己的成本与税费综合判断。\n\n")
}
