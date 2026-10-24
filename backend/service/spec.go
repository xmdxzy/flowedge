package service

// 工作流任务书编译器（Spec Compiler）。
//
// 解决的问题
// ----------
// 画布只对人有意义：节点方框、连线、右侧面板。智能体（workbuddy / dsh / 任何 MCP 客户端）
// 拿到的是一个 workflow_id，它读不到画布上的参数、约束、验收标准，于是只能"盲跑 + 问进度"。
// 这一层把图编译成一份**人和智能体都能读的执行规格**：执行顺序、每个模块吃什么吐什么、
// 关键参数的真实取值、验收门与 FAIL 条件、产物清单、以及"哪些地方图本身没说清"。
//
// 设计约束（违反任一条视为返工）
// ----------------------------
//  1. **零类型分支**：本文件不许出现任何节点类型的字符串字面量（有测试扫源码来锁这条）。
//     所有语义来自 Modules（契约）与 NodeSpecs（执行元数据）。加一个模态/逻辑模块，
//     任务书自动出现它，不需要动这个文件。有测试扫描本文件源码来锁这条。
//  2. **只报事实**：参数值是节点上真实写的值，不填默认值、不猜。默认值住在引擎里，
//     复制一份到这里就是第二份真相（「涡轮 8 步 vs 实际 12 步」的教训）。
//  3. **不确定就写出来**：未声明契约、孤立节点、必填输入没来源、端口类型不匹配、成环，
//     一律进 Warnings 并渲染到任务书末尾。**不许静默通过**。

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"flowedge/backend/model"
)

// SpecParam 任务书里的一个参数条目。
type SpecParam struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Value   string   `json:"value"`
	Unit    string   `json:"unit,omitempty"`
	Options []string `json:"options,omitempty"`
	Help    string   `json:"help,omitempty"`
}

// SpecInputRef 一条入边的可读描述。
type SpecInputRef struct {
	FromNode string `json:"from_node"`
	FromType string `json:"from_type"`
	FromPort string `json:"from_port"`
	ToPort   string `json:"to_port"`
	Kind     string `json:"kind"`
	Mismatch bool   `json:"mismatch,omitempty"` // 端口类型不兼容：图能存，但任务书要标出来
}

// SpecModule 任务书里的一个模块（= 画布上的一个节点）。
type SpecModule struct {
	Step     int            `json:"step"`
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Title    string         `json:"title"`
	Role     string         `json:"role"`
	Layer    int            `json:"layer"`
	Declared bool           `json:"declared"` // false = 该类型没有契约（任务书会标注）
	Inputs   []SpecInputRef `json:"inputs,omitempty"`
	Params   []SpecParam    `json:"params,omitempty"`
	Extra    []SpecParam    `json:"extra,omitempty"` // 契约未登记的字段，仍然照实输出
	Outputs  []Port         `json:"outputs,omitempty"`
	Gates    []string       `json:"gates,omitempty"`
	Agent    string         `json:"agent,omitempty"`
	// Cards 该节点挂的能力卡（按输入 → 本体 → 输出 → 替换排序）。
	// 任务书必须带上它：智能体在"按任务书执行"时，若不知道节点被谁接管，
	// 就会绕过卡片直接调模型，产出与原画布不一致。
	Cards  []SpecCard `json:"cards,omitempty"`
	Status string     `json:"status,omitempty"` // 上次运行状态（有则带）
	// Nondeterministic 该步不承诺「同输入同产出」（如 agent_task）。
	// 任务书必须显式标出来：评测要按它分组，否则「可复现」这个前提被悄悄破坏而测试仍绿。
	Nondeterministic bool `json:"nondeterministic,omitempty"`
}

// SpecCard 任务书里的一个能力卡条目。
type SpecCard struct {
	Stage string `json:"stage"` // input / body / output / replace
	Kind  string `json:"kind"`  // agent / skill / workflow
	ID    string `json:"id"`
	Name  string `json:"name"`
}

// SpecGate 一条验收门。
type SpecGate struct {
	Layer  string `json:"layer"`
	Source string `json:"source"` // 节点 id；"workflow" 表示整条片子
	Rule   string `json:"rule"`
	Fail   string `json:"fail"`
}

// SpecDoc 任务书的结构化形态（同时也是 JSON 导出格式）。
type SpecDoc struct {
	WorkflowID  int            `json:"workflow_id"`
	Name        string         `json:"name"`
	GeneratedAt string         `json:"generated_at"`
	NodeCount   int            `json:"node_count"`
	EdgeCount   int            `json:"edge_count"`
	LayerCount  int            `json:"layer_count"`
	Sides       map[string]int `json:"sides"` // 显存侧分布：调度成本的一眼概览
	Layers      [][]string     `json:"layers"`
	Modules     []SpecModule   `json:"modules"`
	Gates       []SpecGate     `json:"gates"`
	Artifacts   []string       `json:"artifacts"`
	// Entry 入口：谁把任务交进来（manual / workbuddy / dsh / webhook / cron）。
	// 这是**工作流级**事实，节点级接线表达不了——画布上的线只在节点之间。
	Entry     string              `json:"entry,omitempty"`
	Variables map[string][]string `json:"variables,omitempty"`
	Warnings  []string            `json:"warnings"`
	Markdown  string              `json:"markdown,omitempty"`
}

// 门层 → 该层关心哪些参数字段。
//
// 这是**门的词汇表**，不是节点分类：它只说"timing 这一层看哪些键"，
// 不关心是哪个节点写了这些键。任何模块只要声明了 GateLayers，就按这张表取阈值。
var gateParamKeys = map[string][]string{
	"timing": {"fps", "target_seconds", "tolerance_seconds", "min_shot_seconds", "max_shot_seconds", "beat_weights"},
	// identity：文本侧（定妆图/<Picture N>）与像素侧（一致性门的布局/配色/亮度/漂移）
	// 共用一层——九宫格锁锁句子，一致性门锁画面，都是「别换身份」。
	"identity": {"stills", "require_ref_images_every_segment", "max_ref_images", "require_picture_tags",
		"layout_min", "palette_min", "luma_max", "drift_layout_min", "drift_palette_min"},
	"audio":     {"require_audio", "forbid_speech", "max_silence_gap", "loudness_min_db", "loudness", "must_transcribe"},
	"technical": {"min_frames", "duration_min", "duration_max", "min_width", "min_height", "duration_tolerance", "av_drift_max", "fps"},
	"budget":    {"wall_seconds_max", "per_segment_wall_max", "on_exceed"},
}

// gateLayerOrder 门层的固定展示顺序（报告可读性，不影响判定）。
var gateLayerOrder = []string{"timing", "identity", "audio", "technical", "budget"}

// nodeDataInternalKeys 运行时与前端状态字段，不属于"参数"，不进任务书。
var nodeDataInternalKeys = map[string]bool{
	"status": true, "output": true, "error": true, "error_code": true,
	"started_at": true, "ended_at": true, "label": true, "selected": true,
	"dragging": true,
	// 能力卡已经在「处理链」段落里展开成人话，再当参数原样贴一遍 JSON 是重复噪音。
	// 注意只排除 bindings —— task_preset / max_tool_rounds 这些是**真参数**，
	// 任务书必须照录（智能体要靠它复现同一份产出）。
	"bindings": true, "binding": true,
}

// CompileSpec 把一条工作流编译成任务书。
// format: "md"（默认）| "json"。json 也会带 markdown 字段，方便一次取两种。
func CompileSpec(wf *model.Workflow, format string) (*SpecDoc, error) {
	var nodes []model.NodeInstance
	var edges []model.EdgeInstance
	if len(wf.Nodes) > 0 {
		if err := json.Unmarshal(wf.Nodes, &nodes); err != nil {
			return nil, fmt.Errorf("解析工作流节点失败：%w", err)
		}
	}
	if len(wf.Edges) > 0 {
		if err := json.Unmarshal(wf.Edges, &edges); err != nil {
			return nil, fmt.Errorf("解析工作流连线失败：%w", err)
		}
	}

	doc := &SpecDoc{
		WorkflowID:  int(wf.ID),
		Name:        wf.Name,
		GeneratedAt: time.Now().Format(time.RFC3339),
		NodeCount:   len(nodes),
		EdgeCount:   len(edges),
		Sides:       map[string]int{},
		Variables:   map[string][]string{},
	}

	byID := make(map[string]*model.NodeInstance, len(nodes))
	order := make(map[string]int, len(nodes))
	for i := range nodes {
		byID[nodes[i].ID] = &nodes[i]
		order[nodes[i].ID] = i
	}

	// 入口来源：从**角色为 source 的模块**上读 entry_agent（空 = 手动）。
	// 这里不能按节点类型名分支：spec 必须完全由契约派生，
	// TestSpecCompilerHasNoTypeSwitches 会扫源码（连注释一起扫）直接报红。
	// 这条守护拦过我一次，它是对的。
	for i := range nodes {
		if c, ok := ModuleContractOf(nodes[i].Type); !ok || c.Role != RoleSource {
			continue
		}
		if v, ok := nodes[i].Data["entry_agent"].(string); ok && v != "" {
			doc.Entry = v
		}
		break
	}

	// 显存侧分布：直接读 NodeSpecs，不做类型判断
	for i := range nodes {
		side := "none"
		if s, ok := NodeSpecOf(nodes[i].Type); ok && s.Side != "" {
			side = s.Side
		}
		doc.Sides[side]++
	}

	layers, cycle := topoLayers(nodes, edges)
	doc.Layers = layers
	doc.LayerCount = len(layers)
	layerOf := map[string]int{}
	for li, layer := range layers {
		for _, id := range layer {
			layerOf[id] = li
		}
	}

	incoming := map[string][]model.EdgeInstance{}
	outgoing := map[string][]model.EdgeInstance{}
	for _, e := range edges {
		incoming[e.Target] = append(incoming[e.Target], e)
		outgoing[e.Source] = append(outgoing[e.Source], e)
	}

	step := 0
	for _, layer := range layers {
		for _, id := range layer {
			n := byID[id]
			if n == nil {
				continue
			}
			step++
			m := buildSpecModule(step, n, layerOf[id], incoming[id], outgoing[id], byID)
			doc.Modules = append(doc.Modules, m)

			// 门：只从契约声明的 GateLayers 派生
			emitted := 0
			for _, layer := range m.Gates {
				gs := gatesFor(n, layer)
				emitted += len(gs)
				doc.Gates = append(doc.Gates, gs...)
			}
			if len(m.Gates) > 0 && emitted == 0 {
				// 「声明了门层却没有阈值」是最危险的静默失败：看起来有门，其实什么都不查。
				// 门必须有牙齿——不能 FAIL 的检查不算门。
				doc.Warnings = append(doc.Warnings, fmt.Sprintf(
					"节点 `%s`（%s）声明承载 %d 层门（%s），但一个阈值都没有：等于没设门。补 plan.gates 后再出片",
					n.ID, m.Title, len(m.Gates), strings.Join(m.Gates, "/")))
			}
			// 产物清单：谁产媒体，就列谁
			if s, ok := NodeSpecOf(n.Type); ok && s.Media {
				for _, p := range m.Outputs {
					if p.Kind == PortImage || p.Kind == PortVideo || p.Kind == PortAudio {
						doc.Artifacts = append(doc.Artifacts, fmt.Sprintf("%s · %s（%s）",
							n.ID, p.Name, p.Kind))
					}
				}
			}
			if vars := AvailableVariablesFor(n.ID, nodes, edges); len(vars) > 0 {
				for _, v := range vars {
					doc.Variables[n.ID] = append(doc.Variables[n.ID], v.Path)
				}
			}
		}
	}

	// 环：拓扑排不进去的节点也要出现在任务书里，否则"图里有 9 个节点、任务书只有 7 个"没人发现
	if len(cycle) > 0 {
		doc.Warnings = append(doc.Warnings,
			fmt.Sprintf("图里有环，%d 个节点无法确定执行顺序：%s", len(cycle), strings.Join(cycle, ", ")))
		sort.Slice(cycle, func(i, j int) bool { return order[cycle[i]] < order[cycle[j]] })
		for _, id := range cycle {
			n := byID[id]
			if n == nil {
				continue
			}
			step++
			doc.Modules = append(doc.Modules, buildSpecModule(step, n, -1, incoming[id], outgoing[id], byID))
		}
	}

	doc.Warnings = append(doc.Warnings, specWarnings(nodes, edges, byID, incoming)...)
	doc.Gates = dedupeGates(doc.Gates)
	doc.Markdown = renderSpecMarkdown(doc)

	if format == "json" {
		return doc, nil
	}
	return doc, nil
}

// buildSpecModule 单个模块的段落。所有语义来自契约，没有类型分支。
func buildSpecModule(step int, n *model.NodeInstance, layer int,
	in []model.EdgeInstance, out []model.EdgeInstance, byID map[string]*model.NodeInstance) SpecModule {

	c, declared := ModuleContractOf(n.Type)
	m := SpecModule{
		Step: step, ID: n.ID, Type: n.Type, Layer: layer, Declared: declared,
		Status: n.Status,
	}
	if declared {
		m.Title = c.Title
		m.Role = string(c.Role)
		m.Outputs = c.Outputs
		m.Gates = c.GateLayers
		m.Agent = c.Agent
		m.Nondeterministic = c.Nondeterministic
	} else {
		m.Title = n.Type
		m.Role = string(RoleTransform) // 未声明契约时按通用变换处理，并在 Warnings 里点名
	}

	// 入边 → 可读输入
	for _, e := range in {
		src := byID[e.Source]
		srcType := ""
		if src != nil {
			srcType = src.Type
		}
		// 端口按**源节点的类型**解析，不是按节点 id——按 id 查表永远查不到，
		// 会静默退化成 "out/any"，端口不匹配就再也检测不出来。
		fromPort, kind := resolveOutputPort(srcType, e.SourceHandle)
		toPort := resolveInputPort(n.Type, e.TargetHandle)
		m.Inputs = append(m.Inputs, SpecInputRef{
			FromNode: e.Source, FromType: srcType, FromPort: fromPort,
			ToPort: toPort, Kind: kind,
			Mismatch: !PortKindAccepts(PortKind(kind), resolveInputKind(n.Type, e.TargetHandle)),
		})
	}

	// 参数：契约给标签，data 给真实值
	labeled := map[string]bool{}
	for _, kp := range c.KeyParams {
		v, ok := n.Data[kp.Key]
		if !ok {
			continue
		}
		if s := paramValueString(v); s != "" && s != "false" {
			m.Params = append(m.Params, SpecParam{
				Key: kp.Key, Label: kp.Label, Value: s, Unit: kp.Unit,
				Options: kp.Options, Help: kp.Help,
			})
			labeled[kp.Key] = true
		}
	}
	// 契约没覆盖的字段照样输出（按 key 排序，保证输出可 diff）
	var rest []string
	for k := range n.Data {
		if labeled[k] || nodeDataInternalKeys[k] {
			continue
		}
		rest = append(rest, k)
	}
	sort.Strings(rest)
	for _, k := range rest {
		s := paramValueString(n.Data[k])
		if s == "" || s == "false" {
			continue
		}
		m.Extra = append(m.Extra, SpecParam{Key: k, Label: k, Value: s})
	}
	return m
}

// paramValueString 把任意参数值渲染成一行可读文本。长文本截断到 240 字并标注省略。
func paramValueString(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		s := strings.TrimSpace(t)
		if len([]rune(s)) > 240 {
			return string([]rune(s)[:240]) + fmt.Sprintf("…（共 %d 字，已截断）", len([]rune(s)))
		}
		return s
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		s := string(b)
		if len([]rune(s)) > 240 {
			return string([]rune(s)[:240]) + "…（已截断）"
		}
		return s
	}
}

// resolveOutputPort 源节点某个 handle 对应的端口（没有 handle 时取第一个输出）。
func resolveOutputPort(nodeType, handle string) (string, string) {
	c, ok := ModuleContractOf(nodeType)
	if !ok || len(c.Outputs) == 0 {
		return "out", string(PortAny)
	}
	if handle != "" {
		for _, p := range c.Outputs {
			if p.Name == handle {
				return p.Name, string(p.Kind)
			}
		}
	}
	return c.Outputs[0].Name, string(c.Outputs[0].Kind)
}

func resolveInputPort(nodeType, handle string) string {
	c, ok := ModuleContractOf(nodeType)
	if !ok || len(c.Inputs) == 0 {
		return "in"
	}
	if handle != "" {
		for _, p := range c.Inputs {
			if p.Name == handle {
				return p.Name
			}
		}
	}
	return c.Inputs[0].Name
}

func resolveInputKind(nodeType, handle string) PortKind {
	c, ok := ModuleContractOf(nodeType)
	if !ok || len(c.Inputs) == 0 {
		return PortAny
	}
	if handle != "" {
		for _, p := range c.Inputs {
			if p.Name == handle {
				return p.Kind
			}
		}
	}
	return c.Inputs[0].Kind
}

// gatesFor 派生某个节点在某层上的门规则。
//
// 两种来源，都是数据不是分支：
//  1. 节点参数里直接写了该层关心的键（如 quality_gate 的 min_frames）；
//  2. 节点参数里带 plan JSON，且 plan.gates 里有该层（分镜执行的真实五层门）。
func gatesFor(n *model.NodeInstance, layer string) []SpecGate {
	var out []SpecGate

	keys := gateParamKeys[layer]
	for _, k := range keys {
		if v, ok := n.Data[k]; ok {
			if s := paramValueString(v); s != "" && s != "false" {
				out = append(out, SpecGate{
					Layer: layer, Source: n.ID,
					Rule: fmt.Sprintf("%s = %s", k, s),
					Fail: gateFailText(layer),
				})
			}
		}
	}

	// plan.gates：真实片子门的唯一存放处（scripts/film-gates.py 读同一份）
	if raw, ok := n.Data["plan"].(string); ok && strings.Contains(raw, "gates") {
		var plan map[string]interface{}
		if json.Unmarshal([]byte(raw), &plan) == nil {
			if g, ok := plan["gates"].(map[string]interface{}); ok {
				if sub, ok := g[layer].(map[string]interface{}); ok {
					out = append(out, flattenGate(n.ID, layer, sub)...)
				}
			}
		}
	}
	return out
}

// flattenGate 把一层的门对象展平成"键 = 值"若干行（可读、可 diff、不猜语义）。
func flattenGate(nodeID, layer string, m map[string]interface{}) []SpecGate {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]SpecGate, 0, len(keys))
	for _, k := range keys {
		out = append(out, SpecGate{
			Layer: layer, Source: nodeID,
			Rule: fmt.Sprintf("%s.%s = %s", layer, k, paramValueString(m[k])),
			Fail: gateFailText(layer),
		})
	}
	return out
}

func gateFailText(layer string) string {
	switch layer {
	case "timing":
		return "总长/单镜时长/节奏权重任一超出即 FAIL"
	case "identity":
		return "定妆图缺失、段内无参考图、<Picture N> 越界即 FAIL"
	case "audio":
		return "台词转不出原话、氛围段有人声、响度越界、静音缺口超限即 FAIL"
	case "technical":
		return "分辨率/帧数/时长/音画漂移任一不达标即 FAIL"
	case "budget":
		return "墙钟超上限：按 on_exceed 决定拦截还是仅告警"
	default:
		return "该层任一检查不通过即 FAIL"
	}
}

func dedupeGates(in []SpecGate) []SpecGate {
	seen := map[string]bool{}
	out := make([]SpecGate, 0, len(in))
	// 按层排序，保证每次导出顺序一致
	sort.SliceStable(in, func(i, j int) bool {
		ri, rj := layerRank(in[i].Layer), layerRank(in[j].Layer)
		if ri != rj {
			return ri < rj
		}
		if in[i].Source != in[j].Source {
			return in[i].Source < in[j].Source
		}
		return in[i].Rule < in[j].Rule
	})
	for _, g := range in {
		k := g.Layer + "|" + g.Source + "|" + g.Rule
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, g)
	}
	return out
}

func layerRank(l string) int {
	for i, x := range gateLayerOrder {
		if x == l {
			return i
		}
	}
	return len(gateLayerOrder)
}

// specWarnings 图本身说不清的地方。全部照实写，不替用户猜。
func specWarnings(nodes []model.NodeInstance, edges []model.EdgeInstance,
	byID map[string]*model.NodeInstance, incoming map[string][]model.EdgeInstance) []string {

	var warns []string
	for i := range nodes {
		n := &nodes[i]
		c, declared := ModuleContractOf(n.Type)

		if !declared {
			warns = append(warns, fmt.Sprintf("节点 `%s`（%s）没有模块契约：任务书按通用变换处理，端口与参数语义未知", n.ID, n.Type))
			continue
		}
		// 必填输入没有来源
		provided := map[string]bool{}
		for _, e := range incoming[n.ID] {
			provided[resolveInputPort(n.Type, e.TargetHandle)] = true
		}
		for _, p := range c.Inputs {
			if p.Required && !provided[p.Name] && !inputCoveredByParam(n.Data, p) {
				warns = append(warns, fmt.Sprintf("节点 `%s`（%s）的必填输入 `%s`（%s）没有上游，也没有在参数里直接给", n.ID, c.Title, p.Name, p.Kind))
			}
		}
		// 孤立节点：不是 source 又没有入边
		if c.Role != RoleSource && len(incoming[n.ID]) == 0 && len(nodes) > 1 {
			warns = append(warns, fmt.Sprintf("节点 `%s`（%s）没有入边：它要么是孤立节点，要么在靠参数硬编码输入", n.ID, c.Title))
		}
	}
	return warns
}

// inputCoveredByParam 该输入端口的常见字段是否已经在节点参数里给了。
// 只做"存在即算给"的判断，不判断路径有效——有效性由运行期与 preflight 负责。
func inputCoveredByParam(data map[string]interface{}, p Port) bool {
	if data == nil {
		return false
	}
	candidates := []string{p.Name + "_path", p.Name + "s", p.Name}
	// 端口名与参数名不一定同名（image → image_path / mask_path），按 kind 兜一层
	if p.Kind == PortImage {
		candidates = append(candidates, "image_path", "mask_path")
	}
	if p.Kind == PortAudio {
		candidates = append(candidates, "audio_path", "ref_audio")
	}
	if p.Kind == PortVideo {
		candidates = append(candidates, "video_path")
	}
	if p.Kind == PortJSON {
		candidates = append(candidates, "plan", "workflow", "bindings")
	}
	for _, k := range candidates {
		if v, ok := data[k]; ok {
			if s := paramValueString(v); s != "" {
				return true
			}
		}
	}
	return false
}

// topoLayers 按依赖分层（Kahn）。同层内保持画布原始顺序，保证导出可 diff。
// 返回无法排序的节点（成环或指向不存在的节点）。
func topoLayers(nodes []model.NodeInstance, edges []model.EdgeInstance) ([][]string, []string) {
	indeg := map[string]int{}
	adj := map[string][]string{}
	exists := map[string]bool{}
	idx := map[string]int{}
	for i := range nodes {
		exists[nodes[i].ID] = true
		indeg[nodes[i].ID] = 0
		idx[nodes[i].ID] = i
	}
	for _, e := range edges {
		if !exists[e.Source] || !exists[e.Target] {
			continue // 悬空边：不计入拓扑，由 warnings 之外的校验层负责
		}
		adj[e.Source] = append(adj[e.Source], e.Target)
		indeg[e.Target]++
	}

	var layers [][]string
	done := map[string]bool{}
	for len(done) < len(nodes) {
		var cur []string
		for _, n := range nodes {
			if done[n.ID] || indeg[n.ID] > 0 {
				continue
			}
			cur = append(cur, n.ID)
		}
		if len(cur) == 0 {
			break
		}
		sort.Slice(cur, func(i, j int) bool { return idx[cur[i]] < idx[cur[j]] })
		layers = append(layers, cur)
		for _, id := range cur {
			done[id] = true
			for _, nxt := range adj[id] {
				indeg[nxt]--
			}
		}
	}
	var stuck []string
	for _, n := range nodes {
		if !done[n.ID] {
			stuck = append(stuck, n.ID)
		}
	}
	return layers, stuck
}

// AvailableVariablesFor 上游可引用变量（任务书「变量引用」段）。
// 与前端「可用变量（点击复制）」用同一套推导，避免两处不一致。
func AvailableVariablesFor(nodeID string, nodes []model.NodeInstance, edges []model.EdgeInstance) []AvailableVariable {
	byID := make(map[string]*model.NodeInstance, len(nodes))
	for i := range nodes {
		byID[nodes[i].ID] = &nodes[i]
	}
	var out []AvailableVariable
	for _, e := range edges {
		if e.Target != nodeID {
			continue
		}
		src := byID[e.Source]
		if src == nil {
			continue
		}
		for _, f := range DescribeOutputs(src.Type) {
			out = append(out, AvailableVariable{
				NodeID: src.ID,
				Type:   src.Type,
				Path:   fmt.Sprintf("{{%s.%s}}", src.ID, f),
				Label:  fmt.Sprintf("%s.%s", src.Type, f),
			})
		}
	}
	return out
}

// renderSpecMarkdown 渲染成人读的任务书。
func renderSpecMarkdown(doc *SpecDoc) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# 工作流任务书：%s\n\n", doc.Name)
	fmt.Fprintf(&b, "> 由 FlowEdge 于 %s 从画布编译。\n", doc.GeneratedAt)
	b.WriteString("> 这是一份**执行规格**：按「执行顺序」做事，按「验收门」自检，产物落到「产物清单」。\n")
	b.WriteString("> 参数值是画布上真实写了的值；没写的一律不在这里出现——**不要替它补默认值**。\n\n")

	b.WriteString("## 0. 概览\n\n")
	fmt.Fprintf(&b, "- 工作流 #%d · 节点 %d · 连线 %d · 拓扑层 %d\n", doc.WorkflowID, doc.NodeCount, doc.EdgeCount, doc.LayerCount)
	if doc.Entry != "" {
		fmt.Fprintf(&b, "- 入口：%s\n", entryLabel(doc.Entry))
	}
	if len(doc.Sides) > 0 {
		var parts []string
		for _, k := range []string{sideDiffusion, sideLanguage, sideAudio, "none"} {
			if n, ok := doc.Sides[k]; ok && n > 0 {
				parts = append(parts, fmt.Sprintf("%s %d", k, n))
			}
		}
		if len(parts) > 0 {
			fmt.Fprintf(&b, "- 显存侧分布：%s（换侧一次约 180s，同侧连续最省时）\n", strings.Join(parts, " · "))
		}
	}
	if len(doc.Warnings) > 0 {
		fmt.Fprintf(&b, "- **未说清的地方：%d 条**（见 §5，交付前先看）\n", len(doc.Warnings))
	}
	b.WriteString("\n")

	b.WriteString("## 1. 执行顺序\n\n")
	curLayer := -2
	for _, m := range doc.Modules {
		if m.Layer != curLayer {
			curLayer = m.Layer
			if curLayer < 0 {
				b.WriteString("### 顺序未定（成环）\n\n")
			} else {
				fmt.Fprintf(&b, "### 第 %d 层%s\n\n", curLayer+1, map[bool]string{true: "（可与同层并行）", false: ""}[len(doc.Layers) > curLayer && len(doc.Layers[curLayer]) > 1])
			}
		}
		fmt.Fprintf(&b, "#### %d. `%s` %s · %s · %s\n\n", m.Step, m.ID, m.Title, m.Type, m.Role)
		if len(m.Inputs) > 0 {
			b.WriteString("- 输入：\n")
			for _, in := range m.Inputs {
				mark := ""
				if in.Mismatch {
					mark = " ⚠ 端口类型不匹配"
				}
				fmt.Fprintf(&b, "  - `%s`（%s）.%s → `%s`%s\n", in.FromNode, in.FromType, in.FromPort, in.ToPort, mark)
			}
		} else if m.Role != string(RoleSource) {
			b.WriteString("- 输入：（无上游，靠自身参数）\n")
		}
		if len(m.Params) > 0 {
			b.WriteString("- 参数：\n")
			for _, p := range m.Params {
				line := fmt.Sprintf("  - %s = %s", p.Label, p.Value)
				if p.Unit != "" {
					line += " " + p.Unit
				}
				if len(p.Options) > 0 {
					line += fmt.Sprintf("（只能取：%s）", strings.Join(p.Options, " / "))
				}
				if p.Help != "" {
					line += " —— " + p.Help
				}
				b.WriteString(line + "\n")
			}
		}
		if len(m.Extra) > 0 {
			b.WriteString("- 其他参数（契约未登记标签，按原名照录）：\n")
			for _, p := range m.Extra {
				b.WriteString(fmt.Sprintf("  - %s = %s\n", p.Key, p.Value))
			}
		}
		if len(m.Outputs) > 0 {
			var outs []string
			for _, o := range m.Outputs {
				outs = append(outs, fmt.Sprintf("%s(%s)", o.Name, o.Kind))
			}
			fmt.Fprintf(&b, "- 产物端口：%s\n", strings.Join(outs, ", "))
		}
		if len(m.Gates) > 0 {
			fmt.Fprintf(&b, "- 承载门层：%s\n", strings.Join(m.Gates, " / "))
		}
		if m.Agent != "" {
			fmt.Fprintf(&b, "- 契约：%s\n", m.Agent)
		}
		if m.Nondeterministic {
			b.WriteString("- ⚠ **非确定性**：同一输入不保证同一产出。评测时本图必须与确定性图分开统计\n")
		}
		if !m.Declared {
			b.WriteString("- ⚠ **该类型没有模块契约**：端口与参数语义未知，执行前请人工确认\n")
		}
		b.WriteString("\n")
	}

	b.WriteString("## 2. 验收门\n\n")
	declaredLayers := map[string]bool{}
	for _, m := range doc.Modules {
		for _, l := range m.Gates {
			declaredLayers[l] = true
		}
	}
	switch {
	case len(doc.Gates) == 0 && len(declaredLayers) == 0:
		b.WriteString("_没有任何模块声明门层。这条流水线**没有自动验收**——出片后必须人工看。_\n\n")
	case len(doc.Gates) == 0:
		var ls []string
		for l := range declaredLayers {
			ls = append(ls, l)
		}
		sort.Slice(ls, func(i, j int) bool { return layerRank(ls[i]) < layerRank(ls[j]) })
		fmt.Fprintf(&b, "> ⚠ 模块声明了门层（%s），但**没有任何阈值**：等于没设门。\n", strings.Join(ls, " / "))
		b.WriteString("> 出片前必须在 `plan.gates` 里把这层的阈值写出来，否则这一节只是摆设。\n\n")
	default:
		b.WriteString("| 层 | 来源节点 | 规则 | FAIL 条件 |\n|---|---|---|---|\n")
		for _, g := range doc.Gates {
			fmt.Fprintf(&b, "| %s | `%s` | %s | %s |\n", g.Layer, g.Source, escapePipe(g.Rule), escapePipe(g.Fail))
		}
		b.WriteString("\n任一 FAIL 即不许交付。门只拦「能算的错」，画面好不好看不在门里。\n\n")
	}

	b.WriteString("## 3. 产物清单\n\n")
	if len(doc.Artifacts) == 0 {
		b.WriteString("_没有标记为产媒体文件的模块。_\n\n")
	} else {
		for _, a := range doc.Artifacts {
			fmt.Fprintf(&b, "- %s\n", a)
		}
		b.WriteString("\n实际落盘路径以运行的 node_states 为准（`GET /api/runs/{id}`）。\n\n")
	}

	if len(doc.Variables) > 0 {
		b.WriteString("## 4. 变量引用\n\n")
		ids := make([]string, 0, len(doc.Variables))
		for id := range doc.Variables {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			fmt.Fprintf(&b, "- `%s` 可用：%s\n", id, strings.Join(doc.Variables[id], "、"))
		}
		b.WriteString("\n")
	}

	b.WriteString("## 5. 未说清的地方\n\n")
	if len(doc.Warnings) == 0 {
		b.WriteString("_无。所有节点都有契约，必填输入都有来源。_\n")
	} else {
		for _, w := range doc.Warnings {
			fmt.Fprintf(&b, "- %s\n", w)
		}
	}
	b.WriteString("\n---\n\n")
	b.WriteString("*本文件由画布编译生成，不要手改；改画布后重新导出。*\n")
	return b.String()
}

func escapePipe(s string) string { return strings.ReplaceAll(s, "|", "\\|") }

// entryLabel 入口来源的人话名。未知值原样返回——不猜，也不吞掉用户的输入。
func entryLabel(v string) string {
	switch v {
	case "manual":
		return "手动（画布运行 / 客户端按钮）"
	case "workbuddy":
		return "WorkBuddy（外部智能体）"
	case "dsh":
		return "DSH（外部智能体）"
	case "webhook":
		return "Webhook"
	case "cron":
		return "定时"
	}
	return v
}
