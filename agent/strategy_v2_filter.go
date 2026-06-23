package agent

import (
	"fmt"
	"time"

	"github.com/multi-agents-etf-trade-strategy/types"
)

// Strategy V2 过滤器：来自 stratagyV2.py 的 4 道胜率闸门。
//
// 设计原则：
//   - 完全无副作用，所有"状态"都通过 V2State 由调用方持有；
//   - 与 V3 评分逻辑解耦，仅做"是否允许进入 final 候选"的过滤；
//   - 4 个闸门可独立开/关，便于消融实验。
//
// 4 道闸门：
//  1. 突破闸门：当前价 > 21 日前价 × 1.001（来自 V2 `can_enter`）；
//  2. 过热过滤：非持仓且 5 日涨幅 > 15% 禁止建仓；
//  3. 冷却期：止盈/止损卖出后 N 个交易日内禁买；
//  4. 同日黑名单：当日触发止损的标的当日禁买（V2 `ban_etf`）。

// V2FilterConfig 各闸门的开关与参数。零值表示走默认（启用 + V2 同款参数）。
type V2FilterConfig struct {
	BreakoutEnabled   bool    // 闸门 1
	OverheatEnabled   bool    // 闸门 2
	CooldownEnabled   bool    // 闸门 3
	BanEnabled        bool    // 闸门 4
	BreakoutLookback  int     // 闸门 1 的回看根数（默认 21）
	BreakoutMargin    float64 // 闸门 1 的突破容忍度（默认 0.001 即 +0.1%）
	OverheatLookback  int     // 闸门 2 的回看根数（默认 6 → 5 日涨幅）
	OverheatThreshold float64 // 闸门 2 涨幅阈值（默认 0.15 → 15%）
	CooldownTradeDays int     // 闸门 3 冷却交易日数（默认 5）
	StopProfitTrigger float64 // 触发止盈阈值（默认 +0.20）
	StopLossTrigger   float64 // 触发止损阈值（默认 -0.10）
}

func DefaultV2FilterConfig() V2FilterConfig {
	return V2FilterConfig{
		BreakoutEnabled:   true,
		OverheatEnabled:   true,
		CooldownEnabled:   true,
		BanEnabled:        true,
		BreakoutLookback:  21,
		BreakoutMargin:    0.001,
		OverheatLookback:  6,
		OverheatThreshold: 0.15,
		CooldownTradeDays: 5,
		StopProfitTrigger: 0.20,
		StopLossTrigger:   -0.10,
	}
}

// V2State 跨样本状态：冷却字典 + 同日黑名单 + 当前持仓集合。
//
// 在回测中由 backtest.Engine 持有；在 advice 模式下由调用方按需传入（一般为空）。
type V2State struct {
	// CooldownUntil[code] = 该日期（含）之前禁买
	CooldownUntil map[string]time.Time
	// BanToday[code] = 当日触发止损的代码（asOf 滚动到下一日时清空）
	BanToday map[string]bool
	// Holds 当前持仓集合（asOf 当日）。空 map 表示无持仓视角。
	Holds map[string]bool
}

func NewV2State() *V2State {
	return &V2State{
		CooldownUntil: map[string]time.Time{},
		BanToday:      map[string]bool{},
		Holds:         map[string]bool{},
	}
}

// CleanupCooldown 把冷却到期的条目清理掉。
func (s *V2State) CleanupCooldown(today time.Time) {
	for code, until := range s.CooldownUntil {
		if today.After(until) {
			delete(s.CooldownUntil, code)
		}
	}
}

// MarkStopOut 当一个持仓在 today 触发止盈/止损 → 加入 BanToday + 设置冷却。
//
// cooldownEnd 通常 = today + N 个交易日；调用方负责把"交易日"换算成日期。
func (s *V2State) MarkStopOut(code string, today, cooldownEnd time.Time) {
	s.BanToday[code] = true
	s.CooldownUntil[code] = cooldownEnd
}

// ResetBanToday 切换到新交易日时重置同日黑名单。
func (s *V2State) ResetBanToday() {
	s.BanToday = map[string]bool{}
}

// V2FilterDecision 单个候选的过滤决策；Allowed=false 时附带 Reason。
type V2FilterDecision struct {
	Code    string
	Allowed bool
	Reason  string
}

// ApplyV2Filter 对 ScoredETF 列表执行 V2 4 道过滤，返回放行的子集 + 拒绝详情。
//
// klines 必须按时间升序，包含足够长度（≥ BreakoutLookback+1 与 ≥ OverheatLookback）。
// today 用于冷却比对。
func ApplyV2Filter(
	candidates []types.ScoredETF,
	cfg V2FilterConfig,
	state *V2State,
	today time.Time,
) ([]types.ScoredETF, []V2FilterDecision) {
	if cfg == (V2FilterConfig{}) {
		cfg = DefaultV2FilterConfig()
	}
	if state == nil {
		state = NewV2State()
	}

	out := make([]types.ScoredETF, 0, len(candidates))
	dec := make([]V2FilterDecision, 0, len(candidates))

	for _, c := range candidates {
		k := c.ETF.History
		code := c.ETF.Code

		// 闸门 4：同日止损黑名单
		if cfg.BanEnabled && state.BanToday[code] {
			dec = append(dec, V2FilterDecision{Code: code, Allowed: false, Reason: "同日止损黑名单"})
			continue
		}

		// 闸门 3：冷却期（与持仓无关，纯黑名单）
		if cfg.CooldownEnabled {
			if until, ok := state.CooldownUntil[code]; ok && !today.After(until) {
				dec = append(dec, V2FilterDecision{Code: code, Allowed: false,
					Reason: fmt.Sprintf("冷却中，至 %s 解禁", until.Format("2006-01-02"))})
				continue
			}
		}

		// 闸门 1：突破闸门 — 当前价 > T-N 价 × (1 + margin)
		if cfg.BreakoutEnabled {
			lb := cfg.BreakoutLookback
			if lb <= 0 {
				lb = 21
			}
			if len(k) < lb+1 {
				dec = append(dec, V2FilterDecision{Code: code, Allowed: false, Reason: "K 线不足以判断突破"})
				continue
			}
			ref := k[len(k)-1-lb].Close
			now := k[len(k)-1].Close
			if ref <= 0 || now <= ref*(1+cfg.BreakoutMargin) {
				dec = append(dec, V2FilterDecision{Code: code, Allowed: false,
					Reason: fmt.Sprintf("未突破21日前价位 (%.3f vs %.3f)", now, ref)})
				continue
			}
		}

		// 闸门 2：过热过滤 — 非持仓且 5 日涨幅 > 15% 禁止建仓
		if cfg.OverheatEnabled && !state.Holds[code] {
			lb := cfg.OverheatLookback
			if lb <= 0 {
				lb = 6
			}
			if len(k) >= lb {
				start := k[len(k)-lb].Close
				now := k[len(k)-1].Close
				if start > 0 {
					ret := now/start - 1
					if ret > cfg.OverheatThreshold {
						dec = append(dec, V2FilterDecision{Code: code, Allowed: false,
							Reason: fmt.Sprintf("5 日涨幅过热 (%+.2f%% > %.0f%%)",
								ret*100, cfg.OverheatThreshold*100)})
						continue
					}
				}
			}
		}

		out = append(out, c)
		dec = append(dec, V2FilterDecision{Code: code, Allowed: true})
	}
	return out, dec
}
