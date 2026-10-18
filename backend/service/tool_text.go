package service

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// 文本形态的工具调用解析
//
// 背景：并非所有 Ollama 模型模板都支持 OpenAI 的 tool_calls 字段。
// qwen2.5 系列会把「我要调用 calc，参数是 ...」写成 content 里的一段 JSON：
//   {"name": "calc", "arguments": {"expression": "(2+5)*2"}}
// 这里做一层兼容解析，让这类模型也能真正触发本地工具。

var (
	fencedJSONRe = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*?\\})\\s*```")
)

// parseTextToolCalls 从模型文本里提取第一个可识别的工具调用
func parseTextToolCalls(content string, specs []ToolSpec) []ToolCall {
	if len(specs) == 0 {
		return nil
	}
	allowed := make(map[string]bool, len(specs))
	for _, s := range specs {
		allowed[s.Function.Name] = true
	}

	candidates := []string{}
	if t := strings.TrimSpace(content); t != "" {
		candidates = append(candidates, t)
	}
	for _, m := range fencedJSONRe.FindAllStringSubmatch(content, -1) {
		candidates = append(candidates, m[1])
	}
	if start := strings.Index(content, "{"); start >= 0 {
		if end := balancedJSONEnd(content, start); end > start {
			candidates = append(candidates, content[start:end])
		}
	}

	for _, cand := range candidates {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(cand), &raw); err != nil {
			continue
		}

		name := extractToolName(raw)
		if name == "" || !allowed[name] {
			continue
		}

		args := "{}"
		for _, k := range []string{"arguments", "parameters", "args", "input"} {
			if v, ok := raw[k]; ok {
				args = string(v)
				break
			}
		}

		return []ToolCall{{
			ID:   fmt.Sprintf("textcall-%s", name),
			Type: "function",
			Function: struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			}{Name: name, Arguments: args},
		}}
	}
	return nil
}

// extractToolName 兼容 "name" / "tool" / "tool_name" / "function":{"name":...}
func extractToolName(raw map[string]json.RawMessage) string {
	for _, k := range []string{"name", "tool", "tool_name"} {
		if v, ok := raw[k]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil && s != "" {
				return s
			}
		}
	}
	if v, ok := raw["function"]; ok {
		var fn struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(v, &fn) == nil && fn.Name != "" {
			return fn.Name
		}
	}
	return ""
}

// balancedJSONEnd 从 start 的 '{' 开始找配对的 '}'（跳过字符串内的括号）
func balancedJSONEnd(s string, start int) int {
	depth := 0
	inStr := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}
