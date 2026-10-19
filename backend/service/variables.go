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

// varRefRe 变量引用：{{nodeId.path}}。
// path 支持点号与数组索引：a、a.b、a[0]、a[0].b、a.b[1].c
// （此前只支持纯点号路径，数组输出只能靠 data_transform 拼 JSON，体验很差）
var varRefRe = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_-]+)\.([A-Za-z0-9_]+(?:\[\d+\])*(?:\.[A-Za-z0-9_]+(?:\[\d+\])*)*)\s*\}\}`)

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

// jsonPathTokens 把 "a.b[0].c" 拆成 ["a", "b", 0, "c"]。
// TOKEN 里 string 表示对象字段，int 表示数组下标；语法不完整时退化为字段名。
func jsonPathTokens(path string) []interface{} {
	var toks []interface{}
	for _, seg := range strings.Split(path, ".") {
		for seg != "" {
			open := strings.IndexByte(seg, '[')
			if open < 0 {
				toks = append(toks, seg)
				break
			}
			if name := seg[:open]; name != "" {
				toks = append(toks, name)
			}
			closeAt := strings.IndexByte(seg, ']')
			if closeAt < open {
				toks = append(toks, seg[open:]) // 语法残缺：当成普通字段名
				break
			}
			idx := seg[open+1 : closeAt]
			if n, err := strconv.Atoi(idx); err == nil {
				toks = append(toks, n)
			} else {
				toks = append(toks, idx)
			}
			seg = seg[closeAt+1:]
		}
	}
	return toks
}

// lookupOutputField 按路径（含数组索引）从节点输出里取值并转成字符串
func lookupOutputField(v interface{}, path string) (string, bool) {
	var cur interface{} = v
	for _, tok := range jsonPathTokens(path) {
		switch key := tok.(type) {
		case int:
			arr, ok := cur.([]interface{})
			if !ok || key < 0 || key >= len(arr) {
				return "", false
			}
			cur = arr[key]
		case string:
			m, ok := cur.(map[string]interface{})
			if !ok {
				return "", false
			}
			cur, ok = m[key]
			if !ok {
				return "", false
			}
		default:
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
	"text2image":     {"image", "images", "count", "prompts", "text"},
	"asr":            {"text"},
	"tts":            {"audio", "text"},
	"video_compose":  {"video"},
	"loop":           {"text", "all", "iterations", "stopped_by"},
	"sub_workflow":   {"text"},
	"vision":         {"text", "model", "images", "tokens"},
	"prompt_opt":     {"text", "original", "mode"},
	"storyboard":     {"text", "plan", "shots", "title", "style"},
	"shotlist":       {"video", "kind", "text", "shots", "seconds", "detail"},
	"text2video":     {"video", "kind", "text", "original_prompt", "beats", "parts", "segments"},
	"image2image":    {"image", "kind", "source", "strength"},
	"inpaint":        {"image", "kind", "source", "mask"},
	"upscale":        {"image", "kind", "source", "width", "height"},
}

// DescribeOutputs 返回某节点类型可能的输出字段
func DescribeOutputs(nodeType string) []string {
	if f, ok := defaultOutputFields[nodeType]; ok {
		return f
	}
	return []string{"text"}
}
