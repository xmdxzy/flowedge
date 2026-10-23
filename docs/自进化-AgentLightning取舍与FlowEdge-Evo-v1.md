# Agent Lightning 评估 → FlowEdge 自进化工具 v1

> 2026-10-23。评估对象：`microsoft/agent-lightning` v1.0。
> 落地代码：`backend/service/evolution.go`（内核）、`backend/handler/evolution.go`（REST）、
> 自检 `scripts/verify-evolution.py`。
> 引用口径：本文所有"官方说了什么"都指到官方文档页原文；未经原文支持的一律标注"推断"。

## 0. 结论先行

1. **Agent Lightning 的架构可以借，算法不能借。** 它靠 `verl + vLLM` 做 policy 梯度更新，
   前提是"有可训练的权重 + rollout 便宜到能跑几千次"。这两条在 24G 卡 + 分钟级出片上都不成立。
   照搬会得到一个"永远攒不够样本"的训练器。
2. **它最有价值的不是 RL，是那条纪律：训练与推理用同一个 agent、同一套代码，采集挂在旁路上。**
   这条 FlowEdge 已经在做（`evidence.go`），而且做得更细——门是节点级输出，奖励自带归因。
3. **FlowEdge 真正缺的是它的反面：reward 与 agent 在同一套文件里，于是"自进化"最先学会的是改门阈值。**
   所以这次落地的核心不是梯度，是**四道防自欺的门**（燃料门 / 防作弊 / 投影分级 / 配对判定）。

## 1. Agent Lightning v1.0 是什么（按官方原文）

| 说法 | 原文出处 |
|---|---|
| 三组件：API Gateway（存 rollout/endpoints/events，兼 OpenAI 兼容代理）、Rollout Controller（起 agent 进程或 K8s Job）、Customized Trainer（GPU 侧推理与优化） | docs `05-basics`：「Agent Lightning v1.0 consists of three main components…」 |
| rollout = 一次输入上的一次 agent 执行，四态 `QUEUING/RUNNING/SUCCEEDED/FAILED` | 同上：「A rollout is one execution of an agent on one input」 |
| 事件 append-only：`model_request`（prompt/response token ids + logprobs）、`reward`、自定义 | 同上 |
| **优势在 rollout 层级算**，不是细到单次调用 | 同上：「computes advantages at the rollout level, and supports rollout-level loss normalization」 |
| 合并连续调用仅在 token 历史"完全连续"时 | 同上 |
| 零代码改动：agent 只需把 OpenAI 端点指向 Gateway | 同上：「connects an existing agent… without requiring changes to the agent's interaction loop」 |
| 官方**没有**声明"哪些层冻结/可训练"，也**没有**给成本与样本效率数字 | 同一页缺失；`Basics` 页顶部为 dev 文档 |

**推论（必须标明是推论）**：v1.0 的 credit assignment 是"整条轨迹一个 advantage + GRPO 多 rollout 对比"，
不存在"把 reward 精确分摊到第 3 次调用"的机制。网络上流传的"hierarchical credit assignment"属于 v0.x 概念，
v1.0 文档未出现 —— 按 §0.12① 的纪律，不在设计里引用它。

## 2. 取舍矩阵

| 内核 | 照搬？ | 落到哪里 | 理由 |
|---|---|---|---|
| 三组件分离（采集 / 执行 / 学习） | ✅ | 采集=`evidence.go`，执行=`runs`，学习=`evolution.go` | 职责分离本身与训练栈无关 |
| 旁路采集、agent 零改动 | ✅（已在） | 收尾写 `data/evidence/run-<id>.json` | 拦截点从 LLM 代理换成节点收尾：不走 chat/completions 也采得到 |
| rollout 与版本绑定 | ✅（已在） | `Evidence.WorkflowVersion` | 没有它只能按时间窗猜"改前改后"，最容易得出反向结论 |
| 多臂对比（同输入多 rollout 比 reward） | ✅ | 同 caseId 的 baseline/candidate 配对 | 与 GRPO 同构，比的是门分不是梯度 |
| **policy 梯度更新** | ❌ | 换**文本梯度**：改参数值/提示词，用门验证 | 无可训练权重、无 GPU 训练预算、样本量差 3 个数量级 |
| **海量免费 rollout** | ❌ | 换**三级投影**：L0 结构分（免费）→ L1 干跑 → L2 真渲 | 一次 L2 是分钟级且抢显存，"闭眼跑一千次"不存在 |
| **无限并发 attempt** | ❌ | 串行 + 已有 `governor_gate` 资源准入 | 显存单侧驻留，并发是资源问题 |
| ——**反向补一条**—— | ➕ | **reward hacking 防线** | 它的 reward 在 agent 之外（外部测试用例）；FlowEdge 的门在同一张图里，不加锁就等于让考生改分数册 |

## 3. 落地架构

### 3.1 五个端点 = 闭环五步

```
GET  /api/evolution/status    现在漏在哪、燃料够不够（只读）
POST /api/evolution/propose   该改哪一处（只读，产候选不落盘）
POST /api/evolution/audit     这个改法是不是在改评分标准（dry-run）
     ↓ 真跑复用已有 POST /api/evidence/trial（含快照 + 结构守卫 + 可选取跑，不新增写图路径）
POST /api/evolution/decide    改了到底有没有更好（配对判定）
```

**为什么最后一步不自动晋升**：判定链路再严，也只是"规则更严的自我评价"。
引擎该做的是把结论做到"人只需要看一眼"，不是替人按下晋升。

### 3.2 四道门（这是全部价值所在）

| 门 | 实现 | 拦什么 | 失效后果 |
|---|---|---|---|
| **燃料门** | `InspectFuel` | 无门运行 / unscored>20% / 可配对 case<8 / 类别<2 | 样本不够也出结论 → 把小样本噪声当进化 |
| **防作弊** | `AuditVariant` | 改门判据值 / 删门节点 / 删判据键 / 节点数变少 | 进化收敛到"最松的门"，分数涨、质量不变 |
| **投影分级** | `ProjectionFor` | 会改渲染成本的参数（steps/fps/seconds/model…）必须 L2 | 用免费指标冒充"已验证" |
| **配对判定** | `Decide` | 缺 caseId 不配 / 任一侧 null 作废 / 口径缩小 / fail 级回退 / token 涨 / 耗时涨>10% / 胜少负多 | 均分上涨只靠一个 case，其余变差 |

### 3.3 判据 vs 产出参数（最易写错的一处）

`gateParamKeys` 是一张扁平表，`loudness_min_db`（门上的判据）和 `loudness`（后期上的归一目标）
**都命中 `loudness`**。一刀切会得出"只要名字在门词汇表里就一律不许调"，
那样连唯一有标准答案的改进（响度归一到 -14 LUFS）都会被自己拦掉。

所以 `Knob` 分两位：

| 字段 | 含义 | 谁用 |
|---|---|---|
| `GateLocked` | **门节点上**的判据 → 永不自动调 | `AuditVariant` 硬拦 |
| `GateRelated` | 门层词汇但长在**产出节点**上 → 可调，须留名 | 只给 `warnings` |

`Propose` 会主动把规则表里"顺带改门下限"的编辑摘掉，并记进 `stripped` —— 摘掉不是隐藏。

### 3.4 状态机（与序策同构，不自造第二套）

`proposed → evaluated → promoted / rejected → rolled_back`

少一档 `shadowing`：FlowEdge 的"影子"就是在同一批 case 上再跑一次真 run，
与 `evaluated` 是同一个动作，多一档只会让人停在中间态。

## 4. 一个不能回避的现状

真机扫描（2026-10-23，13 条工作流含验证图）：

```
就绪度：1/13 条工作流接了门（覆盖 8%）
结论：门覆盖率不到一半：能进化的只有那几条图，其余改动全是盲改
```

**真实 12 条工作流里，一条 `quality_gate` 都没有。** 也就是说：

- 归因的输入（`GateChecks`）在生产图里根本不存在；
- `data/evidence/` 至今为空；
- 现阶段讨论"用 RL 还是文本梯度"没有意义 —— 先要有尺子。

这不是本方案的缺陷，是本方案**第一个要报出来的事实**。任何自进化系统在没有门的情况下，
都只会把随机改动包装成"优化"。

## 5. 验收判据（已验 / 未验分开）

### 5.1 已验证（真机，隔离实例 18080 + 独立 db 副本）

`python scripts/verify-evolution.py --base http://127.0.0.1:18080 --workflow-id 112` → **6/6 通过**

| 断言 | 结果 |
|---|---|
| status 可访问并给就绪度 | 1/13，覆盖 8% |
| 可调面非空 | 10 个旋钮 |
| 门判据标为不可调 | 4 个 locked（min_frames / loudness_min_db / max_silence_gap / require_audio） |
| 改门判据被拒 | `rejected`，理由「从 -20 改成 -40 —— 改评分标准不算改进」 |
| from 不一致被拒 | `rejected`（乐观锁生效） |
| decide 可判定 | `promote`：10 对、10 胜 0 负、门分 +0.333、token -1.1%、耗时 -3.2% |

另有 4 类 hack 拦截（放宽阈值 / 删门节点 / 删判据键 / 关 require_audio）与
7 类判定（inconclusive、promote、token 涨、口径缩小、fail 级回退、胜少负多、null 排除）
由 `backend/service/evolution_test.go` 11 个用例覆盖，`go test ./service ./handler` 全绿。

### 5.2 **未验证**（不许当已完成读）

| 项 | 为什么没验 | 怎么验 |
|---|---|---|
| 真渲一次的门分变化 | 需 120s+/段 + 17GB 显存，且当前生产图无门 | 先给一条真图接 `quality_gate`，跑 baseline/candidate 各 8 个 case |
| 文本梯度（LLM 生成变体） | 本次只落了规则候选 + 结构，未接 LLM | 由 `agent_task` 读归因生成变体，再走 audit → trial → decide |
| 生产 8080 引擎上的接口 | 外壳（30000）未运行，按纪律不重启常驻服务 | 下次启动即生效（`dist/FlowEdge.exe` 已构建） |
| 回归监测 / 自动回滚 | 未做 | 晋升后按周统计 `CR_clean` 与门分分布，跌破基线即 `rolled_back` |

## 6. 下一步（按性价比排序）

| 优先级 | 事项 | 为什么排这个位置 | 验收判据 |
|---|---|---|---|
| **P0** | 给主流程接 `quality_gate`（至少 1 条主力出片图） | 没有门，后面全是空转 | 该图跑一次后 `data/evidence/run-*.json` 里 `gateChecks` 非空 |
| **P0** | 评测层注入 `caseId / role / category` | 缺 caseId 永远凑不出配对 | `GET /api/evolution/status?workflowId=N` 的 `pairableCases ≥ 8` |
| P1 | 接文本梯度：LLM 读 `attribution` 生成 N 个提示词变体 | 规则表只有 4 条，覆盖不了提示词类改进 | 变体过 `audit` 无 hack；`decide` 能给出 promote/keep |
| P1 | 晋升落库 + 回归监测（`rolled_back`） | 现在结论只在响应里，不留痕 | 晋升后 7 天门分跌破基线自动打回 |
| P2 | 前端「进化」面板 | 接口已齐，缺可视化 | 一屏看到：门覆盖率 / 失分排行 / 候选 / 判定 |

## 7. 一句话

Agent Lightning 教的是"**怎么把采集与学习解耦**"，不是"**怎么用 RL 训练 agent**"。
FlowEdge 的可训练面不是权重，是提示词与参数；它的学习信号不是梯度，是门。
所以这次借的是骨架，扔的是优化器，补的是**它不需要但我们必须有的那一道防自欺的门**。

---

# v2 · 闭环补全（2026-10-23 下午）

v1 只做到「读证据 → 给结论」。v2 把它闭成一条能自己跑起来的环，并把 v1 里
标着"未验证"的四项中的两项做实（生产端点已生效、文本梯度已接模型），
真渲级门分与前端面板仍留待下一步。过程中抓到 **3 个真 bug**，每一个都会让整套
自进化在真机上静默失效。

## 8. 新增端点（v1 的五个 → 十一个）

| 端点 | 干什么 | 关键纪律 |
|---|---|---|
| `POST /api/evolution/instrument` | 给没有门的图接一条 `quality_gate` | 默认 dry-run；**只能接在终点前**；阈值从图里的生成参数反推 |
| `POST /api/evolution/evaluate` | 按 case 列表 + 指定版本图跑配对（异步串行） | 用**内存图**跑历史版本，不动当前图 |
| `GET /api/evolution/batch` | 一批评测的进度与逐 run 结果 | `scored` 与 `done` 分开报 |
| `POST /api/evolution/variants` | 文本梯度：模型读归因产变体 | 过了门锁 + 审计才叫 accepted |
| `GET /api/evolution/monitor` | 晋升后回归体检，给回滚目标版本 | 只有一侧证据 → `insufficient`，不猜 |
| `POST /api/evolution/rollback` | 按版本号回滚 | 默认 dry-run；**回滚前再快照一次**，滚错能滚回 |

## 9. 三个真 bug（都会让自进化在真机上静默失效）

### 9.1 门判不过 → 被记成「没测到」（最严重）

`execQualityGateNode` / `execConsistencyGateNode` 判不过时返回 `(nil, err)`，
引擎在失败分支只写 `node.Error`，把产物丢掉。而证据层的判据是
**「失败的门有没有留下 checks」**（`gateFacts`）：
有 checks = 测了不合格（`failed`），没 checks = 门自己崩了（`unscored`）。

于是每一次"质量确实不合格"都被记成 `unscored`：

- 燃料门看到 `unscored` 占比高 → 判定"数据不够，别下结论"；
- 归因看到的是"门在崩"而不是"质量在漏"；
- 评测口径直接反向（这正是证据信封最想防的那件事）。

**修法**：失败时保留 `out`（引擎一个 `if out != nil`），两个门判不过时返回
带 `checks` 的产物。真机验证：candidate 侧两例 `result=failed` 而不是 `unscored`。

### 9.2 版本号在收尾时读「当前最新」

原来写完证据才 `ev.WorkflowVersion = latestVersion(...)`。运行期间有人改图并
存版本，一次旧图的运行会被记到新版本上 —— 版本号看着都对，事后查不出来。
**修法**：起跑时钉进 `workflow_runs.workflow_version`，信封只搬不改。

### 9.3 门不能插在中间（设计约束，读代码才看得见）

引擎的 `upstream` 只来自**直接前驱**。把门插在生成节点与消费节点之间，
下游拿到的是门的输出 `{gate, checks, path}` 而不是 `{video: ...}`，整条图报
「缺产物」。所以 `instrument` 只把门接在 `end` 之前：`end` 不消费产物，
门失败整条 run 记 failed —— 验收照样有牙齿，产物传递一点不动。

## 10. 真机验证（隔离实例 18080 + db 副本，未动生产）

`python scripts/verify-evolution-loop.py --base http://127.0.0.1:18080` → **29/29 通过**

这条脚本自己建图、真起跑、真回滚（用 `input_source` 接本地 mp4，不占显存）：

```
[A] 接尺子   门接在终点前 / min_frames=117 由 124 帧 ×0.95 反推 / 落图前有快照
[B] 跑配对   baseline 2/2 过门，candidate 2/2 判不过且 scored=2、result=failed
[C] 判定     pairableCases=2、两类类别、decide=keep_baseline（worse=2）
[D] 盯回归   monitor=regressed（rollbackTo=v1）→ rollback 先快照 v2 再回滚，阈值回到 1
```

`python scripts/verify-evolution.py --workflow-id 121` 仍 **6/6**（v1 的牙齿没被改坏）。

**文本梯度真调模型**（Ollama 本机，单次约 60s）：模型提出"把 `min_frames` 调小"，
被审计整条拒绝 —— `全部编辑都在改门判据（data.min_frames）：这是改评分标准`。
这是防作弊线在真模型上的第一次实战命中。

**生产库接门普查（dry-run，只读，15 条工作流）**：

| 分类 | 条数 | 明细 |
|---|---|---|
| 可自动接门 | **11** | `修士斗法·30秒成片`、`多模态演示`、`still-a`×2、`still-b`、`音频验收｜分镜2镜`、`音频验收｜分镜直填`、`音频修复验证｜单镜`、`ref2va｜念稿验证`、`语音闭环｜合成→转写`、`station-selftest-tts` |
| 已有门 | 1 | `canonical 出片`（cg-1 + qg-1） |
| 接不了 | 3 | `MOSS 文字渲染` 没有 `end` 节点；`lemo-opuscar 链路验证`、`station-selftest-llm` 上游无生成参数（接空门不如不接） |

接上之后门覆盖率 1/15（7%）→ 12/15（80%）。修士斗法的反推值：
`min_frames=117`（124 帧 ×0.95）、`min_width=736`、`min_height=416`、
`duration 4.91–5.43s`（124÷24fps=5.17s ±5%）。

> **没有替你改生产图。** 接门会改变线上行为（从"跑完就算成"变成"过门才算成"），
> 属于破坏性变更，需要一次明确确认。确认后一条命令：
>
> ```
> curl -X POST 127.0.0.1:8080/api/evolution/instrument \
>   -H 'Content-Type: application/json' -d '{"workflowId":39,"apply":true}'
> ```
>
> 落图前会自动存快照，回滚用 `POST /api/evolution/rollback {"workflowId":39,"toVersion":N,"apply":true}`。

## 11. 仍未验证（不许当已完成读）

| 项 | 原因 | 怎么验 |
|---|---|---|
| **真渲级**门分（120s/段、17GB 显存） | 本轮刻意用 mp4 夹具，不抢生产显存 | 给主力图接门后，baseline/candidate 各跑到 ≥8 对 |
| ~~生产 8080 上的新端点~~ | **已生效**（实例 13:25 重启后跑的是新代码，`/evolution/batch`、`/monitor` 均返回 400 参数校验，`instrument` 对 wf 39 dry-run 正常） | — |
| 前端「进化」面板 | 本轮只做接口 | 一屏看：门覆盖率 / 失分排行 / 待判定 |
| 晋升记录持久化 | monitor 靠证据里的 role+版本，没有单独晋升表 | 若要审计"谁在哪天按的晋升"，再加表 |

## 12. 闭环现在的样子

```
instrument（接尺子）
   → evaluate ×2（baseline / candidate，注入 caseId+role+版本）
   → status（燃料够不够 + 漏在哪）
   → propose / variants（规则候选 + 文本梯度）
   → audit（改评分标准一律拒）
   → trial（快照 + 写图 + 可选取跑）
   → decide（配对判定：promote / keep_baseline / inconclusive）
   → 人按晋升（引擎不自动）
   → monitor（退化就报，给回滚目标版本）
   → rollback（先快照再回滚，可再滚回）
```

每一环都有"数据不够就不下结论"的出口：没有门 → 燃料门拦；没有 role/caseId →
配对数为 0 → inconclusive；门崩了（没 checks）→ unscored 而不是 failed。

---

# v3 · 生产落图（2026-10-23 13:50）

上一节末尾问的那件事，已经做了。落图过程中抓到第 4 个真 bug，也第一次真用上了
回滚。

## 13. 落图结果

| 项 | 数 | 明细 |
|---|---|---|
| 落图接门 | **10** | wf 2 多模态演示 · wf 39 修士斗法 · wf 47 音频修复 · wf 48 音频验收｜分镜2镜 · wf 50 ref2va · wf 56/57 still-a · wf 58 still-b · wf 74 station-selftest-tts |
| 撤回 | 1 | wf 3 语音闭环｜合成→转写 |
| 未动 | 4 | wf 113 已有 2 门 · wf 49 本就不可执行 · wf 112/73 上游无可反推参数 · wf 116 无 end 节点 |

门覆盖率 **1/15（7%）→ 10/15（67%）**。每条落图前都自动存了快照
（`workflow_versions` v1 = 接门前的原图），回滚一条命令。

## 14. 第 4 个真 bug：阈值反推漏乘 segments

```
v.frames = 124        // 单段帧数
v.segments = 6        // 段数
```

`deriveGateData` 只读了 `frames`，把 124 当成成片帧数：
`duration_max = 124 ÷ 24 × 1.05 = 5.43s`。

而 run#49 的真实产物是 **1484 帧 / 31.05s** —— 门第一次跑就会把一条合格成片判成
「时长超标」。**这比不接门更糟**：它让门看起来在工作，实际在乱杀，而且乱杀的理由
（5.43s 上限）在界面上看着还挺合理。

**修法**：反推时 `frames × segments`，并把插帧（`interp=true` 会让实测帧数≈2×）
写进注释——帧数只取下界（宁松不误杀），时长才是强判据（插帧不改时长）。
落地单元测试 `TestInstrumentGates_MultipliesSegments` 钉住 706 / 29.45 / 32.55。

**生产图已就地修正**（`spec/apply` + `allowGateChange` + 逐条 reason）：
`min_frames 117→706`、`duration 4.91–5.43 → 29.45–32.55`。

## 15. 阈值不是拍脑袋：每条门都用历史产物复核过

落图后拿每张图**最近一次成功运行的真实产物**跑 ffprobe，与门阈值对账：

| 工作流 | 实测（历史产物） | 门阈值 | 结论 |
|---|---|---|---|
| 修士斗法 | 736×416 / 1484 帧 / 31.05s / 有音轨 | 736×416 / ≥706 帧 / 29.45–32.55s | PASS |
| 音频修复·单镜 | 608×352 / 124 帧 / 5.17s | 608×352 / ≥117 / 4.91–5.43s | PASS |
| ref2va·念稿 | 608×352 / 107 帧 / 4.46s | 608×352 / ≥101 / 4.24–4.68s | PASS |
| 多模态演示 | 768×768 / 4.0s / 有音轨 | 768×768 + 必带音轨 | PASS |
| still-a ×2 · still-b | 768×1024 / 1 帧 | 768×1024 | PASS |
| station-selftest-tts | 音频 5.89s | 必带音轨 | PASS |
| 音频验收｜分镜2镜 | 无成功历史 | 608×352 | 待首次真跑 |
| canonical 出片 | 无成功历史 | 五层门（既有） | 待真渲级评测 |

这张表是这件事唯一值得信的部分：**门的阈值必须能被历史产物复核**，
否则「接了门」只是接了一个会随机杀片的裁判。

## 16. 撤回的那一条：wf 3 的上游是文本

wf 3 的终点前驱是 `asr`，输出只有 `{text}`（见 `variables.go` 的
`defaultOutputFields`）。`pickArtifactPath` 只认 `video/audio/image`，
门接上去必然报「缺少输入产物」—— 一条能跑的链路会被改成永远失败。

用 `POST /api/evolution/rollback {"toVersion":1,"apply":true}` 撤回，
快照留下 v2（带门版本），这一次回滚也是回滚路径在**生产图**上的第一次实战。

> 若将来要量这条链，正确做法不是把门插在 asr 后面，而是让门取 `tts` 的音频
> 输出（另开一条支路），因为「合成→转写」这段的产物本来就是文本。

## 17. 落图之后，自进化长什么样

- 生产跑一次 → 门自动产出 `gateChecks` → evidence 里有 `result=passed/failed`
  （不再是 unscored）→ `status` 的失分归因第一次有东西可归。
- 生产跑**没有** `caseId/role`，所以能归因、不能配对。要做「改前改后」判定，
  仍需评测层用 `POST /api/evolution/evaluate` 起配对跑（那条路径注入 caseId+role）。
- `instrument` 的 segments 修正在 `dist/FlowEdge.exe` 里，**下次引擎启动生效**；
  已落图的 10 条不受影响（阈值已手工修正并被历史产物复核过）。

## 18. 仍未验证（v3 状态）

| 项 | 原因 | 怎么验 |
|---|---|---|
| 门在生产真跑里的第一次判决 | 需要一次真实渲染（H3/flux 抢显存），未获授权 | 下次出片后看 `data/evidence/` 里 `gateChecks` 非空 |
| 真渲级配对评测 | 同上 | `evaluate` 跑 ≥8 对 |
| wf 48 / canonical 的阈值 | 无成功历史可对账 | 首次成功跑后重跑本文 §15 的复核 |

# v4 · 晋升台账 + 进化面板（2026-10-23 傍晚）

v3 留了两件事：闭环里「人按下晋升」这个动作只存在于对话里（不可审计），
以及四个端点只有 API 没有界面。本节补这两块。

## 19. 晋升台账（promotions 表 + 两个端点）

```
POST /api/evolution/promote     人按下晋升或保留（唯一能写 action=promote/keep 的地方）
GET  /api/evolution/promotions  台账查询（谁在哪天凭什么判定了哪个版本）
```

记录形状（`backend/model/promotion.go`）：`action`（promote/keep/rollback/archive）
× `source`（promote/decide/monitor/rollback）分开记 —— 审计时既能看到
「引擎当时怎么判」（archive，actor=engine），也能看到「人最后按的什么」
（promote/keep，actor 可署名）。

**三条校验牙齿**（`service.ValidatePromotion`，单测钉住）：

| 规则 | 对应的事故 |
|---|---|
| 空理由不许落库 | 台账退化成日记，「当时凭什么」一栏永远空着 |
| promote/rollback 必须给 toVersion | 记了「晋升」却不知道升到哪版，复核要翻遍所有版本 |
| action/source 必须在枚举内 | 台账里混进看不懂的记录，审计时要猜 |

**decide/monitor 结论归档**：两个端点加 `record` 参数（decide body / monitor query），
归档为 `action=archive`。判 promote 也只记 archive —— 引擎说 promote 是「建议」，
不等于人按了，那一下必须走 promote 端点。

**回滚自动落库**：`rollback apply=true` 成功后写一条（from=快照版本号，
即回滚前状态的精确存档）。归档失败只警告不翻账 —— 图已经改完了，
把成功响应改成 500 会让人以为没滚、反而再滚一次。

## 20. 前端「进化」面板

`frontend/src/components/EvolutionPanel.tsx` + `src/lib/evolution.ts`，
顶栏「进化」按钮 / 命令面板进入。一屏五区：燃料（门覆盖率 + 就绪度）→
失分排行（按 lossShare 排序）→ 候选（from→to + 依据）→ 判定（配对数值）→
晋升台账（晋升/保留表单，理由必填）。

配色全部走主题令牌（`--fe-*-soft/line` 成对），组件里零颜色字面量，
深浅主题各截一张验过（`outputs/ui-theme-2026-10-23/`）。

## 21. 第 5 个真 bug：omitempty 炸掉整棵 React 树

真机截图时抓到：选中工作流后面板白屏，控制台一句
`Cannot read properties of undefined (reading 'length')`。

根因：Go 的 `omitempty` 把空 slices 整个省掉（`fuel.blockers`/`fuel.categories`
不在 JSON 里），nil slice 序列化成 `null`（`decision.reasons`）。
面板按「完整形状」直接 `.length`/`.map` → 抛异常 → 没有错误边界，
整棵树卸载。

修法：三处全部 `?? []` 兜底 + 类型改成可选；**回归测试用真实最小形状**
（故意不写这些键）钉住。这条与 §9 的「门失败丢 checks」是同一类病：
**后端「空」与前端「无」不是一回事**，跨语言边界上默认值必须显式。

## 22. 验证状态（v4）

| 项 | 状态 |
|---|---|
| 服务单测（record/list/latest/validation/归档映射） | 6 用例全过 |
| 处理器单测（坏 action 400 / 缺理由 400 / 404 / dry-run / 隔离 / decide 归档） | 7 用例全过 |
| 前端单测（含 omitempty 回归） | 113 用例全过（+1 回归） |
| `tsc --noEmit` / `npm run build` | 干净 |
| 后端全量 `go test ./...` | 全过 |
| 隔离实例端到端（`verify-evolution-loop.py`，44 断言） | 44/44 过（含台账四类记录齐全） |
| 深浅主题截图 | `outputs/ui-theme-2026-10-23/` 三张 |
| 门在生产真跑里的第一次判决 | 仍未验（同 §18：需要一次真实渲染授权） |
