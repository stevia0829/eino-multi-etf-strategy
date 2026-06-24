# Fan-out & Synthesize——用 Go 实现一个「5 人投委会」多 Agent 架构

> 如果让达利欧、西蒙斯、巴菲特、利弗莫尔、索罗斯一起开晨会讨论今天的 ETF 操作，会是什么场面？

早上 8:30，5 位风格迥异的投资大师围坐在圆桌前。西蒙斯推过来一张表格：「21 日加权动量回归跑完了，Top3 是半导体、日经和通信。」达利欧看了一眼沪深 300：「等等，大盘 MA120 还没站上去，仓位上限 60%。」利弗莫尔翻开 K 线图：「日经 MACD 金叉第 3 天，量比 1.8，这是突破前高信号。」索罗斯若有所思：「但美联储昨晚偏鹰，小心反身性反转。」巴菲特喝着可乐：「溢价率 2.1% 了，别在贪婪时追高。」

这就是本文要拆解的系统——**用 Go 实现的 Multi-Agent ETF 决策引擎**。它不是一个让 AI 替你决策的黑箱，而是一个**让 AI 替你阅读海量信息，你只读最后的投委会纪要**的透明管道。

---

## 一、Fan-out 架构全景

整个系统的工作流由 `orchestrator/pipeline.go` 的 `Pipeline.Run()` 编排，分为三个核心阶段：

```
Stage 1 (Serial)    Stage 2 (Fan-out)          Stage 3 (Fan-in)
                                    ┌─ NewsAgent (Top5 批量) ─┐
                                    ├─ GlobalMarketAgent ─────┤
ScreenerAgent ──────→              ├─ TechnicalAgent (Top5) ──┤──→ FinalAgent
 (规则, 无 LLM)                      ├─ RegimeAgent ───────────┤    (LLM + 规则)
                                    ├─ MoneyFlowAgent ────────┤
                                    └─ MemoryAgent ───────────┘
```

**Stage 1** 是串行的——必须先跑完 Screener 确定 Top5 候选，后面的 Agent 才知道分析什么。

**Stage 2** 是 pure fan-out——6 路 Agent 完全并行执行，互不依赖。关键代码：

```go
// orchestrator/pipeline.go:92-215
var wg sync.WaitGroup

// News: Top1 + Top5 剩余 N-1 扇出
wg.Add(1)
go func() {
    defer wg.Done()
    n, _ := p.News.Run(ctx, target)
    state.News = n
    // Top5 其余并发抓新闻...
}()

// Global
wg.Add(1)
go func() {
    defer wg.Done()
    g, _ := p.Global.Run(ctx, target)
    state.Global = g
}()

// Tech: Top1 + Top5 剩余 N-1 扇出
wg.Add(1)
go func() {
    defer wg.Done()
    t, _ := p.Tech.Run(ctx, target)
    state.Tech = t
    // Top5 其余并发做技术面分析...
}()

// Regime
wg.Add(1)
go func() {
    defer wg.Done()
    r, _ := p.Regime.Run(ctx)
    state.Regime = r
}()

// MoneyFlow
wg.Add(1)
go func() {
    defer wg.Done()
    m, _ := p.MoneyFlow.Run(ctx, target)
    state.MoneyFlow = m
}()

wg.Wait()
```

注意 News 和 Tech 内部还有**第二层 fan-out**：对 Top5 剩余的 4 只 ETF，每只独立拉取新闻搜索和技术面分析，进一步并发。算下来，6 个 Agent 的高层扇出 + 内部 2×(N-1) 的子扇出，在 Stage 2 中最多有约 **15 个 goroutine 同时跑**。在 Go 的运行时下，这完全没有性能压力——goroutine 的调度开销只有几 KB。

**Stage 3** 是 FinalAgent 的 fan-in：汇聚所有 Agent 的输出 + 板块自适应权重 + 跨标相对校准 + 长期记忆，输出结构化 JSON 决策。

---

## 二、5 位委员的角色与权重

FinalAgent 的 system prompt（`agent/final.go:25-184`）可能是这个项目里最长的函数——超过 160 行，是一份完整的「投委会章程」。它明确托管了 5 种交易哲学：

### 2.1 詹姆斯·西蒙斯——量化派因子工程师

- **对应因子**：Quant（量化动量），权重依板块 20%~30%
- **判据**：「严格按系统下发的 weights 加权；不让故事凌驾于数字之上」
- **信息来源**：Screener 输出的 Top5 候选列表，每只含完整指标（MA5/MA20/MA60/RSI/MACD/动量/波动率/量比/策略3量化分/年化/R²）
- **关键纪律**：识别「哪些因子对该板块本来就不相关」并在 reasoning 中明示降权理由

西蒙斯派的核心不是「信 AI」，而是「信因子」。他的贡献是确保整个决策的底座始终锚定在可重复的量化分上，不为一条新闻标题改变排名。

### 2.2 瑞·达利欧——全天候宏观派

- **对应因子**：Regime（宏观环境），权重 5%~30%（债券 ETF 高达 30%）
- **判据**：「risk_off 必须空仓；不同板块对应不同经济周期象限」
- **信息来源**：RegimeAgent 输出的沪深 300 趋势 + 回撤 + 仓位上限
- **一票否决权**：当 `regime.trend == "risk_off"` 时，无论量化分多高，强制 `avoid`

```go
// agent/regime.go:148-163
func CapRecommendation(reco string, regime *types.RegimeAnalysis) string {
    switch regime.Trend {
    case "risk_off":
        return "avoid"               // 一票否决
    case "bear":
        switch reco {
        case "strong_buy", "buy":
            return "hold"             // bear 降一档
        }
    }
    return reco
}
```

达利欧派的存在保证了策略在系统风险面前不会「技术面看多就算满仓冲」——2026 年 4 月关税冲击期间，这条硬约束让策略躲过了沪深 300 的 8%+ 回撤。

### 2.3 杰西·利弗莫尔——趋势突破派

- **对应因子**：Tech（技术面），权重 20%~30%（海外 ETF 高达 30%）
- **判据**：「只在多头排列 + 突破前高时进攻；止损必须明确且不可移动」
- **信息来源**：TechnicalAgent 对 Top5 的逐只技术分析（趋势/MA/MACD/RSI/支撑压力/建议持有区间）
- **纪律**：给出明确的 entry_price / stop_loss / take_profit，止损设在 MA20/MA60 最近支撑下方 1%

利弗莫尔派不是看一两根 K 线就决定买卖，而是要求在 FinalAgent 的 reasoning 中显式说明「目标 ETF 的 MA 排列状态 vs 其他 Top5 候选的 MA 排列状态」——跨标对比是防止「单标自我麻醉」的最好方法。

### 2.4 乔治·索罗斯——反身性宏观派

- **对应因子**：Global + Flow（海外联动 + 资金面），权重合计 10%~30%
- **判据**：「趋势自我强化时加仓，反身性反转时果断撤退」
- **信息来源**：GlobalMarketAgent（美股前夜 + 日韩盘中） + MoneyFlowAgent（北向资金 + ETF 申赎）
- **独特视角**：不只关心「现在是涨还是跌」，更关心「市场的认知与基本面是否出现背离」

索罗斯派在 FinalAgent 的 prompt 里被赋予了「反身性提示」义务——如果 News 显示极度乐观但 Flow 显示资金在撤，就要标注「情绪与资金面背离，需警惕反转」。

### 2.5 沃伦·巴菲特——价值安全边际派

- **对应因子**：News（消息面），权重 5%~15%
- **判据**：「premium_pct ≥ +1.5% 视为追高警告；宁可错过也绝不在恐慌反弹中梭哈」
- **信息来源**：NewsAgent 对 Top5 的逐只新闻情绪分析
- **否决权**：溢价率 ≥ +3% 时，`strong_buy` / `buy` 强制降为 `hold`

```go
// agent/regime.go:174-187
func CapByPremium(reco string, premiumPct float64) (string, string) {
    if premiumPct >= 0.03 {
        return "hold", "溢价率 +3% 以上，追高风险显著，强制降档"
    }
    if premiumPct >= 0.015 && reco == "strong_buy" {
        return "buy", "溢价偏高，由 strong_buy 降为 buy"
    }
    return reco, ""
}
```

巴菲特的角色在系统中是「反向安全阀」——当其他 4 位委员一致看多时，他站出来问一句：**这个价格，你真的愿意付吗？**

---

## 三、板块自适应权重——不是所有 ETF 都该问同样的问题

如果对日经 ETF 也用「北向资金」打分，就像问一个东京居民「沪深 300 跌了你慌不慌」——问题本身错了。

`agent/factor_weights.go` 定义了 18 个板块的自适应权重表，核心思想是：**不同板块的驱动因子不同，问对问题比问多问题重要**。关键板块的权重设计及理由：

| 板块 | Quant | Tech | News | Global | Regime | Flow | 设计理由 |
|------|-------|------|------|--------|--------|------|----------|
| **海外** (纳指/日经/德国30) | 0.30 | 0.30 | 0.05 | **0.25** | 0.05 | 0.05 | 标的在境外交易，A 股资金面几乎无效；海外指数走势 + 场内技术面是唯一真实信号。News 压到 0.05 因为「日经 ETF 的新闻」99% 是日股新闻的中文翻译，与 ETF 价格无增量因果 |
| **港股** | 0.30 | 0.25 | 0.05 | **0.20** | 0.10 | 0.10 | 南向资金 + 港股流动性 + 美股传导；Global 提到 0.20 反映港美双重映射，Flow 保留 0.10 用于南向资金代理 |
| **科技/新能源** | 0.30 | 0.25 | 0.10 | 0.10 | 0.10 | 0.15 | A 股成长板块：北向 + 产业政策 + 宏观风险偏好共同作用；Flow 提到 0.15 反映北向资金对成长板块的定价权 |
| **军工** | 0.30 | 0.25 | **0.15** | 0.05 | 0.10 | 0.15 | 事件驱动型：地缘冲突、军费预算等消息面权重明显高于其他 A 股板块；Global 压到 0.05（军工与美股军工联动弱） |
| **消费/金融** | 0.30 | 0.25 | 0.10 | 0.05 | **0.15** | 0.15 | 顺周期：宏观经济（CPI/PMI）对消费影响大，Regime 提到 0.15 |
| **地产** | 0.30 | 0.20 | 0.15 | 0.05 | **0.20** | 0.10 | 政策强驱动：Regime 提到 0.20 反映「政策底 → 市场底」的宏观传导，Tech 压到 0.20（地产 ETF 的趋势信号常滞后于政策信号） |
| **贵金属** | 0.30 | 0.25 | 0.05 | **0.20** | 0.05 | 0.15 | 与美元/美债/海外避险情绪强相关；Global 提到 0.20（COMEX 金价是唯一真实 beta），Regime 压到 0.05（沪深 300 与金价无因果） |
| **债券** | 0.20 | 0.20 | 0.05 | 0.10 | **0.30** | 0.15 | 利率资产：宏观利率/流动性是唯一主要驱动；Quant 和 Tech 各压 0.20（债券动量的信噪比远低于股票），Regime 提到 0.30 独占鳌头 |
| **宽基** | 0.30 | 0.25 | 0.05 | 0.10 | **0.20** | 0.10 | 顺周期/大盘：宏观环境权重抬高，Flow 压到 0.10（宽基的北向信号粒度太粗） |
| **公用** | 0.30 | 0.25 | 0.05 | 0.05 | **0.20** | 0.15 | 防御型：受利率和宏观情绪驱动，News/Global 弱相关 |

对应的因子相关性提示文本（`FactorRelevanceNote`）会注入到 FinalAgent 的 system prompt，让 LLM 在分析每个标的前先确认「我该看哪些因子」：

```go
// agent/factor_weights.go:73-89
case "海外":
    return "标的为海外指数 ETF：海外联动(Global)是核心驱动；" +
           "北向资金/沪深300 宏观对其影响极弱，需大幅降权；" +
           "技术面(Tech)反映 ETF 自身溢价/流动性，仍重要。"
case "债券":
    return "利率类资产：宏观环境(利率/流动性)是主要驱动；" +
           "动量与技术面在债券上敏感度低。"
```

这个设计不是拍脑袋的——它是基于「因子本身的物理含义」而非「主观偏好」来定权的。海外 ETF 的 Global 权重 0.25 不是「我觉得海外重要」，而是「海外股指是该 ETF 的唯一真实 beta」。

---

## 四、跨标相对校准——西蒙斯派的因子相对强度

孤立看「新闻情绪 65 分」没有意义。如果同类 ETF 的平均新闻情绪是 40 分，65 分就是强信号；如果平均是 80 分，65 分反而是弱信号。

这就是 `agent/final.go:379-456` 中 peer 校准的逻辑：

```go
// agent/final.go:381-387
nAdj, tAdj := 1.0, 1.0
if peerNewsAvg, ok := peerAvgNews(st.NewsList, bestCode); ok {
    nAdj = relativeAdjust(n, peerNewsAvg)
}
if peerTechAvg, ok := peerAvgTech(st.TechList, bestCode); ok {
    tAdj = relativeAdjust(t, peerTechAvg)
}
```

`peerAvgNews` 计算 Top5 中除 best 外所有 ETF 的新闻 Score 均值。`relativeAdjust` 根据 best 相对 peers 的偏离计算调权系数：

```go
// agent/final.go:449-456
func relativeAdjust(bestScore, peerAvg float64) float64 {
    if peerAvg <= 0 {
        return 1.0
    }
    diff := (bestScore - peerAvg) / peerAvg  // 相对偏离
    factor := 1.0 + clamp(diff*0.5, -0.10, 0.10)
    return factor
}
```

调权范围限制在 ±10%——不会让一个因子因为 peer 比较彻底翻盘，但足以体现「相对优势」。逻辑是：

- best 新闻分显著高于 peers（高出 15%+） → 调权 × 1.10（这个标的的新闻面确实值得多看）
- best 显著低于 peers（低出 15%+） → 调权 × 0.90（新闻面不是它的竞争优势）
- 无显著差异 → 不调整

这与西蒙斯的「因子相对强度」思想一致：**排名不取决于绝对分数，而取决于你在同类中的分位**。

---

## 五、反身性风控链——5 道硬约束

FinalAgent 跑完 LLM 之后，不会直接把 LLM 的输出写入 `FinalDecision`。它先经过 5 道规则风控的层层校验（`agent/final.go:239-305`）：

### 第 1 道：risk_off 一票否决

```go
// agent/final.go:256
dec.Recommendation = CapRecommendation(dec.Recommendation, st.Regime)
```

如果沪深 300 跌破 MA120 且 60 日回撤 ≥ 8%，任何 `strong_buy` / `buy` / `hold` 都强制降为 `avoid`。**不依赖 LLM，规则直接拦截**。

### 第 2 道：bear 降档

如果 Regime 判定为 `bear`（中期空头），`strong_buy` 和 `buy` 都降为 `hold`。bear 环境下还允许 `hold`——因为经历过「大跌后反弹 20%」的人都知道，完全空仓会踏空 V 型反转。

### 第 3 道：溢价 ≥ 3% 强制 hold

溢价率 = (市价 - IOPV) / IOPV。当市场追涨热情把 ETF 价格推高到 IOPV 之上 3% 时，回归净值的下行风险已经显著大于继续上涨的空间。此时 `strong_buy` 和 `buy` 一律降为 `hold`。

溢价在 1.5%~3% 之间时，仅 `strong_buy` 降为 `buy`——机制更温和，保留入场可能但强调谨慎。

### 第 4 道：回撤冷却

```go
// agent/final.go:273-279
if capped, note := CapByPullbackCooldownForState(dec.Recommendation, target, st); note != "" {
    dec.Recommendation = capped
    dec.Reasoning += "【回撤冷却】" + note + "。"
}
```

近 5 日高点回撤 ≥ 5%，且价格尚未收复 MA5 或最近阴线半分位时，标记「回撤冷却」提示。如果当前已持有该标的，不降档（避免频繁换手），仅在 reasoning 中注入风险提示。如果未持仓（新开仓场景），降为 `hold`。

### 第 5 道：盈亏比下限 1.4

```go
// agent/final.go:280-285
if _, note := CapByRiskReward(dec.Recommendation, dec.EntryPrice, dec.StopLoss, dec.TakeProfit); note != "" {
    dec.Reasoning += "【盈亏比提示】" + note + "。"
}
```

入场止损止盈计划的有效盈亏比必须 ≥ 1.4——意味着每承担 1 元风险，预期收益至少 1.4 元。如果不满足，LLM 的建议不会被降档（不干预主信号），但 reasoning 中会追加明确的盈亏比风险提示，由 9:20 的集合竞价复核 Agent 决定是否拦截追入。

---

## 六、规则兜底——如果 LLM 挂了怎么办？

整个系统有 6 个 Agent 依赖 LLM（News / Global / Tech / Final / Memory / PreOpen）。如果 DeepSeek 挂了，整个 advice 流程就不能出了吗？

这正是 `llm/resilient.go` 存在的理由——**Resilient Client 提供了「重试 + 多级 provider 降级 + 静态兜底」三层保护**：

```
DeepSeek (主)
  ↓ 失败 (重试 2 次，指数退避 500ms → 1s)
MoonShot (降级 #1)
  ↓ 失败
Doubao (降级 #2)
  ↓ 失败
Qwen (降级 #3)
  ↓ 全部失败
Static Fallback → 返回 "{}"
```

```go
// llm/resilient.go:70-95
func (r *Resilient) Chat(ctx context.Context, system, user string, opts ...ChatOptions) (string, error) {
    clients := append([]Client{r.primary}, r.fallbacks...)
    for i, c := range clients {
        out, err := r.callWithRetry(ctx, c, system, user, opts...)
        if err == nil {
            return out, nil
        }
        r.logf("[llm] provider %s failed: %v", c.Name(), err)
    }
    if r.static != nil {
        r.logf("[llm] all providers failed, use static fallback")
        return r.static(system, user), nil  // 返回 "{}"
    }
    return "", fmt.Errorf("all providers failed")
}
```

当 Static Fallback 返回 `"{}"` 时，FinalAgent 的 `callLLMJSON` 会失败，触发 `ruleBasedFinal()`：

```go
// agent/final.go:239-247
err := callLLMJSON(ctx, a.LLM, finalSystemPrompt, user, dec, func(raw string) {
    if dec.Reasoning == "" {
        dec.Reasoning = raw
    }
})
if err != nil {
    ruleBasedFinal(dec, st)
    return dec, nil
}
```

**规则版决策不需要 LLM**——它纯粹基于：
- Screener 的量化分（21 日加权动量）
- Regime 的趋势判定（510300 K 线规则推导）
- 板块自适应权重
- 5 道风控链

它在 reasoning 字段中输出类似这样的结构化摘要：

```
规则版决策（板块=科技 自适应权重 Q30%/T25%/N10%/G10%/R10%/F15%）：
量化73.5 / 技术65.0 / 消息55.0 / 海外60.0 / 宏观75.0 / 资金58.0 → 综合 68.3，建议 buy。
仓位上限 85%。【因子相关性】成长性板块：北向资金+产业政策(News)+宏观风险偏好共同作用；
技术面与量化动量为核心。
```

同样，NewsAgent 挂了也不影响主流程——News 的 `scoreOr` 函数在指针为 nil 时返回默认值 50，整体加权分只少了 5%~15% 的信息量。

> **设计哲学**：LLM 是「增幅器」不是「必需品」。它能提升推理质量和人话可读性，但从不应成为单点故障。

---

## 这条管道不是在「替你决策」

回到开头的隐喻：5 位委员开完晨会后，**不是给你一个「买」或「不买」的指令**，而是给你一份 **5 派共识纪要**：

- 西蒙斯说：量化模型显示日经 ETF 动量分 0.85，排名第 1
- 达利欧说：沪深 300 在 bull 区，仓位可以给到 100%
- 利弗莫尔说：日经 MACD 金叉第 3 天，量比 1.8，多头排列
- 索罗斯说：但美联储偏鹰，注意情绪资金背离
- 巴菲特说：溢价 2.1%，别追高

然后轮到你——这位真正的「投资委员会主席」——做自己的判断。

**这个架构的精髓不是「让 AI 替你决策」，而是「让 AI 替你阅读海量信息，你只读最后的投委会纪要」。**

完整代码开源在 [GitHub](https://github.com)。如果你也在探索 Multi-Agent 架构在金融领域的应用，欢迎 Star 和交流。

---

*上一篇回顾：《ETF 轮动策略的量化核心——从聚宽公式到 Go 语言复刻》已发布，覆盖 21 日加权动量回归的逐行拆解、72 只 ETF 池设计、归一化的两条路径陷阱。*
