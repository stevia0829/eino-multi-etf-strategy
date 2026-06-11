package agent

import (
	"testing"

	"github.com/eino-multi-etf-strategy/types"
)

func makeStateWithHolds(holds []string, top5 []types.ScoredETF, news []types.NewsAnalysis, tech []types.TechnicalAnalysis) *types.AgentState {
	return &types.AgentState{
		Screener: &types.ScreenerResult{
			Top5: top5,
			Best: top5[0],
		},
		NewsList:     news,
		TechList:     tech,
		CurrentHolds: holds,
	}
}

func TestBuildHoldReviewsFallback_KeepWhenStrongSignal(t *testing.T) {
	top := []types.ScoredETF{
		{ETF: types.ETF{Code: "513100", Name: "纳指ETF", Sector: "海外"}, Score: 80, Action: string(ActionStrongBuy), ActionDesc: "强烈买入", IsCurrentHold: true},
		{ETF: types.ETF{Code: "518880", Name: "黄金ETF", Sector: "贵金属"}, Score: 72, Action: string(ActionBuy)},
	}
	news := []types.NewsAnalysis{{ETFCode: "513100", Sentiment: "positive", Score: 70}}
	tech := []types.TechnicalAnalysis{{ETFCode: "513100", Trend: "up", Score: 75}}
	st := makeStateWithHolds([]string{"513100"}, top, news, tech)
	out := buildHoldReviewsFallback(st)
	if len(out) != 1 {
		t.Fatalf("expect 1 review, got %d", len(out))
	}
	if out[0].Action != "keep" {
		t.Fatalf("expect keep, got %s", out[0].Action)
	}
	if !out[0].InTop || out[0].Rank != 1 {
		t.Fatalf("expect InTop+rank1, got InTop=%v rank=%d", out[0].InTop, out[0].Rank)
	}
}

func TestBuildHoldReviewsFallback_RotateWhenAvoidOrDownTrend(t *testing.T) {
	top := []types.ScoredETF{
		{ETF: types.ETF{Code: "513100", Name: "纳指ETF", Sector: "海外"}, Score: 78, Action: string(ActionStrongBuy)},
		{ETF: types.ETF{Code: "515220", Name: "煤炭ETF", Sector: "能源"}, Score: 60, Action: string(ActionAvoid), ActionDesc: "回避", IsCurrentHold: true},
	}
	news := []types.NewsAnalysis{{ETFCode: "515220", Sentiment: "negative", Score: 30}}
	tech := []types.TechnicalAnalysis{{ETFCode: "515220", Trend: "down", Score: 30}}
	st := makeStateWithHolds([]string{"515220"}, top, news, tech)
	out := buildHoldReviewsFallback(st)
	if len(out) != 1 || out[0].Action != "rotate" {
		t.Fatalf("expect rotate, got %+v", out)
	}
}

func TestBuildHoldReviewsFallback_MultipleHolds(t *testing.T) {
	top := []types.ScoredETF{
		{ETF: types.ETF{Code: "513100", Name: "纳指ETF", Sector: "海外"}, Score: 80, Action: string(ActionStrongBuy), IsCurrentHold: true},
		{ETF: types.ETF{Code: "518880", Name: "黄金ETF", Sector: "贵金属"}, Score: 70, Action: string(ActionBuy)},
		{ETF: types.ETF{Code: "515220", Name: "煤炭ETF", Sector: "能源"}, Score: 50, Action: string(ActionHoldOnly), ActionDesc: "观望", IsCurrentHold: true},
	}
	news := []types.NewsAnalysis{
		{ETFCode: "513100", Sentiment: "positive", Score: 70},
		{ETFCode: "515220", Sentiment: "neutral", Score: 50},
	}
	tech := []types.TechnicalAnalysis{
		{ETFCode: "513100", Trend: "up", Score: 70},
		{ETFCode: "515220", Trend: "flat", Score: 45},
	}
	st := makeStateWithHolds([]string{"513100", "515220"}, top, news, tech)
	out := buildHoldReviewsFallback(st)
	if len(out) != 2 {
		t.Fatalf("expect 2 reviews, got %d", len(out))
	}
	codes := map[string]string{}
	for _, r := range out {
		codes[r.ETFCode] = r.Action
	}
	if codes["513100"] != "keep" {
		t.Fatalf("513100 expect keep, got %s", codes["513100"])
	}
	if codes["515220"] != "trim" && codes["515220"] != "rotate" {
		t.Fatalf("515220 expect trim/rotate, got %s", codes["515220"])
	}
}

func TestBuildHoldReviewsFallback_IgnoresHoldOutsideCandidates(t *testing.T) {
	top := []types.ScoredETF{
		{ETF: types.ETF{Code: "513100", Name: "纳指ETF", Sector: "海外"}, Score: 80, Action: string(ActionStrongBuy)},
	}
	st := makeStateWithHolds([]string{"999999"}, top, nil, nil)
	out := buildHoldReviewsFallback(st)
	if len(out) != 0 {
		t.Fatalf("expect 0 reviews when hold not in candidates, got %d", len(out))
	}
}

func TestSanitizeHoldReviews_FillsMissingFieldsAndDropsAlien(t *testing.T) {
	top := []types.ScoredETF{
		{ETF: types.ETF{Code: "513100", Name: "纳指ETF", Sector: "海外"}, Score: 80, Action: string(ActionStrongBuy), ActionDesc: "强烈买入", IsCurrentHold: true},
	}
	news := []types.NewsAnalysis{{ETFCode: "513100", Sentiment: "positive", Score: 70}}
	tech := []types.TechnicalAnalysis{{ETFCode: "513100", Trend: "up", Score: 75}}
	st := makeStateWithHolds([]string{"513100"}, top, news, tech)
	llm := []types.HoldReview{
		{ETFCode: "513100", Action: ""},     // missing/invalid action
		{ETFCode: "999999", Action: "keep"}, // alien hold, must drop
		{ETFCode: "888888", Action: "keep"}, // not a hold, must drop
	}
	out := sanitizeHoldReviews(llm, st)
	if len(out) != 1 || out[0].ETFCode != "513100" {
		t.Fatalf("expect single 513100 review, got %+v", out)
	}
	if out[0].ETFName != "纳指ETF" || out[0].Sector != "海外" {
		t.Fatalf("expect name/sector filled, got %+v", out[0])
	}
	if out[0].Action != "keep" {
		t.Fatalf("expect rule-based keep, got %s", out[0].Action)
	}
	if out[0].Rank != 1 || !out[0].InTop {
		t.Fatalf("expect rank=1 InTop, got rank=%d InTop=%v", out[0].Rank, out[0].InTop)
	}
	if out[0].Rationale == "" {
		t.Fatalf("expect rationale auto-filled")
	}
}

func TestCollectHoldsList_DedupAndCompat(t *testing.T) {
	st := &types.AgentState{
		CurrentHolds: []string{"513100", "  ", "518880", "513100"},
		CurrentHold:  "159928",
	}
	got := collectHoldsList(st)
	want := []string{"513100", "518880", "159928"}
	if len(got) != len(want) {
		t.Fatalf("expect %v, got %v", want, got)
	}
	for i, v := range want {
		if got[i] != v {
			t.Fatalf("idx %d expect %s got %s", i, v, got[i])
		}
	}
}
