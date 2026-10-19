package service

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"flowedge/backend/model"
)

// 变量聚合器节点（aggregate）
//
// 解决的真实痛点：菱形 DAG 汇合时，下游只能拿到「每一路各自的对象」，
// 想把两路文本拼起来、把三张图的路径收成数组，此前只能写 code 节点手搓。
//
// 节点配置（全部可选）：
//   mode        object（默认）| list | text | first | last
//   field       取值字段，默认 text；设为 * 表示整个输出对象
//   inputs      显式指定参与聚合的节点 ID，逗号分隔；不填则取全部上游
//   separator   text 模式的连接符，默认空行
//   include_empty 是否保留空值，默认 false
//
// 输出：
//   text        各路上游的文本按顺序连接
//   count       实际聚合到几路
//   values      各路取值组成的数组（mode=list 时最有用）
//   nodes       {节点ID: 取值} 的映射（mode=object 时最有用）
func (e *EngineV2) execAggregateNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	mode, _ := node.Data["mode"].(string)
	if mode == "" {
		mode = "object"
	}
	field, _ := node.Data["field"].(string)
	if field == "" {
		field = "text"
	}
	separator, ok := node.Data["separator"].(string)
	if !ok {
		separator = "\n\n"
	}
	includeEmpty, _ := node.Data["include_empty"].(bool)

	// 稳定顺序：按上游节点 ID 排序，避免 Go map 随机遍历导致每次结果不同
	ids := make([]string, 0, len(upstream))
	for k := range upstream {
		ids = append(ids, k)
	}
	sort.Strings(ids)

	// 显式清单优先：顺序即用户指定的顺序（对应「先写开头，再写正文」这种真实需求）
	if raw, ok := node.Data["inputs"].(string); ok && strings.TrimSpace(raw) != "" {
		explicit := make([]string, 0, 4)
		for _, s := range strings.Split(raw, ",") {
			if s = strings.TrimSpace(s); s != "" {
				explicit = append(explicit, s)
			}
		}
		if len(explicit) > 0 {
			ids = ids[:0]
			for _, id := range explicit {
				if _, exists := upstream[id]; exists {
					ids = append(ids, id)
				}
			}
		}
	}

	if len(ids) == 0 {
		return nil, fmt.Errorf("aggregate node has no upstream input")
	}

	values := make([]interface{}, 0, len(ids))
	nodes := make(map[string]interface{}, len(ids))
	texts := make([]string, 0, len(ids))

	for _, id := range ids {
		raw := upstream[id]
		var picked interface{} = raw
		if field != "*" {
			if m, ok := raw.(map[string]interface{}); ok {
				if v, exists := m[field]; exists {
					picked = v
				} else {
					picked = nil
				}
			} else {
				picked = nil
			}
		}
		if picked == nil && !includeEmpty {
			continue
		}
		values = append(values, picked)
		nodes[id] = picked
		if s := stringifyValue(picked); s != "" {
			texts = append(texts, s)
		} else if includeEmpty {
			texts = append(texts, "")
		}
	}

	out := map[string]interface{}{
		"count":  len(values),
		"text":   strings.Join(texts, separator),
		"values": values,
		"nodes":  nodes,
	}

	switch mode {
	case "list":
		out["aggregate"] = values
	case "text":
		out["aggregate"] = strings.Join(texts, separator)
	case "first":
		if len(values) > 0 {
			out["aggregate"] = values[0]
			out["text"] = stringifyValue(values[0])
		}
	case "last":
		if len(values) > 0 {
			out["aggregate"] = values[len(values)-1]
			out["text"] = stringifyValue(values[len(values)-1])
		}
	default: // object
		out["aggregate"] = nodes
	}

	return out, nil
}

// stringifyValue 把任意输出值转成可读文本（字符串原样，其余 JSON 编码）
func stringifyValue(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%v", x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprintf("%v", x)
		}
		return string(b)
	}
}
