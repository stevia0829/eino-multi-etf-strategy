package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/eino-multi-etf-strategy/agent"
	"github.com/eino-multi-etf-strategy/backtest"
	"github.com/eino-multi-etf-strategy/config"
	"github.com/eino-multi-etf-strategy/datasource"
	"github.com/eino-multi-etf-strategy/orchestrator"
	"github.com/eino-multi-etf-strategy/report"
)

func main() {
	var (
		dateFlag    = flag.String("date", "", "回测/复盘的基准日期 (YYYY-MM-DD)，为空时取当天最新行情")
		timeFlag    = flag.String("time", "09:30", "模拟运行时刻 (HH:MM, 24h, 仅 advice 模式生效)，作为指数/数据源 AsOf 锚点")
		reportDir   = flag.String("report-dir", "report", "Markdown 报告输出目录")
		skipReport  = flag.Bool("skip-report", false, "仅打印结果，不落地报告")
		currentHold = flag.String("current-hold", "", "可选：当前持仓 ETF 代码（如 159915），用于在报告中给出持仓对照建议；留空则跳过该章节，系统不做任何本地持久化")
		mode        = flag.String("mode", "advice", "运行模式：advice（默认，单次出报告） / backtest（历史胜率回测）")
		btStart     = flag.String("bt-start", "", "backtest 起始日 (YYYY-MM-DD)")
		btEnd       = flag.String("bt-end", "", "backtest 结束日 (YYYY-MM-DD)，默认 --date 或今天")
		btStep      = flag.Int("bt-step", 5, "backtest 采样间隔（交易日）")
		btHold      = flag.Int("bt-hold", 5, "backtest 持有期（交易日）")
		btMax       = flag.Int("bt-max", 60, "backtest 最大样本数")
		btVariant   = flag.String("bt-variant", "both", "回测变体：v3 / v3v2 / both")
	)
	flag.Parse()

	if *mode == "backtest" {
		runBacktest(*btStart, *btEnd, *dateFlag, *btStep, *btHold, *btMax, *btVariant, *reportDir)
		return
	}

	asOf := time.Time{}
	if *dateFlag != "" {
		t, err := time.ParseInLocation("2006-01-02", *dateFlag, time.Local)
		if err != nil {
			fmt.Println("invalid --date, expect YYYY-MM-DD:", err)
			os.Exit(2)
		}
		asOf = t
	}
	// --time 默认 09:30；与 --date 合并成一个完整的 AsOf 锚点。
	// 若 --date 为空（取当天行情），则忽略 --time（保持 zero 值，让下游用"当前时刻"逻辑）。
	if !asOf.IsZero() && *timeFlag != "" {
		hm, err := time.Parse("15:04", *timeFlag)
		if err != nil {
			fmt.Println("invalid --time, expect HH:MM:", err)
			os.Exit(2)
		}
		asOf = time.Date(asOf.Year(), asOf.Month(), asOf.Day(), hm.Hour(), hm.Minute(), 0, 0, asOf.Location())
	}

	cfg := config.Load()
	fmt.Println("=== A 股 ETF 开盘前多 Agent 分析 ===")
	fmt.Printf("主模型: %s/%s\n", cfg.LLM.Primary.Name, cfg.LLM.Primary.Model)
	if len(cfg.LLM.Fallbacks) > 0 {
		fmt.Print("降级链: ")
		for _, f := range cfg.LLM.Fallbacks {
			if f.Enabled {
				fmt.Printf("%s/%s ", f.Name, f.Model)
			}
		}
		fmt.Println()
	}
	if asOf.IsZero() {
		fmt.Println("基准日期: 当天最新行情")
	} else {
		fmt.Printf("基准日期: %s (回测/复盘模式)\n", asOf.Format("2006-01-02"))
	}
	fmt.Println("时间:", time.Now().Format("2006-01-02 15:04:05"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pipe, err := orchestrator.NewPipeline(cfg)
	if err != nil {
		fmt.Println("init pipeline error:", err)
		os.Exit(1)
	}
	pipe.Screener.AsOf = asOf

	state, err := pipe.Run(ctx)
	if err != nil {
		fmt.Println("pipeline error:", err)
		os.Exit(1)
	}

	// 持仓对照（无状态：仅本次会话使用 --current-hold 传入的值）
	state.CurrentHold = *currentHold
	if state.Screener != nil {
		state.HoldAdvice = agent.BuildHoldAdvice(*currentHold, state.Screener.Top5)
	}

	fmt.Println()
	fmt.Println("--- Top5 候选 ---")
	for i, e := range state.Screener.Top5 {
		fmt.Printf("%d) %s(%s) sector=%s score=%.2f action=%s reason=%s\n",
			i+1, e.ETF.Name, e.ETF.Code, e.ETF.Sector, e.Score, e.Action, e.Reason)
	}

	fmt.Println()
	fmt.Println("--- 最佳目标 ---")
	best := state.Screener.Best
	fmt.Printf("%s(%s) 板块=%s 价格=%.3f 综合分=%.2f 动作=%s\n",
		best.ETF.Name, best.ETF.Code, best.ETF.Sector, best.ETF.Price, best.Score, best.ActionDesc)

	if state.HoldAdvice != nil {
		fmt.Println()
		fmt.Println("--- 持仓对照 ---")
		fmt.Println(state.HoldAdvice.Suggestion)
	}

	fmt.Println()
	fmt.Println("--- 各 Agent 分析 ---")
	if state.Regime != nil {
		printJSON("Regime", state.Regime)
	}
	if state.MoneyFlow != nil {
		printJSON("MoneyFlow", state.MoneyFlow)
	}
	if state.News != nil {
		printJSON("News", state.News)
	}
	if state.Global != nil {
		printJSON("Global", state.Global)
	}
	if state.Tech != nil {
		printJSON("Technical", state.Tech)
	}

	fmt.Println()
	fmt.Println("=== 最终交易决策 ===")
	if state.Final != nil {
		fmt.Printf("综合评分: %.2f\n", state.Final.OverallScore)
		fmt.Printf("建议: %s\n", state.Final.Recommendation)
		fmt.Printf("入场: %.3f  止损: %.3f  止盈: %.3f\n", state.Final.EntryPrice, state.Final.StopLoss, state.Final.TakeProfit)
		fmt.Println("理由:", state.Final.Reasoning)
	}

	if !*skipReport {
		w := report.NewWriter(*reportDir)
		path, err := w.Save(state)
		if err != nil {
			fmt.Println("write report error:", err)
		} else {
			fmt.Println()
			fmt.Println("📄 Markdown 报告已生成:", path)
		}
	}
}

func printJSON(label string, v interface{}) {
	b, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println("[" + label + "]")
	fmt.Println(string(b))
}

// runBacktest 历史胜率回测：
//   - 不调用 LLM，仅使用 Screener + Regime + MoneyFlow + 规则版决策
//   - 在 [bt-start, bt-end] 区间按 bt-step 采样，持有 bt-hold 个交易日看收益
//   - 输出胜率、平均收益、Sharpe，并按 Recommendation/Regime/Sector 分桶
//   - variant=v3      : 纯 V3 评分
//   - variant=v3v2    : V3 评分 + V2 4 道闸门
//   - variant=both    : 同区间跑两遍，输出 A/B 对比报告
func runBacktest(startStr, endStr, dateStr string, step, hold, maxSamples int, variant, reportDir string) {
	parse := func(s string, def time.Time) time.Time {
		if s == "" {
			return def
		}
		t, err := time.ParseInLocation("2006-01-02", s, time.Local)
		if err != nil {
			fmt.Println("invalid date, expect YYYY-MM-DD:", err)
			os.Exit(2)
		}
		return t
	}
	end := parse(endStr, parse(dateStr, time.Now()))
	start := parse(startStr, end.AddDate(0, -2, 0)) // 默认近 2 个月

	fmt.Println("=== A 股 ETF 多 Agent 历史回测 ===")
	fmt.Printf("区间: %s ~ %s · 持有期: %d 日 · 采样步长: %d · 最大样本: %d · 变体: %s\n",
		start.Format("2006-01-02"), end.Format("2006-01-02"), hold, step, maxSamples, variant)

	ds := datasource.ETFDataSource(datasource.NewEastMoneyDataSource())

	if reportDir == "" {
		reportDir = "report"
	}
	_ = os.MkdirAll(reportDir, 0o755)

	runOne := func(v string) *backtest.Result {
		eng := backtest.NewEngine(ds)
		eng.HoldDays = hold
		eng.MaxSamples = maxSamples
		eng.Variant = v

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		res, err := eng.Run(ctx, start, end, step)
		if err != nil {
			fmt.Printf("[%s] backtest error: %v\n", v, err)
			os.Exit(1)
		}
		executed := res.Wins + res.Losses
		fmt.Printf("[%s] 样本=%d 实际建仓=%d 胜率=%.2f%% 平均加权收益=%+.2f%% Sharpe=%.2f\n",
			v, res.Total, executed, res.WinRate*100, res.AvgReturn*100, res.Sharpe)
		return res
	}

	switch variant {
	case "v3", "v3v2", "joinquant":
		res := runOne(variant)
		filename := fmt.Sprintf("backtest-%s-%s.md", variant, time.Now().Format("20060102-150405"))
		path := filepath.Join(reportDir, filename)
		if err := os.WriteFile(path, []byte(backtest.BuildMarkdown(res)), 0o644); err != nil {
			fmt.Println("write backtest report error:", err)
			return
		}
		abs, _ := filepath.Abs(path)
		fmt.Println("📄 回测报告已生成:", abs)
	case "both":
		resV3 := runOne("v3")
		resV3V2 := runOne("v3v2")
		filename := fmt.Sprintf("backtest-compare-%s.md", time.Now().Format("20060102-150405"))
		path := filepath.Join(reportDir, filename)
		md := backtest.BuildCompareMarkdown(resV3, resV3V2)
		if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
			fmt.Println("write compare report error:", err)
			return
		}
		abs, _ := filepath.Abs(path)
		fmt.Println("📄 V3 vs V3+V2 对比回测报告已生成:", abs)
	default:
		fmt.Printf("invalid --bt-variant: %s (expect v3/v3v2/both/joinquant)\n", variant)
		os.Exit(2)
	}
	_ = agent.NewScreenerAgent
}
