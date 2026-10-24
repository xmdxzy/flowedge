# WorkBuddy 可编程性排查（决定路线 ② 能否成立）

> 日期：2026-10-23 · 目标：让 FlowEdge 的 `agent_task` 能真正驱动 WorkBuddy 完成代码任务
> 结论：**找到了完整的技术通路，但它绑定在用户的活动会话上，不能用于隔离评测**

## 一、五条通道的最终结论

| 通道 | 结果 | 关键证据 |
|---|---|---|
| `codebuddy -p "..."` | **无产出** | exit 0、stdout/stderr 全空（node 22 / 24 同） |
| `codebuddy --serve` | **服务可用，但没有执行体** | 消息提交返回 202、会话被创建（`messageCount:1`），但 `/history` 的 `requests: []`，没有 agent 输出 |
| `codebuddy --acp`（stdio） | 无回包 | `initialize` 发出后 40s 无任何响应 |
| `codebuddy --bg` | 无产出 | 与 `-p` 相同，`codebuddy ps` 挂住 |
| **桌面端 worker（8895）** | **可驱动，但不能用于评测** | ACP 完整握手 + `session/prompt` 成功，agent 真的调了工具（见 §三） |

## 二、serve 模式的完整技术细节（已打通，但缺执行体）

以上都是可以直接复用的：

1. **选对 bundle**：`bin/codebuddy` 是 launcher，本机 `dist/` 只有 `codebuddy-lite-wb.mjs`，
   必须设 `CODEBUDDY_FORCE_LITE_WB_BUNDLE=1` 才不会报 `Cannot find module '../dist/codebuddy'`。
2. **启动**：`--serve --port <p> --auth password --permission-mode bypassPermissions`
   （`--auth none` 被忽略，但启动输出会打印 `Password  <43 字符>`）。
3. **鉴权**：`Authorization: Bearer <Password>`。注意 query / cookie / `X-Password` 全部 401，
   只有 Bearer 有效 —— 401 变 404 就是认证通过、路径不对的信号。
4. **路由**：前缀是 `/api/v1/`（不是 `/api/`）。实际存在的：
   `health` / `info` / `sessions` / `jobs` / `workers` / `runs` / `runs/:id` / `runs/:id/stream` / `acp` / `acp/connect` / `process/*` / `fs/*`
5. **提交任务**：`POST /api/v1/runs`，body 是「通用消息」：
   ```json
   {"version":"1.0","id":"<uuid>","type":"message",
    "source":{"platform":"generic","sender":{"id":"x"},"conversation":{"id":"<uuid>","type":"direct"}},
    "payload":{"text":"..."},"timeoutMs":120000}
   ```
   返回 `202 {"data":{"runId":"<server uuid>","status":"accepted"}}`（**用服务端返回的 runId 查**）。
6. **ACP over HTTP**：`POST /api/v1/acp/connect` → `{connectionId, sessionToken}`；
   之后每个 JSON-RPC 请求都要带 `Accept: application/json, text/event-stream`
   + `acp-connection-id` + `acp-session-token` 两个头，响应是 SSE。
   `initialize` / `session/new` / `session/prompt` 全部 200，`bypassPermissions` 生效。
   **但 serve 实例没有 worker，prompt 只更新会话标题，不产生 `agent_message_chunk` 或 `tool_call`。**

## 三、桌面端 worker（8895）能真干活，但用错了地方

- 8895 接受**同一个 Password**（CLI 打印的那个），`/api/v1/health` 200。
- ACP 握手 + `session/prompt` 成功，返回里出现真实的
  `agent_message_chunk`、`agent_thought_chunk`、`tool_call`（`Write .../ai建模/tools/ue_mh_age_probe.py`）。
- **但它的 `session/new` 没有新建会话**：返回的 sessionId 恒为
  `97920e84-c50f-4f7f-a597-30b70352787e`，也就是你当前正在用的那个会话（cwd `桌面\ai建模`）。
- 后果：我那条 "在当前目录建 hello.txt" 的测试 prompt 进了你的活动会话，
  并且我收到了那个会话的 20031 条历史消息（包含你正在做的 UE MetaHuman 年龄相关讨论）。

**这属于污染用户工作区，不可接受**。已立即停止对 8895 的一切调用，未再发送任何消息。
`wb-worker-work` 目录是空的 —— agent 没有在我的目录里干活，它在你的 cwd 里工作。

## 四、这对路线 ② 意味着什么

要么「不隔离」——用你的活动会话跑评测，会覆盖你的 cwd、打乱你的上下文，而且 sessionId 无法指定；
要么「隔离」——但隔离出来的 serve 实例没有执行体。

**两条路都走不通**。路线 ② 作为「引擎调用 WorkBuddy 完成一个隔离的代码任务」在这台机器上不成立，
缺的是 WorkBuddy 侧一个「能为外部调用者新建独立会话并带 worker 的执行入口」。

## 五、可行的替代（供决策）

| 方案 | 做法 | 代价 |
|---|---|---|
| A | ② 标 N/A，只测可测的三格（①③④），并把 ④ 换成**干净上下文的子 agent**（消除出题人信息优势） | 四格缺一，但每格都可信 |
| B | ② 用「引擎 + 桌面端会话」跑，接受污染你的 cwd 与会话历史 | 数据能拿到，但会弄乱你的工作区，且不可复现 |
| C | 继续攻通道：找 WorkBuddy 是否有其它能新建 worker 的入口（如 `--serve` 的 worker 相关开关、或改打包产物补 full bundle） | 时延不确定，可能需要动安装目录 |

## 六、已产出的可复用件

`.tmp/pipeline-bench/` 下：
- `probe_wb_http.py`：抓 Password + 试鉴权方式
- `probe_wb_api2.py`：列 `/api/v1/*` 端点 + 提交消息
- `probe_acp_stream.py`：ACP over HTTP 完整握手（serve 实例）
- `probe_wb_worker.py`：驱动桌面端 worker（**已停用**，会污染活动会话）

---

# 第二轮：为什么无头 worker 发了 prompt 却零事件（定案）

> 2026-10-24 04:40–05:45 · 目标：把「零事件」这个现象追到机制层，而不是停在"不可用"。

## 一、直接原因：会话级写者独占

```
GET /api/v1/sessions/live        → {"sessionId":"0c42a799-...","writerOccupied":true}
GET /api/v1/traces               → status:"running", spanCount:749（桌面端正在跑用户的活）
```

**`writerOccupied: true`** —— 这个会话被另一个 worker 独占写入。
往它发 `session/prompt`，消息只能排队，而队列的消费者是占着它的那个 worker，
它优先跑自己的请求。所以我的 prompt 既不执行也不报错，只是安静地挂着。

## 二、第二层：`session/new` 根本不新建会话

之前我以为客户端解析错了 sessionId。这轮把 SSE 响应完整摊开：

```
session/new 响应（2 条消息）
  id=None | notify | params.sessionId=0c42a799-... | update=config_option_update
  id=2    | result | result.keys=configOptions,models,modes,multitaskOn,sessionId
                   | RESULT.sessionId=0c42a799-...
```

**result 里就是 `0c42a799`**，也就是「评估后继续」那个会话（217 条消息，属于用户）。
它返回的是「最近的会话」，不是新会话——`result.keys` 里有 `configOptions/models/modes`，
这是一组"打开会话"的返回，不是"创建会话"。

所以链路是：`session/new` 给你当前会话 → 你往里发 prompt → 它被 writer 占用 → 排队。

## 三、第三层：`--bg` 起 worker 依赖 host，且没有隔离办法

| 尝试 | 结果 |
|---|---|
| `--bg`（共享 config，桌面端有活跃会话） | 成功起 worker（url 11578），但绑的仍是当前会话 |
| `--bg`（`CODEBUDDY_CONFIG_DIR` 指向空目录） | **完全静默**，`--bg` 不产出任何输出，`codebuddy ps` 也挂住 |
| `--bg`（共享 config，桌面端 worker 全退之后） | **静默失败**，没有新监听端口，日志为空 |

`CODEBUDDY_CONFIG_DIR` 本身是生效的（`--serve` 的 `Config` 行确实指到了新目录），
但换到空目录后 `--bg` 就不工作——因为它需要向 host 的 registry 注册自己，
而 registry 在共享配置里。

**桌面端 worker 全部退出之后，`--bg` 也起不来了**。这是最硬的一条证据：
无头 worker 不是独立进程，它寄生在 GUI host 上。

## 四、完整的依赖链（这就是"为什么打不通"）

```
桌面端 GUI (Electron host)
   └── 会话窗口 → 启动 CLI 进程 → 该进程起 HTTP server (如 8895) 并注册为 worker
                                    └── agent 执行（模型调用走 host 的通道）
无头路径（-p / --serve / --bg / ACP）
   ├── 都没有自己的"会话窗口"
   ├── session/new 只能返回 host 已有的会话
   ├── 那个会话被 host 的 worker 以 writer 独占
   └── 于是：消息能收、会话能查、工具 schema 能加载，就是不推理
```

**结论**：WorkBuddy 的 agent 执行能力**架构上绑定 GUI host + 活动会话**。
不是少一个开关、不是鉴权问题、不是模型凭证问题（`failover.json` 的 `lastSuccessAt`
就在我发 prompt 的同一分钟，通道是通的）。

要打通，需要 WorkBuddy 提供「无头会话 + 与之绑定的 worker 执行」入口。这不在 FlowEdge 能改的范围内。

## 五、给评测的落地方案

既然引擎无法直接调 WorkBuddy，长流程对比采用**结构等价**的两条：

| 路线 | 做法 | 实测 |
|---|---|---|
| WorkBuddy 一把梭 | 1 个干净子 agent 做完整任务 | 124.7s / 73 条 |
| WorkBuddy 分段 | 按引擎图结构，逐段派子 agent + 每段验收 | 274.7s / 73 条 |
| FlowEdge 编排 + DSH | 同一张 15 节点图，执行体换成可调用的 DSH | 943–1039s / 73 条 |

分段那条就是「FE+WorkBuddy」的结构等价物：图结构、验收点、信息粒度完全一致，
**唯一差别是派发由人完成**（因为引擎调不动 WorkBuddy）。这个差别已计入 274.7s 的耗时里。

## 六、副作用记录

为验证隔离，我 `taskkill` 了自己用 `--bg` 起的那个 worker（端口 11578，PID 18864），
没有动桌面端会话进程。但排查过程中桌面端的 CLI worker（8895 / 5246）在某个时点自行退出，
**疑似用户在 GUI 侧关闭了会话窗口**——`WorkBuddy.exe` 主进程仍在，重开会话窗口即可恢复。
