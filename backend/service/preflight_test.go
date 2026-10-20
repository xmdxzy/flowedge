package service

import (
	"testing"

	"flowedge/backend/model"
)

// 保存期参数校验：缺 prompt 的 text2image 必须被拦（2026-10-20 审计：
// 「text2image node has no prompt」这类失败在图保存时就是必然的）
func TestValidateNodeParams_Text2ImageNoPrompt(t *testing.T) {
	nodes := []model.NodeInstance{
		{ID: "t", Type: "text2image", Data: map[string]interface{}{"batch": float64(2)}},
	}
	issues := ValidateNodeParams(nodes, map[string]bool{})
	if len(issues) != 1 || issues[0].NodeID != "t" {
		t.Fatalf("缺 prompt 的 text2image 应被拦，got %+v", issues)
	}

	// prompts 批量（一行一条）也算有提示词
	nodes[0].Data["prompts"] = "一只猫\n一只狗"
	if issues := ValidateNodeParams(nodes, map[string]bool{}); len(issues) != 0 {
		t.Fatalf("有 prompts 应放行，got %+v", issues)
	}
}

// upscale 无 image_path 但有上游入边 → 放行（上游动态供图）
func TestValidateNodeParams_UpscaleInboundEdgeOK(t *testing.T) {
	nodes := []model.NodeInstance{{ID: "u", Type: "upscale", Data: map[string]interface{}{}}}
	if issues := ValidateNodeParams(nodes, map[string]bool{"u": true}); len(issues) != 0 {
		t.Fatalf("有入边的 upscale 应放行，got %+v", issues)
	}
	if issues := ValidateNodeParams(nodes, map[string]bool{}); len(issues) != 1 {
		t.Fatalf("无入边无路径的 upscale 应被拦，got %+v", issues)
	}
}

// tts 节点 + TTS 不可达 → 预检必须报缺失（2026-10-20 审计：
// dial tcp 127.0.0.1:8102 拒连这类失败应该在入口就说明「TTS 没起」）
func TestPreflightDeps_TTSMissing(t *testing.T) {
	nodes := []model.NodeInstance{{ID: "s", Type: "tts", Data: map[string]interface{}{"text": "x"}}}
	caps := Capabilities{}
	missing := PreflightDeps(nodes, caps, nil)
	if len(missing) != 1 || missing[0].Dep != "tts" || missing[0].NodeID != "s" {
		t.Fatalf("TTS 不可达应报缺失，got %+v", missing)
	}
	caps.TTS.OK = true
	if missing := PreflightDeps(nodes, caps, nil); len(missing) != 0 {
		t.Fatalf("TTS 就绪应放行，got %+v", missing)
	}
}

// ComfyUI 没在跑但启动条件满足（ensureComfy 会自动拉起）→ 预检不得误伤
func TestPreflightDeps_ComfyAutoStartNotBlocked(t *testing.T) {
	nodes := []model.NodeInstance{{ID: "t", Type: "text2image", Data: map[string]interface{}{"prompt": "x"}}}
	caps := Capabilities{} // ComfyUI 未在跑
	// 构造一个「可启动」的 Supervisor（启动条件用不存在的路径 → 不可启动，作为对照组）
	sup := NewSupervisor("http://127.0.0.1:8188", "http://127.0.0.1:8101", "http://127.0.0.1:8102")
	sup.specs["comfyui"].Binary = "" // 空二进制 = 不可启动
	sup.specs["comfyui"].Args = nil
	if missing := PreflightDeps(nodes, caps, sup); len(missing) != 1 {
		t.Fatalf("启动条件不满足的 ComfyUI 应被拦，got %+v", missing)
	}
	// 恢复可启动状态（用真实探测到的路径）→ 不再拦
	sup2 := NewSupervisor("http://127.0.0.1:8188", "http://127.0.0.1:8101", "http://127.0.0.1:8102")
	if sup2.Startable("comfyui") {
		if missing := PreflightDeps(nodes, caps, sup2); len(missing) != 0 {
			t.Fatalf("可自动启动的 ComfyUI 不应被拦，got %+v", missing)
		}
	}
}
