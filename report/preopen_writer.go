package report

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/multi-agents-etf-trade-strategy/types"
)

// SavePreOpen 落地 9:24 集合竞价复核报告，文件名 preopen-report-YYYYMMDD-HHmmss.md。
func SavePreOpen(dir string, a *types.PreOpenAnalysis) (string, error) {
	if dir == "" {
		dir = "report"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	now := a.GeneratedAt
	if now.IsZero() {
		now = time.Now()
	}
	filename := fmt.Sprintf("preopen-report-%s.md", now.Format("20060102-150405"))
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(BuildPreOpenMarkdown(a)), 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	abs, _ := filepath.Abs(path)
	return abs, nil
}

// BuildPreOpenMarkdown 纯函数渲染。
func BuildPreOpenMarkdown(a *types.PreOpenAnalysis) string {
	var b strings.Builder
	b.WriteString("# A 股 ETF 集合竞价复核报告 (9:24 PreOpenAgent)\n\n")
	now := a.GeneratedAt
	if now.IsZero() {
		now = time.Now()
	}
	b.WriteString(fmt.Sprintf("- 生成时间: `%s`\n", now.Format("2006-01-02 15:04:05")))
	if a.BaseReportPath != "" {
		b.WriteString(fmt.Sprintf("- 基准报告: `%s`\n", a.BaseReportPath))
	}
	if len(a.CurrentHolds) > 0 {
		b.WriteString(fmt.Sprintf("- 当前持仓: `%s`\n", strings.Join(a.CurrentHolds, ", ")))
	} else if a.CurrentHold != "" {
		b.WriteString(fmt.Sprintf("- 当前持仓: `%s`\n", a.CurrentHold))
	}
	b.WriteString(fmt.Sprintf("- 大盘集合竞价情绪: **`%s`**\n\n", a.MarketBias))
	b.WriteString("---\n\n")

	// 大盘
	b.WriteString("## 一、大盘 510300 集合竞价快照\n\n")
	b.WriteString("| 昨收 | 撮合价 | IOPV | 溢价率 | 跳空% |\n|---|---|---|---|---|\n")
	b.WriteString(fmt.Sprintf("| %.4f | %.4f | %.4f | %+.2f%% | %+.2f%% |\n\n",
		a.Market.PrevClose, a.Market.AuctionPrice, a.Market.IOPV,
		a.Market.PremiumPct*100, a.Market.GapPct*100))

	// 标的复核
	b.WriteString("## 二、目标标的逐只复核\n\n")
	b.WriteString("| 名称 | 代码 | 昨收 | 撮合价 | IOPV | 溢价率 | 跳空% | 8:50 入场 | 入场偏离% | 复核结论 | 备注 |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, s := range a.Snapshots {
		b.WriteString(fmt.Sprintf("| %s | `%s` | %.4f | %.4f | %.4f | %+.2f%% | %+.2f%% | %.4f | %+.2f%% | **`%s`** | %s |\n",
			s.ETFName, s.ETFCode, s.PrevClose, s.AuctionPrice, s.IOPV,
			s.PremiumPct*100, s.GapPct*100,
			s.EntryPrice, s.EntryGapPct*100,
			s.Verdict, s.Note))
	}
	b.WriteString("\n")

	// 调整后价位
	b.WriteString("## 三、调整后入场 / 止损 / 止盈\n\n")
	b.WriteString("| 名称 | 代码 | 复核结论 | 调整入场 | 止损 | 止盈 | 盈亏比 |\n|---|---|---|---|---|---|---|\n")
	for _, s := range a.Snapshots {
		ratio := "—"
		if s.AdjEntry > 0 && s.AdjStopLoss > 0 && s.AdjTakeProf > 0 {
			risk := s.AdjEntry - s.AdjStopLoss
			if risk > 1e-6 {
				ratio = fmt.Sprintf("1 : %.2f", (s.AdjTakeProf-s.AdjEntry)/risk)
			}
		}
		entry := fmt.Sprintf("%.4f", s.AdjEntry)
		if s.AdjEntry <= 0 {
			entry = "—"
		}
		b.WriteString(fmt.Sprintf("| %s | `%s` | `%s` | %s | %.4f | %.4f | %s |\n",
			s.ETFName, s.ETFCode, s.Verdict, entry, s.AdjStopLoss, s.AdjTakeProf, ratio))
	}
	b.WriteString("\n")

	// 综合论证
	if a.Summary != "" {
		b.WriteString("## 四、综合论证\n\n> ")
		b.WriteString(strings.ReplaceAll(a.Summary, "\n", "\n> "))
		b.WriteString("\n\n")
	}
	if a.FinalAction != "" {
		b.WriteString(fmt.Sprintf("**最终建议**：%s\n\n", a.FinalAction))
	}

	b.WriteString("---\n")
	b.WriteString("> ⚠️ 本报告基于 9:20-9:25 集合竞价不可撤单期撮合数据，仅供研究参考；")
	b.WriteString("溢价率 / 跳空数据存在最后 30 秒突变风险，开盘前请以场内最新报价为准。\n")
	return b.String()
}
