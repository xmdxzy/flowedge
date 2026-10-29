package service

import (
	"os"
	"path/filepath"
)

// H3 PDD 8-step 加速 LoRA（阿里 PAI 官方，FL2VA / Ref2VA 各一份）。
//
// 与 turbo 不同：PDD 是按变体训练的 8 步蒸馏，不能和 turbo 叠加。
// 文件不在位时调用方必须显式报错——不能静默退回 12 步假装加速成功。
const (
	h3PDDFL2VAName  = "MiniMax-H3-FL2VA-Acc-8Step.safetensors"
	h3PDDRef2VAName = "MiniMax-H3-Ref2VA-Acc-8Step.safetensors"
)

// H3PDDLoRAFile 按 kind 返回 PDD LoRA 文件名；不存在返回空串。
// h3_r2v 用 Ref2VA 版，其余（h3 / h3_i2v）用 FL2VA 版。
func H3PDDLoRAFile(kind string) string {
	name := h3PDDFL2VAName
	if kind == "h3_r2v" {
		name = h3PDDRef2VAName
	}
	candidates := []string{
		filepath.Join(ModelsRoot(""), "comfyui", "loras", name),
		filepath.Join("D:/AI/ComfyUI/models/loras", name),
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return filepath.Base(c)
		}
	}
	return ""
}
