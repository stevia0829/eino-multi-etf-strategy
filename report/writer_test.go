package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multi-agents-etf-trade-strategy/types"
)

func mkState() *types.AgentState {
	asOf := time.Date(2024, 6, 28, 0, 0, 0, 0, time.UTC)
	scoredETF := types.ScoredETF{
		ETF: types.ETF{
			Code: "513130", Name: "恒生科技ETF", Sector: "港股",
			Price: 0.456,
		},
		Score:  78.5,
		Reason: "策略3 score=0.812 (年化 35.20% × R²0.83) · 趋势线性强（R²≥0.7）",
		Indicators: map[string]float64{
			"MA5": 0.45, "MA20": 0.42, "MA60": 0.40,
			"RSI": 62, "Strategy3Score": 0.812, "AnnualizedReturn": 0.352, "WeightedR2": 0.83,
		},
	}
	return &types.AgentState{
		Screener: &types.ScreenerResult{
			Top5:     []types.ScoredETF{scoredETF},
			Best:     scoredETF,
			AsOfDate: asOf,
		},
		News: &types.NewsAnalysis{
			Sector: "港股科技", Sentiment: "positive", Score: 70,
			Highlight: []string{"AI 政策利好", "南向资金流入"},
			Summary:   "整体偏多",
		},
		Global: &types.GlobalMarketAnalysis{
			USPrev:    types.MarketSnapshot{Index: "NASDAQ", Change: 80, ChangePc: 0.5, Note: "科技股领涨"},
			JPToday:   types.MarketSnapshot{Index: "N225", Change: 200, ChangePc: 0.6, Note: "AI概念活跃"},
			KRToday:   types.MarketSnapshot{Index: "KOSPI", Change: 10, ChangePc: 0.3, Note: "外资买入"},
			Sentiment: "positive", Score: 65, Summary: "海外整体支持",
		},
		Tech: &types.TechnicalAnalysis{
			ETFCode: "513130", Trend: "up", Score: 72,
			Signals:    map[string]string{"MA": "多头排列"},
			Indicators: scoredETF.Indicators,
			Summary:    "趋势向上",
			Support1:   0.42, Support2: 0.40, Resistance: 0.48,
			HoldRange: "0.418 - 0.485",
		},
		Final: &types.FinalDecision{
			TargetETF: scoredETF, OverallScore: 73.2, Recommendation: "buy",
			EntryPrice: 0.455, StopLoss: 0.418, TakeProfit: 0.485,
			Reasoning: "整体逻辑：趋势 + 消息共振。",
			ScoreBreakdown: map[string]float64{
				"quant": 78.5, "quant_weight": 0.35, "quant_part": 27.475,
				"news": 70, "news_weight": 0.20, "news_part": 14.0,
				"global": 65, "global_weight": 0.15, "global_part": 9.75,
				"tech": 72, "tech_weight": 0.30, "tech_part": 21.6,
			},
		},
	}
}

func TestBuildMarkdown_HasRequiredSections(t *testing.T) {
	md := BuildMarkdown(mkState(), time.Date(2024, 6, 28, 9, 0, 0, 0, time.UTC))
	want := []string{
		"目标 ETF",
		"恒生科技ETF",
		"513130",
		"建议持有区间",
		"0.418 - 0.485",
		"阻力位",
		"消息面研判",
		"加权综合评分",
		"量化动量",
		"消息面",
		"技术面",
	}
	for _, w := range want {
		if !strings.Contains(md, w) {
			t.Errorf("markdown should contain %q\nGot: %s", w, md)
		}
	}
}

func TestWriter_Save(t *testing.T) {
	dir := filepath.Join(os.TempDir(), "etf-report-test")
	defer os.RemoveAll(dir)
	w := NewWriter(dir)
	path, err := w.Save(mkState())
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() == 0 {
		t.Fatalf("report file empty")
	}
	if !strings.HasSuffix(path, ".md") {
		t.Fatalf("expect .md, got %s", path)
	}
}
