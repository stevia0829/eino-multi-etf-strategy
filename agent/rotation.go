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
}

func DefaultRotationParams() RotationParams {
	return RotationParams{
		MDays:                    21,
		MaxScore:                 6,
		MinScore:                 0, // 对齐聚宽 g.min_score=0：负分动量直接出局
		ScoreThresholdMultiplier: 1.1,
		TopN:                     5,
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

// BuildHoldAdvice 把"用户当前持仓"与本次 Top5 排名做对照，返回持仓建议。
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

	for _, e := range Strategy3Pool {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		// 拉取 m_days+1 长度，便于一次性算 T 日和 T-1 日 score
		need := p.MDays + 1
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
		if score > p.MaxScore {
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
			if c.Score > p.MaxScore {
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

	if p.TopN > 0 && len(filtered) > p.TopN {
		filtered = filtered[:p.TopN]
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
