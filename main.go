package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/multi-agents-etf-trade-strategy/agent"
	"github.com/multi-agents-etf-trade-strategy/backtest"
	"github.com/multi-agents-etf-trade-strategy/config"
	"github.com/multi-agents-etf-trade-strategy/datasource"
	"github.com/multi-agents-etf-trade-strategy/orchestrator"
	"github.com/multi-agents-etf-trade-strategy/report"
	"github.com/multi-agents-etf-trade-strategy/types"
)

func main() {
	var (
		dateFlag    = flag.String("date", "", "回测/复盘的基准日期 (YYYY-MM-DD)，为空时取当天最新行情")
		timeFlag    = flag.String("time", "09:30", "模拟运行时刻 (HH:MM, 24h, 仅 advice 模式生效)，作为指数/数据源 AsOf 锚点")
		reportDir   = flag.String("report-dir", "report", "Markdown 报告输出目录")
		skipReport  = flag.Bool("skip-report", false, "仅打印结果，不落地报告")
		currentHold = flag.String("current-hold", "", "可选：当前持仓 ETF 代码（支持多个，逗号分隔，如 159915,512660），用于在报告中给出持仓对照与 FinalAgent 持仓评审；留空则跳过该章节，系统不做任何本地持久化")
		holdFile    = flag.String("hold-file", "holding.json", "持仓文件路径（由 claw/微信桥维护）。intraday 模式下优先于 --current-hold 读取；格式 {\"holds\":[\"159915\",...]} 或裸数组 [\"...\"]；文件不存在则回退到 --current-hold")
		mode        = flag.String("mode", "advice", "运行模式：advice（默认，单次出报告） / backtest（历史胜率回测） / intraday（盘中实时盯盘）")
		btStart     = flag.String("bt-start", "", "backtest 起始日 (YYYY-MM-DD)")
		btEnd       = flag.String("bt-end", "", "backtest 结束日 (YYYY-MM-DD)，默认 --date 或今天")
		btStep      = flag.Int("bt-step", 5, "backtest 采样间隔（交易日，状态化模式下忽略）")
		btHold      = flag.Int("bt-hold", 5, "backtest 持有期（已废弃，实际由信号驱动持仓）")
		btMax       = flag.Int("bt-max", 60, "backtest 最大样本数")
		btVariant   = flag.String("bt-variant", "both", "回测变体：v3 / v3v2 / v3p1 / v3opt / both / both_p1 / both_opt / both_lo / joinquant")
		btNextOpen  = flag.Bool("bt-next-open", false, "回测入场改用次日开盘价（消除前视偏差）；默认关=维持信号日收盘入场。单变体模式生效；both_lo 变体忽略此 flag 自带 A/B")
	)
	flag.Parse()

	if *mode == "backtest" {
		runBacktest(*btStart, *btEnd, *dateFlag, *btStep, *btHold, *btMax, *btVariant, *reportDir, *btNextOpen)
		return
	}

	if *mode == "intraday" {
		runIntraday(*currentHold, *holdFile, *skipReport, *reportDir)
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
	holds := parseCurrentHolds(*currentHold)
	pipe.CurrentHolds = holds

	state, err := pipe.Run(ctx)
	if err != nil {
		fmt.Println("pipeline error:", err)
		os.Exit(1)
	}

	// 持仓对照（无状态：仅本次会话使用 --current-hold 传入的值）
	state.CurrentHolds = holds
	if len(holds) > 0 {
		state.CurrentHold = holds[0]
	}
	if state.Screener != nil && len(holds) > 0 {
		state.HoldAdvices = agent.BuildHoldAdvices(holds, state.Screener.Top5)
		if len(state.HoldAdvices) > 0 {
			first := state.HoldAdvices[0]
			state.HoldAdvice = &first
		}
	}

	fmt.Println()
	fmt.Println("--- Top5 候选 ---")
	for i, e := range state.Screener.Top5 {
		tag := ""
		if e.IsCurrentHold {
			tag = " 🟦持仓"
		}
		fmt.Printf("%d) %s(%s)%s sector=%s score=%.2f action=%s reason=%s\n",
			i+1, e.ETF.Name, e.ETF.Code, tag, e.ETF.Sector, e.Score, e.Action, e.Reason)
	}

	fmt.Println()
	fmt.Println("--- 最佳目标 ---")
	best := state.Screener.Best
	fmt.Printf("%s(%s) 板块=%s 价格=%.3f 综合分=%.2f 动作=%s\n",
		best.ETF.Name, best.ETF.Code, best.ETF.Sector, best.ETF.Price, best.Score, best.ActionDesc)

	if len(state.HoldAdvices) > 0 {
		fmt.Println()
		fmt.Println("--- 持仓对照 ---")
		for _, a := range state.HoldAdvices {
			fmt.Printf("· %s : %s\n", a.CurrentHold, a.Suggestion)
		}
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
		if len(state.Final.HoldReviews) > 0 {
			fmt.Println()
			fmt.Println("--- 持仓评审 (HoldReviews) ---")
			for _, r := range state.Final.HoldReviews {
				inTop := "外"
				if r.InTop {
					inTop = fmt.Sprintf("Top%d", r.Rank)
				}
				fmt.Printf("· %s(%s) [%s] 分数=%.2f 建议=%s news=%s tech=%s\n  %s\n",
					r.ETFName, r.ETFCode, inTop, r.Score, r.ActionDesc, r.NewsBias, r.TechTrend, r.Rationale)
			}
		}
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

// parseCurrentHolds 解析 --current-hold 逗号分隔的多持仓列表，去空白 + 去重保序。
// 留空（""）时返回 nil，整段 advice 行为与"未提供持仓"一致。
func parseCurrentHolds(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	seen := make(map[string]struct{}, len(parts))
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// runIntraday 盘中实时盯盘：
//   - 优先复用今日已落地的报告（8:50 早报 + 9:24 集合竞价 + 上次盯盘），不重跑 LLM pipeline
//   - 找不到今日早报时，退化到跑一遍 advice pipeline 兜底（仅此时才落早报）
//   - 持仓来源优先 holding.json（claw/微信桥维护），回退 --current-hold
//   - 最后用 IntradayWatchAgent 结合实时行情给出买卖指导
func runIntraday(currentHoldRaw, holdFile string, skipReport bool, reportDir string) {
	holds, holdSrc := loadHolds(holdFile, currentHoldRaw)

	fmt.Println("=== 盘中实时盯盘 ===")
	fmt.Println("时间:", time.Now().Format("2006-01-02 15:04:05"))
	if len(holds) > 0 {
		fmt.Printf("持仓(%s): %s\n", holdSrc, strings.Join(holds, ", "))
	} else {
		fmt.Printf("持仓(%s): 空仓（仅盯盘今日 Top5）\n", holdSrc)
	}

	// 第一步：优先复用今日 8:50 早报，避免重跑 LLM pipeline（30~60s → 读文件 <0.1s）
	fmt.Println("\n[1/2] 加载今日报告 Top5 + 决策...")
	state, srcPath, err := report.LoadLatestAgentState(reportDir)
	if err != nil {
		fmt.Println("  读今日报告出错:", err)
	}
	freshFromPipeline := false
	if state == nil || state.Final == nil || state.Screener == nil || len(state.Screener.Top5) == 0 {
		fmt.Println("  未找到可用的今日报告，退化到跑 advice pipeline（耗时较长）...")
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		state, err = runIntradayPipeline(ctx, holds)
		cancel()
		if err != nil {
			fmt.Println("pipeline error:", err)
			os.Exit(1)
		}
		freshFromPipeline = true
		srcPath = ""
	} else {
		fmt.Printf("  复用早报: %s\n", srcPath)
	}
	fmt.Printf("  Top1: %s(%s)  分数: %.2f\n", state.Screener.Best.ETF.Name, state.Screener.Best.ETF.Code, state.Screener.Best.Score)

	// 持仓注入（无论哪条路径）
	state.CurrentHolds = holds
	if len(holds) > 0 {
		state.CurrentHold = holds[0]
	}

	// 仅在 pipeline 兜底生成新报告时落地早报；读路径不覆盖已有报告
	if freshFromPipeline && !skipReport {
		w := report.NewWriter(reportDir)
		if path, err := w.Save(state); err == nil {
			fmt.Println("  早报已生成:", path)
		}
	}

	// 跨报告上下文：集合竞价复核 + 上次盯盘（缺失不阻塞）
	preOpen, prePath, _ := report.LoadLatestPreOpen(reportDir)
	if preOpen != nil {
		fmt.Printf("  集合竞价复核: %s (bias=%s)\n", prePath, preOpen.MarketBias)
	}
	prevIntradays, _ := report.LoadRecentIntraday(reportDir, 5)
	if len(prevIntradays) > 0 {
		fmt.Printf("  历次盯盘: %d 份（最近趋势用）\n", len(prevIntradays))
	}

	// 第二步：盘中实时盯盘
	fmt.Println("\n[2/2] 获取实时行情并分析...")

	ds := datasource.NewEastMoneyDataSource()
	conf := types.IntradayWatchConfig{
		CurrentHolds:  holds,
		FinalDecision: state.Final,
		Top5:          state.Screener.Top5,
		PreOpen:       preOpen,
		PrevIntradays: prevIntradays,
	}

	watchCtx, watchCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer watchCancel()

	watcher := agent.NewIntradayWatchAgent(ds, conf)
	result, err := watcher.Run(watchCtx)
	if err != nil {
		fmt.Println("盘中盯盘错误:", err)
		os.Exit(1)
	}

	// 输出到终端
	fmt.Print(agent.FormatIntradayCLI(result))

	// 落地 JSON 快照
	if !skipReport {
		snapDir := filepath.Join(reportDir, "intraday")
		os.MkdirAll(snapDir, 0o755)
		snapFile := filepath.Join(snapDir, fmt.Sprintf("intraday-%s.json", time.Now().Format("20060102-150405")))
		data, _ := json.MarshalIndent(result, "", "  ")
		if err := os.WriteFile(snapFile, data, 0o644); err == nil {
			abs, _ := filepath.Abs(snapFile)
			fmt.Println("\n📄 盘中快照已保存:", abs)
		}
	}
}

// runIntradayPipeline 跑一遍 advice pipeline 作为盘中兜底（今日早报缺失时）。
func runIntradayPipeline(ctx context.Context, holds []string) (*types.AgentState, error) {
	cfg := config.Load()
	pipe, err := orchestrator.NewPipeline(cfg)
	if err != nil {
		return nil, fmt.Errorf("init pipeline: %w", err)
	}
	pipe.CurrentHolds = holds
	return pipe.Run(ctx)
}

// loadHolds 解析持仓来源：优先 holding.json（claw/微信桥维护），文件不存在/解析失败时回退 --current-hold。
// 返回 (holds, 来源标签)。文件存在且解析成功即信任其内容（含空仓）。
func loadHolds(holdFile, currentHoldRaw string) ([]string, string) {
	if holds, ok := tryLoadHoldsFile(holdFile); ok {
		return holds, holdFile
	}
	return parseCurrentHolds(currentHoldRaw), "--current-hold"
}

// tryLoadHoldsFile 容忍两种格式：{"holds":[...]} / {"codes":[...]} 或裸数组 [...]。
func tryLoadHoldsFile(path string) ([]string, bool) {
	if strings.TrimSpace(path) == "" {
		return nil, false
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, false // 文件不存在 → 用 flag
	}
	var obj struct {
		Holds []string `json:"holds"`
		Codes []string `json:"codes"`
	}
	if json.Unmarshal(buf, &obj) == nil {
		return normalizeHoldsList(append(obj.Holds, obj.Codes...)), true
	}
	var arr []string
	if json.Unmarshal(buf, &arr) == nil {
		return normalizeHoldsList(arr), true
	}
	return nil, false // 解析失败 → 用 flag
}

// normalizeHoldsList 去空白 + 去重保序；空切片归一为 nil。
func normalizeHoldsList(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// runBacktest 历史胜率回测：
//   - 不调用 LLM，仅使用 Screener + Regime + MoneyFlow + 规则版决策
//   - 在 [bt-start, bt-end] 区间按 bt-step 采样，持有 bt-hold 个交易日看收益
//   - 输出胜率、平均收益、Sharpe，并按 Recommendation/Regime/Sector 分桶
//   - variant=v3      : 纯 V3 评分
//   - variant=v3v2    : V3 评分 + V2 4 道闸门
//   - variant=both    : 同区间跑两遍，输出 A/B 对比报告
func runBacktest(startStr, endStr, dateStr string, step, hold, maxSamples int, variant, reportDir string, nextOpenDefault bool) {
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

	// nextOpen：入场时机（消除前视偏差）。both_lo 变体会自行翻转做 A/B，单变体沿用 flag。
	nextOpen := nextOpenDefault

	fmt.Println("=== A 股 ETF 多 Agent 历史回测 ===")
	fmt.Printf("区间: %s ~ %s · 持有期: %d 日 · 采样步长: %d · 最大样本: %d · 变体: %s\n",
		start.Format("2006-01-02"), end.Format("2006-01-02"), hold, step, maxSamples, variant)

	ds := datasource.ETFDataSource(datasource.NewEastMoneyDataSource())

	// 对比模式 (both/both_p1/both_opt/both_lo) 下，两个变体必须跑在完全相同的数据基线上。
	// 用 CachedDataSource 包装底层源：第一个变体拉数据并缓存，第二个变体直接复用。
	// 单变体模式 (v3/v3v2/v3p1/v3opt/joinquant) 不包装（无对比需求）。
	var sharedDS *datasource.CachedDataSource
	if variant == "both" || variant == "both_p1" || variant == "both_opt" || variant == "both_lo" {
		sharedDS = datasource.NewCachedDataSource(ds)
		ds = sharedDS
	}

	if reportDir == "" {
		reportDir = "report"
	}
	_ = os.MkdirAll(reportDir, 0o755)

	runOne := func(v string) *backtest.Result {
		eng := backtest.NewEngine(ds)
		eng.HoldDays = hold
		eng.MaxSamples = maxSamples
		eng.Variant = v
		eng.EntryNextOpen = nextOpen

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		res, err := eng.Run(ctx, start, end, step)
		if err != nil {
			fmt.Printf("[%s] backtest error: %v\n", v, err)
			os.Exit(1)
		}
		entryMode := "收盘"
		if nextOpen {
			entryMode = "次日开盘"
		}
		executed := res.Wins + res.Losses
		fmt.Printf("[%s·入场%s] 样本=%d 实际建仓=%d 胜率=%.2f%% 平均加权收益=%+.2f%% Sharpe=%.2f\n",
			v, entryMode, res.Total, executed, res.WinRate*100, res.AvgReturn*100, res.Sharpe)
		return res
	}

	switch variant {
	case "v3", "v3v2", "joinquant", "v3p1", "v3opt":
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
	case "both_p1":
		resV3 := runOne("v3")
		resV3P1 := runOne("v3p1")
		filename := fmt.Sprintf("backtest-compare-p1-%s.md", time.Now().Format("20060102-150405"))
		path := filepath.Join(reportDir, filename)
		md := backtest.BuildP1CompareMarkdown(resV3, resV3P1)
		if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
			fmt.Println("write p1 compare report error:", err)
			return
		}
		abs, _ := filepath.Abs(path)
		fmt.Println("📄 P0 vs P1 对比回测报告已生成:", abs)
	case "both_opt":
		// v3opt 先跑（需要 41 根 K 线），缓存后 v3（22 根）直接从缓存截取，保证基线一致
		resV3Opt := runOne("v3opt")
		resV3 := runOne("v3")
		filename := fmt.Sprintf("backtest-compare-opt-%s.md", time.Now().Format("20060102-150405"))
		path := filepath.Join(reportDir, filename)
		md := backtest.BuildOptCompareMarkdown(resV3, resV3Opt)
		if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
			fmt.Println("write opt compare report error:", err)
			return
		}
		abs, _ := filepath.Abs(path)
		fmt.Println("📄 v3 vs v3opt（P0+P1 优化）对比回测报告已生成:", abs)
	case "both_lo":
		// 前视偏差 A/B：同一 v3，信号日收盘入场（A）vs 次日开盘入场（B）。sharedDS 保证两遍数据基线一致。
		nextOpen = false
		resClose := runOne("v3")
		nextOpen = true
		resNextOpen := runOne("v3")
		filename := fmt.Sprintf("backtest-compare-lo-%s.md", time.Now().Format("20060102-150405"))
		path := filepath.Join(reportDir, filename)
		md := backtest.BuildLookaheadCompareMarkdown(resClose, resNextOpen)
		if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
			fmt.Println("write lookahead compare report error:", err)
			return
		}
		abs, _ := filepath.Abs(path)
		fmt.Println("📄 入场时机 A/B（前视偏差）对比回测报告已生成:", abs)
	default:
		fmt.Printf("invalid --bt-variant: %s (expect v3/v3v2/v3p1/v3opt/both/both_p1/both_opt/both_lo/joinquant)\n", variant)
		os.Exit(2)
	}
	_ = agent.NewScreenerAgent
}
