package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/multi-agents-etf-trade-strategy/datasource"
	"github.com/multi-agents-etf-trade-strategy/types"
)

// IntradayWatchAgent 盘中实时盯盘 Agent。
//
// 核心场景：早报说"等回踩买入"，结果今天一直跌下去——需要一个实时信号告诉你
//   - 回踩到位了，可以入场了  (pullback_entry)
//   - 趋势已破，别买了        (abandon)
//   - 已经起飞，追涨风险高    (chase_now)
//   - 继续等，还没到入场区    (wait)
//
// 对持仓：
//   - 正常持有                (hold)
//   - 建议减仓/止盈           (trim_now)
//   - 止损触发                (stop_out)
//   - 加仓好时机              (add)
//
// 完全规则驱动（不调 LLM），延迟 < 2 秒，适合盘中反复刷新。
type IntradayWatchAgent struct {
	DS   datasource.ETFDataSource
	RQ   datasource.RealtimeQuoter // 实时报价器（通常 = DS 的类型断言）
	Conf types.IntradayWatchConfig
}

// NewIntradayWatchAgent 构造盘中盯盘 Agent。
func NewIntradayWatchAgent(ds datasource.ETFDataSource, conf types.IntradayWatchConfig) *IntradayWatchAgent {
	a := &IntradayWatchAgent{DS: ds, Conf: conf}
	if rq, ok := ds.(datasource.RealtimeQuoter); ok {
		a.RQ = rq
	}
	return a
}

// Run 执行盘中盯盘分析。
func (a *IntradayWatchAgent) Run(ctx context.Context) (*types.IntradayAnalysis, error) {
	if a.RQ == nil {
		return nil, fmt.Errorf("数据源不支持实时报价 (RealtimeQuoter)")
	}

	// 1. 合并目标列表：持仓 + 推荐 Top5，去重
	targets := a.mergeTargets()
	if len(targets) == 0 {
		return nil, fmt.Errorf("无盯盘目标（需提供 --current-hold 或今日报告 Top5）")
	}

	// 2. 大盘快照（510300）
	market := a.snapshot(ctx, "510300", "沪深300ETF华泰柏瑞", "宽基", false, false)

	// 3. 逐只快照 + 判定
	snapshots := make([]types.IntradaySnapshot, 0, len(targets))
	for _, t := range targets {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		s := a.snapshot(ctx, t.Code, t.Name, t.Sector, t.IsHold, t.IsTopPick)
		snapshots = append(snapshots, s)
	}

	// 4. 排序：urgent 优先 → watch → info
	sort.SliceStable(snapshots, func(i, j int) bool {
		return priorityRank(snapshots[i].Priority) < priorityRank(snapshots[j].Priority)
	})

	// 5. 统计 + 总结
	urgentCount := 0
	for _, s := range snapshots {
		if s.Priority == "urgent" {
			urgentCount++
		}
	}

	summary := a.buildSummary(market, snapshots, urgentCount)

	return &types.IntradayAnalysis{
		GeneratedAt: time.Now(),
		MarketIndex: market,
		Snapshots:   snapshots,
		UrgentCount: urgentCount,
		Summary:     summary,
	}, nil
}

// intradayTarget 合并后的盯盘目标。
type intradayTarget struct {
	Code, Name, Sector string
	IsHold             bool
	IsTopPick          bool
}

func (a *IntradayWatchAgent) mergeTargets() []intradayTarget {
	seen := make(map[string]bool)
	var out []intradayTarget

	// 持仓优先
	for _, code := range a.Conf.CurrentHolds {
		code = strings.TrimSpace(code)
		if code == "" || seen[code] {
			continue
		}
		seen[code] = true
		name, sector := lookupETFName(code)
		out = append(out, intradayTarget{Code: code, Name: name, Sector: sector, IsHold: true})
	}

	// 推荐 Top5
	for _, e := range a.Conf.Top5 {
		if seen[e.ETF.Code] {
			// 已在持仓列表中，标记 IsTopPick
			for i := range out {
				if out[i].Code == e.ETF.Code {
					out[i].IsTopPick = true
				}
			}
			continue
		}
		seen[e.ETF.Code] = true
		out = append(out, intradayTarget{
			Code: e.ETF.Code, Name: e.ETF.Name, Sector: e.ETF.Sector, IsTopPick: true,
		})
	}

	return out
}

// snapshot 拉取单只标的的实时快照并生成判定。
func (a *IntradayWatchAgent) snapshot(
	ctx context.Context, code, name, sector string, isHold, isTopPick bool,
) types.IntradaySnapshot {
	s := types.IntradaySnapshot{
		ETFCode: code, ETFName: name, Sector: sector,
		IsHold: isHold, IsTopPick: isTopPick,
	}

	// 实时报价
	q, err := a.RQ.FetchRealtimeQuote(code)
	if err == nil && q.Price > 0 {
		s.Price = q.Price
		s.PrevClose = q.PrevClose
		s.ChangePct = q.ChangePct
		s.IOPV = q.IOPV
		if q.IOPV > 0 {
			s.Premium = (q.Price - q.IOPV) / q.IOPV
		}
	}

	// 近期 K 线（11 根 = 计算 MA5/MA10 + 趋势判断）
	klines, err := a.DS.GetKLine(code, 11)
	if err == nil && len(klines) >= 5 {
		last := klines[len(klines)-1]
		// 如果实时价 > 0，用实时价覆盖最后一根的 Close（盘中 K 线 Close 尚未更新）
		if s.Price > 0 {
			last.Close = s.Price
			if s.Price > last.High {
				last.High = s.Price
			}
			if s.Price < last.Low || last.Low == 0 {
				last.Low = s.Price
			}
		} else {
			// 没有实时报价时退化用 K 线收盘价
			s.Price = last.Close
			s.PrevClose = prevClose(klines)
		}
		s.Open = last.Open
		s.DayHigh = last.High
		s.DayLow = last.Low

		// 技术指标
		s.MA5 = sma(klines, 5)
		s.MA10 = sma(klines, 10)
		if s.MA5 > 0 {
			s.VsMA5 = (s.Price - s.MA5) / s.MA5
		}
		s.RecentTrend = recentTrend(klines)
	}

	// 报告关键价位（优先集合竞价修正价，回退 8:50 早报原价）
	entry, stop, tp := a.reportLevels(code)
	s.EntryPrice = entry
	s.StopLoss = stop
	s.TakeProfit = tp
	s.PreOpenVerdict = a.preOpenVerdict(code)
	if entry > 0 {
		s.DistToEntry = (s.Price - entry) / entry
	}
	if stop > 0 {
		s.DistToStop = (s.Price - stop) / stop
	}
	if tp > 0 {
		s.DistToTP = (s.Price - tp) / tp
	}

	// 与历次盯盘对比：取该 code 在最近 N 次快照里的价格序列（老→新），算 Δ 与跨刷新趋势
	if len(a.Conf.PrevIntradays) > 0 {
		var series []float64
		for _, pa := range a.Conf.PrevIntradays { // 已按老→新排序
			if pa == nil {
				continue
			}
			for _, p := range pa.Snapshots {
				if p.ETFCode == code && p.Price > 0 {
					series = append(series, p.Price)
					break
				}
			}
		}
		if len(series) > 0 {
			s.PrevPrice = series[len(series)-1] // 最近一次
			if s.Price > 0 {
				s.PriceDelta = (s.Price - s.PrevPrice) / s.PrevPrice
				series = append(series, s.Price) // 追加当前价，参与趋势判定
			}
			s.PrevTrend = intradayTrend(series)
		}
	}

	// 判定
	a.judge(&s)
	// 有上次盯盘价时，把 Δ + 跨刷新趋势追加进 rationale（不改信号，只增语境）
	if s.PrevPrice > 0 {
		extra := fmt.Sprintf("距上次盯盘 %+.2f%%", s.PriceDelta*100)
		if s.PrevTrend != "" {
			extra += "，" + s.PrevTrend
		}
		s.Rationale = fmt.Sprintf("%s %s。", s.Rationale, extra)
	}
	return s
}

// intradayTrend 给定某 code 的历次盯盘价（老→新，含当前价），返回简短趋势标签。
// 至少 3 个有效点才判定（避免两点误判）；单调下行=连跌，单调上行=连涨，其余=震荡。
func intradayTrend(prices []float64) string {
	if len(prices) < 3 {
		return ""
	}
	down := 0
	for i := 1; i < len(prices); i++ {
		if prices[i] < prices[i-1] {
			down++
		}
	}
	steps := len(prices) - 1
	switch {
	case down >= steps:
		return fmt.Sprintf("近%d次连跌", len(prices))
	case down == 0:
		return fmt.Sprintf("近%d次连涨", len(prices))
	default:
		return fmt.Sprintf("近%d次震荡", len(prices))
	}
}

// reportLevels 从今日报告提取入场/止损/止盈价。
// 优先级：9:24 集合竞价复核 (Conf.PreOpen) 的修正价位 > 8:50 早报 (FinalDecision) 原价。
// preopen 的 adj_entry=0 表示"等回踩/不追"，此时回退到早报原入场价作为盯盘参考线。
func (a *IntradayWatchAgent) reportLevels(code string) (entry, stop, tp float64) {
	// 基线：8:50 早报
	if a.Conf.FinalDecision != nil {
		if a.Conf.FinalDecision.TargetETF.ETF.Code == code {
			entry, stop, tp = a.Conf.FinalDecision.EntryPrice, a.Conf.FinalDecision.StopLoss, a.Conf.FinalDecision.TakeProfit
		}
		for _, p := range a.Conf.FinalDecision.Picks {
			if p.ETFCode == code {
				entry, stop, tp = p.EntryPrice, p.StopLoss, p.TakeProfit
			}
		}
	}
	// 覆盖：集合竞价撮合修正后的价位（更贴近当日开盘现实，仅 >0 才覆盖）
	if a.Conf.PreOpen != nil {
		for _, ps := range a.Conf.PreOpen.Snapshots {
			if ps.ETFCode != code {
				continue
			}
			if ps.AdjEntry > 0 {
				entry = ps.AdjEntry
			}
			if ps.AdjStopLoss > 0 {
				stop = ps.AdjStopLoss
			}
			if ps.AdjTakeProf > 0 {
				tp = ps.AdjTakeProf
			}
			break
		}
	}
	return entry, stop, tp
}

// preOpenVerdict 取该标的在 9:24 集合竞价复核中的结论（仅作展示参考，不覆盖盘中实时信号）。
func (a *IntradayWatchAgent) preOpenVerdict(code string) string {
	if a.Conf.PreOpen == nil {
		return ""
	}
	for _, ps := range a.Conf.PreOpen.Snapshots {
		if ps.ETFCode == code {
			return ps.Verdict
		}
	}
	return ""
}

// judge 核心判定逻辑——纯规则，根据快照数据生成 Signal / Priority / Rationale。
func (a *IntradayWatchAgent) judge(s *types.IntradaySnapshot) {
	price := s.Price
	if price <= 0 {
		s.Signal = "wait"
		s.SignalCn = "无法获取实时行情"
		s.Priority = "info"
		s.Rationale = "实时报价拉取失败，无法判定。"
		return
	}

	if s.IsHold {
		a.judgeHold(s)
	} else {
		a.judgeBuy(s)
	}
}

// judgeHold 持仓侧判定。
func (a *IntradayWatchAgent) judgeHold(s *types.IntradaySnapshot) {
	price := s.Price

	// 1. 止损触发（最高优先级）
	if s.StopLoss > 0 && price <= s.StopLoss {
		s.Signal = "stop_out"
		s.SignalCn = fmt.Sprintf("止损触发！当前价 %.3f ≤ 止损价 %.3f", price, s.StopLoss)
		s.Priority = "urgent"
		s.Rationale = fmt.Sprintf("价格已跌破早报止损线 %.3f（跌幅 %.1f%%），建议立即平仓。", s.StopLoss, s.DistToStop*100)
		return
	}

	// 2. 接近止损（< 2%）
	if s.StopLoss > 0 && s.DistToStop < 0.02 {
		s.Signal = "trim_now"
		s.SignalCn = fmt.Sprintf("逼近止损（距止损仅 %.1f%%）", s.DistToStop*100)
		s.Priority = "urgent"
		s.Rationale = fmt.Sprintf("当前价距止损 %.3f 仅剩 %.1f%%，建议减仓防守。", s.StopLoss, s.DistToStop*100)
		return
	}

	// 3. 接近或超过止盈
	if s.TakeProfit > 0 && price >= s.TakeProfit*0.97 {
		if price >= s.TakeProfit {
			s.Signal = "trim_now"
			s.SignalCn = fmt.Sprintf("已达止盈目标 %.3f（当前 %.3f）", s.TakeProfit, price)
			s.Priority = "watch"
			s.Rationale = "价格已达止盈线，考虑分批止盈落袋为安。"
		} else {
			s.Signal = "trim_now"
			s.SignalCn = fmt.Sprintf("接近止盈（距目标 %.1f%%）", s.DistToTP*100)
			s.Priority = "watch"
			s.Rationale = fmt.Sprintf("距止盈 %.3f 仅差 %.1f%%，可考虑提前减仓。", s.TakeProfit, -s.DistToTP*100)
		}
		return
	}

	// 4. 当日大跌（> 3%）+ 跌破 MA5
	if s.ChangePct < -3 && s.VsMA5 < 0 {
		s.Signal = "trim_now"
		s.SignalCn = fmt.Sprintf("大跌 %.1f%% 且跌破 MA5，建议减仓", s.ChangePct)
		s.Priority = "watch"
		s.Rationale = fmt.Sprintf("当日跌幅 %.1f%%，价格低于 MA5 %.1f%%，短线趋势走弱。", s.ChangePct, s.VsMA5*100)
		return
	}

	// 5. 一切正常
	s.Signal = "hold"
	s.SignalCn = "正常持有"
	s.Priority = "info"
	reasons := []string{}
	if s.ChangePct > 0 {
		reasons = append(reasons, fmt.Sprintf("今日涨 %.1f%%", s.ChangePct))
	} else {
		reasons = append(reasons, fmt.Sprintf("今日跌 %.1f%%", s.ChangePct))
	}
	if s.StopLoss > 0 {
		reasons = append(reasons, fmt.Sprintf("距止损 %.1f%%", s.DistToStop*100))
	}
	if len(reasons) > 0 {
		s.Rationale = strings.Join(reasons, "，") + "，暂无风险信号。"
	} else {
		s.Rationale = "未检测到风险信号。"
	}
}

// judgeBuy 买入侧判定（今日推荐但尚未买入的标的）。
func (a *IntradayWatchAgent) judgeBuy(s *types.IntradaySnapshot) {
	price := s.Price

	// 无入场价参考 → 纯实时信号
	if s.EntryPrice <= 0 {
		if s.ChangePct > 2 {
			s.Signal = "chase_now"
			s.SignalCn = fmt.Sprintf("今日已涨 %.1f%%，追涨风险高", s.ChangePct)
			s.Priority = "watch"
			s.Rationale = "无入场价参考，但当日涨幅较大，追涨需谨慎。"
		} else if s.ChangePct < -3 {
			s.Signal = "abandon"
			s.SignalCn = fmt.Sprintf("今日大跌 %.1f%%，趋势可能已破", s.ChangePct)
			s.Priority = "watch"
			s.Rationale = "无入场价参考，但当日跌幅显著，建议观望。"
		} else {
			s.Signal = "wait"
			s.SignalCn = "走势平稳，继续观察"
			s.Priority = "info"
			s.Rationale = fmt.Sprintf("今日变动 %.1f%%，无明确信号。", s.ChangePct)
		}
		return
	}

	entry := s.EntryPrice
	dist := s.DistToEntry // (price-entry)/entry，负=低于入场价

	// 1. 已回踩到入场价附近（±2%）→ pullback_entry
	if dist >= -0.02 && dist <= 0.02 {
		s.Signal = "pullback_entry"
		s.SignalCn = fmt.Sprintf("回踩到位！当前 %.3f ≈ 入场价 %.3f", price, entry)
		s.Priority = "urgent"
		s.Rationale = fmt.Sprintf("价格已回调至入场区间 [%.3f, %.3f]，符合早报等回踩策略，可以入场。",
			entry*0.98, entry*1.02)
		return
	}

	// 2. 跌破入场价 2%~5% → 可能 deeper pullback，继续等
	if dist < -0.02 && dist >= -0.05 {
		s.Signal = "wait"
		s.SignalCn = fmt.Sprintf("已低于入场价 %.1f%%，可能还在回踩中", -dist*100)
		s.Priority = "watch"
		s.Rationale = fmt.Sprintf("价格低于入场价 %.3f，跌幅 %.1f%%。回踩可能未结束，继续等待企稳信号。", entry, -dist*100)
		return
	}

	// 3. 跌破入场价 > 5% → abandon，趋势可能已破
	if dist < -0.05 {
		// 但如果 MA5 还在上方且近期趋势 up，可能只是 deep pullback
		if s.RecentTrend == "up" && s.MA5 > 0 && price >= s.MA5*0.99 {
			s.Signal = "wait"
			s.SignalCn = fmt.Sprintf("深跌 %.1f%% 但仍守住 MA5，等反弹", -dist*100)
			s.Priority = "watch"
			s.Rationale = fmt.Sprintf("虽低于入场价 %.1f%%，但近 5 日趋势向上且守住 MA5 (%.3f)，可能是极限洗盘。", -dist*100, s.MA5)
			return
		}
		s.Signal = "abandon"
		s.SignalCn = fmt.Sprintf("跌破入场价 %.1f%%，趋势可能已破", -dist*100)
		s.Priority = "urgent"
		s.Rationale = fmt.Sprintf("价格远低于建议入场价，跌幅 %.1f%%，且近 5 日趋势 %s，建议放弃该标的。", -dist*100, trendCn(s.RecentTrend))
		return
	}

	// 4. 已高于入场价 > 2% → chase_now（追涨成本增加）
	//    注：dist ∈ (0, 0.02] 已在上方 pullback_entry 分支并入"可入场"区间，故此处只处理 > 2%。
	if dist > 0.02 {
		s.Signal = "chase_now"
		s.SignalCn = fmt.Sprintf("已高于入场价 %.1f%%，追涨需谨慎", dist*100)
		s.Priority = "watch"
		s.Rationale = fmt.Sprintf("当前价高于入场价 %.3f，追涨成本增加。涨幅 %.1f%%。", entry, dist*100)
		return
	}

	// 5. 兜底（理论不可达：dist 取值已被上方四个区间完整覆盖；保留以防新增区间时漏判）
	s.Signal = "wait"
	s.SignalCn = "继续等待入场时机"
	s.Priority = "info"
	s.Rationale = fmt.Sprintf("当前价 %.3f vs 入场价 %.3f，未进入入场区间。", price, entry)
}

// ── 辅助函数 ──────────────────────────────────────────────

func (a *IntradayWatchAgent) buildSummary(market types.IntradaySnapshot, snaps []types.IntradaySnapshot, urgent int) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("大盘 %s(%.1f%%)｜", market.ETFName, market.ChangePct))
	if urgent > 0 {
		b.WriteString(fmt.Sprintf("⚠️ %d 只标的发出紧急信号", urgent))
	} else {
		b.WriteString("暂无紧急信号")
	}
	for _, s := range snaps {
		if s.Priority == "urgent" {
			b.WriteString(fmt.Sprintf("\n⚡ %s(%s): %s", s.ETFName, s.ETFCode, s.SignalCn))
		}
	}
	return b.String()
}

func priorityRank(p string) int {
	switch p {
	case "urgent":
		return 0
	case "watch":
		return 1
	default:
		return 2
	}
}

func trendCn(t string) string {
	switch t {
	case "up":
		return "向上"
	case "down":
		return "向下"
	default:
		return "横盘"
	}
}

// lookupETFName 从 Strategy3Pool 查代码对应的名称和板块。
func lookupETFName(code string) (name, sector string) {
	for _, e := range Strategy3Pool {
		if e.Code == code {
			return e.Name, e.Sector
		}
	}
	return code, ""
}

// sma 简单移动平均。
func sma(klines []types.KLine, n int) float64 {
	if len(klines) < n || n <= 0 {
		return 0
	}
	var sum float64
	for i := len(klines) - n; i < len(klines); i++ {
		sum += klines[i].Close
	}
	return sum / float64(n)
}

// recentTrend 近 5 日趋势：close[5] vs close[1]。
func recentTrend(klines []types.KLine) string {
	if len(klines) < 5 {
		return "flat"
	}
	n := len(klines)
	old := klines[n-5].Close
	newest := klines[n-1].Close
	if newest > old*1.01 {
		return "up"
	}
	if newest < old*0.99 {
		return "down"
	}
	return "flat"
}

// prevClose 取倒数第二根 K 线的收盘（= 昨收）。
func prevClose(klines []types.KLine) float64 {
	if len(klines) < 2 {
		return 0
	}
	return klines[len(klines)-2].Close
}

// FormatIntradayCLI 将盘中分析结果格式化为终端友好输出。
func FormatIntradayCLI(a *types.IntradayAnalysis) string {
	var b strings.Builder
	b.WriteString("\n═══ 盘中实时盯盘 ═══\n")
	b.WriteString(fmt.Sprintf("时间: %s\n", a.GeneratedAt.Format("2006-01-02 15:04:05")))
	b.WriteString(fmt.Sprintf("大盘: %s  %.2f%%\n", a.MarketIndex.ETFName, a.MarketIndex.ChangePct))
	b.WriteString("\n")

	for _, s := range a.Snapshots {
		icon := signalIcon(s.Signal, s.Priority)
		tags := ""
		if s.IsHold {
			tags += " 🟦持仓"
		}
		if s.IsTopPick {
			tags += " ⭐推荐"
		}
		b.WriteString(fmt.Sprintf("%s %s(%s)%s\n", icon, s.ETFName, s.ETFCode, tags))
		b.WriteString(fmt.Sprintf("   当前 %.3f  涨跌 %.2f%%  vsMA5 %.2f%%  趋势%s\n",
			s.Price, s.ChangePct, s.VsMA5*100, trendCn(s.RecentTrend)))
		if s.EntryPrice > 0 {
			b.WriteString(fmt.Sprintf("   入场 %.3f(距入场 %+.2f%%)  止损 %.3f(距止损 %+.2f%%)  止盈 %.3f\n",
				s.EntryPrice, s.DistToEntry*100, s.StopLoss, s.DistToStop*100, s.TakeProfit))
		}
		// 跨报告上下文：集合竞价复核结论 + 距上次盯盘涨跌 + 跨刷新趋势
		var chips []string
		if s.PreOpenVerdict != "" {
			chips = append(chips, "竞价:"+s.PreOpenVerdict)
		}
		if s.PrevPrice > 0 {
			chips = append(chips, fmt.Sprintf("距上次盯盘 %+.2f%%", s.PriceDelta*100))
		}
		if s.PrevTrend != "" {
			chips = append(chips, s.PrevTrend)
		}
		if len(chips) > 0 {
			b.WriteString(fmt.Sprintf("   [%s]\n", strings.Join(chips, " | ")))
		}
		b.WriteString(fmt.Sprintf("   → %s [%s] %s\n\n", s.SignalCn, s.Priority, s.Rationale))
	}

	if a.UrgentCount > 0 {
		b.WriteString(fmt.Sprintf("⚡ 共 %d 条紧急信号，请立即关注！\n", a.UrgentCount))
	} else {
		b.WriteString("✅ 暂无紧急信号\n")
	}

	return b.String()
}

func signalIcon(signal, priority string) string {
	if priority == "urgent" {
		return "⚡"
	}
	switch signal {
	case "pullback_entry", "add":
		return "🎯"
	case "chase_now":
		return "🔥"
	case "stop_out", "abandon":
		return "🛑"
	case "trim_now":
		return "⚠️"
	default:
		return "📊"
	}
}
