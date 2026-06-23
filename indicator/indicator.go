package indicator

import (
	"math"

	"github.com/multi-agents-etf-trade-strategy/types"
)

func MA(klines []types.KLine, n int) float64 {
	if len(klines) < n || n <= 0 {
		return 0
	}
	sum := 0.0
	for i := len(klines) - n; i < len(klines); i++ {
		sum += klines[i].Close
	}
	return sum / float64(n)
}

func RSI(klines []types.KLine, period int) float64 {
	if len(klines) <= period {
		return 50
	}
	gain, loss := 0.0, 0.0
	for i := len(klines) - period; i < len(klines); i++ {
		diff := klines[i].Close - klines[i-1].Close
		if diff >= 0 {
			gain += diff
		} else {
			loss -= diff
		}
	}
	if loss == 0 {
		return 100
	}
	rs := (gain / float64(period)) / (loss / float64(period))
	return 100 - 100/(1+rs)
}

func MACD(klines []types.KLine) (dif, dea, hist float64) {
	if len(klines) < 35 {
		return 0, 0, 0
	}
	ema12 := EMA(klines, 12)
	ema26 := EMA(klines, 26)
	dif = ema12 - ema26

	difs := make([]types.KLine, 0, 9)
	for i := len(klines) - 9; i < len(klines); i++ {
		sub := klines[:i+1]
		d := EMA(sub, 12) - EMA(sub, 26)
		difs = append(difs, types.KLine{Close: d})
	}
	dea = EMA(difs, 9)
	hist = (dif - dea) * 2
	return
}

func EMA(klines []types.KLine, n int) float64 {
	if len(klines) == 0 {
		return 0
	}
	k := 2.0 / (float64(n) + 1)
	ema := klines[0].Close
	for i := 1; i < len(klines); i++ {
		ema = klines[i].Close*k + ema*(1-k)
	}
	return ema
}

func Volatility(klines []types.KLine, n int) float64 {
	if len(klines) < n {
		return 0
	}
	returns := make([]float64, 0, n)
	for i := len(klines) - n + 1; i < len(klines); i++ {
		r := (klines[i].Close - klines[i-1].Close) / klines[i-1].Close
		returns = append(returns, r)
	}
	mean := 0.0
	for _, r := range returns {
		mean += r
	}
	mean /= float64(len(returns))
	variance := 0.0
	for _, r := range returns {
		variance += (r - mean) * (r - mean)
	}
	variance /= float64(len(returns))
	return math.Sqrt(variance)
}

func Momentum(klines []types.KLine, n int) float64 {
	if n <= 0 || len(klines) < n+1 {
		return 0
	}
	base := klines[len(klines)-1-n].Close
	if base == 0 {
		return 0
	}
	return (klines[len(klines)-1].Close - base) / base
}

func VolumeRatio(klines []types.KLine, n int) float64 {
	if len(klines) < n+1 {
		return 1
	}
	avgVol := 0.0
	for i := len(klines) - n - 1; i < len(klines)-1; i++ {
		avgVol += klines[i].Volume
	}
	avgVol /= float64(n)
	if avgVol == 0 {
		return 1
	}
	return klines[len(klines)-1].Volume / avgVol
}
