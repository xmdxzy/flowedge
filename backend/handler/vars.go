package handler

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"flowedge/backend/model"
)

// 评测 case 的输入注入。
//
// 为什么需要它 —— 2026-10-23 发现的深层缺口：
// `evaluate` 的 case 只带 `caseId`（当标签用），`start` 只读 `node.data.prompt`。
// 于是"跑 8 个 case 做配对评测"实际是把**同一张图、同一个 prompt 跑 8 遍**：
// 只能测稳定性，测不了"换个输入还成不成" —— 而后者才是评测的本意。
//
// 为什么在起跑前渲染，而不是给引擎加一条 vars 通道：
// 引擎的执行签名是 (node, upstream)，加参数要穿透 5 层，且会把"评测语义"渗进执行器。
// 而 `evaluate` 本来就有"用内存图跑指定版本"的通道（Nodes/Edges），
// vars 渲染正好挂在这条已有通道上：**不动库、不影响别的 run**。

// renderNodeVars 把 data 里字符串中的 {{key}} 换成 vars[key]。
//
// 三条纪律：
//  1. **只动 data 里的字符串**，不碰 type / id / position —— 那些不是"输入"。
//  2. **缺失的 key 原样保留**。静默替换成空串会让 case 跑出"空 prompt"却显示成功，
//     这是最难查的一类假成功；原样保留时起码人在产物里看得见 {{x}}。
//  3. 递归进嵌套的 map / 数组（data 里常有结构化参数）。
func renderNodeVars(nodesJSON []byte, vars map[string]interface{}) []byte {
	if len(vars) == 0 || len(nodesJSON) == 0 {
		return nodesJSON
	}
	var nodes []model.NodeInstance
	if err := json.Unmarshal(nodesJSON, &nodes); err != nil {
		return nodesJSON // 解析不了就原样返回，让下游的解析错误去报真实原因
	}
	changed := false
	for i := range nodes {
		if nodes[i].Data == nil {
			continue
		}
		if v, did := renderValueVars(nodes[i].Data, vars); did {
			if m, ok := v.(map[string]interface{}); ok {
				nodes[i].Data = m
				changed = true
			}
		}
	}
	if !changed {
		return nodesJSON
	}
	out, err := json.Marshal(nodes)
	if err != nil {
		return nodesJSON
	}
	return out
}

func renderValueVars(v interface{}, vars map[string]interface{}) (interface{}, bool) {
	switch t := v.(type) {
	case string:
		s, did := substituteVars(t, vars)
		return s, did
	case map[string]interface{}:
		changed := false
		for k := range t {
			if nv, did := renderValueVars(t[k], vars); did {
				t[k] = nv
				changed = true
			}
		}
		return t, changed
	case []interface{}:
		changed := false
		for i := range t {
			if nv, did := renderValueVars(t[i], vars); did {
				t[i] = nv
				changed = true
			}
		}
		return t, changed
	}
	return v, false
}

// substituteVars 替换 {{key}} 与 {{ key }}；没有匹配的 key 时原样返回。
func substituteVars(s string, vars map[string]interface{}) (string, bool) {
	if !strings.Contains(s, "{{") {
		return s, false
	}
	orig := s
	var b strings.Builder
	changed := false
	for {
		i := strings.Index(s, "{{")
		if i < 0 {
			b.WriteString(s)
			break
		}
		j := strings.Index(s[i:], "}}")
		if j < 0 {
			b.WriteString(s)
			break
		}
		b.WriteString(s[:i])
		name := strings.TrimSpace(s[i+2 : i+j])
		if raw, ok := vars[name]; ok {
			if val, usable := varString(raw); usable {
				b.WriteString(val)
				changed = true
			} else {
				// 值是 null：当成"没提供"，原样保留占位
				b.WriteString(s[i : i+j+2])
			}
		} else {
			// 缺失的 key 原样保留，见函数注释第 2 条
			b.WriteString(s[i : i+j+2])
		}
		s = s[i+j+2:]
	}
	if !changed {
		return orig, false // 注意：此时 s 已被切片推进过，必须返回 orig
	}
	return b.String(), true
}

// varString 把任意 JSON 值转成替换文本。
//
// 为什么 vars 收 interface{} 而不是 string：
// MCP 的 `flowedge_run_workflow` 里 vars 声明是 `{"type":"object"}`，
// 外部智能体最自然的写法是 {"n": 2}（数字）。用 map[string]string 接，
// gin 会直接 400 —— 而那是一条"看起来完全合理"的调用。
//
// 数字要按人类写法给：{"n": 2} 应该替换成 "2"，不是 "2.000000"。
func varString(v interface{}) (string, bool) {
	switch t := v.(type) {
	case nil:
		return "", false
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case float64: // JSON 里的数字都是 float64
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10), true
		}
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case int:
		return strconv.Itoa(t), true
	case int64:
		return strconv.FormatInt(t, 10), true
	default:
		// 数组/对象：按 Go 语法展开，够调试用。真要精细格式由调用方自己转成字符串。
		return fmt.Sprint(t), true
	}
}
