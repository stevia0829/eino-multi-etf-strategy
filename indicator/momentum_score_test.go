package indicator

import (
	"math"
	"testing"
	"time"

	"github.com/multi-agents-etf-trade-strategy/types"
)

func mkKlines(closes []float64) []types.KLine {
	out := make([]types.KLine, len(closes))
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, c := range closes {
		out[i] = types.KLine{
			Date:   base.AddDate(0, 0, i),
			Open:   c, Close: c, High: c, Low: c, Volume: 1000,
		}
	}
	return out
}

func TestMomentumScore_Uptrend(t *testing.T) {
	closes := make([]float64, 21)
	for i := range closes {
		// 每日 +1% 复合上涨
		closes[i] = math.Pow(1.01, float64(i))
	}
	score, ann, r2 := MomentumScore(mkKlines(closes), 21)
	if score <= 0 {
		t.Fatalf("expect positive score for uptrend, got %v", score)
	}
	if ann <= 0 {
		t.Fatalf("expect positive annualized, got %v", ann)
	}
	if r2 < 0.99 {
		t.Fatalf("strict log-linear series should have R²≈1, got %v", r2)
	}
}

func TestMomentumScore_Downtrend(t *testing.T) {
	closes := make([]float64, 21)
	for i := range closes {
		closes[i] = math.Pow(0.99, float64(i))
	}
	score, ann, _ := MomentumScore(mkKlines(closes), 21)
	if score >= 0 {
		t.Fatalf("expect negative score for downtrend, got %v", score)
	}
	if ann >= 0 {
		t.Fatalf("expect negative annualized, got %v", ann)
	}
}

func TestMomentumScore_NotEnoughData(t *testing.T) {
	score, ann, r2 := MomentumScore(mkKlines([]float64{1, 2, 3}), 21)
	if score != 0 || ann != 0 || r2 != 0 {
		t.Fatalf("expect zeros when not enough data, got %v %v %v", score, ann, r2)
	}
}

func TestMomentumScore_NonPositiveClose(t *testing.T) {
	closes := []float64{1, 2, 0, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21}
	score, _, _ := MomentumScore(mkKlines(closes), 21)
	if score != 0 {
		t.Fatalf("expect 0 when any close <= 0, got %v", score)
	}
}

func TestMomentumScore_BadParam(t *testing.T) {
	score, _, _ := MomentumScore(mkKlines([]float64{1, 2, 3}), 1)
	if score != 0 {
		t.Fatalf("mDays<=1 should return 0, got %v", score)
	}
}
