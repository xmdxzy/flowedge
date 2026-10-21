package service

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// 模型 bundle 层：把「逻辑角色 → 具体权重文件 + 默认参数」从代码里搬进 JSON。
//
// 为什么必须做这一步（2026-10-21）：
//
//	此前 Wan / H3 / 超分 / flux 的权重文件名硬编码在 comfy_media.go 的 8 处，
//	step/cfg 也写死在函数里。结果换任何 checkpoint 都要改代码重编 exe，
//	别人机器上没有同名文件时**开箱即失败**，报错还是 ComfyUI 的
//	"400 prompt_outputs_failed_validation"——看不出是权重名不对。
//
// 设计约束（很关键，违反就会把现有环境跑坏）：
//
//  1. 内置默认值必须与改造前的硬编码值**逐字相同**。零配置启动行为不变。
//  2. 文件里只写想覆盖的字段，其余从内置默认继承（不用抄一整份）。
//  3. store 为 nil 时全部方法仍可用并返回内置默认——测试里用结构体字面量
//     构造 Text2ImageService 的老路径不能因此 panic。
//  4. 找不到文件不报错、不阻断启动：写一份默认文件出来，让用户有东西可改。
const (
	RoleT2IPrimary     = "t2i.primary"      // 文生图主模型
	RoleT2IBatch       = "t2i.batch"        // 批量出图（同 prompt 多张）
	RoleT2IMultiPrompt = "t2i.multi_prompt" // 多提示词批量（分镜图/角色表）
	RoleImg2Img        = "img2img"          // 图生图
	RoleInpaint        = "inpaint"          // 局部重绘
	RoleUpscale        = "upscale"          // 图片超分
	RoleVideoWan       = "video.wan"        // Wan2.1 文生视频
	RoleVideoH3Fl2va   = "video.h3.fl2va"   // MiniMax-H3 首帧生视频（含音频）
	RoleVideoH3Ref2va  = "video.h3.ref2va"  // MiniMax-H3 参考图/参考音频生视频
)

// ModelBundle 一个逻辑角色的完整描述。
//
// Files 的槽位含义按角色而定，读取方统一用 File(role, slot) 取：
//
//	t2i.* / img2img / inpaint → ckpt
//	upscale                   → model
//	video.wan                 → unet / clip / vae
//	video.h3.*                → unet / clip / video_vae / audio_vae
type ModelBundle struct {
	Label  string             `json:"label,omitempty"`
	Files  map[string]string  `json:"files,omitempty"`
	Params map[string]float64 `json:"params,omitempty"`
	// Ladder 精度降级链（高→低，第一项应与 files 里的默认权重一致）。
	//
	// 用途：OOM 时先换更省显存的权重，再考虑降分辨率。
	// 为什么精度优先于分辨率：换精度只损失细节保真，而降分辨率会改变构图，
	// 用户对后者的容忍度明显更低。
	//
	// 留空 = 该角色没有多精度权重，跳过这一档（本机权重多为 fp8/int8，
	// 默认全部为空——这条路径只在用户自己落位了 gguf/Q4 等变体后才生效）。
	Ladder []string `json:"ladder,omitempty"`
	Notes  string   `json:"notes,omitempty"`
}

// BareBundleFile 落盘格式：只允许覆盖，不要求全量。
type bundleFile struct {
	Schema  int                     `json:"$schema,omitempty"`
	Comment string                  `json:"_comment,omitempty"`
	Bundles map[string]*ModelBundle `json:"bundles"`
}

// BundleStore 线程安全的 bundle 仓库。
//
// path 为空 = 纯内存（回落到内置默认），用于测试与「不想落盘」的调用方。
type BundleStore struct {
	path    string
	mu      sync.RWMutex
	file    map[string]*ModelBundle // 仅文件里显式配置的部分
	loadErr error                   // 只记录不抛出：坏文件不应该让引擎起不来
}

// DefaultCheckpoint 内置默认主 checkpoint（与改造前 NewText2ImageService 的默认值一致）
const DefaultCheckpoint = "flux1-schnell-fp8.safetensors"

// builtinBundles 内置默认。这些值必须与 2026-10-21 改造前的硬编码逐字相同，
// 否则「升级引擎」会变成「悄悄换模型」。
func builtinBundles() map[string]*ModelBundle {
	return map[string]*ModelBundle{
		RoleT2IPrimary: {
			Label:  "文生图主模型",
			Files:  map[string]string{"ckpt": DefaultCheckpoint},
			Params: map[string]float64{"steps": 4, "cfg": 1.0, "width": 1024, "height": 1024},
			Notes:  "flux schnell 是 4 步蒸馏模型。换非 schnell 权重（如 flux dev / SDXL）必须同时把 steps 提到 20+、cfg 提到 3.5+，否则出糊图。",
		},
		RoleT2IBatch: {
			Label:  "批量出图（同提示词多张）",
			Files:  map[string]string{"ckpt": DefaultCheckpoint},
			Params: map[string]float64{"steps": 4, "cfg": 1.0},
			Notes:  "共用一次模型加载，batch_size 交给 ComfyUI。",
		},
		RoleT2IMultiPrompt: {
			Label:  "多提示词批量（分镜图/角色表）",
			Files:  map[string]string{"ckpt": DefaultCheckpoint},
			Params: map[string]float64{"steps": 4, "cfg": 1.0},
			Notes:  "N 个 prompt 编进一张图共享权重，避免每条都重走 checkpoint 载入。",
		},
		RoleImg2Img: {
			Label:  "图生图",
			Files:  map[string]string{"ckpt": DefaultCheckpoint},
			Params: map[string]float64{"steps": 8, "cfg": 1.0, "strength": 0.6},
		},
		RoleInpaint: {
			Label:  "局部重绘",
			Files:  map[string]string{"ckpt": DefaultCheckpoint},
			Params: map[string]float64{"steps": 10, "cfg": 1.0, "denoise": 1.0},
			Notes:  "flus-schnell 不是专用重绘模型，蒙版内修补偏弱；落位 Flux-Fill 后把 ckpt 指过去。",
		},
		RoleUpscale: {
			Label:  "图片超分",
			Files:  map[string]string{"model": "RealESRGAN_x4plus.pth"},
			Params: map[string]float64{"scale": 4},
		},
		RoleVideoWan: {
			Label: "Wan2.1 文生视频",
			Files: map[string]string{
				"unet": "wan2.1_t2v_1.3B_fp16.safetensors",
				"clip": "umt5_xxl_fp8_e4m3fn_scaled.safetensors",
				"vae":  "wan_2.1_vae.safetensors",
			},
			Params: map[string]float64{
				"width": 832, "height": 480, "frames": 33, "fps": 16, "steps": 20, "cfg": 6.0, "shift": 8.0,
			},
			Notes: "1.3B 轻量底座，24GB 可跑。换 14B 需同步改 unet 与显存档案。",
		},
		RoleVideoH3Fl2va: {
			Label: "MiniMax-H3 首帧生视频（含立体声）",
			Files: map[string]string{
				"unet":      "minimax_h3_fl2va_pruned_int8_convrot.safetensors",
				"clip":      "qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors",
				"video_vae": "minimax_h3_video_vae_fp16.safetensors",
				"audio_vae": "minimax_h3_audio_vae_fp32.safetensors",
			},
			Params: map[string]float64{"width": 608, "height": 352, "frames": 124, "steps": 20, "fps": 24},
			Notes:  "帧数必须落在 17k+5 网格（124/141/158…）。必须走 8189 独立实例（torch 2.14）。",
		},
		RoleVideoH3Ref2va: {
			Label: "MiniMax-H3 参考生视频（锁角色 + 念稿）",
			Files: map[string]string{
				"unet":      "minimax_h3_ref2va_pruned_int8_convrot.safetensors",
				"clip":      "qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors",
				"video_vae": "minimax_h3_video_vae_fp16.safetensors",
				"audio_vae": "minimax_h3_audio_vae_fp32.safetensors",
			},
			Params: map[string]float64{"width": 608, "height": 352, "frames": 124, "steps": 20, "fps": 24},
			Notes:  "ref2va 是**另一个 checkpoint**，只有它暴露 ref_audios，能 1:1 透传参考音频。",
		},
	}
}

// LoadBundleStore 从 path 读配置；文件不存在时写一份默认出来。
//
// 语义：
//   - path == "" → 纯内存，只有内置默认
//   - 读失败 / JSON 坏 → 记 loadErr，继续用内置默认（不让引擎起不来）
//   - 首次运行 → 落盘内置默认，用户拿到可编辑的文件
func LoadBundleStore(path string) *BundleStore {
	s := &BundleStore{path: path, file: map[string]*ModelBundle{}}
	if strings.TrimSpace(path) == "" {
		return s
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// 落盘默认配置：用户需要看到「原来模型名在这里改」
			if werr := s.writeDefaults(); werr != nil {
				s.loadErr = fmt.Errorf("写入默认模型配置失败：%w", werr)
			}
			return s
		}
		s.loadErr = fmt.Errorf("读取模型配置失败：%w", err)
		return s
	}
	var bf bundleFile
	if err := json.Unmarshal(raw, &bf); err != nil {
		s.loadErr = fmt.Errorf("解析模型配置失败（继续用内置默认）：%w", err)
		return s
	}
	for role, b := range bf.Bundles {
		if b == nil {
			continue
		}
		s.file[strings.TrimSpace(role)] = b
	}
	return s
}

// LoadErr 配置文件的问题（nil = 没问题）。用于能力面板提示，不用于阻断。
func (s *BundleStore) LoadErr() error {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.loadErr
}

// Path 配置文件位置（空 = 纯内存）
func (s *BundleStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// File 取文件槽位；文件没配则回落内置默认。store 为 nil 时只看内置默认。
func (s *BundleStore) File(role, slot string) string {
	if s == nil {
		if b, ok := builtinBundles()[role]; ok {
			return b.Files[slot]
		}
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if b, ok := s.file[role]; ok && b.Files != nil {
		if v := strings.TrimSpace(b.Files[slot]); v != "" {
			return v
		}
	}
	if b, ok := builtinBundles()[role]; ok {
		return b.Files[slot]
	}
	return ""
}

// ConfiguredFile 只返回**文件里显式配置**的槽位（不回落默认）。
// 用于「用户是否主动覆盖了主 checkpoint」这类判断。
func (s *BundleStore) ConfiguredFile(role, slot string) string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if b, ok := s.file[role]; ok && b.Files != nil {
		return strings.TrimSpace(b.Files[slot])
	}
	return ""
}

// Param 取数值参数（文件优先，回落内置，再回落 fallback）
func (s *BundleStore) Param(role, key string, fallback float64) float64 {
	if s != nil {
		s.mu.RLock()
		if b, ok := s.file[role]; ok && b.Params != nil {
			if v, ok := b.Params[key]; ok {
				s.mu.RUnlock()
				return v
			}
		}
		s.mu.RUnlock()
	}
	if b, ok := builtinBundles()[role]; ok && b.Params != nil {
		if v, ok := b.Params[key]; ok {
			return v
		}
	}
	return fallback
}

// ParamInt Param 的整数便捷版（阈值取整，避免 3.999 变 3）
func (s *BundleStore) ParamInt(role, key string, fallback int) int {
	v := s.Param(role, key, float64(fallback))
	return int(v + 0.5)
}

// Ladder 精度降级链（文件优先，回落内置；内置默认基本为空）
func (s *BundleStore) Ladder(role string) []string {
	if s != nil {
		s.mu.RLock()
		if b, ok := s.file[role]; ok && len(b.Ladder) > 0 {
			out := make([]string, len(b.Ladder))
			copy(out, b.Ladder)
			s.mu.RUnlock()
			return out
		}
		s.mu.RUnlock()
	}
	if b, ok := builtinBundles()[role]; ok && len(b.Ladder) > 0 {
		out := make([]string, len(b.Ladder))
		copy(out, b.Ladder)
		return out
	}
	return nil
}

// Resolve 返回合并后的完整 bundle（内置默认 + 文件覆盖），供 /api/models/bundles 展示
func (s *BundleStore) Resolve(role string) *ModelBundle {
	builtin, hasBuiltin := builtinBundles()[role]
	out := &ModelBundle{Files: map[string]string{}, Params: map[string]float64{}}
	if hasBuiltin {
		out.Label, out.Notes = builtin.Label, builtin.Notes
		for k, v := range builtin.Files {
			out.Files[k] = v
		}
		for k, v := range builtin.Params {
			out.Params[k] = v
		}
	}
	if s != nil {
		s.mu.RLock()
		if b, ok := s.file[role]; ok {
			if b.Label != "" {
				out.Label = b.Label
			}
			if b.Notes != "" {
				out.Notes = b.Notes
			}
			for k, v := range b.Files {
				if strings.TrimSpace(v) != "" {
					out.Files[k] = v
				}
			}
			for k, v := range b.Params {
				out.Params[k] = v
			}
		}
		s.mu.RUnlock()
	}
	return out
}

// Roles 全部已知角色（内置 + 文件新增），升序
func (s *BundleStore) Roles() []string {
	seen := map[string]bool{}
	for k := range builtinBundles() {
		seen[k] = true
	}
	if s != nil {
		s.mu.RLock()
		for k := range s.file {
			seen[k] = true
		}
		s.mu.RUnlock()
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Set 运行时修改一个角色的槽位（不落盘，供测试与 /api/models/bundles 试用）
func (s *BundleStore) Set(role, slot, value string) {
	if s == nil || role == "" || slot == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		s.file = map[string]*ModelBundle{}
	}
	b, ok := s.file[role]
	if !ok || b == nil {
		b = &ModelBundle{}
		s.file[role] = b
	}
	if b.Files == nil {
		b.Files = map[string]string{}
	}
	b.Files[slot] = value
}

// Merge 用给定 map 覆盖（用于一次性批量设置）
func (s *BundleStore) Merge(role string, files map[string]string, params map[string]float64) {
	if s == nil || role == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		s.file = map[string]*ModelBundle{}
	}
	b, ok := s.file[role]
	if !ok || b == nil {
		b = &ModelBundle{}
		s.file[role] = b
	}
	if b.Files == nil {
		b.Files = map[string]string{}
	}
	for k, v := range files {
		b.Files[k] = v
	}
	if len(params) > 0 {
		if b.Params == nil {
			b.Params = map[string]float64{}
		}
		for k, v := range params {
			b.Params[k] = v
		}
	}
}

// Save 把当前配置写回 path（path 为空时返回错误而不是静默成功）
func (s *BundleStore) Save() error {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return fmt.Errorf("模型配置未绑定文件路径，无法保存")
	}
	s.mu.RLock()
	snapshot := make(map[string]*ModelBundle, len(s.file))
	for k, v := range s.file {
		snapshot[k] = v
	}
	s.mu.RUnlock()
	bf := bundleFile{Schema: 1, Comment: bundleFileComment, Bundles: snapshot}
	raw, err := json.MarshalIndent(bf, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(s.path, append(raw, '\n'), 0o644)
}

const bundleFileComment = "逻辑角色 → 权重文件 + 默认参数。只写想覆盖的字段，其余继承内置默认。改完重启引擎生效；换非 4 步蒸馏权重时必须同时改 steps/cfg。"

// writeDefaults 首次运行时落盘一份可编辑的默认配置
func (s *BundleStore) writeDefaults() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	bf := bundleFile{Schema: 1, Comment: bundleFileComment, Bundles: builtinBundles()}
	raw, err := json.MarshalIndent(bf, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, append(raw, '\n'), 0o644)
}

// ---------- Text2ImageService 的便捷访问器 ----------
//
// 全部对 nil receiver / nil store 安全：测试里用 &Text2ImageService{BaseURL: ...}
// 字面量构造时，Bundles 为 nil，这些方法仍然返回内置默认值。

func (s *Text2ImageService) bundleFile(role, slot string) string {
	if s == nil {
		return ""
	}
	return s.Bundles.File(role, slot)
}

func (s *Text2ImageService) bundleParam(role, key string, fallback float64) float64 {
	if s == nil {
		return fallback
	}
	return s.Bundles.Param(role, key, fallback)
}

func (s *Text2ImageService) bundleParamInt(role, key string, fallback int) int {
	if s == nil {
		return fallback
	}
	return s.Bundles.ParamInt(role, key, fallback)
}

// Reload 重新读文件（配置改成热加载时用）
func (s *BundleStore) Reload() error {
	if s == nil || strings.TrimSpace(s.path) == "" {
		return fmt.Errorf("模型配置未绑定文件路径")
	}
	fresh := LoadBundleStore(s.path)
	s.mu.Lock()
	s.file = fresh.file
	s.loadErr = fresh.loadErr
	s.mu.Unlock()
	return fresh.loadErr
}
