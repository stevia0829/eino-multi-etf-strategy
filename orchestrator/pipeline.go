package orchestrator

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/eino-multi-etf-strategy/agent"
	"github.com/eino-multi-etf-strategy/config"
	"github.com/eino-multi-etf-strategy/datasource"
	"github.com/eino-multi-etf-strategy/llm"
	"github.com/eino-multi-etf-strategy/types"
)

type Pipeline struct {
	Screener  *agent.ScreenerAgent
	News      *agent.NewsAgent
	Global    *agent.GlobalMarketAgent
	Tech      *agent.TechnicalAgent
	Regime    *agent.RegimeAgent
	MoneyFlow *agent.MoneyFlowAgent
	Final     *agent.FinalAgent
	Logger    *log.Logger
	// CurrentHolds 是用户通过 --current-hold 传入的当前持仓代码列表（支持多个），仅用于本次 advice 决策。
	// 仅本次会话使用，系统不做任何本地持久化。
	CurrentHolds []string
}

// NewPipeline 通过统一配置构建多 agent pipeline。
// LLM 客户端使用 Resilient 包装：主模型 + 多个备选模型 + 静态兜底。
func NewPipeline(cfg *config.Config) (*Pipeline, error) {
	ds := datasource.ETFDataSource(datasource.NewEastMoneyDataSource())

	client, err := cfg.LLM.Build(staticFallback)
	if err != nil {
		return nil, fmt.Errorf("build llm client: %w", err)
	}

	return &Pipeline{
		Screener:  agent.NewScreenerAgent(ds),
		News:      agent.NewNewsAgent(client),
		Global:    agent.NewGlobalMarketAgent(client),
		Tech:      agent.NewTechnicalAgent(client),
		Regime:    agent.NewRegimeAgent(ds),
		MoneyFlow: agent.NewMoneyFlowAgent(ds),
		Final:     agent.NewFinalAgent(client),
		Logger:    log.Default(),
	}, nil
}

// staticFallback：当所有 LLM 都不可达时的最后保护层 —— 直接返回空 JSON，让上层走规则兜底。
func staticFallback(system, user string) string {
	return "{}"
}

// Run 模拟 eino compose.Graph 的多 agent 编排：
//
//	start → ScreenerAgent → [ NewsAgent ‖ GlobalMarketAgent ‖ TechnicalAgent ] → FinalAgent → end
func (p *Pipeline) Run(ctx context.Context) (*types.AgentState, error) {
	holds := normalizeHolds(p.CurrentHolds)
	state := &types.AgentState{
		CurrentHolds: holds,
		CurrentHold:  firstHold(holds),
	}

	p.Logger.Println("[pipeline] step1: screener running…")
	p.Screener.CurrentHolds = holds
	scr, err := p.Screener.Run(ctx)
	if err != nil {
		return nil, fmt.Errorf("screener: %w", err)
	}
	if scr == nil || len(scr.Top5) == 0 {
		return nil, fmt.Errorf("no qualified ETF found")
	}
	state.Screener = scr
	target := scr.Best
	p.Logger.Printf("[pipeline] step1 done. best=%s(%s) score=%.2f", target.ETF.Name, target.ETF.Code, target.Score)

	p.Logger.Println("[pipeline] step2: news / global / technical / regime / moneyflow fan-out…")
	var wg sync.WaitGroup
	wg.Add(5)

	go func() {
		defer wg.Done()
		n, e := p.News.Run(ctx, target)
		if e != nil {
			p.Logger.Printf("[news] error: %v", e)
		}
		state.News = n
	}()
	go func() {
		defer wg.Done()
		// 同步查询时间锚点：所有指数报价不得晚于该时刻（回测一致性）。
		// 当 --date 指定时：若 AsOf 已含具体时刻（hour 非 0），直接使用；否则锚定到当日 09:30。
		if !p.Screener.AsOf.IsZero() {
			anchor := p.Screener.AsOf
			if anchor.Hour() == 0 && anchor.Minute() == 0 {
				anchor = time.Date(anchor.Year(), anchor.Month(), anchor.Day(), 9, 30, 0, 0, anchor.Location())
			}
			p.Global.Fetcher.WithAsOf(anchor)
		}
		g, e := p.Global.Run(ctx, target)
		if e != nil {
			p.Logger.Printf("[global] error: %v", e)
		}
		state.Global = g
	}()
	go func() {
		defer wg.Done()
		t, e := p.Tech.Run(ctx, target)
		if e != nil {
			p.Logger.Printf("[tech] error: %v", e)
		}
		state.Tech = t
	}()
	go func() {
		defer wg.Done()
		// 同步 AsOf 给 RegimeAgent，保证回测一致性
		p.Regime.AsOf = p.Screener.AsOf
		r, e := p.Regime.Run(ctx)
		if e != nil {
			p.Logger.Printf("[regime] error: %v", e)
		}
		state.Regime = r
	}()
	go func() {
		defer wg.Done()
		p.MoneyFlow.AsOf = p.Screener.AsOf
		m, e := p.MoneyFlow.Run(ctx, target)
		if e != nil {
			p.Logger.Printf("[moneyflow] error: %v", e)
		}
		state.MoneyFlow = m
	}()
	wg.Wait()
	p.Logger.Println("[pipeline] step2 done.")

	p.Logger.Println("[pipeline] step3: final agent aggregating…")
	final, err := p.Final.Run(ctx, state)
	if err != nil {
		return state, fmt.Errorf("final: %w", err)
	}
	state.Final = final
	p.Logger.Printf("[pipeline] step3 done. recommendation=%s score=%.2f", final.Recommendation, final.OverallScore)
	return state, nil
}

var _ llm.Client = (*llm.Resilient)(nil) // 编译期接口断言

// normalizeHolds 去掉空白 / 重复，保持原顺序，便于多持仓 advice 一致地传入下游。
func normalizeHolds(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = trimHold(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func firstHold(in []string) string {
	if len(in) == 0 {
		return ""
	}
	return in[0]
}

// trimHold 包成单独函数，便于将来扩展（如代码格式标准化）。
func trimHold(s string) string {
	return strings.TrimSpace(s)
}
