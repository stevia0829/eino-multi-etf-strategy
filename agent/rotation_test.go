package agent

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/multi-agents-etf-trade-strategy/types"
)

// mockDS 简单的测试数据源：根据 (code, asOf, days) 构造确定性 K 线。
type mockDS struct{}

func (m *mockDS) ListAllETFs() ([]types.ETF, error) { return nil, nil }
func (m *mockDS) GetKLine(code string, days int) ([]types.KLine, error) {
	return m.GetKLineAsOf(code, days, time.Time{})
}
func (m *mockDS) GetKLineAsOf(code string, days int, asOf time.Time) ([]types.KLine, error) {
	if asOf.IsZero() {
		asOf = time.Now()
	}
	klines := make([]types.KLine, days)
	// 用 code 作为种子，让不同 ETF 有不同的趋势斜率
	seed := 0
	for _, ch := range code {
		seed += int(ch)
	}
	slope := float64((seed%7)-3) * 0.003 // [-0.009, +0.009]
	base := 1.0 + float64(seed%10)*0.1
	for i := 0; i < days; i++ {
		closep := base * math.Exp(slope*float64(i))
		klines[i] = types.KLine{
			Date: asOf.AddDate(0, 0, -(days - i - 1)),
			Open: closep, Close: closep, High: closep * 1.01, Low: closep * 0.99,
			Volume: 1e9,
		}
	}
	return klines, nil
}

func TestScreener_AsOfRespected(t *testing.T) {
	ds := &mockDS{}
	scr := NewScreenerAgent(ds)
	scr.AsOf = time.Date(2024, 6, 28, 0, 0, 0, 0, time.Local)

	res, err := scr.Run(context.Background())
	if err != nil {
		t.Fatalf("screener run: %v", err)
	}
	if res == nil || len(res.Top5) == 0 {
		t.Fatalf("expect non-empty Top5")
	}
	// AsOfDate 应该等于指定的 AsOf
	if !res.AsOfDate.Equal(scr.AsOf) {
		t.Fatalf("AsOfDate=%v, want %v", res.AsOfDate, scr.AsOf)
	}
	for _, e := range res.Top5 {
		if e.Score < 0 || e.Score > 100 {
			t.Fatalf("score out of [0,100]: %v", e.Score)
		}
		if _, ok := e.Indicators["Strategy3Score"]; !ok {
			t.Fatalf("missing Strategy3Score indicator")
		}
		if e.Reason == "" {
			t.Fatalf("expect non-empty reason")
		}
	}
}

func TestRotation_DefaultParams(t *testing.T) {
	p := DefaultRotationParams()
	if p.MDays != 21 {
		t.Fatalf("MDays should be 21, got %v", p.MDays)
	}
	if p.MaxScore != 6 || p.MinScore != 0 {
		t.Fatalf("default thresholds wrong: %+v", p)
	}
	if p.ScoreThresholdMultiplier != 1.1 {
		t.Fatalf("default multiplier should be 1.1, got %v", p.ScoreThresholdMultiplier)
	}
}

func TestRotation_RankReturnsSorted(t *testing.T) {
	ag := NewRotationAgent(&mockDS{})
	cands, err := ag.Rank(context.Background())
	if err != nil {
		t.Fatalf("rank: %v", err)
	}
	for i := 1; i < len(cands); i++ {
		if cands[i-1].Score < cands[i].Score {
			t.Fatalf("not sorted desc at %d: %v < %v", i, cands[i-1].Score, cands[i].Score)
		}
	}
}

func TestStrategy3Pool_NotEmpty(t *testing.T) {
	if len(Strategy3Pool) < 50 {
		t.Fatalf("etf pool size seems off: %d", len(Strategy3Pool))
	}
	seen := map[string]bool{}
	for _, e := range Strategy3Pool {
		if seen[e.Code] {
			t.Fatalf("duplicate code in pool: %s", e.Code)
		}
		seen[e.Code] = true
	}
}
