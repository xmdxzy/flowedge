package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ComfyUI 工作流导入
//
// 为什么这是「生态广度」里最大的单点杠杆（2026-10-21）：
//
//	自研节点永远追不上 ComfyUI 社区的更新速度。但社区里躺着几万个
//	workflow JSON——把它们直接吃进来，等于白拿整个生态的资产，
//	而不是每个玩法都自己实现一遍节点。
//
// 格式只认 **API 格式**（ComfyUI 菜单 "Export (API)"）。
// UI 格式（含 nodes/links 数组）需要图遍历重写，容易出错；
// 这里选择显式拒绝并告诉用户怎么导出，而不是猜着转换——
// 猜错的转换会产出一张"看着能跑但接错线"的图，比直接失败更糟。
//
// 绑定语法（bindings：目标 path → 来源）：
//
//	"6.text":   "{{prompt}}"       来自节点 prompt 或上游 text
//	"5.width":  "1024"             字面量；数值字段自动转 number
//	"10.image": "@file:data/in.png" 上传本地图片后填文件名
//	"3.ckpt_name": "{{ckpt}}"      来自工作流变量
//
// 只写想改的字段：未绑定的输入保持 JSON 原值（作者的原参数）。

// ComfyWorkflowSpec 一次导入执行
type ComfyWorkflowSpec struct {
	Graph      map[string]interface{} `json:"workflow"`
	File       string                 `json:"workflow_file"`
	Bindings   map[string]string      `json:"bindings"`
	OutputNode string                 `json:"output_node"`
	// TimeoutSec 覆盖默认超时（导入的工作流可能比内置的重得多）
	TimeoutSec int `json:"timeout_sec"`
}

// numericInputs ComfyUI 里必须传数字的常见输入。传字符串会 400。
// 不做全量类型推导（那需要 object_info 的 schema），只覆盖权重类之外的高频项。
var numericInputs = map[string]bool{
	"width": true, "height": true, "batch_size": true, "length": true,
	"seed": true, "noise_seed": true, "steps": true, "cfg": true,
	"denoise": true, "fps": true, "frame_idx": true, "strength": true,
	"scale": true, "start_at_step": true, "end_at_step": true,
	"shift": true, "shift_video": true, "shift_audio": true,
	"multiplier": true, "strength_model": true, "strength_clip": true,
	"num_frames": true, "duration": true, "chunks": true, "head_chunks": true,
	"seq_threshold": true, "ref_image_size": true, "bit_depth": true,
}

var boolInputs = map[string]bool{
	"clip_skip": true, "noise_augmentation": true, "tiled": true,
}

// ParseComfyWorkflow 解析 API 格式 JSON。
//
// 明确区分三种失败，因为它们对应完全不同的用户动作：
//   - JSON 不合法 → 检查复制是否完整
//   - UI 格式 → 改用 Export (API)
//   - 缺 class_type → 该键不是节点（多半混进了 extra/client_id）
func ParseComfyWorkflow(raw []byte) (map[string]interface{}, error) {
	var parsed map[string]interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("工作流 JSON 解析失败（复制可能不完整）：%w", err)
	}
	if _, isUI := parsed["nodes"]; isUI {
		return nil, fmt.Errorf("这是 ComfyUI 的 UI 格式（含 nodes/links），无法直接执行；" +
			"请在 ComfyUI 里用「导出 (API) / Export (API)」重新导出——API 格式的每个键都是数字节点 id")
	}
	graph := make(map[string]interface{}, len(parsed))
	nodes := 0
	for id, v := range parsed {
		obj, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		class, _ := obj["class_type"].(string)
		if class == "" {
			// extra / client_id / version 之类：不是节点，剔除而不是报错
			continue
		}
		if _, ok := obj["inputs"].(map[string]interface{}); !ok {
			obj["inputs"] = map[string]interface{}{}
		}
		graph[id] = obj
		nodes++
	}
	if nodes == 0 {
		return nil, fmt.Errorf("工作流里没有任何节点（每个节点都需要 class_type 与 inputs）")
	}
	return graph, nil
}

// LoadComfyWorkflowFile 从 data/comfy_workflows/<name>.json 读（name 可带或不带 .json）
func LoadComfyWorkflowFile(name string) (map[string]interface{}, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("workflow_file 为空")
	}
	// 只允许文件名，不允许路径穿越
	if strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		return nil, fmt.Errorf("workflow_file 只能是 %s 下的文件名，不允许路径", ComfyTemplateDir)
	}
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		name += ".json"
	}
	full := filepath.Join(ComfyTemplateDir, name)
	raw, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("找不到工作流文件 %s（可用：%s）", full, strings.Join(ListComfyTemplates(), ", "))
		}
		return nil, err
	}
	return ParseComfyWorkflow(raw)
}

// ApplyBindings 把 bindings 写进图，返回实际写入的字段（留痕用）。
//
// 失败一律显式：目标节点不存在、目标输入不存在、变量缺失，都报出来。
// 静默跳过会让用户以为「绑定生效了」，最后得到一张用原参数的图。
func ApplyBindings(graph map[string]interface{}, bindings map[string]string, vars map[string]string) ([]string, error) {
	if len(bindings) == 0 {
		return nil, nil
	}
	applied := make([]string, 0, len(bindings))
	for path, raw := range bindings {
		nodeID, field, ok := strings.Cut(path, ".")
		if !ok || nodeID == "" || field == "" {
			return nil, fmt.Errorf("绑定 %q 格式应为 <节点id>.<输入名>（如 6.text）", path)
		}
		obj, exists := graph[nodeID].(map[string]interface{})
		if !exists {
			return nil, fmt.Errorf("绑定 %q 指向的节点 %s 不存在于该工作流", path, nodeID)
		}
		if strings.EqualFold(field, "class_type") {
			return nil, fmt.Errorf("绑定 %q 不允许修改 class_type（节点类型属于工作流本身）", path)
		}
		inputs, _ := obj["inputs"].(map[string]interface{})
		if inputs == nil {
			inputs = map[string]interface{}{}
			obj["inputs"] = inputs
		}
		val, err := resolveBindingValue(field, raw, vars)
		if err != nil {
			return nil, fmt.Errorf("绑定 %q：%w", path, err)
		}
		inputs[field] = val
		applied = append(applied, path)
	}
	return applied, nil
}

// resolveBindingValue 解析单个绑定值
func resolveBindingValue(field, raw string, vars map[string]string) (interface{}, error) {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "{{") && strings.HasSuffix(trimmed, "}}") {
		key := strings.TrimSpace(trimmed[2 : len(trimmed)-2])
		v, ok := vars[key]
		if !ok {
			return nil, fmt.Errorf("变量 {{%s}} 没有取值（可用的：%s）", key, joinKeys(vars))
		}
		return coerceByField(field, v), nil
	}
	if strings.HasPrefix(trimmed, "@file:") {
		// 上传交给调用方（需要实例地址），这里只把路径透传出去
		return map[string]interface{}{comfyUploadMarker: strings.TrimPrefix(trimmed, "@file:")}, nil
	}
	return coerceByField(field, raw), nil
}

// comfyUploadMarker 标记「这个值是待上传的本地文件」
const comfyUploadMarker = "__flowedge_upload__"

// coerceByField 按字段名把字符串转成 ComfyUI 期望的类型
func coerceByField(field, s string) interface{} {
	lower := strings.ToLower(field)
	if numericInputs[lower] {
		if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
			return n
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return f
		}
		return s // 解析不了就原样传，让 ComfyUI 报出来
	}
	if boolInputs[lower] {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "1", "yes":
			return true
		case "false", "0", "no":
			return false
		}
	}
	return s
}

// resolveUploads 把 @file: 标记替换成已上传的文件名
func (s *Text2ImageService) resolveUploads(graph map[string]interface{}, baseURL string) ([]string, error) {
	var uploaded []string
	for id, v := range graph {
		obj, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		inputs, ok := obj["inputs"].(map[string]interface{})
		if !ok {
			continue
		}
		for field, val := range inputs {
			m, ok := val.(map[string]interface{})
			if !ok {
				continue
			}
			local, has := m[comfyUploadMarker].(string)
			if !has {
				continue
			}
			name, err := s.UploadImageAt(baseURL, local)
			if err != nil {
				return nil, fmt.Errorf("节点 %s.%s 的图片上传失败：%w", id, field, err)
			}
			inputs[field] = name
			uploaded = append(uploaded, name)
		}
	}
	return uploaded, nil
}

// RunImportedWorkflow 执行一张导入的 ComfyUI 工作流
func (s *Text2ImageService) RunImportedWorkflow(spec ComfyWorkflowSpec, vars map[string]string, baseURL string) ([]MediaOutput, []string, error) {
	if baseURL == "" {
		baseURL = s.BaseURL
	}
	var graph map[string]interface{}
	var err error
	if spec.Graph != nil {
		// 走一遍 Parse 以统一校验（Graph 可能来自 HTTP body，字段可能不全）
		raw, merr := json.Marshal(spec.Graph)
		if merr != nil {
			return nil, nil, merr
		}
		graph, err = ParseComfyWorkflow(raw)
	} else {
		graph, err = LoadComfyWorkflowFile(spec.File)
	}
	if err != nil {
		return nil, nil, err
	}

	applied, err := ApplyBindings(graph, spec.Bindings, vars)
	if err != nil {
		return nil, nil, err
	}
	if _, err := s.resolveUploads(graph, baseURL); err != nil {
		return nil, nil, err
	}

	timeout := 15 * time.Minute
	if spec.TimeoutSec > 0 {
		timeout = time.Duration(spec.TimeoutSec) * time.Second
	}
	outs, err := s.RunGraphAt(baseURL, graph, timeout)
	if err != nil {
		return nil, applied, err
	}
	return outs, applied, nil
}

func joinKeys(m map[string]string) string {
	if len(m) == 0 {
		return "无"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}
