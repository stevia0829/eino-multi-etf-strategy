package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/eino-multi-etf-strategy/datasource"
	"github.com/eino-multi-etf-strategy/indicator"
	"github.com/eino-multi-etf-strategy/types"
)

// Strategy3Pool 来源于 strategy.py 中 g.etf_pool_3，覆盖
// 黄金/白银/原油/海外指数/恒生/科技/新能源/军工/消费/医药等 60+ 标的。
// Code 使用东财风格的 6 位代码（去掉 .XSHE/.XSHG 后缀）。
var Strategy3Pool = []struct {
	Code, Name, Sector string
}{
	{"518880", "黄金ETF", "贵金属"},
	{"161226", "国投白银LOF", "贵金属"},
	{"501018", "南方原油LOF", "能源"},
	{"159985", "豆粕ETF", "农产品"},
	{"513520", "日经ETF", "海外"},
	{"513100", "纳指ETF", "海外"},
	{"513300", "纳斯达克ETF", "海外"},
	{"513400", "道琼斯ETF", "海外"},
	{"159529", "标普消费ETF", "海外"},
	{"513030", "德国ETF", "海外"},
	{"159329", "沙特ETF", "海外"},
	{"513130", "恒生科技ETF", "港股"},
	{"513090", "香港证券ETF", "港股"},
	{"513120", "港股创新药ETF", "港股"},
	{"159206", "卫星ETF", "军工"},
	{"159218", "卫星ETF招商", "军工"},
	{"159227", "航空航天ETF", "军工"},
	{"159565", "汽车零部件ETF", "汽车"},
	{"562500", "机器人ETF", "科技"},
	{"159819", "人工智能ETF", "科技"},
	{"159363", "创业板人工智能ETF", "科技"},
	{"512480", "半导体ETF", "科技"},
	{"512760", "芯片ETF", "科技"},
	{"515880", "通信ETF", "科技"},
	{"515230", "软件ETF", "科技"},
	{"515050", "通信ETF华夏", "科技"},
	{"159786", "VRETF", "科技"},
	{"159890", "云计算ETF", "科技"},
	{"516160", "新能源ETF", "新能源"},
	{"515790", "光伏ETF", "新能源"},
	{"159755", "电池ETF", "新能源"},
	{"512660", "军工ETF", "军工"},
	{"159732", "消费电子ETF", "科技"},
	{"159992", "创新药ETF", "医药"},
	{"159852", "软件ETF沪", "科技"},
	{"159851", "金融科技ETF", "金融"},
	{"159869", "游戏ETF", "传媒"},
	{"516780", "稀土ETF", "材料"},
	{"159928", "消费ETF", "消费"},
	{"512690", "酒ETF", "消费"},
	{"515170", "食品饮料ETF", "消费"},
	{"512010", "医药ETF", "医药"},
	{"512980", "传媒ETF", "传媒"},
	{"159378", "通用航空ETF", "军工"},
	{"159611", "电力ETF", "公用"},
	{"159766", "旅游ETF", "消费"},
	{"515220", "煤炭ETF", "能源"},
	{"159865", "养殖ETF", "农产品"},
	{"562800", "稀有金属ETF", "材料"},
	{"560860", "工业有色ETF", "材料"},
	{"510050", "上证50ETF", "宽基"},
	{"510300", "沪深300ETF", "宽基"},
	{"159922", "中证500ETF", "宽基"},
	{"159531", "中证2000ETF", "宽基"},
	{"159915", "创业板ETF", "宽基"},
	{"588080", "科创50ETF易方达", "宽基"},
	{"588380", "科创创业ETF", "宽基"},
	{"160211", "国泰小盘LOF", "宽基"},
	{"512000", "券商ETF", "金融"},
	{"512800", "银行ETF", "金融"},
	{"510880", "红利ETF", "宽基"},
	{"511090", "30年国债ETF", "债券"},

	// === 2026-06 补充：腾讯财经接口 (qt.gtimg.cn) 校验过名称的新增标的 ===
	{"588000", "科创50ETF华夏", "宽基"},
	{"159949", "创业板50ETF", "宽基"},
	{"560610", "A500ETF招商", "宽基"},
	{"159338", "中证A500ETF", "宽基"},
	{"512170", "医疗ETF", "医药"},
	{"159770", "机器人ETF天弘", "科技"},
	{"159892", "恒生医药ETF", "港股"},
	{"513880", "日经225ETF", "海外"},
	{"159509", "纳指科技ETF", "海外"},
	{"515000", "科技ETF", "科技"},
	{"515650", "消费50ETF", "消费"},
	{"159996", "家电ETF", "消费"},
	{"515260", "电子ETF", "科技"},
	{"515700", "新能源车ETF平安", "新能源"},
	{"515030", "新能源车ETF华夏", "新能源"},
	{"159792", "港股通互联网ETF", "港股"},
	{"588200", "科创芯片ETF", "科技"},
	{"588160", "科创新材料ETF", "材料"},
}

// RotationParams 对应 strategy.py 中的策略 3 参数。
type RotationParams struct {
	MDays                    int     // 动量参考天数 (g.m_days, default 21)
	MaxScore                 float64 // 动量过滤上限 (g.max_score, default 6)
	MinScore                 float64 // 动量过滤下限 (g.min_score, default 0)
	ScoreThresholdMultiplier float64 // 高分情形下日间增长阈值 (g.score_threshold_multiplier, default 1.1)
	TopN                     int     // 返回前 N 名

	// ── P1 学术增强（可选；默认全关，保持 P0 行为不变） ──────────────
	// EnableDualMomentum 启用双周期动量（Antonacci 2014）：要求 LongLookback 日年化 >= LongMinAnnualized。
	EnableDualMomentum bool
	LongLookback       int     // 默认 252（约 1 年）
	LongMinAnnualized  float64 // 默认 0.0（即"跑赢无风险≈0%"的简化版）

	// EnableConvexity 启用凸性调整（Daniel & Moskowitz 2016）：score := score / max(σ_n, floor)。
	EnableConvexity     bool
	ConvexityLookback   int     // 默认 21
	ConvexitySigmaFloor float64 // 防 0 除/小波动放大，默认 0.05（年化 5%）

	// ── P1 风控旁路（Regime-aware） ──────────────────────────────────
	// RegimeAwareP1 启用后，仅在 RegimeTrend ∈ {bear, risk_off} 时
	// 真正生效 P1-1 双动量过滤 + P1-2 凸性调整；
	// bull/neutral_up/neutral 时跳过，让趋势市的高 σ 赢家裸奔。
	// 设计依据：P1-2 在温和单边市与"动量加速"机制冲突（短样本 -19 pp），
	// 但其论文设计目的是防"动量崩盘"，弱市才是它该工作的场景。
	RegimeAwareP1 bool
	// RegimeTrend 由调用方（回测引擎 / 实盘 pipeline）在 Rank 之前注入，
	// 取值与 classifyRegime 一致：bull / neutral_up / neutral / bear / risk_off。
	// 空字符串视为"未知"，按"启用 P1"保守处理（避免漏过崩盘期）。
	RegimeTrend string

	// MustIncludeCodes 强制保留进结果列表的 ETF 代码（典型用法：当前持仓）。
	// 它不会改变评分逻辑、不会插队、不会让标的"绕过"MinScore / 过热过滤，
	// 仅在 TopN 截断时给这些标的一个豁免位：若分数本就在 TopN 内则不影响，
	// 若分数被截断到 TopN 外，则按客观分数追加在结果末尾。
	// 设计目的：advice 模式下让用户能在同一份候选里直接看到持仓的客观动量分，
	// 自行判断"主信号没推荐我，但持仓动量是不是确实差到要切"。
	// 仅对 advice 生效；回测引擎不传该字段，回测行为完全不变。
	MustIncludeCodes []string
}

func DefaultRotationParams() RotationParams {
	return RotationParams{
		MDays:                    21,
		MaxScore:                 6,
		MinScore:                 0, // 对齐聚宽 g.min_score=0：负分动量直接出局
		ScoreThresholdMultiplier: 1.1,
		TopN:                     5,

		// P1 默认全关：保持 P0 主流程行为，由调用方/回测变体显式启用
		EnableDualMomentum:  false,
		LongLookback:        252,
		LongMinAnnualized:   0.0,
		EnableConvexity:     false,
		ConvexityLookback:   21,
		ConvexitySigmaFloor: 0.05,

		// 默认开启 Regime-aware：P1 仅在 bear/risk_off 时生效；
		// 调用方若关闭此开关，则恢复"全局启用 P1"的旧行为（与首版 v3p1 相同）。
		RegimeAwareP1: true,
		RegimeTrend:   "",
	}
}

// RotationCandidate 记录单个 ETF 的轮动评分快照。
type RotationCandidate struct {
	ETF        types.ETF
	Score      float64 // strategy_3 score (= annualized * R²)
	Annualized float64 // 年化收益率
	R2         float64 // 加权 R²
	PrevScore  float64 // T-1 日的 score（用于阈值过滤）
	HasPrev    bool    // 是否真的拿到了 T-1 score；区分"PrevScore 为负"与"无数据"
	Klines     []types.KLine
}

// RotationAction 把策略 3 中"是否继续持仓 / 是否轮动"的语义抽象成
// 与本地持仓状态无关的纯函数信号，由用户拿着这个信号去和自己的实盘比对：
//
//	StrongBuy  - 动量加速向上：score_T ≥ score_{T-1} × 1.1，鼓励新建仓
//	Buy        - 动量温和向上：score_T > score_{T-1}，可建仓 / 持有
//	HoldOnly   - 动量减速：score_T ≤ score_{T-1}，已持仓可继续，但不建议新建仓
//	Avoid      - 趋势失效：score < 0 或 R² 极低，建议清仓 / 不入场
type RotationAction string

const (
	ActionStrongBuy RotationAction = "strong_buy"
	ActionBuy       RotationAction = "buy"
	ActionHoldOnly  RotationAction = "hold_only"
	ActionAvoid     RotationAction = "avoid"
)

// Action 返回该候选标的对应的"持仓动作语义"。
// 该函数完全无状态：不依赖任何本地持仓信息，
// 仅基于 T 日 / T-1 日的策略 3 score 与 R² 判断。
func (c RotationCandidate) Action() RotationAction {
	if c.Score < 0 || c.R2 < 0.3 {
		return ActionAvoid
	}
	if c.HasPrev {
		// 负分反转：T-1 ≤ 0、T 由负转正，视为新出现的买入信号
		if c.PrevScore <= 0 && c.Score > 0 {
			return ActionBuy
		}
		if c.PrevScore > 0 {
			if c.Score >= c.PrevScore*1.1 {
				return ActionStrongBuy
			}
			if c.Score > c.PrevScore {
				return ActionBuy
			}
			return ActionHoldOnly
		}
	}
	// 没有 T-1 数据时，视分数高低给保守判断
	if c.Score >= 0.3 {
		return ActionBuy
	}
	return ActionHoldOnly
}

// ActionLabel 返回中文人类可读标签，便于 markdown / CLI 展示。
func (a RotationAction) Label() string {
	switch a {
	case ActionStrongBuy:
		return "强烈买入（动量加速）"
	case ActionBuy:
		return "买入（动量向上）"
	case ActionHoldOnly:
		return "观望（动量减速，已持仓可继续）"
	case ActionAvoid:
		return "回避（趋势失效）"
	}
	return string(a)
}

// BuildHoldAdvice 把"用户当前持仓"与本次合并后的候选列表（已含持仓豁免位）做对照，返回持仓建议。
// 当 currentHold 为空字符串时返回 nil，调用方据此跳过对应章节。
// 完全无状态：currentHold 仅本次会话内使用，不做任何持久化。
func BuildHoldAdvice(currentHold string, top []types.ScoredETF) *types.HoldAdvice {
	if currentHold == "" || len(top) == 0 {
		return nil
	}
	advice := &types.HoldAdvice{
		CurrentHold: currentHold,
		BestCode:    top[0].ETF.Code,
		BestName:    top[0].ETF.Name,
	}
	for i, e := range top {
		if e.ETF.Code == currentHold {
			// HitTop 维持原语义：是否进入"原 Top5 主名次"内（含豁免追加位也算 hit）。
			advice.HitTop = true
			advice.Rank = i + 1
			advice.HitName = e.ETF.Name
			advice.Action = e.Action
			advice.ActionDesc = e.ActionDesc
			break
		}
	}
	advice.Suggestion = composeHoldSuggestion(advice, top[0])
	return advice
}

// BuildHoldAdvices 在 advice 模式下为多只持仓批量生成持仓对照建议。
// 返回顺序与 currentHolds 一致；空持仓 / 无候选时返回 nil。
func BuildHoldAdvices(currentHolds []string, top []types.ScoredETF) []types.HoldAdvice {
	if len(currentHolds) == 0 || len(top) == 0 {
		return nil
	}
	out := make([]types.HoldAdvice, 0, len(currentHolds))
	for _, code := range currentHolds {
		code = strings.TrimSpace(code)
		if code == "" {
			continue
		}
		if a := BuildHoldAdvice(code, top); a != nil {
			out = append(out, *a)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func composeHoldSuggestion(a *types.HoldAdvice, best types.ScoredETF) string {
	if !a.HitTop {
		return fmt.Sprintf("当前持仓 %s 未进入 Top5，建议轮动至 %s(%s)。",
			a.CurrentHold, best.ETF.Name, best.ETF.Code)
	}
	switch RotationAction(a.Action) {
	case ActionStrongBuy:
		return fmt.Sprintf("当前持仓命中 Top%d 且动量加速，建议继续持有，可适度加仓。", a.Rank)
	case ActionBuy:
		return fmt.Sprintf("当前持仓命中 Top%d 且动量向上，建议继续持有。", a.Rank)
	case ActionHoldOnly:
		return fmt.Sprintf("当前持仓命中 Top%d 但动量已减速，建议持有观察，不再加仓。", a.Rank)
	case ActionAvoid:
		return fmt.Sprintf("当前持仓命中 Top%d 但趋势失效，建议轮动至 %s(%s)。",
			a.Rank, best.ETF.Name, best.ETF.Code)
	}
	return fmt.Sprintf("当前持仓命中 Top%d，参考动作：%s。", a.Rank, a.ActionDesc)
}

// RotationAgent 实现 strategy.py 中 get_etf_rank 的核心算法。
type RotationAgent struct {
	DS     datasource.ETFDataSource
	Params RotationParams
	// AsOf 指定基准日期；为零值时取当天最新行情。
	AsOf time.Time
}

func NewRotationAgent(ds datasource.ETFDataSource) *RotationAgent {
	return &RotationAgent{DS: ds, Params: DefaultRotationParams()}
}

// Rank 拉取 etf_pool_3 中每只标的最近 m_days+1 天 K 线，
// 计算 T 日 + T-1 日 score，按 strategy_3 规则过滤后排序返回。
//
// 过滤规则（来自 strategy.py）：
//   - keep min_score <= score <= max_score
//   - 若任意标的 score > max_score，则只保留满足
//     score_T >= score_{T-1} * score_threshold_multiplier 的标的（避免动量见顶）
func (a *RotationAgent) Rank(ctx context.Context) ([]RotationCandidate, error) {
	p := a.Params
	if p.MDays <= 1 {
		p = DefaultRotationParams()
	}

	candidates := make([]RotationCandidate, 0, len(Strategy3Pool))
	hasOverMax := false

	// ── P1 Regime-aware 闸门 ────────────────────────────────────────
	// 只有在 RegimeAwareP1=true 且当前 trend ∈ {bear, risk_off, ""(未知)} 时，
	// 才真正应用 P1-1 双动量过滤 + P1-2 凸性调整；
	// trend 已知是 bull/neutral_up/neutral 时跳过，让趋势市的高 σ 赢家裸奔。
	p1Active := !p.RegimeAwareP1 // 关闭闸门时恢复"全局启用"
	if p.RegimeAwareP1 {
		switch p.RegimeTrend {
		case "bear", "risk_off", "":
			p1Active = true
		default:
			p1Active = false
		}
	}
	useDualMomentum := p.EnableDualMomentum && p1Active
	useConvexity := p.EnableConvexity && p1Active

	// 过热门槛 MaxScore 与 useConvexity 联动：
	//   - useConvexity=true 时 score 量纲被 σ 放大约 5~20 倍，必须放宽 MaxScore（否则全员"过热"卡死）；
	//   - useConvexity=false 时回到原 P0 量纲（典型 score ∈ [0, 8]），保留 MaxScore=6 的过热判定。
	// 这样 v3p1 在 bull/neutral 时与 v3 完全等价，避免 v3p1 短样本被无差别放宽 MaxScore 拖累。
	maxScore := p.MaxScore
	if useConvexity && maxScore < 30 {
		maxScore = 30
	}

	// 数据需求：默认 m+1；若开启双动量需要 LongLookback+1
	need := p.MDays + 1
	if useDualMomentum && p.LongLookback+1 > need {
		need = p.LongLookback + 1
	}
	if useConvexity && p.ConvexityLookback+2 > need {
		need = p.ConvexityLookback + 2
	}

	for _, e := range Strategy3Pool {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		klines, err := a.DS.GetKLineAsOf(e.Code, need, a.AsOf)
		if err != nil || len(klines) < p.MDays {
			continue
		}

		// T 日 score = 用最后 mDays 根 K 线
		score, ann, r2 := indicator.MomentumScore(klines, p.MDays)

		// T-1 日 score = 去掉最后一根
		var prev float64
		hasPrev := false
		if len(klines) > p.MDays {
			prev, _, _ = indicator.MomentumScore(klines[:len(klines)-1], p.MDays)
			hasPrev = true
		}

		// ── P1-1 双周期动量过滤（Antonacci 2014）─────────────────────
		// 252 日年化 < 阈值 → 视为长期下跌趋势，直接出局
		if useDualMomentum {
			lb := p.LongLookback
			if lb <= 0 {
				lb = 252
			}
			if len(klines) >= lb {
				longAnn := indicator.AnnualizedReturnN(klines, lb)
				if longAnn < p.LongMinAnnualized {
					continue
				}
			}
		}

		// ── P1-2 凸性调整（Daniel & Moskowitz 2016）──────────────────
		// score := score / max(σ_n, floor)；T 与 T-1 都做，保证 1.1 倍门槛口径一致
		if useConvexity {
			lb := p.ConvexityLookback
			if lb <= 0 {
				lb = 21
			}
			floor := p.ConvexitySigmaFloor
			if floor <= 0 {
				floor = 0.05
			}
			sig := indicator.VolatilityN(klines, lb)
			if sig < floor {
				sig = floor
			}
			score /= sig
			if hasPrev && len(klines) > 1 {
				sigPrev := indicator.VolatilityN(klines[:len(klines)-1], lb)
				if sigPrev < floor {
					sigPrev = floor
				}
				prev /= sigPrev
			}
		}

		etf := types.ETF{
			Code: e.Code, Name: e.Name, Sector: e.Sector,
			Price:   klines[len(klines)-1].Close,
			History: klines,
		}

		c := RotationCandidate{
			ETF:        etf,
			Score:      score,
			Annualized: ann,
			R2:         r2,
			PrevScore:  prev,
			HasPrev:    hasPrev,
			Klines:     klines,
		}
		if score > maxScore {
			hasOverMax = true
		}
		candidates = append(candidates, c)
	}

	// 1) 区间过滤：只剔除 score < MinScore 的"垃圾"标的；
	//    对 score > MaxScore 的"过热"标的保留入候选池，由步骤 2 用日间增长门槛再做晋级判定。
	filtered := candidates[:0]
	for _, c := range candidates {
		if c.Score >= p.MinScore {
			filtered = append(filtered, c)
		}
	}

	// 2) 过热门槛（仅当池内存在 score > MaxScore 标的时启用）：
	//    - 对 Score > MaxScore 的标的，要求 Score >= PrevScore × 1.1 才放行（动量持续加速）；
	//    - 对 Score <= MaxScore 的标的，正常放行。
	if hasOverMax && p.ScoreThresholdMultiplier > 0 {
		again := filtered[:0]
		for _, c := range filtered {
			if c.Score > maxScore {
				if c.HasPrev && c.PrevScore > 0 && c.Score >= c.PrevScore*p.ScoreThresholdMultiplier {
					again = append(again, c)
				}
				continue
			}
			again = append(again, c)
		}
		// 极端情况下全部被过滤，则放宽回 filtered，保证 pipeline 不空跑
		if len(again) > 0 {
			filtered = again
		}
	}

	// 3) 按 score 降序
	sort.SliceStable(filtered, func(i, j int) bool {
		return filtered[i].Score > filtered[j].Score
	})

	// 4) TopN 截断 + 持仓豁免：
	//    - 默认行为：截到前 TopN 名；
	//    - 若 MustIncludeCodes 非空（advice 持仓注入），先记录每只持仓在 filtered 中的命中位置；
	//    - 若持仓本就在 TopN 内：不额外处理；
	//    - 若持仓在 TopN 外但通过了 MinScore + 过热闸门：按客观分数追加在结果末尾，
	//      不插队、不抬分、不绕过过滤；
	//    - 持仓未通过过滤（已被剔除）则忽略，避免污染候选。
	if p.TopN > 0 && len(filtered) > p.TopN {
		head := filtered[:p.TopN]
		tail := filtered[p.TopN:]
		if len(p.MustIncludeCodes) > 0 {
			must := make(map[string]struct{}, len(p.MustIncludeCodes))
			for _, c := range p.MustIncludeCodes {
				if c = strings.TrimSpace(c); c != "" {
					must[c] = struct{}{}
				}
			}
			// 已在 head 中的持仓不重复追加
			for _, c := range head {
				delete(must, c.ETF.Code)
			}
			// 在 tail 里按原始客观分数顺序挑出持仓追加
			for _, c := range tail {
				if _, ok := must[c.ETF.Code]; ok {
					head = append(head, c)
					delete(must, c.ETF.Code)
				}
			}
		}
		filtered = head
	}
	return filtered, nil
}

// BuildReason 返回轮动入选的人话理由，用于 ScoredETF.Reason 展示。
func (c RotationCandidate) BuildReason() string {
	parts := []string{}
	if c.Annualized > 0 {
		parts = append(parts, "年化动量正向")
	} else {
		parts = append(parts, "动量为负但满足过滤区间")
	}
	if c.R2 >= 0.7 {
		parts = append(parts, "趋势线性强（R²≥0.7）")
	} else if c.R2 >= 0.4 {
		parts = append(parts, "趋势线性中等")
	} else {
		parts = append(parts, "趋势线性弱")
	}
	if c.PrevScore > 0 && c.Score > c.PrevScore {
		parts = append(parts, "动量较前日继续走强")
	}
	return strings.Join(parts, "；")
}
