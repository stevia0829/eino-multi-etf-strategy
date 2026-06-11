package agent

import (
	"testing"

	"github.com/eino-multi-etf-strategy/types"
)

func TestBuildNewsKeywords_CoalUsesPreciseKeywords(t *testing.T) {
	kw := buildNewsKeywords(types.ScoredETF{ETF: types.ETF{
		Code:   "515220",
		Name:   "煤炭ETF",
		Sector: "能源",
	}})
	has := make(map[string]bool, len(kw))
	for _, k := range kw {
		has[k] = true
	}
	for _, want := range []string{"515220", "煤炭ETF", "煤炭", "动力煤", "焦煤", "迎峰度夏"} {
		if !has[want] {
			t.Fatalf("keywords missing %q: %v", want, kw)
		}
	}
	if has["能源"] {
		t.Fatalf("broad sector keyword should be suppressed for coal ETF: %v", kw)
	}
}

func TestCapByPullbackCooldown_DowngradesUnrepairedPullback(t *testing.T) {
	target := types.ScoredETF{
		ETF: types.ETF{
			Code:  "515220",
			Name:  "煤炭ETF",
			Price: 1.305,
			History: []types.KLine{
				{Open: 1.38, High: 1.423, Low: 1.36, Close: 1.389},
				{Open: 1.343, High: 1.376, Low: 1.332, Close: 1.360},
				{Open: 1.350, High: 1.353, Low: 1.298, Close: 1.305},
				{Open: 1.305, High: 1.320, Low: 1.300, Close: 1.310},
				{Open: 1.310, High: 1.315, Low: 1.300, Close: 1.305},
			},
		},
		Indicators: map[string]float64{"MA5": 1.3338},
	}
	got, note := CapByPullbackCooldown("buy", target)
	if got != "hold" || note == "" {
		t.Fatalf("expect buy downgraded to hold with note, got %q note=%q", got, note)
	}
}

func TestCapByPullbackCooldownForState_KeepsExistingHolding(t *testing.T) {
	target := types.ScoredETF{
		ETF: types.ETF{
			Code:  "515220",
			Name:  "煤炭ETF",
			Price: 1.305,
			History: []types.KLine{
				{Open: 1.38, High: 1.423, Low: 1.36, Close: 1.389},
				{Open: 1.343, High: 1.376, Low: 1.332, Close: 1.360},
				{Open: 1.350, High: 1.353, Low: 1.298, Close: 1.305},
				{Open: 1.305, High: 1.320, Low: 1.300, Close: 1.310},
				{Open: 1.310, High: 1.315, Low: 1.300, Close: 1.305},
			},
		},
		Indicators: map[string]float64{"MA5": 1.3338},
	}
	got, note := CapByPullbackCooldownForState("buy", target, &types.AgentState{CurrentHold: "515220"})
	if got != "buy" || note == "" {
		t.Fatalf("expect existing holding keep buy with warning, got %q note=%q", got, note)
	}
}

func TestCapByPullbackCooldownForState_KeepsWhenHoldingUnknown(t *testing.T) {
	target := types.ScoredETF{
		ETF: types.ETF{
			Code:  "515220",
			Name:  "煤炭ETF",
			Price: 1.305,
			History: []types.KLine{
				{Open: 1.38, High: 1.423, Low: 1.36, Close: 1.389},
				{Open: 1.343, High: 1.376, Low: 1.332, Close: 1.360},
				{Open: 1.350, High: 1.353, Low: 1.298, Close: 1.305},
				{Open: 1.305, High: 1.320, Low: 1.300, Close: 1.310},
				{Open: 1.310, High: 1.315, Low: 1.300, Close: 1.305},
			},
		},
		Indicators: map[string]float64{"MA5": 1.3338},
	}
	got, note := CapByPullbackCooldownForState("buy", target, &types.AgentState{})
	if got != "buy" || note == "" {
		t.Fatalf("expect unknown holding keep buy with warning, got %q note=%q", got, note)
	}
}

func TestCapByPullbackCooldown_KeepsAfterRepair(t *testing.T) {
	target := types.ScoredETF{
		ETF: types.ETF{
			Code:  "515220",
			Name:  "煤炭ETF",
			Price: 1.365,
			History: []types.KLine{
				{Open: 1.38, High: 1.423, Low: 1.36, Close: 1.389},
				{Open: 1.343, High: 1.376, Low: 1.332, Close: 1.360},
				{Open: 1.350, High: 1.353, Low: 1.298, Close: 1.305},
				{Open: 1.305, High: 1.320, Low: 1.300, Close: 1.310},
				{Open: 1.310, High: 1.365, Low: 1.300, Close: 1.365},
			},
		},
		Indicators: map[string]float64{"MA5": 1.3458},
	}
	got, note := CapByPullbackCooldown("buy", target)
	if got != "buy" || note != "" {
		t.Fatalf("expect repaired pullback keep buy, got %q note=%q", got, note)
	}
}

func TestCapByRiskReward_WarnsPoorTradePlan(t *testing.T) {
	got, note := CapByRiskReward("buy", 1.36, 1.27, 1.45)
	if got != "buy" || note == "" {
		t.Fatalf("expect poor risk/reward keep buy with warning, got %q note=%q", got, note)
	}
}

func TestCapByRiskReward_KeepsExecutableTradePlan(t *testing.T) {
	got, note := CapByRiskReward("buy", 1.36, 1.30, 1.46)
	if got != "buy" || note != "" {
		t.Fatalf("expect executable risk/reward keep buy, got %q note=%q", got, note)
	}
}

func TestApplyVerdictRule_CurrentHoldingLowOpenDoesNotChase(t *testing.T) {
	s := types.PreOpenSnapshot{
		ETFCode:      "515220",
		ETFName:      "煤炭ETF",
		PrevClose:    1.40,
		AuctionPrice: 1.35,
		IOPV:         1.348,
		PremiumPct:   0.0015,
		GapPct:       -0.0357,
		EntryPrice:   1.37,
		EntryGapPct:  -0.0146,
		AdjEntry:     1.37,
	}
	applyVerdictRule(&s, -0.002, map[string]struct{}{"515220": {}})
	if s.Verdict != "wait_pullback" || s.AdjEntry != 0 {
		t.Fatalf("expect held low-open target wait without add, got verdict=%s adj=%.4f note=%s", s.Verdict, s.AdjEntry, s.Note)
	}
}

func TestApplyVerdictRule_NewPositionWeakLowOpenDoesNotChase(t *testing.T) {
	s := types.PreOpenSnapshot{
		ETFCode:      "515220",
		ETFName:      "煤炭ETF",
		PrevClose:    1.40,
		AuctionPrice: 1.385,
		IOPV:         1.383,
		PremiumPct:   0.0014,
		GapPct:       -0.0107,
		EntryPrice:   1.41,
		EntryGapPct:  -0.0177,
		AdjEntry:     1.41,
	}
	applyVerdictRule(&s, -0.004, nil)
	if s.Verdict != "wait_pullback" || s.AdjEntry != 0 {
		t.Fatalf("expect weak low-open target wait, got verdict=%s adj=%.4f note=%s", s.Verdict, s.AdjEntry, s.Note)
	}
}

func TestApplyVerdictRule_RiskRewardBlocksPreOpenChase(t *testing.T) {
	s := types.PreOpenSnapshot{
		ETFCode:      "515220",
		ETFName:      "煤炭ETF",
		PrevClose:    1.40,
		AuctionPrice: 1.36,
		IOPV:         1.358,
		PremiumPct:   0.0015,
		GapPct:       -0.002,
		EntryPrice:   1.36,
		EntryGapPct:  0,
		AdjEntry:     1.36,
		AdjStopLoss:  1.27,
		AdjTakeProf:  1.45,
	}
	applyVerdictRule(&s, 0, nil)
	if s.Verdict != "wait_pullback" || s.AdjEntry != 0 {
		t.Fatalf("expect poor risk/reward preopen target wait, got verdict=%s adj=%.4f note=%s", s.Verdict, s.AdjEntry, s.Note)
	}
}
