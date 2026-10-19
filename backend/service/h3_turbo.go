package service

import (
	"os"
	"path/filepath"
)

// H3 turbo LoRA 探测
//
// 官方文档：turbo 蒸馏把默认 20 步压到 8 步（T2V/I2V），4 步用于 R2V。
// 实测长片最痛的就是「每段 4 分钟」，这一项能把 6 段从 25 分钟压到 ~10 分钟。
// 权重不在位时返回空串，调用方自动退回 base 20 步，不影响可用性。
const h3TurboLoRAName = "minimax_h3_turbo_v4_step600_ema.safetensors"

// H3 双调度（video / audio sigma shift）与 turbo 步数
//
// 依据（2026-10-19 复核）：
//   - ComfyUI 官方 H3 文档：模型定义默认 shift=12 / audio_shift=3；
//     并明确「音频在联合去噪中比画面晚收敛，台词与音色需要 12+ 步，
//     8 步时音频往往是短板」——这是"胡言乱语"的主因之一。
//   - turbo LoRA 模型卡（larryvrh / Abiray 的 ComfyUI 精简版）：
//     video shift 12 / audio shift 6 / steps 8–12 / res_multistep / LoRA 0.8–1.8；
//     并直接指出「音频爆音、错位几乎都是调度参数没配对齐」。
//
// 所以：base 用官方 12/3，turbo 用 12/6，步数默认 12（不是 8）。
const (
	h3ShiftVideo        = 12.0
	h3ShiftAudioBase    = 3.0
	h3ShiftAudioTurbo   = 6.0
	h3TurboDefaultSteps = 12
)

// H3TurboLoRAFile 返回 LoRA 文件名（ComfyUI 侧按名字加载），不存在则空
func H3TurboLoRAFile() string {
	if v := os.Getenv("H3_TURBO_LORA"); v != "" {
		if _, err := os.Stat(v); err == nil {
			return filepath.Base(v)
		}
		return ""
	}
	candidates := []string{
		filepath.Join(ModelsRoot(""), "comfyui", "loras", h3TurboLoRAName),
		filepath.Join("D:/AI/ComfyUI/models/loras", h3TurboLoRAName),
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return filepath.Base(c)
		}
	}
	return ""
}
