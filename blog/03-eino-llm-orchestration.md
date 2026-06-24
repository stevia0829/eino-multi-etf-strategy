# eino框架下的LLM编排实践 — 从Prompt Engineering到多模型降级

> 一个量化策略系统里，LLM到底该放在什么位置？

这是个好问题。在过去几个月里，我用Go语言从零搭建了一个多Agent量化策略系统，技术栈上选择了字节开源的[eino](https://github.com/cloudwego/eino)作为LLM编排框架。过程中最大的收获不是代码本身，而是对LLM在量化系统中定位的深度思考。这篇文章想和你分享这套系统的LLM编排实践——从Prompt Engineering的精细打磨，到多模型降级的健壮性设计，再到"规则兜底"这一核心理念。

---

## 1. LLM的"配角"定位

先说结论：**在本项目中，LLM不是决策者，是"信息分析师"。**

很多人一提到"AI+量化"，脑海里浮现的画面是：模型读完K线图，直接下单买入。现实远没有那么浪漫。量化的核心是数学——动量计算、权重评分、风险控制——这些事情LLM做不好，也不需要它来做。LLM真正擅长的是**阅读、摘要和上下文关联**，而这些恰恰是传统量化模型最薄弱的环节。

具体来说，在这个系统里，LLM的职责是：

- **阅读财经新闻标题**（Global Agent）：判断隔夜美股是涨是跌，亚太市场情绪如何，有没有地缘政治风险事件
- **解读跨市场联动**（Global Agent）：人民币汇率变动对港股通资金流向的影响、美债收益率上行对成长股估值的压制
- **识别技术形态**（Tech Agent）：均线排列、MACD金叉死叉、布林带收窄——这些是量化指标能算出来的，但LLM可以把它们翻译成人类可读的判断

而**决策权始终在动量模型手里**。weightedScore的计算完全基于数学指标：RSI、MACD、成交量变化率、价格动量等。LLM只输出一个`reasoning`字段，作为人类阅读的参考，不参与任何数值计算。

更重要的是，**规则兜底确保系统永远不会因为LLM不可用而失效**。这一点在第四节会详细展开。先记住这句话：LLM是增幅器，不是必需品。

---

## 2. Prompt Engineering实战

说Prompt是"玄学"的朋友，大概率没系统性地设计过生产级Prompt。在这个项目里，Prompt Engineering遵循三条铁律：

### 2.1 角色定义：五位委员的"人设"

系统采用多Agent架构，其中Final Agent负责综合各子Agent的分析，给出最终投资建议。它的System Prompt（`finalSystemPrompt`）定义了五位虚拟委员：

- **全球宏观委员**：解读跨境资金流动、货币政策预期、地缘政治风险
- **技术分析委员**：判断趋势强度、支撑阻力位、技术指标的共振情况
- **行业轮动委员**：分析板块强弱、资金在行业间的切换方向
- **风险控制委员**：评估仓位合理性、回撤风险、极端事件概率
- **综合委员（主席）**：汇总各方观点，形成最终投资建议

每个角色都有明确的视角和发言边界。比如技术分析委员不会评论美联储加息，全球宏观委员不会讨论MACD金叉。这种**边界清晰的角色分工**，是防止LLM胡言乱语的第一道防线。

### 2.2 JSON Schema约束：让LLM说人话

自由格式的输出是Prompt Engineering的最大敌人。LLM会不由自主地啰嗦、跑题、编造数据。解决方案是**严格的JSON Schema约束**：

```json
{
  "decision": "BUY | HOLD | SELL",
  "confidence": 0.0-1.0,
  "reasoning": {
    "overall": "整体逻辑，不超过140字",
    "key_risk": "关键风险，不超过100字",
    "action_points": "操作要点，不超过110字"
  }
}
```

三段式reasoning设计（整体逻辑 <= 140字 / 关键风险 <= 100字 / 操作要点 <= 110字）是反复调试的结果。太短信息量不足，太长LLM容易注入噪声。140/100/110的比例也经过权衡：整体逻辑需要最完整的篇幅来建立因果链；关键风险需要言简意赅地直击要害；操作要点居中，既要有指导性又不能太啰嗦。

### 2.3 禁止编造规则：把LLM的嘴堵上

这是最容易被忽视但最重要的一条。来看一个真实的反面案例：

> 某次测试中，LLM在没有数据支撑的情况下，输出"北向资金持续流出是日经ETF的主要风险点"。

这里面有两个问题：第一，日经ETF（跟踪日本股市）与北向资金（外资进出A股）根本不在同一个市场，逻辑上就不通；第二，系统当时根本没有日经ETF的数据源，LLM纯粹是在"脑补"。

解决方案是在Prompt中加入**禁止编造规则**（Anti-Hallucination Rules）：

- 不得提及系统中未接入的数据源（如北向资金、龙虎榜、两融余额等）
- 不得对未持有的标的发表具体观点
- 风险分析必须基于已获取的数据，不得凭空推测
- 如果信息不足以做出判断，明确说"信息不足"，而不是编造结论

这些规则看似简单，但在实践中效果显著。LLM的"一本正经地胡说八道"往往是因为没有被明确告知边界在哪里。设定好边界，它反而会老实很多。

---

## 3. 模型降级链：Resilient Client的三级防护

Prompt Engineering解决了"说得好"的问题，但"说得出来"的前提是LLM服务本身可用。生产环境中，API限流、服务降级、网络抖动都是家常便饭。对此，系统实现了一个**三级降级链**。

### 3.1 降级架构

```
Primary Model (DeepSeek-V3)
    |
    v (失败)
Fallback Models (依次尝试)
    - GPT-4o-mini
    - Qwen-Max
    |
    v (全部失败)
Static Fallback (确定性规则)
```

### 3.2 指数退避重试

在每一级模型调用内部，还实现了一个指数退避重试机制：

```go
func callLLMWithRetry(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
    baseDelay := 500 * time.Millisecond
    maxRetries := 2

    for attempt := 0; attempt <= maxRetries; attempt++ {
        resp, err := client.Chat(ctx, req)
        if err == nil {
            return resp, nil
        }
        if attempt < maxRetries {
            delay := baseDelay * (1 << attempt) // 500ms, 1s
            time.Sleep(delay)
        }
    }
    return nil, fmt.Errorf("all retries exhausted")
}
```

注意这里选择的参数：**baseDelay=500ms，maxRetries=2**。这不是随便拍的。baseDelay太短（比如100ms），对API限流恢复没有意义；太长（比如2s），会明显拉长调用链路。maxRetries=2意味着每个模型最多尝试3次，在可靠性边际效应递减之前止损。

### 3.3 Factory自动提升机制

当某个Fallback模型的失败率超过阈值（比如1分钟内失败3次），ResilientClient会自动将该模型标记为"不可用"并跳过。当Primary恢复后，又会自动提升回Primary。这个自动升降级逻辑全部封装在`ModelFactory`中，业务代码完全无感知。

这种设计的关键思路是：**降级不是惩罚，是容错**。不要因为偶发的网络波动就永久降级模型，给每个模型回归的机会。

---

## 4. 规则兜底的艺术

如果三级降级链全部失败怎么办？答案是：**走确定性规则路径，系统照常运行。**

这是整个LLM编排设计中最核心的理念。每个Agent都同时实现了LLM路径和规则路径：

### Global Agent
- **LLM路径**：读取新闻标题，输出全球市场环境判断
- **规则路径**（`ruleBasedGlobalFromQuotes`）：直接读取各市场指数的涨跌幅，用阈值规则判断市场情绪。比如：标普500涨跌幅绝对值 < 0.3% → 中性； > 1.5% → 明确偏多/偏空

### Final Agent
- **LLM路径**：综合各子Agent输出，生成带reasoning的投资建议
- **规则路径**（`RuleBasedDecision`）：`weightedScore > thresholdBuy` → BUY, `weightedScore < thresholdSell` → SELL, 否则 HOLD。不输出reasoning，只输出决策和置信度

### Tech Agent
- **LLM路径**：对技术指标做文字解读
- **规则路径**：**无LLM，只跑指标计算**。MACD、RSI、布林带这些本身就是数学公式，不需要LLM也能运行

这种设计的哲学可以总结为一句话：**LLM is an amplifier, not essential。**

把LLM想象成音响系统的功放——有它，声音更饱满、更动听；没它，信号照样能传过去，只是单调一些。量化的"信号"是数学指标和规则，LLM让它更易于人类理解和决策，但绝不依赖它来生成信号本身。

---

## 5. Cost与Latency优化

### 5.1 按需调用：只在advice模式调LLM

系统有两种运行模式：
- **回测模式（backtest）**：纯量化计算，零LLM调用，零成本
- **建议模式（advice）**：完整的Agent链路，调用LLM生成人类可读建议

这个区分带来的直接好处是：跑一次10年的历史回测只需要几十秒，完全不涉及API费用；日常运行的advice模式每天只在盘后调用一次，月均API开销可以控制在几十元以内。

### 5.2 Temperature=0.2：量化的第一要务是稳定

所有Agent的LLM调用都设置`temperature=0.2`。这个值的选择逻辑是：
- `0`：过于僵硬，输出千篇一律，反而可能丢失重要的上下文细微差异
- `0.5`以上：创造性太强，同一天问两遍可能给出不同结论，这在量化场景下是灾难
- `0.2`：在确定性和灵活性之间取得了最佳平衡，保证输出的一致性足以用于对比和回溯

### 5.3 callLLMJSON统一入口

所有Agent的LLM调用都通过`callLLMJSON`这个统一入口。它做了两件事：自动剥离markdown code fence（LLM经常在JSON外面包一层```` ```json ````），以及统一的错误处理和日志记录。

```go
func callLLMJSON[T any](ctx context.Context, client *ResilientClient, prompt string) (*T, error) {
    resp, err := client.Chat(ctx, ChatRequest{
        Messages: []Message{{Role: "user", Content: prompt}},
    })
    if err != nil {
        return nil, err
    }
    // 自动剥离 markdown code fence
    content := stripMarkdownFence(resp.Content)
    var result T
    if err := json.Unmarshal([]byte(content), &result); err != nil {
        return nil, fmt.Errorf("JSON parse failed: %w, raw: %s", err, content)
    }
    return &result, nil
}
```

这个薄薄一层的封装，省去了每个Agent里重复的JSON清洗代码，也方便集中监控LLM调用的成功率和延迟。

---

## 6. LLM在量化里的正确用法

回看整个系统的设计，可以提炼出一组清晰的原则：

**LLM擅长做的事情：**
- 阅读财经新闻，提取关键信息并建立因果关系
- 解读跨市场信号：美债收益率→科技股估值，人民币→中概股
- 用自然语言描述技术形态：不是"MACD=0.5"，而是"MACD金叉叠加成交量放大，短期动能偏强"
- 提供人类能理解的市场叙事，辅助投资决策

**LLM不擅长做的事情：**
- 精确计算回测指标（那是pandas/numpy/slice的事）
- 判断最佳买卖点（那是动量模型的事）
- 替代风险控制系统（那是VaR/最大回撤/夏普比率的事）

一个反面案例：在v2版本中，因为Tech Agent未能成功运行，`weightedScore`中混入了一个50分的兜底分数。这导致动量模型给出的评分偏离了真实状态，回测收益直接出现了严重偏差。如果当时LLM直接参与评分计算（而不是只输出reasoning），这种问题会更加隐蔽和致命。

**放在正确的位置：**
- 放在消息面解读上：它是增幅器，让量化模型"看懂"新闻
- 放在跨境联动分析上：它是连接器，把不同市场的信号串联成叙事
- 放在最终建议的可解释性上：它是翻译器，把数学决策翻译成人话

**放在错误的位置：**
- 放在评分计算里：它是噪声源，浮点数运算的不确定性会污染整个策略
- 放在实时决策里：延迟和可用性不可控，会破坏策略的确定性
- 放在风险控制里：rule-based的风控永远比LLM的风控更可靠

---

## 结语

这套系统的LLM编排实践，本质上是在回答一个问题：**在一个精确性至关重要的领域，如何安全地引入不确定性？**

答案是通过三层防护：Prompt Engineering确保LLM输出的质量可控；模型降级链确保服务的可用性；规则兜底确保任何情况下系统都不会停摆。

让AI做它擅长的事：阅读、总结、给出观点。让数学做它擅长的事：精确计算、回测、风险控制。两者各司其职，才是量化+LLM的正确打开方式。

---

*下一篇预告：AI Native开发体验——从Claw到SOLO，10天从零搭建量化交易系统的真实记录。*

*项目地址：https://github.com/1lann/go-quant*

> 本文始发于微信公众号「AI量化实践」，转载请联系作者。
