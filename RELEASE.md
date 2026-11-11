# 发版怎么做

目标是两件事：别人 clone 下来能跑，下载到的 exe 开起来是对的。
每一步都要有能贴出来的结果，不接受「应该没问题」。

## 1. 先看工作区

```bash
git status --short
```

只应该有你要提交的改动。`data/` 里别有 `.bak`、`.stale`、`.log` 这类跑测试落下的东西。
版本号要对齐：CHANGELOG 顶上的标题、`frontend/package.json` 的 version、这次要打的 tag。

## 2. 全量自检

```bash
python scripts/check-all.py --full
```

最后一行要是 `RESULT: ALL-PASS`。前端有 60 多个用例，后端 500 多个，跑满大概两分钟。

再扫一遍仓库，确认没把个人信息带出去：

```bash
python scripts/privacy-scan.py
```

## 3. 构建

```bash
python scripts/build-windows.py --version 2026.11.11
```

出来是 `dist/FlowEdge.exe`（20 MB 上下）和 `dist/data/pipelines.json`。
`dist/` 不进仓库，只作为 Release 附件。数据文件必须跟 exe 一起发，少了引擎直接退出。

## 4. 拿产物冒烟

```bash
python scripts/release-check.py --expect 2026.11.11
```

它会在临时目录里真起一个实例，然后断言 20 项：健康检查、版本号、壳不缓存、favicon 的 MIME、
哈希产物 immutable、缺失资源返回 404、前端深链、11 条流水线的 id、未知 API 返回 JSON。
任何一项不过就退出码 1，别发。

想再看一眼界面，手动做三步就够：

1. 打开首页，切一次主题，刷新，偏好还在。
2. 进画布，从模板新建，跑一次干跑，看体检结论说的是不是人话。
3. 进运行，点一条历史记录，状态、耗时、节点表都在。

DevTools 控制台要是干净的。有报错先修。

## 5. 提交、打 tag、写 Release

```bash
git add -A
git commit -m "release: v2026.11.11"
git tag -a v2026.11.11 -m "FlowEdge v2026.11.11"
git push origin main --tags
```

Release 说明照这个写：

```markdown
## 这版改了什么
- 三条以内，说用户能感觉到的变化

## 怎么升级
- 下载 FlowEdge.exe 和 data/pipelines.json，替换旧 exe，保留原来的 data/ 目录
- 旧版本建的工作流和产物还能用；pipelines.json 有改动时必须一起换

## 已知问题
- 没修完的、验证不充分的写在这里
```

附件放 `FlowEdge.exe` 和 `data/pipelines.json`，需要的话再加 `FlowEdge-客户端.bat`。

## 6. 发完十分钟内

找个空目录解开 exe，双击能起、浏览器能开，确认不是本机缓存撑着。
`/api/health` 的 version 是这次 tag。下载链接能点开。Release 里没混进数据库和片子。

## 每次都要确认不发的东西

`运行/`、`作品/`、`outputs/`、`deliveries/`、`exports/`、`.tmp/`、`data/flowedge.db`、
`data/*.bak-*`、`logs/`、`node_modules/`、`frontend/dist/`，还有本地运行时目录和内部档案
（`.gitignore` 最后两段列的就是它们）。

这些都在 `.gitignore` 里，但 `git add -A` 之前还是看一眼 `git status`。
