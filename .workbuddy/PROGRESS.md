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
| B1 | P0 修缺陷 12 项 | | 33 → ? | |
| B2 | P1 事件日志+trace+token | | | |
| B3 | P1 重跑+队列+列表+JSONPath+CORS | | | |
| B4 | P2 检索（BM25/RRF/MMR/批量/引用/进度） | | | |
| B5 | P2 可选（元数据/rerank/PDF+DOCX） | | | |
| B6 | P3 注册表+代码节点 | | | |
| B7 | P3 可选（聚合器/credentials/插件） | | | |
| B8 | P4 旗帜 C1（VRAM 调度） | | | |
| B9 | P4 旗帜 C2/C3（ComfyUI/离线自证） | | | |
| B10 | P4 体验与分发 | | | |

## 关键决策与偏差

| 日期 | 决策 | 理由 |
|---|---|---|
| 2026-10-19 | 重复连线定为 **warn** 而非 error | 引擎对重复边能正常执行（propagate 逐边递减），设 error 会阻止导入历史数据；改为前端 `isValidConnection` 拦截新建 |
