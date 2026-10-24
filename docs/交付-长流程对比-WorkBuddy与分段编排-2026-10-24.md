# 长流程复杂任务：WorkBuddy 一把梭 vs 分段编排

> 日期：2026-10-24 凌晨 · 资产 `.tmp/expr-bench/` · 全部数字为实测
> 任务：exprkit（5 阶段从零实现，73 条用例，基线 1/73）

## 一、结论

| 路线 | 执行方式 | 墙钟 | 用例 | 说明 |
|---|---|---|---|---|
| **WorkBuddy 一把梭** | 1 个 agent 看全部规格，一次做完 | **124.7s** | 73/73 | 最快 |
| **WorkBuddy 分段**（5 段 + 每段验收） | 5 个子 agent 串行，每段只做一段 | **274.7s** | 73/73 | 慢 2.2× |
| **FlowEdge + DSH**（同一张图） | 引擎驱动 5 段，每段 `command` 验收 | **943.1s / 1039.1s** | 73/73 | 慢 7.6–8.3× |
| **FlowEdge + WorkBuddy** | 引擎 `agent_task` → WorkBuddy | — | — | **不可构建**（见 §四） |

**两个独立的开销，乘起来才是 8 倍**：

```
一把梭 124.7s
  × 2.2  ← 分段本身：每段重新建立上下文、重读规格、重启 agent
  ≈ 274.7s（WorkBuddy 分段，人代派发）
  × 3.6  ← 执行体差异：DSH 单段比 WorkBuddy 单段慢 3.6 倍
  ≈ 991s（FlowEdge 编排，DSH 执行）
```

**分段编排在这个任务上是负优化**：它把 5 段固定成本付了 5 次，而任务本身没有"必须分段"的依赖——一个 agent 顺着做完更省。

## 二、任务：exprkit（为什么它算"长流程复杂"）

五阶段严格依赖链，每阶段只依赖前一阶段的公开接口：

| 阶段 | 模块 | 交付 | 用例 |
|---|---|---|---|
| 1 词法 | `stage1_tokenize.py` | `Token` / `tokenize` | 13 |
| 2 语法 | `stage2_parse.py` | `parse` → AST（5 种节点、优先级 + 右结合 `^`） | 14 |
| 3 求值 | `stage3_eval.py` | `evaluate` / `EvalError` | 13 |
| 4 函数 | `stage4_funcs.py` | `FUNCTIONS` / `call` / `fold_constants` | 17 |
| 5 接口 | `stage5_cli.py` | `format_number` / `run` / `main` | 16 |

**全部 73 条从零实现**（基线 1/73，五个模块的函数都是 `NotImplementedError`）。

这不是"修 6 个 bug"，是从空文件写到能过 73 条断言——单段工作量比之前的 minipipe 大得多。

## 三、三组实测明细

### A. WorkBuddy 一把梭（子 agent，干净上下文）

**124.7s**，73/73，`cheated=false`。

一个 agent 从 `CONTRACT.md` 读到全量通过，中间自己做了 5 次阶段性自测。它的汇报里明确处理了几处容易错的点：`-2^2 == -4`（一元负低于 `^`）、`%.6f` 而非 `repr`（保证 `1/3 → "0.333333"`）、折叠与求值共用同一路径（避免漂移）。

### B. WorkBuddy 分段（5 个子 agent + 每段验收，人代派发）

**274.7s**，73/73，`cheated=false`。五段全部一次通过，每段验收只花毫秒级。

| 段 | agent 输出要点 | 本段验收 |
|---|---|---|
| s1 | 逐字符扫描，`3.` / `.5` / `1.2.3` 带位置抛错 | 13/13 |
| s2 | 递归下降 `expr→term→unary→power→primary` | 14/14 |
| s3 | `EvalError` 覆盖未定义变量/除零/未知节点 | 13/13 |
| s4 | 元数表驱动校验，折叠失败保留原节点不抛错 | 17/17 |
| s5 | 整数不带小数点、6 位四舍五入、`main` 退出码 | 16/16 |

**这 274.7s 里包含我作为派发者的工具调用开销**——因为 WorkBuddy 没有可被程序调用的入口（§四），派发只能由人代劳。所以它是"人机协作分段"的真实成本。

### C. FlowEdge + DSH（同一张 15 节点图）

```
start → prep(建副本)
      → s1(agent) → g1(command: 本段验收)   ← 不过就整图停
      → s2(agent) → g2
      → s3(agent) → g3
      → s4(agent) → g4
      → s5(agent) → g5
      → judge(全量) → if_else 门 → end-ok / end-fail
```

两轮：**943.1s**（原始）和 **1039.1s**（优化后），都是 73/73、`cheated=false`、五段全绿。

| 段 | 轮1 | 轮2（优化后） |
|---|---|---|
| s1 | 85s | 95s |
| s2 | 256s | 191s |
| s3 | 228s | 341s |
| s4 | 195s | 226s |
| s5 | 152s | 171s |
| 每段验收 g1–g5 | 3–7s | 3–6s |
| **合计** | **943.1s** | **1039.1s** |

**图优化试了什么、结果如何**：轮2 给每段提示词加了「上游接口摘要」，明确要求"不要读上游实现文件"。理论上能省 s2–s5 的文件阅读时间。
**实际没有收益**（中位 991.1s vs 943.1s，反而慢 5%），因为单段耗时波动（191–341s）远大于优化能省的量。

**结论**：这个任务上，提示词级微调淹没在模型波动里；要验证这类优化必须多次重复取中位数，单轮对比不可信。

## 四、为什么 FlowEdge + WorkBuddy 构建不出来（本轮新证据）

上一轮已确认 `-p` / `--acp`(stdio) / `--serve` 三条通道都不产出 agent 输出。本轮继续挖到了两层：

**1. `--bg` 能起 worker，但 worker 不干活**

`CODEBUDDY_FORCE_LITE_WB_BUNDLE=1 codebuddy --bg --name X -p "..."` 成功注册了一个新 worker：

```
pid=18864 kind=interactive cwd=... url=http://127.0.0.1:11578
```

它的 ACP 握手完整可用，而且 **`session/new` 返回了全新会话 ID**（`0c42a799-...`，不是用户的活动会话），`bypassPermissions` 生效——看上去一切就绪。

但 `session/prompt` 发出去后 **300 秒零事件**：没有 `agent_message_chunk`、没有 `tool_call`、没有文件产物。

**2. 能跑 agent 的只有 GUI 当前会话**

对比同一套 ACP 代码打到桌面端的 `8895`（`isCurrent=True`）：立刻出现真实 `tool_call`（`Write .../ai建模/tools/...`）。

区别只有一点：**8895 是 GUI 正在使用的会话，11578 是我无头起的**。

**所以 WorkBuddy 的 agent 执行能力绑在 GUI 会话上**。`--bg` 起的 worker 只是一个"收消息的空壳"：会话能建、消息能存、工具定义能加载（`usage_update` 里 `tools: 26772` tokens），就是不推理。

这也解释了之前 `--serve` 实例为什么"提交 202、会话有 `messageCount:1`、但 `requests: []`"——同一个原因。

**要让它成立，需要 WorkBuddy 侧提供「无头会话 + worker 执行」的入口**。FlowEdge 侧无法绕过，改成注册假 worker 也没用（`POST /api/v1/workers` 只是把外部地址加进注册表，执行能力仍在对方）。

## 五、分段编排什么时候才值得

这次五段全绿，所以分段唯一的收益（早停）没体现出来。它的价值在**失败场景**：

| 场景 | 一把梭 | 分段 + 每段验收 |
|---|---|---|
| 全绿（本次） | 124.7s | 274.7s（**慢 2.2×**） |
| 第 3 段就错 | 跑完全部才知道，≈124.7s 后发现 | 第 3 段验收立刻停，≈180s 就止损 |
| 最后一条错，重跑 | 整轮 124.7s 重来 | 只重跑最后一段 ≈50s |

**判据**：

- 单段能独立验收、且失败概率高 → 分段值得（早停 + 局部重跑）
- 任务依赖强、每段都要看全局 → 一把梭更省
- 执行体越慢，分段越亏（DSH 单段 3.6× 于 WorkBuddy，分段成本被放大）

本次 exprkit 属于第二类：五阶段虽然串行依赖，但每段都短（20–60 行），一个 agent 顺着写完比拆开更快。

## 六、公平性

| 项 | 状态 |
|---|---|
| 同一任务 | 同一份 fixture（73 条用例、基线 1/73），同一个判分器（哈希 + skip 扫描） |
| 信息对等 | 一把梭拿到"契约 + 全部测试"；分段每段拿到"契约 + 本段测试 + 上游接口摘要"。两边的信息量都是工程上合理的 |
| 干净上下文 | WorkBuddy 侧两个都是全新子 agent，不知道 fixture 的设计过程 |
| 作弊 | 三条全部 `cheated=false` |
| **已知偏差** | ① **WorkBuddy 分段是人代派发**（含工具调用开销），比全自动路径略慢；② **FlowEdge 侧执行体是 DSH 不是 WorkBuddy**（因为 §四），所以 991s 里同时含"分段成本"和"DSH 慢"两个因素，不能直接当作"FE 用 WorkBuddy 的成绩" |
| 样本量 | FE 两轮；WorkBuddy 两条路线各 1 轮（子 agent 每次耗时稳定在 100–280s，波动小于 DSH） |

## 七、复现

```bash
cd .tmp/expr-bench
python benchctl.py prepare                 # 建副本
python benchctl.py stage 3                 # 单段验收
python grade.py --workdir runs/solo/work   # 判分

python run_expr.py --agent dsh --label fe-dsh   # 引擎编排（五段）
# WorkBuddy 侧：派干净子 agent，prompt 见 s1..s5.txt / solo.txt
```
