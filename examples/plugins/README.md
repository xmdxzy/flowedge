# 插件示例

把某个目录整个复制到 `data/plugins/` 下就能用，**不需要重编译 exe**。

```bash
cp -r examples/plugins/text_stats  data/plugins/
# 重启引擎，节点面板里就会出现「文本统计」
```

## 两种插件形态

### 1. `http` 型 —— 任何语言都能写

`plugin.json`：

```json
{
  "name": "my_node",
  "title": "我的节点",
  "type": "http",
  "url": "http://127.0.0.1:9000/handle",
  "description": "一句话说明这个节点做什么"
}
```

引擎会 POST 到 `url`：

```json
{"input": { ...上游输出... }, "config": { ...节点配置... }}
```

端点必须返回一个 JSON object，它就是这个节点的输出。

### 2. `script` 型 —— 进程内解释执行（yaegi）

`plugin.json` 里写 `"type": "script"` 与 `"entry": "main.go"`，脚本必须定义：

```go
func Execute(inputJSON string, configJSON string) (string, error)
```

- 入参出参都是 **JSON 字符串**（不是 map）：跨解释器边界时用 map 类型对不上，
  每次都要反射搬一遍，出错信息还极难懂。
- 脚本里**不要写 import**：引擎注入白名单标准库
  （`strings` `strconv` `fmt` `sort` `math` `math/rand` `time` `encoding/json`
  `unicode` `bytes` `crypto/sha256` `regexp` `errors` `path`）。
- 写 `import "os"` 之类会被静态预检直接拒绝，节点拒绝加载。
- 返回的不是 JSON 时会被当成单个 `text` 字段。

## 字段说明

| 字段 | 说明 |
|---|---|
| `name` | 节点类型标识（字母数字下划线连字符），画布上按这个类型识别 |
| `title` | 显示名 |
| `description` | 显示在节点说明里，写「什么时候用它」 |
| `category` | 分类（`text` / `image` / `data` …） |
| `type` | `script` 或 `http` |
| `entry` | script 型的脚本文件名（默认 `main.go`） |
| `url` | http 型的端点 |
| `side` | 显存侧别：`diffusion` / `language` / `speech` / 空（不占显存） |
| `timeout_ms` | http 型超时，默认 30000 |

## 加载失败不会让引擎起不来

坏插件只在日志里记一行，其余插件照常加载。常见错误：

| 现象 | 原因 |
|---|---|
| `缺少 plugin.json` | 目录里没有描述文件 |
| `name "x y" 只能含字母数字下划线连字符` | name 会变成节点类型标识 |
| `脚本必须定义 func Execute(...)` | 签名不对，加载期就报，不等运行 |
| `import "os" 在代码沙箱中被禁止` | 黑名单包 |
| `http 型插件必须配 url` | 少 `url` |
