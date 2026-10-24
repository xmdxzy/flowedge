# Goal 引擎图 · 设计 v1

> 日期：2026-10-24 ｜ 状态：已落库到运行实例（#128 内层 / #129 外层），正在做冒烟验证
> 目标：把「一句话目标」变成一组可执行任务，**执行 → 质检 → 循环**，直到全部完成或明确阻塞。

---

## 0. 结论

1. **两条工作流，不是一条**：`Goal`（外层，管循环与终局）+ `Goal-Step`（内层，管单步执行与质检）。原因：FlowEdge 的 DAG 不允许环，`loop` 节点只能引用一条**已保存**的工作流作为循环体。
2. **质检门只裁决，不执行**：执行者（Executor）只做 `next` 指向的那一步；质检门（Judge）只判断「完成没有 / 下一步是什么」。两者都调 DSH，但角色严格分开。
3. **三个终态**：`DONE`（成功）、`BLOCKED`（明确阻塞）、`MAX_ITER`（预算耗尽）。只有 `DONE` 走成功终点；后两者必须让 run 失败，不能伪装成完成。
4. **状态在文本里传递**：每轮把状态包（JSON）作为下一轮输入；质检门输出新的状态包。不依赖 DSH 会话记忆。

---

## 1. 图结构

### 1.1 外层 `Goal`（#129）

```
start-1（{{goal}}）
  → init-1（code：把 goal 包成初始状态 JSON）
  → loop-1（mode=until，循环体=Goal-Step #128，until 输出含 GOAL_END，max 8 轮）
  → gate-1（if_else：输出含 "VERDICT: DONE"？）
       ├─ true  → end-ok（成功）
       └─ false → fail-1（command：echo GOAL_BLOCKED && exit 1）→ end-fail（失败）
```

### 1.2 内层 `Goal-Step`（#128）

```
start-1（循环注入的状态包）
  → exec-1（agent_task · dsh · 执行者）
  → judge-1（agent_task · dsh · 质检门）
  → normalize-1（code：规范化 VERDICT/STATUS）
  → end-1（把规范化后的文本交回 loop）
```

### 1.3 为什么要拆两层

| 约束 | 后果 | 设计 |
|---|---|---|
| DAG 禁止环 | 不能画 `judge → exec` 的回边 | 用 `loop.until` 包住内层工作流 |
| `loop` 只认 `workflow_id` | 循环体必须已保存 | 先建 #128，再建 #129 |
| 内层每次全新执行 | 状态不能靠内存 | 状态包以文本逐轮传递 |
| 外层要判「循环是正常停还是耗尽」 | 不能只看 `stopped_by` | 外层再判 `VERDICT: DONE` |

---

## 2. 状态包（State）

```json
{
  "goal": "用户目标原文",
  "tasks": [
    {"id": "T1", "desc": "可执行任务", "status": "done", "evidence": "文件/命令输出"}
  ],
  "next": "T2",
  "workspace": "C:/.../goal-runs/xxx",
  "iteration": 2,
  "history": ["T1: 已创建 ...", "T2: 测试失败 ..."]
}
```

规则：

- `init-1` 产出初始状态：`tasks=[]`、`next="把目标拆成 3-7 个可执行任务，并执行第一步"`、`iteration=0`。
- 执行者第一次运行时负责**拆任务**（3–7 个），然后只执行 `next` 指向的第一步。
- 质检门更新 `tasks[].status/evidence`，给出新的 `next`，并递增 `iteration`。
- `STATE:` 行必须是**单行 JSON**；执行者下一轮只认这一行。
- `workspace` 由调用方通过 `vars` 传入；同一 workspace 同一时间只跑一个 Goal。

---

## 3. 三个角色

### 3.1 执行者（exec-1）

- 节点：`agent_task`，`agent_id=dsh`，`timeout_ms=900000`。
- 只做 `next` 指向的一步；不重新规划整个目标；不改验收标准。
- 所有产物写 `workspace`；用可预测文件名；不写 workspace 之外。
- 结束时给：做了什么、证据（命令/文件/测试输出）、风险。
- 不承诺可复现（`agent_task` 本身标记 `Nondeterministic`）。

### 3.2 质检门（judge-1）

- 节点：`agent_task`，`agent_id=dsh`，`timeout_ms=600000`。
- 不执行、不改文件；只判断。
- 判定规则：
  1. 逐条核对 `tasks`；全部 `done` 且有证据才 `DONE`。
  2. 证据不足、结果不一致、产物缺失、测试失败 → `CONTINUE`，给唯一 `next`。
  3. 同一个 `next` 连续两轮无新证据 → `BLOCKED`。
  4. 不信「我完成了」这句话，只信证据。
- 输出最后四行：`ANALYSIS / VERDICT / NEXT / STATE`。

### 3.3 状态规范化（normalize-1）

- 节点：`code`（yaegi Go），纯字符串处理，无外部依赖。
- 作用：
  1. 取**最后一条** `VERDICT:`，归一化为 `DONE / CONTINUE / BLOCKED`。
  2. `DONE/BLOCKED` → `STATUS: GOAL_END`；其余 → `STATUS: GOAL_CONTINUE`。
  3. 删除原始 `VERDICT/STATUS` 行再重写，避免质检门在分析里提到 `GOAL_END` 造成误停。
- 没有 `VERDICT` 时默认 `CONTINUE`（fail-safe：宁可多跑一轮，也不提前宣布完成；由 `max_iterations` 兜底）。

---

## 4. 终止、失败与预算

| 情况 | 判定 | 外层走向 | run 状态 |
|---|---|---|---|
| 全部任务完成且有证据 | `VERDICT: DONE` + `STATUS: GOAL_END` | `end-ok` | success |
| 明确阻塞（缺依赖/冲突/两轮无进展） | `VERDICT: BLOCKED` + `STATUS: GOAL_END` | `fail-1` exit 1 | **failed** |
| 达到 `max_iterations` 仍未 DONE | 最后一条 `STATUS: GOAL_CONTINUE` | `fail-1` exit 1 | **failed** |
| 执行者/质检门节点失败 | loop 节点失败 | 下游 skipped | failed |
| 循环节点超时 | loop 节点失败 | 下游 skipped | failed |

**硬规则**：只有 `VERDICT: DONE` 能到 `end-ok`；`GOAL_END` 只表示「循环可以停」，不等于「目标完成」。这两件事必须分开，否则 `BLOCKED` 会被误判成成功。

预算：

- `max_iterations=8`（可调；硬上限 100）。
- 单轮执行 15 min、质检 10 min。
- 循环总超时 8 h（需要引擎补丁，见 §6）。
- workspace 每轮复用；执行者和质检门共享同一个工作目录，但质检门被要求不改文件。

---

## 5. 提示词（已落库）

### 5.1 执行者

```
你是 Goal 引擎的【执行者】。你只执行当前这一步，不重新规划整个目标，也不修改验收标准。

状态包（JSON；若为空或仍是占位符，说明调用方没传 goal，直接输出 BLOCKED）：
{{start-1.text}}

规则：
1. 读状态包的 goal / tasks / next / workspace。
2. tasks 为空时，先把 goal 拆成 3-7 个可执行任务（每个任务有 id/desc/status/evidence），然后执行 next 指向的第一步。
3. 只做 next 指向的那一步；不要顺手做后面的步骤。
4. 所有产物写到 workspace 下，用可预测的文件名；不要写 workspace 之外的路径。
5. 不要伪造结果；没做完、做失败、缺依赖，都如实写。
6. 结束时给：做了什么（≤5 行）、证据（命令/文件/测试输出，≤20 行）、风险（≤3 行）。
7. 不要提问，直接动手。
```

### 5.2 质检门

```
你是 Goal 引擎的【质检门】。你不执行任务，只判断目标是否完成、下一步做什么。

原始状态包：
{{start-1.text}}

执行者本轮报告：
{{exec-1.text}}

规则：
1. 逐条核对 goal 下的可执行任务；只有全部完成且有证据才 DONE。
2. 证据不足、结果不一致、产物缺失、测试失败 → CONTINUE，并给出唯一 next。
3. 同一个 next 连续两轮没有新证据 → BLOCKED。
4. 不要相信「我完成了」这句话，只信证据（文件/命令输出/测试结果）。
5. 你不执行任务、不改文件。
6. 如果执行者报告里没有可验证的证据，判 CONTINUE，并明确要它补哪条证据。

输出纯文本，最后四行严格如下（不要 markdown 代码块）：
ANALYSIS: <≤5 行>
VERDICT: DONE | CONTINUE | BLOCKED
NEXT: <CONTINUE 时唯一可执行动作；DONE/BLOCKED 写 ->
STATE: <单行 JSON：{"goal":"...","tasks":[...],"next":"...","workspace":"...","iteration":N,"history":[...]}>
```

---

## 6. 引擎前置条件（本轮实测发现）

| # | 问题 | 证据 | 处理 | 状态 |
|---|---|---|---|---|
| 1 | `loop` 契约没声明 `until` | 任务书输出「循环方式 = until（只能取：count / while / foreach）」 | `nodecontract.go` 的 Options 改为 `count/foreach/until`，补 `condition_type`/`compare_value` | ✅ 源码已改，待重编译 |
| 2 | `loop` 默认超时 3 分钟 | run #145 在 3m0s 被 `node timeout after 3m0s` 杀掉，当时 `exec-1` 还在调 DSH | `nodespec.go` 给 loop `MaxTimeout: 8h`；节点 `timeout_ms=28800000` | ✅ 源码已改，当前二进制仍按 30min 上限 |
| 3 | 循环体必须是已保存工作流 | `loop` 只读 `workflow_id` | 先建 #128，再建 #129 | ✅ 已落库 |
| 4 | 内层 `start` 必须是 nodes[0] | `executeInnerWorkflow` 只覆盖 `sub[0]` 的 prompt | 生成时固定顺序 | ✅ |
| 5 | 没有 `goal/workspace` 变量 | start 的 `{{goal}}`、workdir 的 `{{workspace}}` | 运行 API 的 `vars` 注入；文档写死必填 | ✅ |
| 6 | DSH 是外部智能体 | `data/agents.json` 的 `dsh` | 使用 `agent_task`，不是 `llm` 节点 | ✅ |
| 7 | **内层工作流的 `{{workspace}}` 不会被 vars 替换** | `renderNodeVars` 只处理外层图；内层由 `executeInnerWorkflow` 从 DB 读，不经过 vars。run #146 的 `exec-1` cwd 是字面量 `运行\{{workspace}}` | 内层 `workdir` 改静态目录；每轮 workspace 由 `STATE.workspace` 指定 | ✅ 已改 #128 |
| 8 | **多行 prompt 被 cmd.exe 截断** | `dsh_agent.py` 用 `cmd /c dsh ... "<prompt>"` 传参；cmd 按换行切命令。run #146 执行者只收到第一段（状态包/7 条规则全丢），靠读 run 事件自救 | `dsh` 改为 `dsh_adapter.py` + `stdin: "{{prompt}}"`，prompt 走 stdin 不走 argv；`POST /api/toolchain/reload` 热生效 | ✅ 已改并 reload |
| 9 | **第 2 轮 exec 收到 Ctrl+C（0xC000013A）** | run #148/#149 第 2 轮 `exec-1` 退出码 3221225786 | adapter 改为 node 直跑 `@deepseek-ai/dsh/lib/bin.js`（绕开 `cmd /c` 包装）；多行 prompt 落 `.adapter_prompt.md`，argv 只传单行指令 | ✅ run #151 双轮通过 |

> **当前运行实例是旧二进制**：源码已补 loop 8h 上限与 `until` 契约，但要让 UI/任务书生效，需要重新构建并重启引擎。重建前，loop 仍受 30 分钟总上限约束。

---

## 7. 创建与运行

### 7.1 已落库

| 工作流 | ID | 名称 |
|---|---|---|
| 内层 | **128** | Goal-Step · 单步执行 + 质检（内部） |
| 外层 | **129** | Goal · 目标引擎（执行→质检→循环直到完成） |

对应的可版本化文件：

- [goal.json](../../data/workflows/goal.json)
- [goal-step.json](../../data/workflows/goal-step.json)
- 重建脚本：[create-goal-workflows.py](../../scripts/create-goal-workflows.py)
- DSH 执行器：[dsh_adapter.py](../../scripts/dsh_adapter.py) + [dsh-model.yml](../../scripts/dsh-model.yml)（`运行/data/agents.json` 的 `dsh.args` 指向它；改动前备份为 `agents.json.bak-goal-20261024`）

> `agents.json` 的改动是**运行期配置**，已通过 `POST /api/toolchain/reload` 热生效；不需要重启引擎。

### 7.2 运行（API）

```bash
curl --noproxy '*' -X POST http://127.0.0.1:8080/api/workflows/129/run \
  -H "Content-Type: application/json" \
  -d '{
    "invoked_by": "manual",
    "vars": {
      "goal": "在 workspace 下创建 goal-smoke.txt，内容只写 hello goal；然后确认文件存在并输出它的内容。",
      "workspace": "C:/Users/Administrator/Desktop/flowedge/.tmp/goal-workspace"
    }
  }'
```

### 7.3 运行（UI）

- 画布打开 #129，点运行前**先把 `start-1` 的 `{{goal}}` 改成本次目标**；`init-1` 会默认 workspace 到 `.tmp/goal-workspace`。
- 要保留模板，复制一份再改，不要直接改 #129。

### 7.4 查看

- 运行状态：`GET /api/runs/:id`
- 事件：`GET /api/runs/:id/events`
- 任务书：`GET /api/workflows/129/spec?format=md`

---

## 8. 验收用例

| # | 目标 | 期望 | 判据 |
|---|---|---|---|
| A | 单文件创建（本轮的 smoke） | 1 轮 DONE | `goal-smoke.txt` 存在且内容正确；run success |
| B | 3 个有依赖的小任务 | 2–3 轮 DONE | 三个任务都有 evidence；run success |
| C | 缺依赖（要求读一个不存在的文件） | BLOCKED 或 MAX_ITER | run **failed**，不是 success |
| D | 执行者故意说「完成了」但没产物 | 质检门 CONTINUE，补证据 | 不出现假 DONE |
| E | 不传 `goal/workspace` | 明确失败/阻塞 | 不产生空跑成功 |

**反作弊**：质检门只信证据；工程类目标应在内层加一个 `command` 验证节点（跑测试/构建），把 `exit code + stderr` 喂给质检门。v1 先做 LLM 质检，v2 加确定性验证。

### 8.1 验证记录（实跑）

| Run | 时间 | 结果 | 关键事实 |
|---|---|---|---|
| #145 | 07:53–07:56 | **failed** | loop 节点默认 180s 超时，`exec-1` 还在调 DSH 就被杀；`node timeout after 3m0s` |
| #146 | 08:01–08:09 | 手动取消 | 执行者只收到 prompt 第一段（cmd 换行截断）；它从 run 事件里读回状态，仍创建了 `goal-smoke.txt`（10B，`hello goal`）；但质检门同样会被截断 |
| #147 | 08:12–08:18 | failed（按设计） | 循环跑完，但 judge 输出「判定：PASS」而非 `VERDICT: DONE`，normalize 默认 CONTINUE；跑满 8 轮后 `fail-1` 触发。暴露：verdict 归一化要认 PASS/中文，judge 要强制格式 |
| #148 | 08:24 | **failed** | 第 2 轮迭代 `exec-1` 以 `0xC000013A`（Ctrl+C）退出；DSH 连续调用的进程组/会话问题 |
| #149 | 08:27–08:28 | **failed** | 第 1 轮成功，第 2 轮 `exec-1` 同样 `0xC000013A`；确认不是图逻辑，是子进程生命周期 |
| #150 | 08:30–08:31 | **success** | 1 轮 DONE；文件 10B `hello goal`；judge 独立 hex/sha256 核验；`stopped_by=condition` |
| #151 | 08:37–08:39 | **success** | 2 轮：第 1 轮 `a.txt`（A）→ CONTINUE，第 2 轮 `b.txt`（B）→ DONE；`iterations=2`，`stopped_by=condition` |
| #152 | 08:43–08:44 | **success** | adapter 移入 `scripts/` 后再跑 1 轮：DONE，`iterations=1`，产物正确 |

**结论**：Goal 图的结构、循环、判据、终局全部跑通；**单轮 #150 与双轮 #151 均成功**。多轮连续 DSH 调用最初在第 2 轮出现 `0xC000013A`，已通过「node 直跑 `bin.js` + 多行 prompt 落文件」修复；仍建议加 Windows Job Object 做进程树兜底。

**#146 的两个副产物**：

- 内层 `workdir="{{workspace}}"` 未被替换（vars 只作用于外层图），cwd 落到 `运行\{{workspace}}`；已改为静态目录。
- `dsh_agent.py` 的 argv 传参在 cmd.exe 处被换行截断；已切到 `dsh_adapter.py` + stdin，并 `POST /api/toolchain/reload` 热生效。

> 这两个都不是「模型不行」，是**调用链的形状不对**——正好说明为什么 Goal 必须先设计图、再谈模型。

---

## 9. 已知限制

1. **没有确定性验证**：v1 的质检门是 DSH 的 LLM 判断；它可能被漂亮的汇报骗过。工程任务必须尽快加 `verify_command`。
2. **没有逐轮检查点**：`loop` 每次从内层开始重新跑；引擎崩溃会丢失当前轮，外层 `resume` 会重跑整个 loop，而不是从第 N 轮继续。
3. **workspace 手动隔离**：同一 workspace 并发跑两个 Goal 会互相覆盖；v2 用 run-id 自动派生目录。
4. **DSH 会话不复用**：每轮都是新会话，上下文全靠状态包；状态包过长会触发适配器的 prompt 文件回退。
5. **成本/延迟**：每轮 2 次 DSH 调用；8 轮最坏 16 次。复杂目标应把 `max_iterations` 调到 12–16，并观察 DSH 的 429/超时重试。
6. **`fail-1` 是 Windows 命令**：`cmd /c echo GOAL_BLOCKED && exit 1`；跨平台要换 `sh -c`。
7. **UI 契约**：源码已补 `until`，但旧二进制里任务书仍会显示「只能取 count/while/foreach」；重编译后消失。
8. **状态包仍走 prompt 文本**：stdin 解决了 cmd 截断，但长状态仍有长度/转义/可审计性问题。v2 应把 `STATE` 落盘到 `workspace/.goal/state.json`，prompt 只传路径；judge 读文件、写文件，执行者也读文件。这同时解决「崩溃后续跑」。
9. **多轮连续 DSH 调用**：修复前第 2 轮 `exec` 会以 `0xC000013A`（Ctrl+C）退出（run #148/#149）；已改为 node 直跑 `bin.js` + 多行 prompt 落文件，run #151 双轮通过。仍建议加 Windows Job Object 兜底，确保每轮结束没有残留 node 子进程。

---

## 10. 下一步（按优先级）

0. **DSH 进程隔离加固（进行中）**：已用 node 直跑 `bin.js` 绕过 cmd 包装，并把 `dsh_adapter.py` / `dsh-model.yml` 移入 `scripts/`（run #152 通过）；剩余：加 Windows Job Object 兜底，确保每轮结束无残留 node。
1. **重编译引擎**，让 loop `MaxTimeout=8h` + `until` 契约生效；重启前按 AGENTS §0.12⑥ 确认托管服务恢复路径。
2. **跑 A/B/C 三个验收用例**，记录每轮耗时、DSH token、证据质量。
3. **加 verify 节点**：内层改为 `start → exec → verify(command) → judge → normalize → end`；`verify` 跑 `verify_command`（来自 vars），把退出码/stderr 拼进 judge 输入。
4. **状态落盘 + 逐轮检查点**：把 `STATE:` 写到 `workspace/.goal/state.json`，并在每轮结束追加 `history`；崩溃后从最后状态续跑，而不是重跑整个 loop。同时把 prompt 里的状态文本降为「读这个文件」。
5. **接本地模型**：`agent_id` 从 `dsh` 换成 Bonsai 的本地 agent 包装后，Goal 图不变——这正是「图与模型解耦」的价值。

---

## 附录 · 关键实现位置

| 能力 | 文件 |
|---|---|
| loop until / 条件判断 | `backend/service/engine_v2_nodes.go:1095`、`:823` |
| loop 契约 | `backend/service/nodecontract.go:216` |
| loop 超时 | `backend/service/nodespec.go:49` |
| agent_task | `backend/service/agent_exec.go:279` |
| 变量注入 `{{key}}` | `backend/handler/vars.go:31` |
| 节点变量 `{{node.field}}` | `backend/service/variables.go:26` |
| 工作流创建/运行 | `backend/handler/workflow.go` |
| 运行事件 | `backend/service/event_log.go` |
