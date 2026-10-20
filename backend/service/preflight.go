package service

import "flowedge/backend/model"

// 运行前依赖预检。
//
// 背景（2026-10-20 执行记录审计）：17 次 failed 里有 5 次是「服务没起就调」——
// TTS 端口拒连、Ollama 404/拒连、视觉模型没拉。这些失败有个共同点：
// 节点跑进去才报一句连接错误，用户看到的是「工作流失败了」，
// 而不是「TTS 没起，去环境面板点一下启动」。预检把这句话提前到入口。
//
// 边界（避免误伤）：
//   - ComfyUI / ComfyUI-H3 不按「是否在跑」拦截：ensureComfy 会自动拉起并按
//     Exclusive 语义切换实例，正在启动中的实例被拦掉反而是错伤。只拦
//     「启动条件都不满足」（venv/工作树缺失，Startable=false）——那种情况
//     节点执行时也必然失败，且 hint 早已备好。
//   - Ollama / ASR / TTS / 视觉模型必须拦：引擎不托管 Ollama（AGENTS.md），
//     ASR/TTS 也没有 ensure* 自动拉起路径，不起就是不起。

// DepRequirement 一次运行缺失的依赖项
type DepRequirement struct {
	NodeID   string `json:"node_id"`
	NodeType string `json:"node_type"`
	Dep      string `json:"dep"` // ollama / comfyui / comfy_h3 / asr / tts / vision
	Label    string `json:"label"`
	Hint     string `json:"hint"`
}

// PreflightDeps 按工作流节点清单核对依赖可达性。
// caps 由调用方现场探测（ProbeCapabilities，<1.5s），
// 保证「先在环境面板点了启动、再跑工作流」这条路径不被缓存状态误判。
// sup 可为 nil：nil 时跳过 ComfyUI 启动条件检查（只查 Ollama/ASR/TTS/视觉）。
func PreflightDeps(nodes []model.NodeInstance, caps Capabilities, sup *Supervisor) []DepRequirement {
	var missing []DepRequirement
	add := func(n *model.NodeInstance, dep, label string, c Capability) {
		if c.OK {
			return
		}
		missing = append(missing, DepRequirement{
			NodeID: n.ID, NodeType: n.Type, Dep: dep, Label: label, Hint: c.Hint,
		})
	}
	comfyStartable := func(profile string) bool {
		if sup == nil {
			return true
		}
		name := "comfyui"
		if profile == "h3" {
			name = "comfy_h3"
		}
		return sup.Startable(name)
	}
	profileSuffix := func(profile string) string {
		if profile == "h3" {
			return "_h3"
		}
		return "ui"
	}
	addComfy := func(n *model.NodeInstance, profile, label string, c Capability) {
		if !c.OK && !comfyStartable(profile) {
			missing = append(missing, DepRequirement{
				NodeID: n.ID, NodeType: n.Type, Dep: "comfy" + profileSuffix(profile),
				Label: label, Hint: c.Hint,
			})
		}
	}

	for i := range nodes {
		n := &nodes[i]
		switch n.Type {
		case "llm", "storyboard":
			add(n, "ollama", "LLM（Ollama）", caps.Ollama)
		case "vision":
			add(n, "vision", "视觉模型", caps.Vision)
		case "tts":
			add(n, "tts", "TTS（Kokoro）", caps.TTS)
		case "asr":
			add(n, "asr", "ASR（faster-whisper）", caps.ASR)
		case "text2image", "image2image", "inpaint", "upscale":
			addComfy(n, "", "ComfyUI（图像）", caps.ComfyUI)
		case "text2video":
			if backend, _ := n.Data["backend"].(string); backend == "h3" {
				addComfy(n, "h3", "ComfyUI-H3（音视频联合生成）", caps.ComfyH3)
			} else {
				addComfy(n, "", "ComfyUI（视频）", caps.ComfyUI)
			}
		case "shotlist":
			addComfy(n, "h3", "ComfyUI-H3（音视频联合生成）", caps.ComfyH3)
		}
	}
	return missing
}
