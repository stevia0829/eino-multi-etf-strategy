package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/multi-agents-etf-trade-strategy/llm"
	"github.com/multi-agents-etf-trade-strategy/types"
)

// MemoryAgent 长期记忆 Agent。
//
// 职责：
//  1. 默认扫描 MemoryDir（默认 "report"）下最近 MemoryWindow（默认 5）份 markdown 报告；
//  2. 用正则把每份报告压缩成 HistoryMemo（日期 / 目标 ETF / 综合评分 / 建议 / reasoning 摘要）；
//  3. 调用 LLM 把这串纪要再综合成一段"长期记忆复盘"，提示连续追高 / 连续踏空 / 板块切换等 pattern；
//  4. 失败时给出朴素规则版兜底，保证 FinalAgent 总能拿到 memory。
//
// 输出经由 AgentState.Memory 注入 FinalAgent，使其无需直接读文件。
type MemoryAgent struct {
	LLM          llm.Client
	MemoryDir    string
	MemoryWindow int
}

func NewMemoryAgent(c llm.Client) *MemoryAgent {
	return &MemoryAgent{LLM: c, MemoryDir: "report", MemoryWindow: 5}
}

const memorySystemPrompt = `你是一名资深"投委会秘书"，负责把过去若干天投委会的会议纪要压缩成
一份给今日 CIO 阅读的"长期记忆备忘"，帮助识别跨日 pattern。

【你必须从输入纪要中识别的 pattern（若不存在则不要编造）】
1) 连续追高：连续 ≥2 天 strong_buy/buy 同一板块且每日溢价率走高；
2) 连续踏空：连续 ≥2 天 hold/avoid 但事后回看动量加速；
3) 板块切换：今日目标 ETF 板块与最近 2~3 日不同；
4) 评分中枢漂移：综合评分 3 日均值是否明显抬升或下降；
5) 建议反复：同一 ETF 在窗口内被多次推荐，需提示"是否仍处于趋势"；
6) 消息面情绪转向（NEW）：最近 2 日 news_score 从 ≥60 骤降到 ≤40（或反之），标记为"消息面转折"；
7) 板块消息面分化（NEW）：不同板块的 news_sentiment 出现明显分化，提示结构性机会。

仅输出 JSON：
{
  "summary": "<=200 字，用平铺直叙的语气总结过去几天的关键决策与 pattern",
  "patterns": ["最多 4 条，每条 < 30 字，必须能从输入纪要中找到出处"],
  "warnings": ["最多 2 条，给今日 CIO 的注意事项"]
}
约束：
- 严禁编造未在输入纪要中出现的 ETF 代码 / 板块 / 数字；
- 当输入纪要少于 2 份时，summary 中必须明示"历史样本不足"；
- 消息面相关 pattern 必须基于纪要中的 news_sentiment/news_score 字段；
- 不输出任何 markdown 标记。`

// Run 输出 MemorySummary。失败时给规则版兜底。
func (a *MemoryAgent) Run(ctx context.Context) (*types.MemorySummary, error) {
	memos := a.loadMemos()
	out := &types.MemorySummary{Memos: memos}
	if len(memos) == 0 {
		out.Summary = "历史样本不足：report 目录下未找到可用的历史报告。"
		return out, nil
	}

	user := fmt.Sprintf(
		"以下是最近 %d 份投委会会议纪要（按时间倒序，最新一份排第一）：\n%s\n\n请按 JSON Schema 输出长期记忆备忘。",
		len(memos), formatMemosForLLM(memos),
	)

	err := callLLMJSON(ctx, a.LLM, memorySystemPrompt, user, out, func(raw string) {
		if out.Summary == "" {
			out.Summary = raw
		}
	})
	if err != nil || out.Summary == "" {
		ruleBasedMemory(out, memos)
	}
	out.Memos = memos
	return out, nil
}

// loadMemos 扫描目录、解析最近 N 份报告。
func (a *MemoryAgent) loadMemos() []types.HistoryMemo {
	dir := a.MemoryDir
	if dir == "" {
		dir = "report"
	}
	window := a.MemoryWindow
	if window <= 0 {
		window = 5
	}
	matches, err := filepath.Glob(filepath.Join(dir, "etf-report-*.md"))
	if err != nil || len(matches) == 0 {
		return nil
	}
	// 文件名内嵌 YYYYMMDD-HHmmss，字典序 == 时间序，倒序后取前 window 份。
	sort.Sort(sort.Reverse(sort.StringSlice(matches)))
	if len(matches) > window {
		matches = matches[:window]
	}
	out := make([]types.HistoryMemo, 0, len(matches))
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if memo, ok := parseReportMemo(string(raw)); ok {
			out = append(out, memo)
		}
	}
	return out
}

func formatMemosForLLM(memos []types.HistoryMemo) string {
	var b strings.Builder
	for i, m := range memos {
		// 包含消息面摘要以便跨日情绪追踪
		newsLine := ""
		if m.NewsSentiment != "" {
			newsLine = fmt.Sprintf(" | 消息面=%s(%.0f)", m.NewsSentiment, m.NewsScore)
			if m.NewsGist != "" {
				newsLine += fmt.Sprintf(" · %s", m.NewsGist)
			}
		}
		fmt.Fprintf(&b, "%d. %s | %s(%s) | 板块=%s | 评分=%.1f | 建议=%s%s\n   摘要: %s\n",
			i+1, m.Date, m.TargetName, m.TargetCode, m.Sector,
			m.OverallScore, m.Recommendation, newsLine, m.ReasoningGist,
		)
	}
	return b.String()
}

// ruleBasedMemory 朴素规则兜底：扫一遍 memos，发现简单 pattern。
func ruleBasedMemory(out *types.MemorySummary, memos []types.HistoryMemo) {
	if len(memos) == 0 {
		out.Summary = "历史样本不足。"
		return
	}
	patterns := []string{}
	warnings := []string{}

	// 连续看多
	consecutiveBuy := 0
	for _, m := range memos {
		if m.Recommendation == "buy" || m.Recommendation == "strong_buy" {
			consecutiveBuy++
		} else {
			break
		}
	}
	if consecutiveBuy >= 2 {
		patterns = append(patterns, fmt.Sprintf("近 %d 日连续看多", consecutiveBuy))
		warnings = append(warnings, "连续看多需警惕情绪过热与追高")
	}

	// 板块切换
	if len(memos) >= 2 && memos[0].Sector != "" && memos[1].Sector != "" && memos[0].Sector != memos[1].Sector {
		patterns = append(patterns, fmt.Sprintf("板块切换：%s → %s", memos[1].Sector, memos[0].Sector))
	}

	// 评分中枢
	if len(memos) >= 3 {
		avg := (memos[0].OverallScore + memos[1].OverallScore + memos[2].OverallScore) / 3
		patterns = append(patterns, fmt.Sprintf("近 3 日综合评分均值 %.1f", avg))
	}

	// 同一 ETF 重复推荐
	count := map[string]int{}
	for _, m := range memos {
		if m.TargetCode != "" {
			count[m.TargetCode]++
		}
	}
	for code, c := range count {
		if c >= 3 {
			patterns = append(patterns, fmt.Sprintf("ETF %s 在窗口内被推荐 %d 次", code, c))
		}
	}

	// 消息面情绪转向检测
	if len(memos) >= 2 {
		latest := memos[0]
		prev := memos[1]
		if latest.NewsSentiment != "" && prev.NewsSentiment != "" {
			if (latest.NewsSentiment == "negative" && prev.NewsSentiment == "positive") ||
				(prev.NewsScore >= 60 && latest.NewsScore > 0 && latest.NewsScore <= 40) {
				patterns = append(patterns, "消息面情绪从积极转为负面")
				warnings = append(warnings, "消息面转向偏空，注意风险事件是否持续发酵")
			} else if (latest.NewsSentiment == "positive" && prev.NewsSentiment == "negative") ||
				(prev.NewsScore <= 40 && latest.NewsScore >= 60) {
				patterns = append(patterns, "消息面情绪从负面转为积极")
			}
		}
		// 板块消息面分化（跨板块对比）
		sectors := map[string]string{}
		for _, m := range memos {
			if m.Sector != "" && m.NewsSentiment != "" {
				sectors[m.Sector] = m.NewsSentiment
			}
		}
		if len(sectors) >= 2 {
			hasPos, hasNeg := false, false
			for _, s := range sectors {
				if s == "positive" {
					hasPos = true
				}
				if s == "negative" {
					hasNeg = true
				}
			}
			if hasPos && hasNeg {
				patterns = append(patterns, "板块消息面出现结构性分化")
			}
		}
	}

	out.Summary = fmt.Sprintf(
		"规则版长期记忆复盘（共 %d 份历史报告）：最新建议 %s（%s/%s）。",
		len(memos), memos[0].Recommendation, memos[0].TargetName, memos[0].TargetCode,
	)
	out.Patterns = patterns
	out.Warnings = warnings
}
