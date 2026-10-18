# FlowEdge SOTA 冲刺 · 进度文件

> 用途：上下文压缩后恢复现场的唯一依据。每批完成后追加，不覆盖。
> 基线 tag：`baseline-pre-sota`（commit 4b098a3）
> 规划：`C:\Users\Administrator\.workbuddy\plans\toasty-vortex-tesla-Ppp9KoCy.md`

## 最终状态（2026-10-19 04:00）

| 项 | 基线 | 现在 |
|---|---|---|
| HEAD | `4b098a3` | `925a56d` |
| Go 测试 | 33 | **91（0 失败）** |
| `go vet ./...` | 通过 | 通过 |
| `tsc --noEmit` | 通过 | 通过 |
| `dist/FlowEdge.exe` | 18.3 MB | **31.2 MB**（+12.9MB 来自 yaegi stdlib 符号表） |
| 节点类型 | 13 | 14 + 3 插件 |
| API 端点 | 33 | 39 |

## 批次记录

| 批 | 内容 | commit | 测试数 | 验收 |
|---|---|---|---|---|
| B1 | P0 修缺陷 12 项 | 13dcf32 | 33 → 47 | ✅ 环/自环/悬空/菱形不误判、假数据清除、Ctrl+S、输出 Tab |
| B2 | P1 生产化：事件日志+trace+token、重跑、队列、全局列表、JSONPath、CORS 收紧、前端事件流 | bcd6ecc | 47 → 67 | ✅ 见下 |
| B4 | P2 检索：BM25+RRF、MMR、min_score、批量持久化、结构化引用 | e6b03f1 | 67 → 76 | ✅ 中文 bigram / 双路融合 / df bug 修复 |
| B6 | P3 注册表 + code 节点（yaegi）+ capabilities 下发 node_types + 前端 CodeNode | 8f0b04f 区间 | 76 → 86 | ✅ code E2E `HELLO-OK` |
| B8 | C1 显存调度：引用计数 / 真实卸载 / 串行门 / 时间线 | 56625f9 | 86 → 90 | ✅ 含自死锁回归测试 |
| B9 | C2/C3：ComfyUI 模板化 / 多图视频 / FFMPEG_PATH / 环境驱动置灰 | c199df1 | 90 | ✅ |
| B10 | 分发收口：BaseDir 修复 / 插件接线 / electron-builder 配置 / README / SOTA 对照表 | 925a56d | 91 | ✅ |

## B2 验收记录（02:40）

| 项 | 结果 |
|---|---|
| 事件落库 | `node_started/node_finished` 写 `run_events`，seq 按 run 递增；`node_token` 不入库 |
| `/api/runs/:id/events` | 4 条（start/transform 各 2） |
| `/api/runs/:id/trace` | spans + usage |
| `rerun-from` | t → 202；事件数 4 → 6；坏节点 → 400 |
| API token | 无 token 401 / health 200 / 带 token 200 |
| 0.0.0.0 无 token | 拒绝启动（os.Exit(2)） |
| 踩坑 | ① `AutoMigrate` 漏 `model.Job`；② 续跑 ctx 未带 runID → 事件静默丢失 |

## B6/B8/B10 关键修复

| 问题 | 根因 | 修法 |
|---|---|---|
| code 节点 `undefined: strings` | yaegi 的 `Use` 只注入符号表，包名在解释器作用域内不可见 | 在 Eval 的源码里显式写白名单 `import (...)`，且 import 必须在所有其他声明之前 |
| `expected declaration, found result` | yaegi 顶层只接受声明 | 把用户代码包进 `func __flowedge_run() interface{}` 函数体 |
| 既有 26 个引擎测试全挂 | 测试用 `&EngineV2{}` 字面量绕开构造函数，注册表为 nil | `executeNode` 懒装配 + 双重检查锁 |
| GPU 调度器自死锁 | `AcquireModel` 持锁时调 `GetGPUInfo`（内部再 Lock），`sync.Mutex` 不可重入 | 新增 `gpuInfoLocked` |
| BM25 idf 变负 | df 按出现次数累计而非文档数，`N-n+0.5` 可为负 | df 按文档去重计数 |
| data 写到 `C:\data` | `BaseDir` 用「路径含 Temp」判断 go run，误伤临时目录下的真实 exe | 只认 `go-build` 特征；加回归测试 |

## 关键决策与偏差

| 日期 | 决策 | 理由 |
|---|---|---|
| 2026-10-19 | 重复连线定为 **warn** 而非 error | 引擎对重复边能正常执行；设 error 会阻止导入历史数据；改为前端 `isValidConnection` 拦截新建 |
| 2026-10-19 | 层显存预算串行化 → 改为**文生图串行门** | 拆层会把 GPU 节点与下游混进同一轮，正确性风险高；串行门达到同样「不 OOM」目标且零风险 |
| 2026-10-19 | **跳过 App.tsx 拆分** | 纯工程整洁度，stale closure 回归风险高（编译能过、功能可能是死的），收益不匹配风险 |
| 2026-10-19 | 遥测不可用时不阻断执行 | 单机单卡场景宁可让它跑，也不要因探测失败让整个工作流失败 |
| 2026-10-19 | yaegi 换 +12.9MB 体积 | 换来「纯 Go 代码节点 + 零外部依赖」，符合单 exe 交付红线；已记录在对照表 |
