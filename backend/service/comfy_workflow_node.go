package service

import (
	"fmt"
	"strings"

	"flowedge/backend/model"
)

// comfy_workflow 节点：执行一张从 ComfyUI 导入的工作流。
//
// 节点配置（data）：
//
//	workflow        object 或 string  ComfyUI **API 格式** JSON（Export (API)）
//	workflow_file   string           data/comfy_workflows/<name>.json，二选一
//	bindings        object           目标 path → 来源，见 comfy_workflow.go 注释
//	output_node     string           （可选）只取某节点的产物
//	timeout_sec     number           （可选）覆盖默认 15 分钟
//	prompt          string           便捷字段：自动绑定到 {{prompt}}
//
// 设计取舍：这是「执行别人的图」，不是「解释别人的图」。
// 因此不做节点级智能（不猜哪些是正/负面提示词）——只按用户显式写的
// bindings 改字段，其余保持 JSON 原值。猜错的智能比没有智能更糟。
func (e *EngineV2) execComfyWorkflowNode(node *model.NodeInstance, upstream map[string]interface{}) (interface{}, error) {
	if e.t2i == nil {
		return nil, fmt.Errorf("ComfyUI 未配置：comfy_workflow 节点需要本地 ComfyUI")
	}

	spec := ComfyWorkflowSpec{}
	if wf, ok := node.Data["workflow"]; ok && wf != nil {
		switch v := wf.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				graph, err := ParseComfyWorkflow([]byte(v))
				if err != nil {
					return nil, err
				}
				spec.Graph = graph
			}
		case map[string]interface{}:
			spec.Graph = v
		default:
			return nil, fmt.Errorf("workflow 字段类型不支持（%T）：应为 API 格式 JSON 对象或字符串", wf)
		}
	}
	if f, ok := node.Data["workflow_file"].(string); ok {
		spec.File = f
	}
	if spec.Graph == nil && strings.TrimSpace(spec.File) == "" {
		return nil, fmt.Errorf("comfy_workflow 节点缺少 workflow 或 workflow_file；" +
			"请把 ComfyUI 的「导出 (API)」JSON 粘进来，或放到 " + ComfyTemplateDir + "/ 下再填文件名")
	}
	if v, ok := node.Data["output_node"].(string); ok {
		spec.OutputNode = v
	}
	if v, ok := node.Data["timeout_sec"].(float64); ok && v > 0 {
		spec.TimeoutSec = int(v)
	}
	spec.Bindings = stringMap(node.Data["bindings"])

	vars := collectWorkflowVars(node, upstream)
	// 便捷字段：填了 prompt 就自动绑到 {{prompt}}（用户不必再写一行 binding）
	if p, ok := node.Data["prompt"].(string); ok && strings.TrimSpace(p) != "" {
		if _, exists := vars["prompt"]; !exists {
			vars["prompt"] = p
		}
		if len(spec.Bindings) == 0 {
			spec.Bindings = map[string]string{}
		}
	}

	release := e.enterSide(sideDiffusion)
	defer release()
	if err := e.ensureComfy(""); err != nil {
		return nil, err
	}

	outs, applied, err := e.t2i.RunImportedWorkflow(spec, vars, "")
	if err != nil {
		return nil, err
	}
	if len(outs) == 0 {
		return nil, fmt.Errorf("导入的工作流没有产出任何文件；" +
			"确认图里有 SaveImage / SaveVideo 节点（没有保存节点的图在 ComfyUI 里也是空跑）")
	}

	images := []string{}
	videos := []string{}
	all := []string{}
	for _, o := range outs {
		all = append(all, o.Path)
		switch o.Kind {
		case "image":
			images = append(images, o.Path)
		case "video":
			videos = append(videos, o.Path)
		}
	}
	out := map[string]interface{}{
		"files":         all,
		"count":         len(all),
		"bindings_used": applied,
	}
	if len(images) > 0 {
		out["image"] = images[0]
		out["images"] = images
	}
	if len(videos) > 0 {
		out["video"] = videos[0]
		out["videos"] = videos
	}
	return out, nil
}

// collectWorkflowVars 把上游输出平铺成绑定变量表。
//
// 同时提供两种名字：带前缀（nodeId.field，避免不同节点同名字段打架）与裸字段名
// （写起来短）。裸名后写覆盖先写，因此更"近"的上游更容易被引用到。
func collectWorkflowVars(node *model.NodeInstance, upstream map[string]interface{}) map[string]string {
	vars := map[string]string{}
	for nodeID, raw := range upstream {
		m, ok := raw.(map[string]interface{})
		if !ok {
			if s, isStr := raw.(string); isStr {
				vars[nodeID] = s
			}
			continue
		}
		for k, v := range m {
			s, ok := scalarToString(v)
			if !ok {
				continue
			}
			vars[nodeID+"."+k] = s
			vars[k] = s
		}
	}
	// 节点自己配的静态变量：data.vars 里的键值直接进表（不覆盖上游）
	if m, ok := node.Data["vars"].(map[string]interface{}); ok {
		for k, v := range m {
			if _, exists := vars[k]; exists {
				continue
			}
			if s, ok := scalarToString(v); ok {
				vars[k] = s
			}
		}
	}
	return vars
}

// scalarToString 只把标量转字符串；数组/对象不参与绑定
// （把整个对象塞进 ComfyUI 输入必然是错的）
func scalarToString(v interface{}) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case float64:
		return trimFloat(t), true
	case int:
		return fmt.Sprintf("%d", t), true
	case int64:
		return fmt.Sprintf("%d", t), true
	case bool:
		if t {
			return "true", true
		}
		return "false", true
	}
	return "", false
}

func trimFloat(f float64) string {
	if f == float64(int64(f)) {
		return fmt.Sprintf("%d", int64(f))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", f), "0"), ".")
}

// stringMap 把 JSON 解析出来的 map[string]interface{} 转成 map[string]string
func stringMap(v interface{}) map[string]string {
	m, ok := v.(map[string]interface{})
	if !ok {
		if direct, ok2 := v.(map[string]string); ok2 {
			return direct
		}
		return nil
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		if s, ok := scalarToString(val); ok {
			out[k] = s
		}
	}
	return out
}
