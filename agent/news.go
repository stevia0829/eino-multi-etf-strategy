package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/eino-multi-etf-strategy/datasource"
	"github.com/eino-multi-etf-strategy/llm"
	"github.com/eino-multi-etf-strategy/types"
)

// NewsAgent 板块消息面研判 Agent。
//
// 设计参考 ValueCell 项目（github.com/ValueCell-ai/valuecell）的 news_agent：
//   - 工具前置：先用 NewsFetcher 真实抓取板块/标的的最新新闻（EastMoney 站内搜索）；
//   - LLM 仅做摘要：把抓到的真实新闻列表塞进 user prompt，让 LLM 提炼情绪/催化剂/风险，
//     而非凭空捏造；
//   - 强约束输出：禁止编造没有出现在抓取结果中的事实；
//   - 兜底真实化：LLM 不可达时基于真实新闻标题做规则推断，不再"按板块名瞎猜"。
type NewsAgent struct {
	LLM     llm.Client
	Fetcher *datasource.NewsFetcher
	// MaxItems 单次抓取的最大新闻条数，默认 8。
	MaxItems int
}

func NewNewsAgent(c llm.Client) *NewsAgent {
	return &NewsAgent{LLM: c, Fetcher: datasource.NewNewsFetcher(), MaxItems: 8}
}

const newsSystemPrompt = `你是一名资深 A 股板块研究员。你拿到的每一条"真实新闻"都来自系统已抓取的最新公开资讯，禁止编造未列出的公司名称、数字、政策。

工作流：
1) 阅读"真实新闻列表"中所有标题与摘要；
2) 抽取真实出现的：催化剂(catalysts) / 风险点(risks) / 资金面信号(flows)；
3) 综合给出 sentiment / score / 200 字 summary（必须基于真实新闻原文）；
4) highlight 数组的每一条都必须能在新闻列表中找到出处，不可虚构。

约束：
- 严禁出现"据悉/据传/某券商认为/可能/预计"等无来源虚词；
- 缺少新闻时，必须在 summary 中明示"近期公开资讯有限"，不得编造；
- 仅输出严格 JSON，禁止 markdown，禁止解释。

JSON Schema:
{
  "sector": "板块中文名",
  "sentiment": "positive | neutral | negative",
  "score": 0-100,
  "highlight": ["要点1","要点2"]   // 每条 < 30 字，必须基于真实新闻原文
  "summary": "<=200 字，包含催化剂 / 风险 / 资金面三段式，全部基于真实新闻"
}`

// Run 流程：
//  1. 多关键词抓真实新闻（板块 + ETF 名）；
//  2. 把新闻列表塞 prompt → LLM 摘要；
//  3. LLM 失败 → 用真实新闻列表 + 板块情绪规则给出兜底。
func (a *NewsAgent) Run(ctx context.Context, etf types.ScoredETF) (*types.NewsAnalysis, error) {
	keywords := buildNewsKeywords(etf)
	limit := a.MaxItems
	if limit <= 0 {
		limit = 8
	}
	news := a.Fetcher.FetchMulti(keywords, limit)

	user := fmt.Sprintf(
		"目标 ETF: %s(%s)\n板块: %s\n当前价: %.3f\n近 20 日动量: %.2f%%\n\n[真实新闻列表（按时间倒序，共 %d 条）]\n%s\n\n请基于以上真实新闻输出板块情绪研判（按 JSON Schema）。",
		etf.ETF.Name, etf.ETF.Code, etf.ETF.Sector,
		etf.ETF.Price, etf.Indicators["Momentum20"]*100,
		len(news), formatNewsList(news),
	)

	res := &types.NewsAnalysis{Sector: etf.ETF.Sector}
	err := callLLMJSON(ctx, a.LLM, newsSystemPrompt, user, res, func(raw string) {
		if res.Summary == "" {
			res.Summary = raw
		}
	})
	if err != nil || res.Sentiment == "" {
		return ruleBasedNewsFromItems(etf, news), nil
	}
	if res.Score == 0 {
		res.Score = mapSentimentScore(res.Sentiment)
	}
	if res.Sector == "" {
		res.Sector = etf.ETF.Sector
	}
	return res, nil
}

// buildNewsKeywords 多关键词扇出：
//  1. ETF 自身：代码 / 全名 / 去后缀简称；
//  2. ETF 级行业词：优先用精确标的词，避免"能源"这类泛词把新能源/个股处罚新闻混进煤炭；
//  3. 板块同义词：仅在没有更精确 ETF 词，或板块本身足够窄时补充。
func buildNewsKeywords(etf types.ScoredETF) []string {
	seen := map[string]struct{}{}
	out := []string{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}

	add(etf.ETF.Code)
	add(etf.ETF.Name)
	name := stripFundSuffix(etf.ETF.Name)
	add(name)

	extra := ETFNewsKeywords(etf.ETF.Code)
	for _, k := range extra {
		add(k)
	}

	skipBroad := skipBroadSectorKeyword(etf.ETF.Sector, extra)
	if !skipBroad {
		add(etf.ETF.Sector)
	}

	// 板块同义词扩展
	if !skipBroad {
		switch etf.ETF.Sector {
		case "科技":
			add("半导体")
			add("芯片")
		case "医药":
			add("生物医药")
		case "新能源":
			add("光伏")
			add("锂电池")
		case "金融":
			add("券商")
			add("银行")
		case "消费":
			add("白酒")
		}
	}
	return out
}

func stripFundSuffix(name string) string {
	for _, suffix := range []string{"ETF华夏", "ETF招商", "ETF平安", "ETF天弘", "ETF易方达", "ETF国泰", "ETF沪", "ETF", "etf", "LOF"} {
		name = strings.TrimSuffix(name, suffix)
	}
	return strings.TrimSpace(name)
}

func skipBroadSectorKeyword(sector string, extra []string) bool {
	if len(extra) == 0 {
		return false
	}
	switch sector {
	case "能源", "科技", "消费", "材料", "宽基":
		return true
	default:
		return false
	}
}

// ETFNewsKeywords 返回 ETF 级精确新闻关键词。
// 放在代码中集中维护，便于 Strategy3Pool 新增标的时同步补全行业 / 指数 / 商品别名。
func ETFNewsKeywords(code string) []string {
	return etfNewsKeywordOverrides[code]
}

var etfNewsKeywordOverrides = map[string][]string{
	"518880": {"黄金", "金价", "COMEX黄金", "贵金属"},
	"161226": {"白银", "银价", "COMEX白银", "贵金属"},
	"501018": {"原油", "油价", "布伦特原油", "WTI原油", "OPEC"},
	"159985": {"豆粕", "大豆", "农产品期货", "饲料"},
	"515220": {"煤炭ETF", "中证煤炭", "煤炭", "煤价", "动力煤", "焦煤", "秦皇岛港煤价", "迎峰度夏", "能源保供"},
	"159611": {"电力", "电网", "火电", "绿电", "用电负荷"},
	"513520": {"日经225", "日本股市", "日股", "日元"},
	"513880": {"日经225", "日本股市", "日股", "日元"},
	"513100": {"纳斯达克100", "纳指", "美股科技", "美债收益率"},
	"513300": {"纳斯达克", "纳指", "美股科技", "美债收益率"},
	"159509": {"纳指科技", "美股科技", "人工智能", "半导体"},
	"513400": {"道琼斯", "美股蓝筹", "美股"},
	"513030": {"德国DAX", "德国股市", "欧洲股市"},
	"159529": {"标普消费", "美股消费", "美国消费"},
	"159329": {"沙特", "沙特股市", "中东市场", "油价"},
	"513130": {"恒生科技", "港股科技", "互联网平台", "南向资金"},
	"513090": {"香港证券", "港股券商", "香港交易所", "南向资金"},
	"513120": {"港股创新药", "创新药", "CXO", "生物医药", "南向资金"},
	"159792": {"港股通互联网", "港股互联网", "互联网平台", "南向资金"},
	"159892": {"恒生医药", "港股医药", "创新药", "生物医药"},
	"512480": {"半导体", "芯片", "晶圆", "AI芯片", "国产替代"},
	"512760": {"芯片", "半导体", "晶圆", "AI芯片", "国产替代"},
	"588200": {"科创芯片", "芯片", "半导体", "科创板"},
	"515880": {"通信", "5G", "算力", "光模块", "CPO", "数据中心"},
	"515050": {"通信", "5G", "算力", "光模块", "CPO", "数据中心"},
	"515000": {"科技", "AI", "半导体", "算力", "软件"},
	"515230": {"软件", "信创", "AI应用", "国产软件"},
	"159852": {"软件", "信创", "AI应用", "国产软件"},
	"159890": {"云计算", "算力", "数据中心", "AI服务器"},
	"562500": {"机器人", "人形机器人", "工业机器人", "自动化"},
	"159770": {"机器人", "人形机器人", "工业机器人", "自动化"},
	"159819": {"人工智能", "AI", "算力", "大模型"},
	"159363": {"人工智能", "AI", "创业板人工智能", "大模型"},
	"159732": {"消费电子", "苹果产业链", "AI手机", "MR"},
	"515260": {"电子", "消费电子", "PCB", "半导体"},
	"516160": {"新能源", "光伏", "风电", "储能"},
	"515790": {"光伏", "硅料", "组件", "逆变器"},
	"159755": {"电池", "锂电池", "储能", "动力电池"},
	"515700": {"新能源车", "新能源汽车", "智能驾驶", "动力电池"},
	"515030": {"新能源车", "新能源汽车", "智能驾驶", "动力电池"},
	"512660": {"军工", "国防军工", "航空发动机", "低空经济"},
	"159206": {"卫星", "商业航天", "低轨卫星", "军工"},
	"159218": {"卫星", "商业航天", "低轨卫星", "军工"},
	"159227": {"航空航天", "商业航天", "航空发动机", "军工"},
	"159378": {"通用航空", "低空经济", "航空航天", "军工"},
	"159992": {"创新药", "医药", "CXO", "生物医药"},
	"512170": {"医疗", "医疗器械", "医疗服务", "医保"},
	"512010": {"医药", "医疗", "创新药", "医保"},
	"159928": {"消费", "食品饮料", "白酒", "零售"},
	"512690": {"白酒", "酒企", "食品饮料", "消费"},
	"515170": {"食品饮料", "白酒", "乳制品", "消费"},
	"515650": {"消费50", "消费", "食品饮料", "白酒"},
	"159996": {"家电", "以旧换新", "白电", "消费"},
	"159766": {"旅游", "酒店", "出行", "消费"},
	"159565": {"汽车零部件", "汽车", "智能驾驶", "机器人"},
	"159851": {"金融科技", "互联网金融", "券商IT", "支付"},
	"512000": {"券商", "证券", "资本市场", "成交额"},
	"512800": {"银行", "息差", "红利", "金融"},
	"159869": {"游戏", "传媒", "版号", "AI应用"},
	"512980": {"传媒", "游戏", "影视", "AI应用"},
	"516780": {"稀土", "稀土永磁", "小金属", "材料"},
	"562800": {"稀有金属", "锂", "钴", "小金属"},
	"560860": {"工业有色", "铜", "铝", "有色金属"},
	"588160": {"新材料", "科创新材料", "化工材料", "半导体材料"},
	"510050": {"上证50", "大盘蓝筹", "宽基"},
	"510300": {"沪深300", "大盘蓝筹", "宽基"},
	"159922": {"中证500", "中盘股", "宽基"},
	"159531": {"中证2000", "小微盘", "宽基"},
	"159915": {"创业板", "成长股", "新能源", "医药"},
	"159949": {"创业板50", "成长股", "创业板"},
	"588080": {"科创50", "科创板", "硬科技"},
	"588000": {"科创50", "科创板", "硬科技"},
	"588380": {"科创创业", "科创板", "创业板", "成长股"},
	"560610": {"中证A500", "A500", "宽基"},
	"159338": {"中证A500", "A500", "宽基"},
	"510880": {"红利", "高股息", "央企红利", "低波"},
	"511090": {"30年国债", "长债", "利率债", "债市"},
}

func formatNewsList(items []datasource.NewsItem) string {
	if len(items) == 0 {
		return `（接口未返回新闻；请在 summary 中明示"近期公开资讯有限"。）`
	}
	var b strings.Builder
	for i, it := range items {
		title := it.Title
		if len(title) > 60 {
			title = title[:60] + "…"
		}
		content := strings.ReplaceAll(it.Content, "\n", " ")
		if len(content) > 120 {
			content = content[:120] + "…"
		}
		fmt.Fprintf(&b, "%d. [%s | %s] %s\n   摘要: %s\n", i+1, it.Date, it.Source, title, content)
	}
	return b.String()
}

// ruleBasedNewsFromItems 在 LLM 失败时，用真实新闻标题做朴素情绪打分。
//
// 规则：
//   - 含"涨/上涨/创新高/突破/利好/政策/扶持/订单/中标" → +1
//   - 含"跌/下跌/亏损/利空/退市/调查/处罚/警示/风险" → -1
//   - 综合得分映射到 sentiment/score
//   - highlight 取最多 3 条真实标题
func ruleBasedNewsFromItems(etf types.ScoredETF, items []datasource.NewsItem) *types.NewsAnalysis {
	if len(items) == 0 {
		return &types.NewsAnalysis{
			Sector: etf.ETF.Sector, Sentiment: "neutral", Score: 50,
			Highlight: []string{"近期公开资讯有限，建议人工复核"},
			Summary:   fmt.Sprintf("板块 %s 近期未抓取到公开新闻，按中性处理。", etf.ETF.Sector),
		}
	}
	pos := []string{"涨", "上涨", "创新高", "突破", "利好", "政策", "扶持", "订单", "中标", "增长", "扩产", "回暖", "提速"}
	neg := []string{"跌", "下跌", "亏损", "利空", "退市", "调查", "处罚", "警示", "风险", "下滑", "裁员", "减持", "暴跌"}
	score := 0
	for _, it := range items {
		text := it.Title + it.Content
		for _, w := range pos {
			if strings.Contains(text, w) {
				score++
				break
			}
		}
		for _, w := range neg {
			if strings.Contains(text, w) {
				score--
				break
			}
		}
	}
	sentiment := "neutral"
	scoreF := 50.0
	switch {
	case score >= 2:
		sentiment, scoreF = "positive", clamp01_100(55+float64(score)*5)
	case score <= -2:
		sentiment, scoreF = "negative", clamp01_100(45+float64(score)*5)
	}
	highlight := make([]string, 0, 3)
	for i := 0; i < len(items) && i < 3; i++ {
		t := items[i].Title
		if len(t) > 28 {
			t = t[:28]
		}
		highlight = append(highlight, t)
	}
	return &types.NewsAnalysis{
		Sector:    etf.ETF.Sector,
		Sentiment: sentiment,
		Score:     scoreF,
		Highlight: highlight,
		Summary: fmt.Sprintf(
			"LLM 不可达，基于 %d 条真实新闻标题做规则推断：净情绪得分 %d → %s。Top 标题：%s。",
			len(items), score, sentiment, strings.Join(highlight, " / "),
		),
	}
}

func mapSentimentScore(s string) float64 {
	switch s {
	case "positive":
		return 70
	case "negative":
		return 35
	default:
		return 50
	}
}
