package service

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// 变量系统：让节点能引用任意已完成节点的输出
//
// 语法：{{nodeId.field}}，例如 {{llm-1.text}}、{{rag-1.sources}}、{{http-2.status_code}}
//
// 设计约束：
//   - 只替换「X.Y」形式且 X 确实是已完成节点 ID 的引用；{{input}} 之类的占位符
//     原样保留，交给 data_transform 的模板渲染处理，避免两套语法打架
//   - 节点数据是共享结构，resolveData 返回深拷贝，不污染持久化的定义
//   - 未知节点 / 未知字段保持原文（便于用户一眼看出哪里没取到值）

var varRefRe = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_-]+)\.([A-Za-z0-9_.]+)\s*\}\}`)

// resolveData 深拷贝节点数据并替换其中的变量引用
func resolveData(data map[string]interface{}, ctx map[string]interface{}) map[string]interface{} {
	if len(data) == 0 || len(ctx) == 0 {
		return data
	}
	out := make(map[string]interface{}, len(data))
	for k, v := range data {
		out[k] = resolveValue(v, ctx)
	}
	return out
}

func resolveValue(v interface{}, ctx map[string]interface{}) interface{} {
	switch val := v.(type) {
	case string:
		return resolveString(val, ctx)
	case map[string]interface{}:
		m := make(map[string]interface{}, len(val))
		for k, sub := range val {
			m[k] = resolveValue(sub, ctx)
		}
		return m
	case []interface{}:
		arr := make([]interface{}, len(val))
		for i, sub := range val {
			arr[i] = resolveValue(sub, ctx)
		}
		return arr
	}
	return v
}

// resolveString 替换字符串中的 {{nodeId.path}}
func resolveString(s string, ctx map[string]interface{}) string {
	if !strings.Contains(s, "{{") {
		return s
	}
	return varRefRe.ReplaceAllStringFunc(s, func(match string) string {
		parts := varRefRe.FindStringSubmatch(match)
		if len(parts) != 3 {
			return match
		}
		nodeID, path := parts[1], parts[2]
		out, ok := ctx[nodeID]
		if !ok {
			return match // 该节点尚未完成：保留原文
		}
		if val, found := lookupOutputField(out, path); found {
			return val
		}
		return match
	})
}

// lookupOutputField 按点号路径从节点输出里取值并转成字符串
func lookupOutputField(v interface{}, path string) (string, bool) {
	var cur interface{} = v
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return "", false
		}
		cur, ok = m[seg]
		if !ok {
			return "", false
		}
	}
	switch x := cur.(type) {
	case string:
		return x, true
	case nil:
		return "", true
	case bool:
		return strconv.FormatBool(x), true
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10), true
		}
		return strconv.FormatFloat(x, 'f', -1, 64), true
	case int:
		return strconv.Itoa(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprintf("%v", x), true
		}
		return string(b), true
	}
}

// AvailableVariables 描述某个节点可引用的上游变量（供前端提示，不参与执行）
type AvailableVariable struct {
	NodeID string `json:"node_id"`
	Type   string `json:"type"`
	Path   string `json:"path"` // 建议引用写法：{{id.text}}
	Label  string `json:"label"`
}

// defaultOutputFields 各节点类型的常用输出字段（用于前端变量提示）
var defaultOutputFields = map[string][]string{
	"start":          {"text"},
	"llm":            {"text"},
	"rag_query":      {"text"},
	"http_request":   {"text", "status_code"},
	"data_transform": {"text"},
	"if_else":        {"text", "branch"},
	"text2image":     {"image", "text"},
	"asr":            {"text"},
	"tts":            {"audio", "text"},
	"video_compose":  {"video"},
	"loop":           {"text", "all", "iterations", "stopped_by"},
	"sub_workflow":   {"text"},
}

// DescribeOutputs 返回某节点类型可能的输出字段
func DescribeOutputs(nodeType string) []string {
	if f, ok := defaultOutputFields[nodeType]; ok {
		return f
	}
	return []string{"text"}
}
