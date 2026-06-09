package indicator

import (
	"math"

	"github.com/eino-multi-etf-strategy/types"
)

// MomentumScore 实现策略 3（ETF 轮动）的核心打分算法。
//
// 来源：strategy.py 中 g.etf_pool_3 + g.m_days(=21) 的动量评分逻辑。
// 思路：
//  1. 取最近 m_days 个收盘价 prices；
//  2. 对 log(prices) 做加权一次多项式回归 (权重 np.linspace(1, 2, n))，得到 slope；
//  3. 年化收益率 annualized = exp(slope * 250) - 1；
//  4. 加权 R²：1 - SSE_w / SST_w，其中：
//     SSE_w = Σ wᵢ (yᵢ - ŷᵢ)²
//     SST_w = Σ wᵢ (yᵢ - ȳ)²        ← ȳ 用「未加权」算术均值（对齐聚宽 np.mean(y)）
//  5. score = annualized * R²。
//
// 与聚宽 get_etf_rank / moment_rank 的口径已对齐：
//   - SST_w 中的 ȳ 使用 Σy/n（未加权均值），与 numpy 的 np.mean(y) 一致
//   - R² 不做 0~1 的 clamp，允许负值（震荡序列时 score 会被进一步压低，排名更靠后）
//
// 返回 (score, annualizedReturn, r2)；样本不足时三者均为 0。
func MomentumScore(klines []types.KLine, mDays int) (score, annualized, r2 float64) {
	if mDays <= 1 {
		return 0, 0, 0
	}
	n := len(klines)
	if n < mDays {
		return 0, 0, 0
	}
	closes := make([]float64, mDays)
	for i := 0; i < mDays; i++ {
		c := klines[n-mDays+i].Close
		if c <= 0 {
			return 0, 0, 0
		}
		closes[i] = math.Log(c)
	}

	// 权重: linspace(1, 2, m)
	w := make([]float64, mDays)
	if mDays == 1 {
		w[0] = 1
	} else {
		step := 1.0 / float64(mDays-1)
		for i := 0; i < mDays; i++ {
			w[i] = 1.0 + step*float64(i)
		}
	}

	// 加权线性回归: y = a*x + b
	var sumW, sumWX, sumWY, sumWXX, sumWXY float64
	for i := 0; i < mDays; i++ {
		x := float64(i)
		y := closes[i]
		sumW += w[i]
		sumWX += w[i] * x
		sumWY += w[i] * y
		sumWXX += w[i] * x * x
		sumWXY += w[i] * x * y
	}
	denom := sumW*sumWXX - sumWX*sumWX
	if denom == 0 {
		return 0, 0, 0
	}
	slope := (sumW*sumWXY - sumWX*sumWY) / denom
	intercept := (sumWY - slope*sumWX) / sumW

	annualized = math.Exp(slope*250) - 1

	// 加权 R²：SST_w 中的 ȳ 使用「未加权」均值（对齐聚宽 np.mean(y)）
	var sumY float64
	for i := 0; i < mDays; i++ {
		sumY += closes[i]
	}
	yMeanUnweighted := sumY / float64(mDays)

	var sseW, sstW float64
	for i := 0; i < mDays; i++ {
		x := float64(i)
		y := closes[i]
		yhat := slope*x + intercept
		sseW += w[i] * (y - yhat) * (y - yhat)
		sstW += w[i] * (y - yMeanUnweighted) * (y - yMeanUnweighted)
	}
	if sstW == 0 {
		r2 = 0
	} else {
		// 不再 clamp 到 [0,1]：保持与聚宽一致，允许负 R² 把弱拟合标的的 score 推得更低
		r2 = 1 - sseW/sstW
	}

	score = annualized * r2
	return score, annualized, r2
}
