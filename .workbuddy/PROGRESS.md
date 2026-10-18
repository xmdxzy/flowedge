# FlowEdge SOTA 冲刺 · 进度文件

> 用途：上下文压缩后恢复现场的唯一依据。每批完成后追加，不覆盖。
> 基线 tag：`baseline-pre-sota`（commit 4b098a3）
> 规划：`C:\Users\Administrator\.workbuddy\plans\toasty-vortex-tesla-Ppp9KoCy.md`

## 基线（2026-10-19 01:45）

| 项 | 值 |
|---|---|
| HEAD | `4b098a3` feat(engine): node-level cache |
| 工作区 | 干净 |
| `go vet ./...` | 通过（无输出） |
| `go test ./...` | ok（33 个测试） |
| `tsc --noEmit` | 通过 |
| `dist/FlowEdge.exe` | 19,207,680 B（18.3 MB） |
| Go | `C:\Users\Administrator\AppData\Local\Programs\Go\bin\go.exe`（**不在 PATH**，每次需用绝对路径或 export PATH） |
| 前端 node_modules | 已安装 |

## 批次记录

| 批 | 内容 | commit | 测试数 | 验收 |
|---|---|---|---|---|
| B1 | P0 修缺陷 12 项 | 13dcf32 | 33 → 47 | ✅ 全过（环/自环/悬空/菱形不误判、假数据清除、Ctrl+S、输出 Tab） |
| B2 | P1 生产化：事件日志+trace+token、重跑、队列、全局列表、JSONPath、CORS 收紧、前端事件流 | | 47 → 67 | ✅ 见下 |

## B2 验收记录（2026-10-19 02:40）

| 项 | 结果 |
|---|---|
| 测试 | 67 passed / 0 failed（+20：event log×5、rerun×3、queue×6、JSONPath×6） |
| 真机：成环保存 | 400 + `code=cycle`（B1 已验） |
| 真机：事件落库 | `node_started/node_finished` 写 `run_events`，seq 按 run 递增；`node_token` 不入库 |
| 真机：`/api/runs/:id/events` | 4 条（start/transform 各 2） |
| 真机：`/api/runs/:id/trace` | spans 含 node_id/type/status/duration_ms + usage |
| 真机：`/api/runs/:id/rerun-from` | t → 202；事件数 4 → 6（修复了续跑 runID=0 导致事件丢失的 bug）；坏节点 → 400 |
| 真机：`/api/runs?limit=` | total/limit/offset/runs 正常 |
| 真机：jobs 表 | 行落库、status/trigger 正确；`RecoverInterrupted` 已接线 |
| 真机：API token | 无 token 401 / health 200 / 带 token 200 |
| 真机：0.0.0.0 无 token | 拒绝启动（os.Exit(2)）并打印修复指引 |
| 踩坑修复 | ① `AutoMigrate` 漏了 `model.Job` → 补上；② 续跑 ctx 未带 runID → 事件静默丢失 → 新增 `ResumeFromWithRunID` |

## 关键决策与偏差

| 日期 | 决策 | 理由 |
|---|---|---|
| 2026-10-19 | 重复连线定为 **warn** 而非 error | 引擎对重复边能正常执行（propagate 逐边递减），设 error 会阻止导入历史数据；改为前端 `isValidConnection` 拦截新建 |
