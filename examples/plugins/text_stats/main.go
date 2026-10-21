//go:build ignore

// 这个文件不是 Go 包的一部分（它由 yaegi 在运行时解释）：
// 加 build tag 让 go build / go vet / IDE 都不要把它当编译单元。
// 文本统计插件（script 型示例）
//
// 约定（改不了，改了就加载失败）：
//
//	func Execute(inputJSON string, configJSON string) (string, error)
//
// 入参出参都是 JSON 字符串，不是 map——map 跨解释器边界的类型对不上，
// 每次都要靠反射搬一遍，出错信息还极难懂。JSON 是最稳的边界。
//
// 脚本里**不要写 import**：引擎会注入白名单标准库
// （strings / strconv / fmt / sort / math / time / encoding/json / bytes /
// crypto/sha256 / regexp / errors / path / unicode）。
// 写 import "os" 之类会被静态预检直接拒绝。
//
// 返回 JSON 字符串；返回的不是 JSON 时会被当成单个 text 字段。
func Execute(inputJSON string, configJSON string) (string, error) {
	var in map[string]interface{}
	if err := json.Unmarshal([]byte(inputJSON), &in); err != nil {
		return "", err
	}
	text, _ := in["text"].(string)
	if text == "" {
		// 上游没给 text 时退回整个输入的 JSON 表示，别静默返回空
		text = inputJSON
	}

	chars := 0
	for range text {
		chars++
	}
	lines := 0
	nonEmpty := 0
	if text != "" {
		lines = 1
	}
	for _, line := range strings.Split(text, "\n") {
		if lines == 0 {
			lines = 1
		}
		if strings.TrimSpace(line) != "" {
			nonEmpty++
		}
	}
	if strings.Contains(text, "\n") {
		lines = len(strings.Split(text, "\n"))
	}

	out := map[string]interface{}{
		"chars":         chars,
		"lines":         lines,
		"non_empty":     nonEmpty,
		"text":          text,
		"preview":       firstN(text, 40),
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func firstN(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i] + "…"
		}
		count++
	}
	return s
}
