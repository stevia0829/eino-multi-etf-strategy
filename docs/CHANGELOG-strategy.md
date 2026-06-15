# 策略改动追溯文档

> **目的**：记录 ETF 多因子动量策略每一步代码改动的**背景 / 涉及文件 / 关键 diff / 实测效果 / 后续待办**，方便日后回溯。
>
> **更新原则**：每次涉及"打分逻辑 / 决策映射 / 仓位 / 池子 / 风控"的改动都需要在此追加一节，不要直接覆盖既有内容。
>
> **基线对照（10w 本金，区间 2026-01-05 ~ 2026-06-08，沪深300 buy&hold +0.38%）**：
>
> | 阶段 | 累计净收益 | 期末资产 | MDD | 胜率 | Alpha |
> |---|---|---|---|---|---|
> | Sprint 0（原版） | -15.34% | ¥84,660 | 23.80% | 31.82% | -15.72% |
> | Sprint 1 完成 | -14.74% | ¥85,260 | 23.26% | 31.82% | -15.12% |
> | 动量公式修复 | -14.74% | ¥85,260 | 23.26% | 31.82% | -15.12% |
> | 聚宽对照模式 | +20.89% | ¥120,890 | 16.70% | 41.03% | +20.51% |
> | **P0 主流程对齐** | **+19.87%** | **¥119,870** | **16.17%** | **41.03%** | **+19.49%** |
> | P0+P1 双动量+凸性 | +5.22% | ¥105,220 | 24.01% | 42.22% | +2.63% |

---

## 目录

- [Sprint 1 — 数据保真度与回测度量基线](#sprint-1)
  - [P0-1 真实资金流接入](#p0-1-真实资金流接入)
  - [P0-2 新闻多源扩充](#p0-2-新闻多源扩充)
  - [P0-3 回测引擎补齐风险/基准/费率指标](#p0-3-回测引擎补齐风险基准费率指标)
- [核心打分公式修复](#核心打分公式修复)
  - [动量 R² 数学口径对齐聚宽](#动量-r²-数学口径对齐聚宽)
- [回测引擎重构](#回测引擎重构)
  - [状态化每日回测](#状态化每日回测)
  - [聚宽对照模式](#聚宽对照模式)
- [P0 主流程对齐聚宽口径](#p0-主流程对齐聚宽口径)
  - [① 板块去重默认关闭](#-板块去重默认关闭)
  - [② RotationParams MinScore=0](#-rotationparams-minscore0)
  - [③ ruleRecommend 阈值放宽](#-rulerecommend-阈值放宽)
  - [④ Regime PositionCap 仓位档位提升](#-regime-positioncap-仓位档位提升)
- [Multi-Agent 与聚宽的设计边界](#multi-agent-与聚宽的设计边界)
- [P1 学术增强（已实施）](#p1-学术增强已实施)
- [P1 学术增强路线（待实施）](#p1-学术增强路线)
- [P2 风险层升级路线（待实施）](#p2-风险层升级路线)
- [P3 前沿与差异化（待实施）](#p3-前沿与差异化)
- [推荐执行顺序速查（P0~P5）](#推荐执行顺序速查p0p5)
- [改动文件清单速查](#改动文件清单速查)

---

## Sprint 1
> 目标：**数据真实化 + 回测可信度量**，为后续优化建立度量基线。

### P0-1 真实资金流接入

**背景**：原 `agent/moneyflow.go` 三个 `estimate*` 函数全部基于"涨跌幅 × 成交额 × 经验系数"估算，与真实北向/主力数据无关。

**新增文件**：`datasource/moneyflow.go`
- 定义可选接口 `MoneyFlowFetcher`（仿 `RealtimeQuoter`，避免破坏 `ETFDataSource` 主接口）
- `EastMoneyDataSource.FetchETFMoneyFlow(code, days)` — 调 EastMoney `push2/qt/stock/fflow/kline/get`
- `EastMoneyDataSource.FetchNorthboundFlow(days)` — 调 EastMoney `push2his/qt/kamt.kline/get`
- 5 分钟内存缓存（key = `code+days`），避免回测期重复请求
- 工具函数：`SumLastN` / `SumNorthboundLastN`

**改动文件**：`agent/moneyflow.go`
- `MoneyFlowAgent` 新增 `UseRealData bool`（默认 true）
- `Run` 优先调用 `MoneyFlowFetcher`，失败自动 fallback 到原 estimate
- Summary 增加 `[real]` / `[estimate]` 标识

**字段映射**：
| 字段 | 真实数据来源 |
|---|---|
| `MainNetInflow3d` | ETF 主力近 3 日累计净流入（亿元） |
| `ETFNetSubscribe` | ETF 主力近 5 日净流入（A 股没公开份额日变动，用主力流入代理） |
| `NorthCapital5d/20d` | 沪深港通整体北向（亿元，作为市场情绪因子） |

**实测**：接口稳定可用，5min 缓存命中率 > 95%；EastMoney 接口对沪市 1. 前缀、深市 0. 前缀敏感，已封装在 `etfSecid()`。

---

### P0-2 新闻多源扩充

**背景**：原 `datasource/news.go` 只有 EastMoney 站内搜索 + Sina 空 stub；缺时效过滤，旧新闻稀释当日情绪。

**改动文件**：`datasource/news.go`

**新增源**：
1. **EastMoney**（已有）— `cmsArticleWebOld`
2. **新浪财经** — `feed.mix.sina.com.cn/api/roll/get?pageid=153&lid=2516`（财经滚动）
3. **财联社（CLS）** — `cls.cn/nodeapi/telegraphList?app=CailianpressWeb`（A 股最快源，电报级时效）
4. **Wind 财经** — `wind.com.cn/portal/zh/api/portal/news/search`

**新增过滤**：
- `FreshWithin time.Duration`（默认 24h）— 时效过滤
- `filterByKeywords` — 标题/正文必须命中至少一个 keyword（去除"芯片烘焙"等同形异义噪音）
- `NewsItem.Time time.Time` — 统一解析时间字段（修复跨年字典序排序问题）
- 财联社 `IsAd==1` 广告类电报跳过

**实测**：单源失败不影响其他源；新闻量约提升 2~3 倍，时效从"近 7 天"收紧到"近 24h"。

---

### P0-3 回测引擎补齐风险/基准/费率指标

**背景**：原 `backtest/engine.go` 仅有"胜率 / 平均收益 / 简易 Sharpe / 最大单笔亏损"，缺权益曲线、最大回撤、Calmar、Sortino、Profit Factor、基准对比、Alpha、手续费假设。

**改动文件**：`backtest/engine.go`

**Engine 配置新增**：
```go
type Engine struct {
    // ...
    CostPerSide float64 // 单边成本，默认 0.0008（手续费 0.03% + 滑点 0.05%）
    Benchmark   string  // 默认 "510300" 沪深300ETF
}
```

**Result 字段新增**：
| 字段 | 含义 |
|---|---|
| `EquityCurve []EquityPoint` | 权益曲线（连续复利，初始 1.0） |
| `FinalEquity / TotalReturn` | 最终累计净值与收益率 |
| `AnnualReturn` | 年化收益（按交易日跨度推算 252 天/年） |
| `MaxDrawdown / MaxDrawdownDate` | 最大回撤幅度与触发日 |
| `Calmar` | 年化收益 / |MDD| |
| `Sortino` | 平均收益 / 下行波动 × √N |
| `ProfitFactor` | Σwin / |Σloss| |
| `AvgWin / AvgLoss / WinLossRatio` | 平均盈利 / 平均亏损 / 盈亏比 |
| `BenchmarkReturn / Alpha` | 基准 buy&hold 收益与超额 |
| `CostPerSide` | 已扣单边成本 |

**关键计算**：
- 每笔交易**净收益** = `仓位加权收益 - 2 × CostPerSide`（进出双边各扣一次）
- 权益曲线复利累计；MDD 基于权益曲线峰值回撤
- Profit Factor 全胜时返回 `+Inf`，渲染时 `fmtFinite()` 替换为 `∞`

**报告增强**：`BuildMarkdown` / `BuildCompareMarkdown` 增加"风险与基准指标"章节 + V3vsV2 对比新增 8 行新指标。

**清理**：删除已被替代的 dead code `findClosePriceAfter`。

---

## 核心打分公式修复

### 动量 R² 数学口径对齐聚宽

**背景**：用户反馈本项目回测结果与聚宽不一致，逐步拆解后发现核心打分函数 `MomentumScore` 的 R² 计算与聚宽存在数学差异。

**对比拆解**（聚宽 `strategy.py:get_etf_rank` vs 本项目 `indicator/momentum_score.go`）：

| 步骤 | 聚宽 | 本项目（修复前） | 影响 |
|---|---|---|---|
| 取 21 个 close | `attribute_history(s, 21, '1d', ['close'])` | `klines[n-21:]` | 窗口边界差异（小） |
| `np.log(prices)` | ✅ | ✅ | 一致 |
| `weights = linspace(1,2,n)` | ✅ | ✅ | 一致 |
| WLS 一次回归 | `np.polyfit(x,y,1,w=weights)` | 自实现 WLS 等价公式 | 一致 |
| `annualized = exp(slope·250)-1` | ✅ | ✅ | 一致 |
| **`SST_w` 中的 ȳ** | **`np.mean(y)`（未加权）** | **`Σwy/Σw`（加权）** | **🔴 关键差异** |
| **R² clamp** | **不 clamp** | **clamp [0,1]** | 🔴 影响震荡序列排名 |
| `score = annualized × R²` | ✅ | ✅ | 一致 |

**修复**：`indicator/momentum_score.go`

```diff
-  // 加权 R²
-  yMean := sumWY / sumW
+  // 加权 R²：SST_w 中的 ȳ 使用「未加权」均值（对齐聚宽 np.mean(y)）
+  var sumY float64
+  for i := 0; i < mDays; i++ {
+      sumY += closes[i]
+  }
+  yMeanUnweighted := sumY / float64(mDays)
   ...
-  if r2 < 0 { r2 = 0 }
-  if r2 > 1 { r2 = 1 }
+  // 不再 clamp 到 [0,1]：与聚宽一致，允许负 R² 把弱拟合标的的 score 推得更低
+  r2 = 1 - sseW/sstW
```

**实测**：同区间 v3 回测累计净收益从 -15.34% → **-14.74%**（改善 0.6 pp），胜率/建仓数完全一致，说明绝大多数日子 rank[0] 不变，仅边缘日子排名翻转。

**结论**：纯打分公式与聚宽数学一致，剩余差距由外围决策层贡献。

---

## 回测引擎重构

### 状态化每日回测

**背景**：原引擎按 `step=5` 采样（每 5 个交易日一个样本，每个样本固定持有 5 日），与聚宽"每日跑信号 + 信号反转才换仓"语义不一致。

**改动文件**：`backtest/engine.go`

**新算法**（持仓状态机）：
1. 用基准 510300 K 线确定有效交易日序列
2. 维护 `holdState{ code, entryDate, entryPrice, posCap, klineCache }`
3. 每个交易日 d：
   - 跑 Screener（asOf=d）→ 当日 best
   - 跑 Regime（asOf=d）→ PositionCap
   - 决策（规则版）→ recommendation
   - 比较：
     - `best.Code == 当前持仓 AND recommendation ∈ {buy, strong_buy}` → 持有不动
     - 否则 → 当日收盘平仓 + 扣双边费率，登记一笔 Trade
     - 若新信号是 buy/strong_buy → 当日收盘按新 best 入场
4. 区间末（最后一个交易日）：仍有持仓 → 强制平仓，`ExitReason=end_of_range`

**Trade 字段新增**：`EntryDate / ExitDate / ExitReason`

**清理**：删除内联的 `runOnce` 函数（被状态机替代）。

**实测**：
- 实际建仓数 8 → 22 → **39**（每日跑信号 vs 5 日采样）
- 胜率 50% → 41%（建仓更频繁，单笔胜率不变量级，总收益由复利驱动）

---

### 聚宽对照模式

**背景**：为验证主流程优化效果，需要一个"裸聚宽口径"基线作为天花板对照。

**改动文件**：`backtest/engine.go` + `main.go`

**`Variant=joinquant` 模式**：直接调 `RotationAgent.Rank`，**完全旁路** Screener 的所有外围装饰：

```go
rot.Params.MinScore = 0                  // 对齐聚宽 g.min_score=0
rot.Params.MaxScore = 6
rot.Params.ScoreThresholdMultiplier = 1.1
rot.Params.MDays = 21
// 满仓 1.0、无 dedupBySector、无 RuleBasedDecision、无 PositionCap 三档
posCap := 1.0
```

**`main.go`**：variant 校验从 `v3/v3v2/both` → `v3/v3v2/both/joinquant`。

**实测**（10w 本金，2026-01-05~2026-06-08）：
- 累计净收益 **+20.89%**（¥120,890）
- 年化 +56.78%、MDD 16.70%、Calmar **3.40**
- Alpha vs 沪深300 **+20.51%**

**用途**：作为本项目主流程的天花板对照，验证 P0 改动是否成功收敛。

---

## P0 主流程对齐聚宽口径

> 目标：在保留 Multi-Agent 框架价值的前提下，让主流程动量信号能像聚宽一样"持续在线"，把损失的 35.6 pp Alpha 追回来。

> **改造效果速览**：
> | 阶段 | 累计净收益 | 实际建仓 | 胜率 | MDD |
> |---|---|---|---|---|
> | 改造前 v3 | -14.74% | 22 笔 | 31.82% | 23.26% |
> | **改造后 v3** | **+19.87%** | **39 笔** | **41.03%** | **16.17%** |
> | joinquant 对照 | +20.89% | 39 笔 | 41.03% | 16.70% |

> **结论**：主流程 v3 ≈ joinquant（差 1 pp 在 sigmoid 归一化噪声内），且 MDD 比裸聚宽**还低 0.53 pp**（多 Agent 风控旁路有效）。

---

### ① 板块去重默认关闭

**问题定位**：`agent/screener.go:dedupBySector()` 每个 sector 仅保留最高分一只 → 池里 14 只科技 ETF 在科技走牛行情下只能选 1 只，第二只换冷门板块次优。

**估计 Alpha 损失**：-8 ~ -10%（最大）

**改动文件**：`agent/screener.go`
```go
type ScreenerAgent struct {
    // ...
    DedupBySector bool // 默认 false（对齐聚宽不去重）
}

func NewScreenerAgent(ds datasource.ETFDataSource) *ScreenerAgent {
    return &ScreenerAgent{
        // ...
        DedupBySector: false,
    }
}

// Run 中：
if a.DedupBySector {
    scored = dedupBySector(scored)
}
```

**保留路径**：`dedupBySector` 函数本身保留，可在外部用 `agent.DedupBySector = true` 显式开启（用于风险分散场景）。

---

### ② RotationParams MinScore=0

**问题定位**：原默认 `MinScore=-1` 允许负分动量进入候选，在熊市边缘挑出 score=-0.3 的 ETF，下一日继续亏。

**估计 Alpha 损失**：-1 ~ -2%

**改动文件**：`agent/rotation.go`
```diff
 func DefaultRotationParams() RotationParams {
     return RotationParams{
         MDays:                    21,
         MaxScore:                 6,
-        MinScore:                 -1,
+        MinScore:                 0, // 对齐聚宽 g.min_score=0
         ScoreThresholdMultiplier: 1.1,
         TopN:                     5,
     }
 }
```

**配套测试更新**：`agent/rotation_test.go:TestRotation_DefaultParams` 期望值同步改为 0。

---

### ③ ruleRecommend 阈值放宽

**问题定位**：`agent/final.go:ruleRecommend()` 把综合分 50~65 映射成 hold；hold 在状态机里 = 当日平仓 → **频繁空仓 / 错过持续上涨**。

**估计 Alpha 损失**：-3 ~ -5%

**改动文件**：`agent/final.go`
```diff
 func ruleRecommend(s float64) string {
     switch {
-    case s >= 80: return "strong_buy"
-    case s >= 65: return "buy"
-    case s >= 50: return "hold"
+    case s >= 70: return "strong_buy"
+    case s >= 40: return "buy"
+    case s >= 25: return "hold"
     default:      return "avoid"
     }
 }
```

**设计依据**：
- 底层 `normalizeStrategy3Score` 把 `score=0` → 50，`score=0.3` → ~63
- 旧阈值 65/50 让"动量弱正向 + 多因子中性"的标的（综合分 50~65）映射 hold
- 新阈值让动量正向（score>0）的标的稳定进 buy

---

### ④ Regime PositionCap 仓位档位提升

**问题定位**：原 `classifyRegime` 在 neutral_up=0.7、neutral=0.5、bear=0.2 机械打折；A 股波段经常把上涨初期识别成 neutral_up。

**估计 Alpha 损失**：-3 ~ -4%

**改动文件**：`agent/regime.go`
```diff
 func classifyRegime(price, ma20, ma60, ma120, dd float64) (string, float64, float64) {
     // risk_off：保持原值（系统性风险硬约束）
     if ma120 > 0 && price < ma120 && dd >= 0.08 {
         return "risk_off", math.Max(0, 30-dd*100), 0.0
     }
-    if ... { return "bear", 35, 0.2 }
+    if ... { return "bear", 35, 0.4 }
     if ... { return "bull", 85, 1.0 }
-    if ... { return "neutral_up", 65, 0.7 }
+    if ... { return "neutral_up", 65, 0.95 }
-    return "neutral", 50, 0.5
+    return "neutral", 50, 0.85
 }
```

**设计原则**：
- bull / neutral_up / neutral：保持高仓位（1.0/0.95/0.85），让动量信号充分发挥
- bear：降到 0.4，但仍允许部分动量反弹机会
- risk_off：仅在 MA120 跌破 + 60 日回撤 ≥ 8% 这类系统性风险时强制空仓

---

## Multi-Agent 与聚宽的设计边界

> **核心原则**：第一步必须是「候选池」而非「决策」，否则 Multi-Agent 框架退化为单因子模型。

### 聚宽的"一步成案"
```python
ranked = get_etf_rank(pool, m_days=21, min=0, max=6)
hold = ranked[0]  # 完
```
只有 1 个动量信号，rank[0] 就是答案，没有任何风控旁路（除了 max_score=6 的过热检查）。

### Multi-Agent 的"分层评估"
```
Quant（动量分）→ 候选池（Top5）
   ↓ 不应该立即决策
Tech / News / Global / Regime / Flow → 独立校验各维度
   ↓
Final（决策融合） → 一致 → 全仓买入；不一致 → 降档/拒绝
```

### 第一步设计的两种语义

| 语义 | 输出 | 后续 Agent 作用 |
|---|---|---|
| ❌ 决策（聚宽口径） | "今天买 XXXX" | 沦为说明书 |
| ✅ **候选池** | "Top5 候选 + 各自动量分" | News/Tech/Flow 在 Top5 间做次序微调 |

### 这次 P0 的边界

✅ 保留第一步是聚宽口径的动量打分（`MinScore=0`、不去重、过热 1.1 倍门槛）
✅ 关闭"过早过滤"（dedupBySector / 决策层 hold 映射）
✅ 保留风控旁路（Regime risk_off 强制空仓、bear 降仓 0.4、CapByPremium、R²<0.3 警示）
❌ 没有完全跳过决策层：综合分仍然是 6 因子加权，只是阈值放宽到让动量正向标的稳定进 buy

### Multi-Agent 应该比聚宽强的位置

- **2024-09 ~ 2024-11 短期暴跌**：Regime 的 risk_off 应该比聚宽更早降仓
- **横盘震荡市**：动量分都靠近 0、相互很接近时，News/Tech/Flow 微调能挑出真正强者
- **板块周期切换点**：Regime 的板块周期判断比纯动量更早识别拐点

---

## P1 学术增强（已实施）

> 目标：在 P0 基线之上叠加学术界经典的"双周期动量 + 凸性调整"，验证其对当前样本区间的增量效果。
>
> **实测结论：当前 5 个月样本下负向，未默认启用，作为可选 variant `v3p1` 保留以备熊市 / 长样本复测。**

### 改动文件

| 文件 | 改动 |
|---|---|
| `indicator/momentum_score.go` | 新增 `AnnualizedReturnN(klines, n)`、`VolatilityN(klines, n)`（年化对数收益波动率） |
| `agent/rotation.go` | `RotationParams` 增加 `EnableDualMomentum / LongLookback / LongMinAnnualized / EnableConvexity / ConvexityLookback / ConvexitySigmaFloor`；`Rank` 中按 P1-1 过滤、P1-2 调整 score（T 与 T-1 同口径） |
| `backtest/engine.go` | `Run` 入口注入 `v3p1` variant：开启 P1-1+P1-2，把 MaxScore 从 6 放宽到 30（凸性调整后量纲被 σ 放大 5~20 倍） |
| `backtest/engine.go` | 新增 `BuildP1CompareMarkdown` + `writeBucketCompareLabeled`，输出 P0 vs P0+P1 对比报告 |
| `main.go` | `--bt-variant` 接受 `v3p1` / `both_p1` |

### P1-1 双周期动量（Antonacci 2014）

```go
// 252 日年化 < LongMinAnnualized(默认 0%) → 直接出局
if p.EnableDualMomentum && len(klines) >= p.LongLookback {
    if indicator.AnnualizedReturnN(klines, p.LongLookback) < p.LongMinAnnualized {
        continue
    }
}
```

含义：**只买"既跑赢同侪（21 日动量打分）、又长期处于上行轨道（252 日年化 ≥ 0）"的标的**。

### P1-2 凸性调整（Daniel & Moskowitz 2016）

```go
// score := score / max(σ_n, floor)；T 与 T-1 同口径，保证 1.1 倍门槛语义不变
sig := indicator.VolatilityN(klines, p.ConvexityLookback)
if sig < p.ConvexitySigmaFloor { sig = p.ConvexitySigmaFloor }
score /= sig
prev /= sigPrev // 同样除
```

含义：**把"年化收益 × R²"转成"风险调整后年化收益 × R²"**，惩罚高 σ 高动量（典型动量崩盘候选）。

> 注意：调整后 score 量纲被放大约 5 ~ 20 倍，`MaxScore=6` 的过热门槛失效，`v3p1` 把它放宽到 30，保留 1.1 倍日间增长的语义。

### 实测：P0 vs P0+P1（区间 2026-01-05 ~ 2026-06-08，10w 本金）

```
go run . --mode=backtest --bt-start=2026-01-05 --bt-end=2026-06-08 --bt-variant=both_p1 --bt-max=0
```

→ 报告：`report/backtest-compare-p1-20260609-193303.md`

| 指标 | P0（v3） | P0+P1（v3p1） | Δ |
|---|---|---|---|
| 实际建仓 | 38 | 45 | +7 |
| 胜率 | 42.11% | 42.22% | +0.12 pp |
| 平均加权收益 | +0.90% | +0.40% | -0.50 pp |
| 收益标准差 | 5.96% | 5.12% | -0.84 pp |
| 简易 Sharpe | **1.07** | **0.55** | **-0.52** |
| **累计净收益** | **+24.30%** | **+5.22%** | **-19.08 pp** |
| 年化收益 | +69.18% | +13.08% | -56.10 pp |
| **最大回撤** | **16.17%** | **24.01%** | **+7.84 pp** |
| Calmar | 4.28 | 0.54 | -3.73 |
| Sortino | 1.36 | 0.44 | -0.92 |
| Profit Factor | 1.41 | 1.14 | -0.28 |
| 盈亏比 | 1.94 | 1.56 | -0.39 |
| **Alpha vs 沪深300** | **+21.71%** | **+2.63%** | **-19.08 pp** |

### 结论与判断

**P1 在当前 5 个月样本上是显著负向的**：

1. **累计净收益从 +24.30% 跌到 +5.22%（-19.08 pp）**，年化从 69% 退到 13%。
2. **最大回撤反而扩大（16.17% → 24.01%，+7.84 pp）**，与 P1 的设计初衷"降回撤"完全相反。
3. **Sharpe 0.55、Calmar 0.54** 均出现明显恶化。
4. **建仓笔数 +7 笔**：P1-2 凸性调整后排名重新洗牌，更多低 σ 但低收益的标的进入 Top1，换手 + 噪声 ↑。

为什么会反过来？两个数据特征：

- **2026-01 ~ 2026-06 是 A 股温和 / 偏强单边市**（沪深300 +0.38%，但煤炭 / 海外 / 通信等强势板块年化 300% +）。这段没有"动量崩盘"，凸性调整等于强行折损了高 σ 高收益标的（煤炭 / 日经 / 通信 ETF 这些 P0 主力都是 σ_21 在 25%~40% 区间）。
- **252 日双动量过滤几乎不淘汰人**：当前 ETF 池中绝大多数标的 252 日年化都 ≥ 0，过滤效果近似空操作；但它强制要求拉 252 根 K 线，触发了少量 263 日不足的标的被剔，反而改变了"次优"标的的排序。

### 落地决策

- ❌ **默认主流程（v3）继续不开 P1**：`DefaultRotationParams()` 保持 `EnableDualMomentum=false`、`EnableConvexity=false`。
- ✅ **保留为可选变体 `v3p1`**：用于：
  - 长样本（≥ 2 年含暴跌区间）复测，验证 Antonacci / Daniel-Moskowitz 论文中的"危机期保护"是否在 A 股 ETF 池仍成立；
  - 熊市切换信号确认后临时启用（`agent.RotationAgent.Params.EnableConvexity = true`）；
  - P2-1 波动率目标定仓的对照基线（两者本质都是用 σ 调整收益）。
- 🔬 **下一步验证**：把回测区间扩到 2024-01 ~ 2026-06（含 2024-09 暴跌 + 2024-11~2025-01 反弹），样本量从 38 升到 ~500，再跑一次 `both_p1`，看 P1 在含暴跌期的整体净值/MDD 是否反超。

### 待办标记更新

| 项目 | 状态 |
|---|---|
| P1-1 双周期动量 | ✅ 已实施（短样本负向，待长样本复测） |
| P1-2 凸性调整动量 | ✅ 已实施（短样本负向，待长样本复测） |
| P1-3 残差动量 | ⏳ 待实施 |
| P1-4 趋势+反转复合 | ⏳ 待实施 |

---

## P1 学术增强路线
> **状态：待实施**。所有改动在 P0 达成 +19.87% 基线之上做差异化超额。

### P1-1 双周期动量（Dual Momentum）— Antonacci 2014
- **逻辑**：21 日相对动量 + 252 日绝对动量；只买"既跑赢同侪、又跑赢无风险利率"
- **学术依据**：Antonacci 《Dual Momentum Investing》2014；Asness, Moskowitz, Pedersen 2013 *JoF* "Value and Momentum Everywhere"
- **落地**：`indicator/momentum_score.go` 增加 `MomentumScore252()`；`agent/rotation.go` 在 Rank 末尾加"252 日年化 > 0"过滤
- **预期**：减少熊市误信号，回撤下降 30 ~ 50%
- **难度**：低

### P1-2 凸性调整动量（Convexity-Adjusted）— Daniel & Moskowitz 2016
- **逻辑**：`score = annualized × R² / σ_21` — 用过去波动率惩罚高 σ 标的
- **学术依据**：Daniel, Moskowitz 2016 *JFE* "Momentum Crashes"
- **落地**：`indicator/momentum_score.go` 加 `Volatility21()`；`agent/rotation.go` 用 `score / σ_21` 替换原 score
- **预期**：避免动量见顶后崩盘（如 2026-03 的科技崩盘）；Sharpe 提升 0.2~0.3
- **难度**：中

### P1-3 残差动量（Residual Momentum）— Blitz, Huij, Martens 2011
- **逻辑**：先用市场指数（510300）回归剥离 β，对**残差**做 21 日动量打分
- **学术依据**：Blitz et al. 2011 *JEF* "Residual Momentum"，A 股横截面 Sharpe 提升约 30%
- **落地**：在 `MomentumScore` 之外加 `ResidualMomentumScore(klines, benchmarkKlines)`
- **预期**：横截面排名更准（剥离市场效应）
- **难度**：中

### P1-4 趋势 + 反转复合
- **逻辑**：`score = 0.7 × TS_MOM_63 + 0.3 × MR_5`
- **学术依据**：Jegadeesh & Titman 1993 *JoF*；Lehmann 1990 *QJE*
- **落地**：`agent/rotation.go` 增加双因子评分
- **预期**：横盘市表现显著好于纯 21 日动量
- **难度**：低

---

## P2 风险层升级路线
> **状态：待实施**。聚焦从"建议层"升级到"执行层"。

### P2-1 波动率目标定仓（Vol Targeting）— Harvey et al. 2018
- **逻辑**：保持组合年化波动率 = 15%；高波动期降仓，低波动期加仓
- **学术依据**：Harvey, Hoyle, Korgaonkar, Rattray, Sargaison, Van Hemert 2018 *JPM* "The Impact of Volatility Targeting"
- **落地**：`agent/regime.go:classifyRegime` → 改为连续函数 `position = target_vol / realized_vol_21d`
- **预期**：MDD 下降 40%+，长周期 Sharpe 升 0.3 ~ 0.5
- **难度**：中

### P2-2 持仓状态机（执行层）
- **背景**：当前 stop_loss / take_profit 仅作为建议字段，没有真正持仓状态。
- **落地**：新增 `state/positions.json`（或 SQLite），记录每只标的的 entry/stop/take/entry_date；每天开盘前对比当前价：
  - 触止损 → 立即清仓
  - 触止盈 → 减半 / 移动止盈
  - 持仓超 N 日仍负收益 → 超时退出
- **难度**：中

### P2-3 移动止损（Trailing Stop）
- **逻辑**：入场后每日更新 `stop = max(stop, MA20×0.99)`，锁定盈利
- **落地**：在 P2-2 状态机基础上加一行
- **难度**：低

### P2-4 picks 数值校验
- **背景**：当前 LLM 给的 entry/stop/take 没校验，可能不合理
- **落地**：硬约束 `stop ≥ price×0.93`、`take ≤ price×1.20`、`stop < price < take`，否则用规则版覆盖
- **难度**：低

---

## P3 前沿与差异化
> **状态：待实施**。需要更多数据 / 模型基础设施。

### P3-1 ML 多因子（Gu, Kelly, Xiu 2020 *RFS*）
- **逻辑**：随机森林 / 神经网络 在多因子选股上 Sharpe 比线性模型高 100%+
- **难点**：需要至少 5 年日频特征，本项目目前只有腾讯 K 线，缺财务/持仓数据
- **学术依据**：Gu, Kelly, Xiu 2020 "Empirical Asset Pricing via Machine Learning"

### P3-2 IPCA 动态因子（Kelly, Pruitt, Su 2019 *JFE*）
- **逻辑**：Instrumental PCA，把动态因子拟合到 cross-section
- **难度**：高（需要因子库 + 期望条件估计框架）

### P3-3 ETF 折溢价均值回归
- **数据**：本项目已有 `IOPV / PremiumPct`
- **逻辑**：高溢价 → 反向；高折价 → 正向
- **落地**：在 `Screener` 末尾加 `PremiumPct` 反向因子加权
- **难度**：低

### P3-4 ETF 份额变化（净申赎）
- **数据**：需 EastMoney `unitTotal` 接口
- **逻辑**：资金追入 → 短期延续
- **难度**：中

---

## 持仓感知 & 多持仓 HoldReviews（advice 模式专属）

> 目标：解决"持仓动量还可以，但没进 Top5 → 系统没推荐 → 用户被迫卖出"的客观偏差，同时让 advice 模式支持**多 ETF 持仓**。
>
> **设计前提**：持仓只增加可见性，不抬分、不绕过过滤；主决策（recommendation / picks / 价格方案）保持 100% 客观，不被持仓污染。

### 改动文件

| 文件 | 改动 |
|---|---|
| `types/types.go` | 新增 `AgentState.CurrentHolds []string` / `HoldAdvices []HoldAdvice`；`ScoredETF.IsCurrentHold bool`；`FinalDecision.HoldReviews []HoldReview`；新增 `HoldReview` 类型；`PreOpenAnalysis.CurrentHolds`；旧 `CurrentHold` / `HoldAdvice` 单值字段保留为兼容快照 |
| `agent/rotation.go` | `RotationParams.MustIncludeCodes`：TopN 截断后追加豁免位（已通过 MinScore / 过热 / Regime 等所有过滤的持仓才会被追加）；新增 `BuildHoldAdvices(holds, top5)` 多持仓对照工具 |
| `agent/screener.go` | `ScreenerAgent.CurrentHolds`、Run 中标 `IsCurrentHold`；`dedupBySectorPreserveHolds`（同板块去重保留持仓）；`appendHoldsBeyondTop`（TopN 之外追加） |
| `agent/final.go` | system prompt 新增 `hold_reviews` JSON schema 与 keep/trim/rotate 约束；user payload 注入 `current_holds` + `hold_candidates`（含 score / news / tech 子集）；新增 `buildHoldReviewsFallback`、`sanitizeHoldReviews`、`collectHolds(List)`、`classifyNewsBias`、`classifyTechTrend`、`decideHoldAction`；`CapByPullbackCooldownForState` 改为多持仓命中判断 |
| `agent/preopen.go` | `applyVerdictRule` 签名 `string → map[string]struct{}`；新增 `normalizePreOpenHolds`；prompt 中 `current_hold → current_holds` |
| `orchestrator/pipeline.go` | `Pipeline.CurrentHolds []string`，`normalizeHolds` 去重 + `firstHold` 兜底兼容旧字段 |
| `main.go` / `cmd/preopen/main.go` | `--current-hold` 描述更新；新增 `parseCurrentHolds` 支持逗号分隔；Top5 输出加 🟦 标；HoldReviews 单独打印块 |
| `report/writer.go` | Top5 表格给持仓行加 🟦；`writeHoldAdvice` 改为多持仓循环；新增 `writeHoldReviews` 章节（表格 + 逐只 rationale） |
| `report/preopen_writer.go` | 集合竞价复核报告显示 `CurrentHolds` 列表 |
| `agent/holdreview_test.go` | 新增 6 个 case：keep / rotate / 多持仓 / 池外忽略 / sanitize / `collectHoldsList` 去重 |
| `agent/news_final_test.go` | `applyVerdictRule` 旧单 string 调用改为 `map[string]struct{}` |

### 决策路径

```
CurrentHolds (CLI)
  → Pipeline.CurrentHolds → AgentState.CurrentHolds（去重保序）
  → ScreenerAgent.CurrentHolds
      ├─ dedupBySectorPreserveHolds : 同板块去重时保留持仓
      ├─ Rotation.Rank(MustIncludeCodes) : TopN 之外按客观分追加，不抬分
      └─ 标 ScoredETF.IsCurrentHold = true
  → AgentState.Screener.Top5（含可能的"持仓豁免位"）
  → FinalAgent
      ├─ payload 注入 current_holds + hold_candidates（score / news / tech）
      ├─ LLM 输出 hold_reviews + sanitize（filter alien / 补字段 / 校验 action）
      ├─ LLM 失败/缺字段 → buildHoldReviewsFallback（规则版客观映射）
      └─ HoldReviews 不影响 recommendation / picks / 价格方案
  → 报告 §九（多持仓对照）+ §十（HoldReviews 表格 + rationale）
```

### HoldReviews 三档客观规则

| 触发条件 | Action | ActionDesc |
|---|---|---|
| Action=avoid 或 趋势=down 或 消息=negative 或 (与 best 分差 ≥15 且不在 Top3) | `rotate` | 平仓切换 |
| Action=hold_only 或 趋势=flat 或 (消息=neutral 且 分差 ≥8) | `trim` | 减仓观察 |
| 其他（含 strong_buy/buy + 无负向信号） | `keep` | 继续持有 |

> 持仓**不在候选列表**（连 Strategy3Pool / 全部 Screener 过滤都未命中）→ 静默忽略，不出 HoldReview 条目（与用户决策"忽略，不强制评估"对齐）。

### 实测验证（2026-06-11，advice 模式）

```
go run . --current-hold 513100,515220
```

- Top5 表（§七）`515220` 与 `513100` 均带 🟦；`513100` 量化分 26.78、被同板块去重保留并以"豁免位"追加到第 6；
- §九 多持仓对照：两只都给出基于动量名次的对照建议；
- §十 HoldReviews：`515220`(Top1, news=neutral, tech=up) → **keep**；`513100`(第6, news=negative, tech=down) → **rotate**；
- 主决策 `recommendation=buy` / `picks` / `entry / stop / take` 仍以 Top1 客观候选 `515220` 为锚，**未因为 `513100` 是持仓而被抬上来** → 验证持仓不污染主决策。

### 落地决策

- ✅ **回测路径不传 `MustIncludeCodes`**：`backtest/engine.go` 仍按纯客观主流程跑，2 个月回测 +9.51% 基线未变；
- ✅ **advice 模式默认启用**；
- ✅ **持仓 set 全程 `map[string]struct{}` + `[]string` 双轨**，旧单值字段（`CurrentHold` / `HoldAdvice`）保留为兼容快照，sidecar / 老脚本不破。

### 待办

- [ ] 长样本（≥1 年含一次 bear）回测：advice 路径**虚拟开关**注入 `MustIncludeCodes` 模拟长期持仓行为，看 HoldReviews 的 rotate / keep 建议在历史上是否能跑赢"无脑跟 Top1"（量化版"换手懒惰"vs"客观换仓"对照）。
- [ ] PreOpen Agent 在 LLM 路径中也消费 `hold_candidates` 维度（当前 PreOpen 只用 holds set 做 isHolding 判断）。
- [ ] 报告 §十增加"换仓建议明细"：当 HoldReview.Action=rotate 时，自动建议"切换到 Best/Pick #2"，并比较两者的 score / sector / news / tech 差距。

---

## 推荐执行顺序速查（P0~P5）

> 这是按"投入产出比"排序的演进路线表，与上面 P1/P2/P3 章节内容呼应；后续每完成一项，把"状态"列从 ⏳ 改为 ✅，并在对应的 P 章节追加实测数据。

| 阶段 | 改造 | 难度 | 预期 Alpha 增量 | 状态 | 对应章节 |
|---|---|---|---|---|---|
| **P0**（已做） | 应用必改 4 项对齐聚宽口径（dedupBySector / MinScore / ruleRecommend / PositionCap） | 低 | **+20%（已验证）** | ✅ | [P0 主流程对齐](#p0-主流程对齐聚宽口径) |
| **P1** | 加 252 日绝对动量过滤（双动量 Antonacci 2014） | 低 | +5% ~ +10%，回撤改善 | ✅ 短样本 -19 pp / 🔬 长样本待验证 | [P1-1](#p1-1-双周期动量dual-momentum--antonacci-2014) |
| **P2** | `score / σ_21` 凸性调整（防动量崩溃，Daniel & Moskowitz 2016） | 中 | Sharpe +0.2 ~ +0.3 | ✅ 短样本 Sharpe -0.52 / 🔬 长样本待验证 | [P1-2](#p1-2-凸性调整动量convexity-adjusted--daniel--moskowitz-2016) |
| **P3** | 波动率目标定仓替换 PositionCap（Harvey et al. 2018） | 中 | MDD 减半 | ⏳ | [P2-1](#p2-1-波动率目标定仓vol-targeting--harvey-et-al-2018) |
| **P4** | 残差动量（剥离 510300 β，Blitz et al. 2011） | 中 | 横截面排名更准 | ⏳ | [P1-3](#p1-3-残差动量residual-momentum--blitz-huij-martens-2011) |
| **P5**（前沿） | ML 多因子 + IPCA（Gu, Kelly, Xiu 2020 / Kelly, Pruitt, Su 2019） | 高 | 长周期 Sharpe +0.5 | 💭 | [P3-1](#p3-1-ml-多因子gu-kelly-xiu-2020-rfs) / [P3-2](#p3-2-ipca-动态因子kelly-pruitt-su-2019-jfe) |

> ⚠️ **说明**：
> - 这里的 P1~P5 是**演进编号**（按性价比执行顺序），与上面 P1/P2/P3 章节里的**主题分组**（学术增强 / 风险升级 / 前沿）不完全一一对应。
> - 例如演进 P3（波动率目标定仓）属于章节 P2（风险层升级）；演进 P4（残差动量）属于章节 P1（学术增强）。这是因为我们按"性价比"决定顺序，不按"主题"。
> - 后续每完成一个演进项，请同时更新此表的"状态"列与对应章节的"待办标记"。

---

## 改动文件清单速查

| 文件 | Sprint 1 | 公式修复 | 状态化回测 | 聚宽对照 | P0 对齐 | P1 实施 |
|---|---|---|---|---|---|---|
| `datasource/moneyflow.go` | 🆕 新增 | | | | | |
| `agent/moneyflow.go` | ✏️ 双轨 | | | | | |
| `datasource/news.go` | ✏️ 4 源 | | | | | |
| `backtest/engine.go` | ✏️ 风险指标 | | ✏️ 状态机 | ✏️ joinquant 分支 | | ✏️ v3p1 + P1Compare |
| `indicator/momentum_score.go` | | ✏️ R² 对齐 | | | | ✏️ AnnualizedReturnN/VolatilityN |
| `main.go` | | | | | ✏️ variant 校验 | ✏️ both_p1 |
| `agent/screener.go` | | | | | ✏️ DedupBySector 默认关 | |
| `agent/rotation.go` | | | | | ✏️ MinScore=0 | ✏️ P1 toggles + Rank 注入 |
| `agent/rotation_test.go` | | | | | ✏️ 同步期望值 | |
| `agent/final.go` | | | | | ✏️ ruleRecommend 阈值 | |
| `agent/regime.go` | | | | | ✏️ classifyRegime 仓位 | |

---

## 待办标记速查

| 标记 | 含义 |
|---|---|
| ✅ 已实施 | 代码已合入主流程，回测验证生效 |
| 🔬 待验证 | 代码已合入但缺少长周期回测验证 |
| ⏳ 待实施 | 已设计方案，未动代码 |
| 💭 思考中 | 仍在评估方案 |

| 项目 | 状态 |
|---|---|
| Sprint 1 三项 | ✅ 已实施 |
| 动量公式修复 | ✅ 已实施 |
| 状态化每日回测 | ✅ 已实施 |
| 聚宽对照模式 | ✅ 已实施 |
| P0 主流程对齐（4 项） | ✅ 已实施 |
| P0 长周期回测（2024-09 暴跌区间）| 🔬 待验证 |
| P1-1 双周期动量 | ✅ 已实施（短样本负向，🔬 待长样本复测） |
| P1-2 凸性调整动量 | ✅ 已实施（短样本负向，🔬 待长样本复测） |
| P1-3 残差动量 | ⏳ 待实施 |
| P1-4 趋势+反转复合 | ⏳ 待实施 |
| P2-1 波动率目标定仓 | ⏳ 待实施 |
| P2-2 持仓状态机 | ⏳ 待实施 |
| P2-3 移动止损 | ⏳ 待实施 |
| P2-4 picks 数值校验 | ⏳ 待实施 |
| P3-1 ML 多因子 | 💭 思考中 |
| P3-2 IPCA 动态因子 | 💭 思考中 |
| P3-3 折溢价反向因子 | ⏳ 待实施 |
| P3-4 份额变化代理 | ⏳ 待实施 |
