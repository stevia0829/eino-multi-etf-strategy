package orchestrator

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/multi-agents-etf-trade-strategy/agent"
	"github.com/multi-agents-etf-trade-strategy/config"
	"github.com/multi-agents-etf-trade-strategy/datasource"
	"github.com/multi-agents-etf-trade-strategy/llm"
	"github.com/multi-agents-etf-trade-strategy/types"
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

	p.Logger.Println("[pipeline] step2: fan-out — news/top5-news / global / tech/top5-tech / regime / moneyflow…")

	// fan-out 中的 fan-out：对 Top5 每支 ETF 都抓新闻和技术面，
	// 填充 NewsList / TechList 供 FinalAgent 做跨标比较和 HoldReviews。
	top5 := scr.Top5

	var newsMu, techMu sync.Mutex
	state.NewsList = make([]types.NewsAnalysis, 0, len(top5))
	state.TechList = make([]types.TechnicalAnalysis, 0, len(top5))

	var wg sync.WaitGroup
	// 5 个主路径 + 对 Top5 的 news/tech 子 goroutine
	// news 主路径 + top5 批量 news
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Top1（best）的新闻：同步跑，用于 state.News 和 NewsList[0]
		n, e := p.News.Run(ctx, target)
		if e != nil {
			p.Logger.Printf("[news] best=%s error: %v", target.ETF.Code, e)
		}
		state.News = n
		if n != nil {
			n.ETFCode = target.ETF.Code
			newsMu.Lock()
			state.NewsList = append(state.NewsList, *n)
			newsMu.Unlock()
		}

		// Top5 其余（index 1..）的新闻：扇出并发
		if len(top5) > 1 {
			var wgNews sync.WaitGroup
			for i := 1; i < len(top5); i++ {
				wgNews.Add(1)
				go func(etf types.ScoredETF) {
					defer wgNews.Done()
					ni, e2 := p.News.Run(ctx, etf)
					if e2 != nil {
						p.Logger.Printf("[news] %s(%s) error: %v", etf.ETF.Name, etf.ETF.Code, e2)
						return
					}
					if ni != nil {
						ni.ETFCode = etf.ETF.Code
						newsMu.Lock()
						state.NewsList = append(state.NewsList, *ni)
						newsMu.Unlock()
					}
				}(top5[i])
			}
			wgNews.Wait()
		}
	}()

	// global
	wg.Add(1)
	go func() {
		defer wg.Done()
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

	// tech 主路径 + top5 批量 tech
	wg.Add(1)
	go func() {
		defer wg.Done()
		t, e := p.Tech.Run(ctx, target)
		if e != nil {
			p.Logger.Printf("[tech] best=%s error: %v", target.ETF.Code, e)
		}
		state.Tech = t
		if t != nil {
			t.ETFCode = target.ETF.Code
			techMu.Lock()
			state.TechList = append(state.TechList, *t)
			techMu.Unlock()
		}

		if len(top5) > 1 {
			var wgTech sync.WaitGroup
			for i := 1; i < len(top5); i++ {
				wgTech.Add(1)
				go func(etf types.ScoredETF) {
					defer wgTech.Done()
					ti, e2 := p.Tech.Run(ctx, etf)
					if e2 != nil {
						p.Logger.Printf("[tech] %s(%s) error: %v", etf.ETF.Name, etf.ETF.Code, e2)
						return
					}
					if ti != nil {
						ti.ETFCode = etf.ETF.Code
						techMu.Lock()
						state.TechList = append(state.TechList, *ti)
						techMu.Unlock()
					}
				}(top5[i])
			}
			wgTech.Wait()
		}
	}()

	// regime
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.Regime.AsOf = p.Screener.AsOf
		r, e := p.Regime.Run(ctx)
		if e != nil {
			p.Logger.Printf("[regime] error: %v", e)
		}
		state.Regime = r
	}()

	// moneyflow
	wg.Add(1)
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
	p.Logger.Printf("[pipeline] step2 done. news=%d items  tech=%d items", len(state.NewsList), len(state.TechList))

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
