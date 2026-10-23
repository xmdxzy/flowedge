package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

// 多 ComfyUI 实例路由
//
// 解决的真实问题（2026-10-21）：
//
//	H3 需要 torch 2.14 + 完整量化后端，flux/Wan 在 torch 2.6 的主实例上。
//	这两套环境在**同一张卡**上互斥（谁先占显存谁赢），但必须能共存于一台机器。
//	在此之前这套知识只存在于 AGENTS.md：用户得手动改 H3_BASE_URL、手动停一个实例，
//	搞混了就得到一句 "OOM"（AGENTS §0.2：报错字符串不是根因）。
//
// 做成配置后：
//   - 模型 → 实例的映射是数据，不是代码分支
//   - 同 exclusive_group 的实例自动串行（不需要人记得先停谁）
//   - 新增第三套环境（比如 SD3.5 专用 venv）只是加一条记录
//
// 重要约束：**默认配置必须与改造前行为逐字一致**（8188 主实例 / 8189 H3），
// 否则升级会变成"悄悄换了实例"，表现为本来能出的片突然 OOM。
const instanceSchemaVersion = 1

// Instance 一个 ComfyUI 实例
type Instance struct {
	Name           string   `json:"name"`
	BaseURL        string   `json:"base_url"`
	Models         []string `json:"models,omitempty"`          // 通配符，如 minimax_h3*
	ExclusiveGroup string   `json:"exclusive_group,omitempty"` // 同组互斥（单卡不能同时驻留）
	ReserveVRAM    int      `json:"reserve_vram,omitempty"`    // 启动参数记录（--reserve-vram）
	Enabled        bool     `json:"enabled"`
	Note           string   `json:"note,omitempty"`
}

// InstanceRegistry 实例注册表
type InstanceRegistry struct {
	path     string
	mu       sync.RWMutex
	items    []Instance
	def      string
	groupMu  map[string]*sync.Mutex
	loadNote string
}

// defaultInstances 内置默认：与改造前 H3_BASE_URL / COMFYUI_BASE_URL 的语义一致
func defaultInstances(h3URL string) []Instance {
	if strings.TrimSpace(h3URL) == "" {
		h3URL = "http://127.0.0.1:8189"
	}
	return []Instance{
		{
			Name: "main", BaseURL: "http://127.0.0.1:8188",
			Models:      []string{"flux*", "sd*", "wan*", "RealESRGAN*", "umt5*", "ae.safetensors"},
			ReserveVRAM: 2, Enabled: true,
			Note: "主实例：torch 2.6，跑扩散图像与 Wan 视频",
		},
		{
			Name: "h3", BaseURL: h3URL,
			Models:         []string{"minimax_h3*", "qwen3vl_32b_minimax*"},
			ExclusiveGroup: "gpu-exclusive", ReserveVRAM: 4, Enabled: true,
			Note: "H3 独立工作树：torch 2.14 + 量化后端；与主实例互斥（不能同时驻留）",
		},
		{
			// MOSS-Image：必须走 0.41+ 的 ComfyUI（MOSS 节点在 v0.41.0 之后才合入），
			// 主实例停在 0.31（comfy-kitchen 0.2.27 + torch 2.6 的血泪组合，动不得）。
			// 与 H3 同组的原因和 H3 一样：5B DiT + 9B 编码器约 11GB，加上主实例
			// 驻留的 flux 17GB 是 28GB > 24GB，同卡必 OOM。
			Name: "moss", BaseURL: mossInstanceURL(),
			Models:         []string{"moss_image*", "qwen3.5_9b*", "flux2-vae*"},
			ExclusiveGroup: "gpu-exclusive", ReserveVRAM: 2, Enabled: true,
			Note: "MOSS-Image 专用工作树（ComfyUI 0.41，复用 H3 的 torch 2.14 venv）；文字渲染与布局控制",
		},
	}
}

// mossInstanceURL MOSS 实例地址（MOSS_BASE_URL 可覆盖，默认 8190）
func mossInstanceURL() string {
	if v := strings.TrimSpace(os.Getenv("MOSS_BASE_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://127.0.0.1:8190"
}

// LoadInstanceRegistry 读配置；文件不存在则落一份默认（用户需要看到在哪加实例）
func LoadInstanceRegistry(p, h3URL string) *InstanceRegistry {
	r := &InstanceRegistry{path: p, groupMu: map[string]*sync.Mutex{}}
	r.items = defaultInstances(h3URL)
	r.def = "main"
	if strings.TrimSpace(p) == "" {
		return r
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			_ = r.Save()
		}
		return r
	}
	var f struct {
		Instances []Instance `json:"instances"`
		Default   string     `json:"default"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		r.loadNote = "实例配置解析失败（继续用内置默认）：" + err.Error()
		return r
	}
	if len(f.Instances) > 0 {
		r.items = f.Instances
	}
	if strings.TrimSpace(f.Default) != "" {
		r.def = f.Default
	}
	// 迁移：引擎新增了实例（例如 MOSS），但老配置文件里没有 → 补进去并落盘。
	//
	// 为什么必须在代码里做而不是让用户手改：实例表是数据驱动的，而"数据文件优先于内置默认"
	// 意味着**新版本加了实例，老用户的机器上永远不会生效**。表现是
	// 「后端选了 moss，图却发给了主实例」→ ComfyUI 400，报错完全不指向实例。
	// 2026-10-23 真机复现：`运行/data/instances.json` 只有 main + h3，
	// moss 权重被路由到 8188，而 8188 上根本没有 MOSS 节点。
	if r.mergeBuiltinInstances(defaultInstances(h3URL)) {
		if err := r.Save(); err != nil {
			r.loadNote = "已补齐内置实例但写回失败（本次运行有效）：" + err.Error()
		}
	}
	return r
}

// mergeBuiltinInstances 把内置里「配置还没有、也没被别人覆盖」的实例补进来。
//
// 两条保守规则（宁可少补也不乱补）：
//  1. 同名实例已存在 → 保留用户那份，不动
//  2. 任何已有实例的模型通配与内置实例前缀重叠 → 视为用户已经自己配过，不补
//     （避免把用户故意删掉的实例又复活成两份）
func (r *InstanceRegistry) mergeBuiltinInstances(builtin []Instance) bool {
	// 前提：这份配置是「引擎维护的」。判据是它里面还留着至少一个内置实例名。
	// 用户要是完全自己重写了实例表（一个内置名都不剩），说明他就是要这份清单，
	// 补内置等于回填他删掉的东西——既有测试 TestInstanceRegistry_SaveLoad 盯的就是这条。
	managed := false
	builtinNames := map[string]bool{}
	for _, b := range builtin {
		builtinNames[b.Name] = true
	}
	for _, it := range r.items {
		if builtinNames[it.Name] {
			managed = true
			break
		}
	}
	if !managed {
		return false
	}
	changed := false
	for _, b := range builtin {
		if strings.TrimSpace(b.Name) == "" {
			continue
		}
		exists, covered := false, false
		for _, it := range r.items {
			if it.Name == b.Name {
				exists = true
				break
			}
			for _, have := range it.Models {
				for _, want := range b.Models {
					if modelsOverlap(have, want) {
						covered = true
					}
				}
			}
		}
		if exists || covered {
			continue
		}
		r.items = append(r.items, b)
		changed = true
	}
	return changed
}

// modelsOverlap 两个通配 pattern 是否指向同一族模型。
//
// 必须按**分隔符边界**判断，不能只看字符串前缀：`flux*` 与 `flux2-vae*`
// 在字符上前缀重叠，但一个是 flux checkpoint、一个是 FLUX.2 的 VAE，
// 混为一谈会让「用户已经配过 moss」被误判成立，于是 moss 实例永远补不进来
// （2026-10-23 实测踩到：moss 权重被路由回主实例）。
func modelsOverlap(a, b string) bool {
	a = strings.ToLower(strings.TrimRight(strings.TrimSpace(a), "*"))
	b = strings.ToLower(strings.TrimRight(strings.TrimSpace(b), "*"))
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	if !strings.HasPrefix(b, a) {
		return false
	}
	switch b[len(a)] {
	case '_', '-', '.', '/':
		return true
	}
	return false
}

// Path 配置文件位置（空 = 纯内存）
func (r *InstanceRegistry) Path() string {
	if r == nil {
		return ""
	}
	return r.path
}

// LoadNote 配置加载时的问题（nil 表示正常）
func (r *InstanceRegistry) LoadNote() string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.loadNote
}

// All 全部实例
func (r *InstanceRegistry) All() []Instance {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Instance, len(r.items))
	copy(out, r.items)
	return out
}

// DefaultInstance 默认实例（找不到时返回空）
func (r *InstanceRegistry) DefaultInstance() Instance {
	if r == nil {
		return Instance{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, it := range r.items {
		if it.Name == r.def && it.Enabled {
			return it
		}
	}
	for _, it := range r.items {
		if it.Enabled {
			return it
		}
	}
	return Instance{}
}

// ResolveForModel 按模型名路由到实例。
//
// 匹配顺序：先精确/通配匹配 models，再回落默认实例。
// 没匹配到不报错——未知模型交给默认实例，让它用自己的报错说话
// （预检会给出「权重不在枚举里」，比这里猜更准）。
func (r *InstanceRegistry) ResolveForModel(model string) Instance {
	if r == nil {
		return Instance{}
	}
	model = strings.TrimSpace(model)
	if model != "" {
		r.mu.RLock()
		for _, it := range r.items {
			if !it.Enabled {
				continue
			}
			for _, pat := range it.Models {
				if matchModel(pat, model) {
					r.mu.RUnlock()
					return it
				}
			}
		}
		r.mu.RUnlock()
	}
	return r.DefaultInstance()
}

// matchModel 通配符匹配（不带 * 的模板视为前缀匹配，容忍目录前缀如 models/flux.safetensors）
func matchModel(pattern, model string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return false
	}
	if ok, err := path.Match(pattern, model); err == nil && ok {
		return true
	}
	if ok, err := path.Match(pattern, filepath.Base(model)); err == nil && ok {
		return true
	}
	// "minimax_h3" 这种不带通配的写法按前缀处理（用户不会去记通配符语法）
	if !strings.ContainsAny(pattern, "*?[") {
		return strings.HasPrefix(model, pattern)
	}
	return false
}

// LockForBase 若该实例属于互斥组，返回 unlock 函数（否则返回空操作）。
//
// 为什么要锁：两个实例在同一张 24G 卡上同时驻留 = 必 OOM。
// 改造前靠"人记得先停一个"，现在靠配置里的 exclusive_group。
func (r *InstanceRegistry) LockForBase(baseURL string) func() {
	if r == nil {
		return func() {}
	}
	group := ""
	r.mu.RLock()
	for _, it := range r.items {
		if strings.EqualFold(strings.TrimRight(it.BaseURL, "/"), strings.TrimRight(baseURL, "/")) {
			group = it.ExclusiveGroup
			break
		}
	}
	r.mu.RUnlock()
	if group == "" {
		return func() {}
	}
	r.mu.Lock()
	mu, ok := r.groupMu[group]
	if !ok {
		mu = &sync.Mutex{}
		r.groupMu[group] = mu
	}
	r.mu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// Save 落盘
func (r *InstanceRegistry) Save() error {
	if r == nil || strings.TrimSpace(r.path) == "" {
		return fmt.Errorf("实例配置未绑定文件路径")
	}
	r.mu.RLock()
	items := make([]Instance, len(r.items))
	copy(items, r.items)
	def := r.def
	r.mu.RUnlock()
	raw, err := json.MarshalIndent(map[string]interface{}{
		"$schema":   instanceSchemaVersion,
		"_comment":  "模型 → ComfyUI 实例路由。exclusive_group 相同的实例不会同时驻留（单卡必须的约束）。models 支持通配符。",
		"default":   def,
		"instances": items,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(r.path, append(raw, '\n'), 0o644)
}

// SetDefault 改默认实例名（内存；调 Save 才落盘）
func (r *InstanceRegistry) SetDefault(name string) { r.def = name }

// Replace 整体替换实例列表（供 API 修改）
func (r *InstanceRegistry) Replace(items []Instance) error {
	seen := map[string]bool{}
	for _, it := range items {
		if strings.TrimSpace(it.Name) == "" {
			return fmt.Errorf("实例必须有 name")
		}
		if strings.TrimSpace(it.BaseURL) == "" {
			return fmt.Errorf("实例 %s 缺少 base_url", it.Name)
		}
		if seen[it.Name] {
			return fmt.Errorf("实例名 %s 重复", it.Name)
		}
		seen[it.Name] = true
	}
	r.mu.Lock()
	r.items = items
	r.mu.Unlock()
	return nil
}

// baseForModelName 按权重文件名路由实例（供文生图后端使用）。
//
// 与 baseForKind 的区别：kind 是「节点要什么」，模型名是「这次实际加载哪个文件」。
// MOSS 必须用模型名路由——同一个 text2image 节点换后端就要换实例，
// 按 kind 永远只会落到主实例，然后报 400（主实例没有 UNETLoader 的 moss 权重）。
func (s *Text2ImageService) baseForModelName(model string) string {
	if s == nil {
		return ""
	}
	if s.Instances == nil {
		return s.BaseURL
	}
	inst := s.Instances.ResolveForModel(model)
	if inst.BaseURL != "" {
		return inst.BaseURL
	}
	return s.BaseURL
}

// baseForKind 按媒体类型取实例地址（供 comfy_media 使用）。
// registry 为 nil 时回落到传入的默认地址——这就是改造前的行为。
func (s *Text2ImageService) baseForKind(kind string) string {
	if s == nil {
		return ""
	}
	if s.Instances == nil {
		if kind == "h3" || kind == "h3_i2v" || kind == "h3_r2v" {
			return s.H3BaseURL
		}
		return s.BaseURL
	}
	// 按实际权重名路由：模型决定了需要哪套运行环境，而不是节点类型
	var model string
	switch kind {
	case "h3", "h3_i2v":
		model = s.bundleFile(RoleVideoH3Fl2va, "unet")
	case "h3_r2v":
		model = s.bundleFile(RoleVideoH3Ref2va, "unet")
	case "t2v":
		model = s.bundleFile(RoleVideoWan, "unet")
	case "upscale":
		model = s.bundleFile(RoleUpscale, "model")
	default:
		model = s.ckptName()
	}
	inst := s.Instances.ResolveForModel(model)
	if inst.BaseURL != "" {
		return inst.BaseURL
	}
	if kind == "h3" || kind == "h3_i2v" || kind == "h3_r2v" {
		return s.H3BaseURL
	}
	return s.BaseURL
}

// ckptName 当前主 checkpoint（Checkpoint 字段优先，回落 bundle）
func (s *Text2ImageService) ckptName() string {
	if s == nil {
		return ""
	}
	if strings.TrimSpace(s.Checkpoint) != "" {
		return s.Checkpoint
	}
	return s.bundleFile(RoleT2IPrimary, "ckpt")
}
