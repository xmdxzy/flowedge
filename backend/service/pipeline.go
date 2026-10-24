package service

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"flowedge/backend/model"
)

// 固定出片流水线（FlowEdge 收敛后的唯一产品形态）。
//
// 设计取舍：**图不再由用户画**。用户只选一条流水线、填几个字段，
// 服务端按 data/pipelines.json 组装节点图再去跑。理由：
//   - 27 种节点 × 任意连线的组合空间里，绝大多数组合是跑不通或者没意义的，
//     而"哪条链路真能出片"这份知识原本只存在于模板文件和文档里；
//   - 通用画布需要调色板/检查器/参数校验/任务书/版本等一整套平台配套，
//     它们一行都不产出多模态内容。
//
// 同时保留"可配置"：pipelines.json 里能改字段、改节点、改默认参数，
// 不用重新编译（重启或 POST /api/pipelines/reload 生效）。

// PipelineBind 把字段值写到某个节点的 data 键。
type PipelineBind struct {
	Node string `json:"node"`
	Path string `json:"path"`
}

// PipelineField 向导上的一个输入项。
type PipelineField struct {
	Key      string         `json:"key"`
	Label    string         `json:"label"`
	Type     string         `json:"type"` // text | textarea | path | int | float | bool | enum
	Default  interface{}    `json:"default,omitempty"`
	Min      *float64       `json:"min,omitempty"`
	Max      *float64       `json:"max,omitempty"`
	Options  []string       `json:"options,omitempty"`
	Hint     string         `json:"hint,omitempty"`
	Required bool           `json:"required,omitempty"`
	Bind     []PipelineBind `json:"bind,omitempty"`
}

// PipelineNode 图里的一个节点模板。When 决定它是否入选（"qc" 或 "mode=inpaint"）。
type PipelineNode struct {
	ID   string                 `json:"id"`
	Type string                 `json:"type"`
	When string                 `json:"when,omitempty"`
	Data map[string]interface{} `json:"data"`
}

// PipelineEdge 连线模板。
type PipelineEdge struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
}

// Pipeline 一条出片流水线。
type Pipeline struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Summary    string          `json:"summary"`
	Output     string          `json:"output"` // image | video | audio
	EstSeconds int             `json:"est_seconds,omitempty"`
	Fields     []PipelineField `json:"fields"`
	Nodes      []PipelineNode  `json:"nodes"`
	Edges      []PipelineEdge  `json:"edges"`
}

// BuiltPipeline 一次组装的结果：可直接交给引擎的节点图 + 生效的参数值。
type BuiltPipeline struct {
	Pipeline *Pipeline
	Nodes    []model.NodeInstance
	Edges    []model.EdgeInstance
	Values   map[string]interface{}
}

// PipelineStore 从 JSON 加载的流水线目录，支持热重载。
type PipelineStore struct {
	mu      sync.RWMutex
	path    string
	order   []string
	byID    map[string]*Pipeline
	version int
}

type pipelineFile struct {
	Version   int        `json:"version"`
	Pipelines []Pipeline `json:"pipelines"`
}

// LoadPipelines 读取流水线定义。文件不存在是硬错误：静默给一个空目录
// 会让 /api/pipelines 返回 []、界面上一条流水线都没有，看起来像"产品坏了"。
func LoadPipelines(path string) (*PipelineStore, error) {
	s := &PipelineStore{path: path}
	if err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload 重新读取文件；读取或校验失败时**保留上一份**（不能把能用的目录换成空）。
func (s *PipelineStore) Reload() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("读不到流水线定义 %s: %w", s.path, err)
	}
	var f pipelineFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("流水线定义 %s 不是合法 JSON: %w", s.path, err)
	}
	if len(f.Pipelines) == 0 {
		return fmt.Errorf("流水线定义 %s 里没有任何流水线", s.path)
	}

	byID := make(map[string]*Pipeline, len(f.Pipelines))
	order := make([]string, 0, len(f.Pipelines))
	for i := range f.Pipelines {
		p := &f.Pipelines[i]
		if p.ID == "" {
			return fmt.Errorf("流水线定义 %s 第 %d 条缺少 id", s.path, i+1)
		}
		if _, dup := byID[p.ID]; dup {
			return fmt.Errorf("流水线定义 %s 有重复 id：%s", s.path, p.ID)
		}
		if err := validatePipelineShape(p); err != nil {
			return fmt.Errorf("流水线 %s 定义不合法: %w", p.ID, err)
		}
		byID[p.ID] = p
		order = append(order, p.ID)
	}

	s.mu.Lock()
	s.byID = byID
	s.order = order
	s.version = f.Version
	s.mu.Unlock()
	return nil
}

// validatePipelineShape 拦住"写错了但看起来像对的"定义：
// 节点 id 重复、边指向不存在的节点、字段 bind 指到不存在的节点。
// 这些错误如果放到运行期，表现是"跑一半失败"，最难查。
func validatePipelineShape(p *Pipeline) error {
	if len(p.Nodes) == 0 {
		return fmt.Errorf("没有节点")
	}
	ids := make(map[string]bool, len(p.Nodes))
	for _, n := range p.Nodes {
		if n.ID == "" || n.Type == "" {
			return fmt.Errorf("节点缺少 id 或 type")
		}
		if ids[n.ID] {
			return fmt.Errorf("节点 id 重复：%s", n.ID)
		}
		if _, ok := NodeSpecs[n.Type]; !ok {
			return fmt.Errorf("节点 %s 的类型 %q 未注册（可能写错，或该节点已删除）", n.ID, n.Type)
		}
		ids[n.ID] = true
	}
	for _, e := range p.Edges {
		if !ids[e.Source] || !ids[e.Target] {
			return fmt.Errorf("边 %s 指向不存在的节点（%s → %s）", e.ID, e.Source, e.Target)
		}
	}
	keys := make(map[string]bool, len(p.Fields))
	for _, f := range p.Fields {
		if f.Key == "" {
			return fmt.Errorf("字段缺少 key")
		}
		if keys[f.Key] {
			return fmt.Errorf("字段 key 重复：%s", f.Key)
		}
		keys[f.Key] = true
		switch f.Type {
		case "text", "textarea", "path", "int", "float", "bool", "enum":
		default:
			return fmt.Errorf("字段 %s 的 type %q 不支持（text/textarea/path/int/float/bool/enum）", f.Key, f.Type)
		}
		if f.Type == "enum" && len(f.Options) == 0 {
			return fmt.Errorf("字段 %s 是 enum 但没给 options", f.Key)
		}
		for _, b := range f.Bind {
			if !ids[b.Node] {
				return fmt.Errorf("字段 %s 的 bind 指向不存在的节点 %s", f.Key, b.Node)
			}
			if b.Path == "" {
				return fmt.Errorf("字段 %s 的 bind 缺少 path", f.Key)
			}
		}
	}
	return nil
}

// List 返回流水线目录（保持 JSON 里的顺序，界面按这个顺序排）。
func (s *PipelineStore) List() []Pipeline {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Pipeline, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, *s.byID[id])
	}
	return out
}

// Get 查一条流水线。
func (s *PipelineStore) Get(id string) (*Pipeline, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.byID[id]
	return p, ok
}

// Path 定义文件位置（预热/自检用）。
func (s *PipelineStore) Path() string { return s.path }

// Build 按字段值组装节点图。
//
// 三条纪律：
//  1. 未知字段直接拒绝：拼错的 key 静默忽略会变成"我明明填了却没生效"；
//  2. 缺省值来自定义文件，请求只覆盖它点名的字段；
//  3. 组装结果是**深拷贝**，绝不改 store 里的模板（并发跑两条流水线时会串味）。
func (s *PipelineStore) Build(id string, params map[string]interface{}) (*BuiltPipeline, error) {
	p, ok := s.Get(id)
	if !ok {
		return nil, fmt.Errorf("没有这条流水线：%s（可用：%s）", id, strings.Join(s.IDs(), ", "))
	}
	if params == nil {
		params = map[string]interface{}{}
	}

	known := make(map[string]PipelineField, len(p.Fields))
	for _, f := range p.Fields {
		known[f.Key] = f
	}
	if err := rejectUnknownKeys(params, known); err != nil {
		return nil, err
	}

	values := make(map[string]interface{}, len(p.Fields))
	for _, f := range p.Fields {
		raw, has := params[f.Key]
		if !has {
			raw = f.Default
		}
		v, err := coerceField(f, raw)
		if err != nil {
			return nil, err
		}
		if f.Required && isEmptyValue(v) {
			return nil, fmt.Errorf("缺少必填项「%s」", f.Label)
		}
		values[f.Key] = v
	}

	nodes := make([]model.NodeInstance, 0, len(p.Nodes))
	present := make(map[string]bool, len(p.Nodes))
	for _, tpl := range p.Nodes {
		if tpl.When != "" && !whenMatches(tpl.When, values) {
			continue
		}
		nodes = append(nodes, model.NodeInstance{
			ID:     tpl.ID,
			Type:   tpl.Type,
			Data:   cloneData(tpl.Data),
			Status: "pending",
		})
		present[tpl.ID] = true
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("流水线 %s 组装后没有任何节点（when 条件全为假）", id)
	}

	for _, f := range p.Fields {
		v := values[f.Key]
		for _, b := range f.Bind {
			if !present[b.Node] {
				continue
			}
			for i := range nodes {
				if nodes[i].ID == b.Node {
					setDataPath(nodes[i].Data, b.Path, v)
					break
				}
			}
		}
	}

	edges := make([]model.EdgeInstance, 0, len(p.Edges))
	for _, e := range p.Edges {
		if !present[e.Source] || !present[e.Target] {
			continue
		}
		edges = append(edges, model.EdgeInstance{ID: e.ID, Source: e.Source, Target: e.Target})
	}

	return &BuiltPipeline{Pipeline: p, Nodes: nodes, Edges: edges, Values: values}, nil
}

// IDs 已加载的流水线 id（升序，错误提示用）。
func (s *PipelineStore) IDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := append([]string(nil), s.order...)
	sort.Strings(out)
	return out
}

// ---- 内部工具 ----

func rejectUnknownKeys(params map[string]interface{}, known map[string]PipelineField) error {
	var unknown []string
	for k := range params {
		if _, ok := known[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	avail := make([]string, 0, len(known))
	for k := range known {
		avail = append(avail, k)
	}
	sort.Strings(avail)
	return fmt.Errorf("未知字段 %s（可用：%s）", strings.Join(unknown, ", "), strings.Join(avail, ", "))
}

func coerceField(f PipelineField, raw interface{}) (interface{}, error) {
	if raw == nil {
		return nil, nil
	}
	switch f.Type {
	case "text", "textarea", "path":
		s := strings.TrimSpace(fmt.Sprint(raw))
		return s, nil
	case "enum":
		s := strings.TrimSpace(fmt.Sprint(raw))
		for _, opt := range f.Options {
			if s == opt {
				return s, nil
			}
		}
		return nil, fmt.Errorf("「%s」只能是 %s，收到 %q", f.Label, strings.Join(f.Options, " / "), s)
	case "bool":
		switch t := raw.(type) {
		case bool:
			return t, nil
		case string:
			b, err := strconv.ParseBool(strings.TrimSpace(t))
			if err != nil {
				return nil, fmt.Errorf("「%s」要 true/false，收到 %q", f.Label, t)
			}
			return b, nil
		case float64:
			return t != 0, nil
		}
		return nil, fmt.Errorf("「%s」要 true/false", f.Label)
	case "int":
		n, err := toFloat(raw)
		if err != nil {
			return nil, fmt.Errorf("「%s」要整数：%w", f.Label, err)
		}
		if n != float64(int64(n)) {
			return nil, fmt.Errorf("「%s」要整数，收到 %v", f.Label, raw)
		}
		v := float64(int64(n))
		if err := checkRange(f, v); err != nil {
			return nil, err
		}
		return v, nil
	case "float":
		n, err := toFloat(raw)
		if err != nil {
			return nil, fmt.Errorf("「%s」要数字：%w", f.Label, err)
		}
		if err := checkRange(f, n); err != nil {
			return nil, err
		}
		return n, nil
	}
	return nil, fmt.Errorf("字段 %s 的类型 %q 不支持", f.Key, f.Type)
}

func checkRange(f PipelineField, v float64) error {
	if f.Min != nil && v < *f.Min {
		return fmt.Errorf("「%s」不能小于 %v，收到 %v", f.Label, *f.Min, v)
	}
	if f.Max != nil && v > *f.Max {
		return fmt.Errorf("「%s」不能大于 %v，收到 %v", f.Label, *f.Max, v)
	}
	return nil
}

func toFloat(raw interface{}) (float64, error) {
	switch t := raw.(type) {
	case float64:
		return t, nil
	case float32:
		return float64(t), nil
	case int:
		return float64(t), nil
	case int64:
		return float64(t), nil
	case json.Number:
		return t.Float64()
	case string:
		return strconv.ParseFloat(strings.TrimSpace(t), 64)
	}
	return 0, fmt.Errorf("不是数字：%v", raw)
}

func isEmptyValue(v interface{}) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	}
	return false
}

// whenMatches 判定节点是否入选。支持 "字段" 与 "字段=值" 两种写法；
// 未定义的字段视为假（宁可少一个可选阶段，也不要把用不上的节点塞进图里）。
func whenMatches(cond string, values map[string]interface{}) bool {
	key, want, hasWant := strings.Cut(cond, "=")
	key = strings.TrimSpace(key)
	v, ok := values[key]
	if !ok {
		return false
	}
	if hasWant {
		want = strings.TrimSpace(want)
		return strings.TrimSpace(fmt.Sprint(v)) == want
	}
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return strings.TrimSpace(t) != ""
	}
	return true
}

// cloneData 深拷贝节点 data（经由 JSON，顺带保证值都是 JSON 可表达的）。
func cloneData(in map[string]interface{}) map[string]interface{} {
	if in == nil {
		return map[string]interface{}{}
	}
	raw, err := json.Marshal(in)
	if err != nil {
		out := make(map[string]interface{}, len(in))
		for k, v := range in {
			out[k] = v
		}
		return out
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		out = make(map[string]interface{}, len(in))
		for k, v := range in {
			out[k] = v
		}
	}
	return out
}

// setDataPath 支持点分下钻（data.min_frames 这种嵌套参数）。
func setDataPath(data map[string]interface{}, path string, v interface{}) {
	parts := strings.Split(path, ".")
	cur := data
	for i, p := range parts {
		if i == len(parts)-1 {
			cur[p] = v
			return
		}
		next, ok := cur[p].(map[string]interface{})
		if !ok {
			next = map[string]interface{}{}
			cur[p] = next
		}
		cur = next
	}
}
