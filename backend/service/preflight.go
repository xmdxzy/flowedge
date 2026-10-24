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
//
// Fix / Fixable / FixDep 是「一键修复」三件套。只报「缺什么」时用户还得自己
// 去环境面板找对应开关——把可修复的项直接带上要起哪个依赖（FixDep），
// 前端就能点一下拉起来；不可修的（Ollama 由用户自己托管）给出确切命令。
type DepRequirement struct {
	NodeID   string `json:"node_id"`
	NodeType string `json:"node_type"`
	Dep      string `json:"dep"` // ollama / comfyui / comfy_h3 / asr / tts / vision
	Label    string `json:"label"`
	Hint     string `json:"hint"`
	Fix      string `json:"fix"`     // 可执行的修复动作（人话，能照做）
	Fixable  bool   `json:"fixable"` // 引擎能否代劳（true 时前端给「一键修复」按钮）
	FixDep   string `json:"fix_dep"` // 要拉起的依赖名（对应 /api/deps/:name/start）
}

// depFix 缺依赖时的修复路径。
// 分两类：引擎托管的（asr/tts/comfyui/comfy_h3）能一键拉起；
// 引擎不托管的（Ollama，见 AGENTS.md）只给确切命令——不假装能修。
func depFix(dep string) (fixDep string, fixable bool, fix string) {
	switch dep {
	case "asr":
		return "asr", true, "点「一键修复」启动 ASR 服务（faster-whisper，首次加载模型约 10 秒）"
	case "tts":
		return "tts", true, "点「一键修复」启动 TTS 服务（Kokoro-82M）"
	case "comfyui":
		return "comfyui", true, "点「一键修复」启动 ComfyUI（冷启动约 20–40 秒，启动中不要重复点）"
	case "comfy_h3":
		return "comfy_h3", true, "点「一键修复」启动 ComfyUI-H3；注意它与默认实例互斥，会自动切实例"
	case "ollama":
		// 语言模型现在也是可托管依赖（video 的提示词优化、拆镜、分镜规划都要它）。
		// 以前只能让用户"另开终端跑 ollama serve"——在双击 exe 的交付形态里等于
		// 把最后一步退化成手工操作。已装桌面版时 Start 只会探活复用，不会拉第二个。
		return "ollama", true, "点「一键修复」启动 Ollama（ollama serve）；若本机没装，装 Ollama 桌面版后重试"
	case "vision":
		return "", false, "先 `ollama pull qwen2.5vl:7b` 拉一个视觉模型，再在视觉节点里填上模型名"
	}
	return "", false, "在环境面板检查该项依赖后重试"
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
		fd, fx, ftext := depFix(dep)
		// 服务在跑但内部不可用（缺 Python 依赖等）：一键启动帮不上忙，
		// 必须给出装包命令，否则用户会反复点「一键修复」而毫无变化。
		if c.Degraded {
			fd, fx = "", false
			if c.Hint != "" {
				ftext = c.Hint
			}
		}
		missing = append(missing, DepRequirement{
			NodeID: n.ID, NodeType: n.Type, Dep: dep, Label: label, Hint: c.Hint,
			Fix: ftext, Fixable: fx, FixDep: fd,
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
			dep := "comfy" + profileSuffix(profile)
			fd, fx, ftext := depFix(dep)
			missing = append(missing, DepRequirement{
				NodeID: n.ID, NodeType: n.Type, Dep: dep,
				Label: label, Hint: c.Hint,
				Fix: ftext, Fixable: fx, FixDep: fd,
			})
		}
	}

	// 依赖按节点类型直接判定（不再走"模块契约"抽象）：
	// 节点只剩 13 种，每种要哪个服务是确定的；多一层契约只会把
	// "加了节点忘登记"变成静默放行。
	for i := range nodes {
		n := &nodes[i]
		switch n.Type {
		case "text2image", "image2image", "inpaint", "upscale":
			if c, ok := capabilityOf("comfyui", caps); ok {
				addComfy(n, "", "文生图/图像编辑（ComfyUI）", c)
			}
		case "text2video":
			profile, label := "", "视频生成（ComfyUI）"
			if b, _ := n.Data["backend"].(string); b == "h3" {
				profile, label = "h3", "视频生成（ComfyUI-H3）"
			}
			dep := "comfyui"
			if profile == "h3" {
				dep = "comfy_h3"
			}
			if c, ok := capabilityOf(dep, caps); ok {
				addComfy(n, profile, label, c)
			}
			// H3 默认先过一遍提示词优化（node.data.prompt_opt，缺省 true），
			// 那是语言侧调用：Ollama 没起的话会在节点里炸，而不是在入口说清楚。
			// 显式 prompt_opt=false 的调用方不想用 LLM，就不该被拦。
			if profile == "h3" && promptOptEnabled(n) {
				if c, ok := capabilityOf("ollama", caps); ok {
					add(n, "ollama", "提示词优化（Ollama）", c)
				}
			}
		case "llm", "prompt_opt", "storyboard":
			if c, ok := capabilityOf("ollama", caps); ok {
				add(n, "ollama", "语言模型（Ollama）", c)
			}
		case "shotlist":
			if c, ok := capabilityOf("comfy_h3", caps); ok {
				addComfy(n, "h3", "分镜生成（ComfyUI-H3）", c)
			}
			if v, _ := n.Data["audio_check"].(bool); v {
				if c, ok := capabilityOf("asr", caps); ok {
					add(n, "asr", "音频抽检（ASR）", c)
				}
			}
		case "tts":
			if c, ok := capabilityOf("tts", caps); ok {
				add(n, "tts", "语音合成（TTS）", c)
			}
		}
	}

	return missing
}

// capabilityOf 依赖名 → 探测结果。未知依赖名返回 false：调用方跳过，
// 但 preflight_test 会核对「节点类型清单里每种都至少消耗一个已知依赖」。
func capabilityOf(dep string, caps Capabilities) (Capability, bool) {
	switch dep {
	case "ollama":
		return caps.Ollama, true
	case "comfyui":
		return caps.ComfyUI, true
	case "comfy_h3":
		return caps.ComfyH3, true
	case "asr":
		return caps.ASR, true
	case "tts":
		return caps.TTS, true
	case "vision":
		return caps.Vision, true
	case "ffmpeg":
		return caps.FFmpeg, true
	}
	return Capability{}, false
}

// promptOptEnabled text2video 是否会在跑之前先调 LLM 优化提示词。
// 与执行体保持同一套缺省：只有显式 false 才算关闭（H3 默认开）。
func promptOptEnabled(n *model.NodeInstance) bool {
	if n == nil {
		return true
	}
	if v, ok := n.Data["prompt_opt"].(bool); ok {
		return v
	}
	return true
}
