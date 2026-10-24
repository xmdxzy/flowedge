package service

import (
	"fmt"
	"strings"
)

// Checkpoint 兼容矩阵：构建期提前拦「已知不兼容组合」。
//
// 根因（AGENTS.md §1「已知不可用」）：qwen_image_2.1 出图时 ComfyUI 把它的 VAE
// 误判成 WanVAE（12 通道 vs 4 通道），报 size mismatch。这个报错发生在
// ComfyUI 执行期，用户看到的是「图跑了一半才失败」，而且报错字符串
// （size mismatch）完全不指向根因（VAE 架构误判）。
//
// 上游 bug 修好之前，工程上的正确解不是绕路，而是**在构建图之前显式失败**：
// 把「这个组合当前版本跑不了、为什么、怎么办」说清楚，别让用户等到执行期。
// 与「未配置必须显式失败，绝不伪造结果」是同一条纪律。
//
// 升级 ComfyUI 后从这里删掉对应条目即可（附了解除条件的注释）。

// incompatibleCheckpoint 已知与当前 ComfyUI 版本不兼容的 checkpoint 规则。
// Match 为小写子串匹配；Reason 说根因；Remedy 说怎么办。
type incompatibleCheckpoint struct {
	Match  string
	Reason string
	Remedy string
}

// knownIncompatible 当前生效的不兼容清单。
// 每条都必须能在 AGENTS.md / docs 里指到根因记录，不允许放「疑似」条目——
// 误拦一个能跑的组合比漏拦一个不能跑的更糟（用户被挡在门外且不知道为什么）。
var knownIncompatible = []incompatibleCheckpoint{
	{
		Match:  "qwen_image",
		Reason: "当前 ComfyUI 版本把 qwen_image 的 VAE 误判为 WanVAE（12 通道 vs 4 通道），执行期报 size mismatch，权重本身没有问题",
		Remedy: "升级 ComfyUI 到修复该误判的版本后删除本条目；短期内可把 COMFYUI_CHECKPOINT 切回 flux1-schnell-fp8.safetensors（中文提示词建议先过 LLM 节点翻译）",
	},
	{
		Match:  "qwenimage",
		Reason: "同 qwen_image：当前 ComfyUI 版本对其中 VAE 架构误判（12 通道 vs 4 通道），执行期报 size mismatch",
		Remedy: "升级 ComfyUI 后解除；或换用 flux1-schnell-fp8.safetensors",
	},
}

// CheckCheckpointCompat 构建 ComfyUI 图之前调用。
// 返回 nil 表示放行；返回 error 表示该组合在当前实例上必然失败，
// 错误信息包含根因与修复办法（显式失败优于执行期 size mismatch）。
func CheckCheckpointCompat(ckpt string) error {
	name := strings.ToLower(strings.TrimSpace(ckpt))
	if name == "" {
		return nil // 空 = 用引擎默认，默认值随版本维护，不拦
	}
	for _, r := range knownIncompatible {
		if strings.Contains(name, r.Match) {
			return fmt.Errorf(
				"checkpoint %q 与当前 ComfyUI 版本不兼容：%s。修复：%s",
				ckpt, r.Reason, r.Remedy)
		}
	}
	return nil
}
