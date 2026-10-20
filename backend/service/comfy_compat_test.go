package service

import "testing"

// qwen_image 家族必须在构图前被拦（根因：ComfyUI 把其 VAE 误判为 WanVAE，
// 12 vs 4 通道，执行期报 size mismatch——报错不指向根因）。
func TestCheckCheckpointCompat_QwenImageBlocked(t *testing.T) {
	for _, name := range []string{
		"qwen_image_2.1_turbo.safetensors",
		"QwenImage-2.1-FP8.safetensors",
		"qwenimage_edit_fp8.safetensors",
	} {
		err := CheckCheckpointCompat(name)
		if err == nil {
			t.Fatalf("%q 应被拦截", name)
		}
		// 错误信息必须带修复指引，不能只说「不兼容」
		if !containsFold(err.Error(), "flux1-schnell") && !containsFold(err.Error(), "升级") {
			t.Fatalf("错误信息缺少修复指引：%v", err)
		}
	}
}

// flux 默认组合必须放行（误拦能跑的组合比漏拦更糟）
func TestCheckCheckpointCompat_FluxAllowed(t *testing.T) {
	for _, name := range []string{
		"flux1-schnell-fp8.safetensors",
		"flux1-dev-fp8.safetensors",
		"",
	} {
		if err := CheckCheckpointCompat(name); err != nil {
			t.Fatalf("%q 应放行，got %v", name, err)
		}
	}
}

// BuildMediaGraph 接线验证：不兼容 checkpoint 在图构建入口就失败
func TestBuildMediaGraph_BlocksIncompatibleCheckpoint(t *testing.T) {
	s := &Text2ImageService{BaseURL: "http://127.0.0.1:1", Checkpoint: "qwen_image_2.1_turbo.safetensors"}
	if _, err := s.BuildMediaGraph(MediaRequest{Kind: "img2img", Image: "x.png"}); err == nil {
		t.Fatal("不兼容 checkpoint 应在构图前被拦")
	}
}
