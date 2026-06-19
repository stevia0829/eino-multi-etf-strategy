package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/multi-agents-etf-trade-strategy/agent"
	"github.com/multi-agents-etf-trade-strategy/config"
	"github.com/multi-agents-etf-trade-strategy/datasource"
	"github.com/multi-agents-etf-trade-strategy/llm"
	"github.com/multi-agents-etf-trade-strategy/report"
	"github.com/multi-agents-etf-trade-strategy/types"
)

// cmd/preopen 9:24 集合竞价复核独立二进制。
//
// 流程：
//  1. 加载 8:50 主 agent 落地的 etf-report-*.json sidecar（默认自动取当日最新）
//  2. 拉取 9:20-9:25 集合竞价撮合数据（510300 大盘 + Final + Picks）
//  3. 走规则 + 可选 LLM 综合论证，输出 preopen-report-*.md
func main() {
	var (
		reportDir   = flag.String("report-dir", "report", "8:50 主报告所在目录（用于自动查找 etf-report-*.json）")
		baseReport  = flag.String("base-report", "", "显式指定 8:50 报告 JSON 路径；留空则自动取当日最新")
		outDir      = flag.String("out-dir", "report", "preopen 复核报告输出目录")
		currentHold = flag.String("current-hold", "", "可选：当前已持有 ETF 代码（支持多个，逗号分隔）；用于集合竞价复核时区分加仓与新开仓")
		skipLLM     = flag.Bool("skip-llm", false, "跳过 LLM，仅走规则路径")
	)
	flag.Parse()

	jsonPath, err := resolveBaseReport(*reportDir, *baseReport)
	if err != nil {
		fmt.Println("resolve base report:", err)
		os.Exit(2)
	}
	fmt.Println("基准报告:", jsonPath)

	state, err := loadState(jsonPath)
	if err != nil {
		fmt.Println("load state:", err)
		os.Exit(1)
	}
	if state.Final == nil {
		fmt.Println("base report missing final decision")
		os.Exit(1)
	}
	if *currentHold != "" {
		holds := parseCurrentHolds(*currentHold)
		state.CurrentHolds = holds
		if len(holds) > 0 {
			state.CurrentHold = holds[0]
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ds := datasource.NewEastMoneyDataSource()

	var llmClient llm.Client
	if !*skipLLM {
		cfg := config.Load()
		c, err := cfg.LLM.Build(nil)
		if err != nil {
			fmt.Println("build llm:", err, "→ 退化为规则模式")
		} else {
			llmClient = c
		}
	}

	a := agent.NewPreOpenAgent(llmClient, ds)
	res, err := a.Run(ctx, state)
	if err != nil {
		fmt.Println("preopen run:", err)
		os.Exit(1)
	}
	res.BaseReportPath = jsonPath

	out, err := report.SavePreOpen(*outDir, res)
	if err != nil {
		fmt.Println("save preopen report:", err)
		os.Exit(1)
	}
	fmt.Println("📄 集合竞价复核报告已生成:", out)

	fmt.Println()
	fmt.Println("=== 摘要 ===")
	fmt.Printf("大盘 510300: gap=%+.2f%% bias=%s\n", res.Market.GapPct*100, res.MarketBias)
	for _, s := range res.Snapshots {
		fmt.Printf("- %s(%s) gap=%+.2f%% premium=%+.2f%% verdict=%s adj_entry=%.4f note=%s\n",
			s.ETFName, s.ETFCode, s.GapPct*100, s.PremiumPct*100,
			s.Verdict, s.AdjEntry, s.Note)
	}
	if res.FinalAction != "" {
		fmt.Println("最终建议:", res.FinalAction)
	}
}

// resolveBaseReport 决定使用哪份 JSON sidecar。
func resolveBaseReport(dir, explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("base-report not found: %w", err)
		}
		return explicit, nil
	}
	matches, err := filepath.Glob(filepath.Join(dir, "etf-report-*.json"))
	if err != nil {
		return "", err
	}
	today := time.Now().Format("20060102")
	var todays []string
	for _, m := range matches {
		base := filepath.Base(m)
		if strings.HasPrefix(base, "etf-report-"+today) {
			todays = append(todays, m)
		}
	}
	pool := todays
	if len(pool) == 0 {
		pool = matches
	}
	if len(pool) == 0 {
		return "", fmt.Errorf("no etf-report-*.json found in %s; 请先运行主 agent", dir)
	}
	sort.Strings(pool)
	return pool[len(pool)-1], nil
}

func loadState(path string) (*types.AgentState, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var s types.AgentState
	if err := json.Unmarshal(buf, &s); err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", path, err)
	}
	return &s, nil
}

// parseCurrentHolds 与主程序保持一致：逗号分隔、去空、去重保序。
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
