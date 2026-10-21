package service

import "testing"

// TestNextPrecision_EmptyLadderSkips 没有多精度权重时必须跳过这一档，
// 第一档仍然是降分辨率（保持原有行为）。
func TestNextPrecision_EmptyLadderSkips(t *testing.T) {
	req := MediaRequest{Kind: "t2v", Width: 608, Height: 352, Frames: 124, Steps: 20}
	if _, ok := nextPrecision(req, nil); ok {
		t.Fatal("empty ladder must not degrade precision")
	}
	d, ok := degradeMediaRequest(req, nil)
	if !ok || d.Width >= req.Width {
		t.Fatalf("first step should still be resolution, got %dx%d", d.Width, d.Height)
	}
	if d.Precision != "" {
		t.Errorf("precision must stay empty, got %q", d.Precision)
	}
}

// TestDegradeMediaRequest_PrecisionFirst 配了阶梯时精度优先于分辨率：
// 换精度不改构图，降分辨率会改。
func TestDegradeMediaRequest_PrecisionFirst(t *testing.T) {
	ladder := []string{"flux-fp8.safetensors", "flux-Q4.gguf"}
	req := MediaRequest{Kind: "img2img", Width: 1024, Height: 1024, Steps: 8}
	d1, ok := degradeMediaRequest(req, ladder)
	if !ok {
		t.Fatal("should degrade")
	}
	if d1.Precision != "flux-Q4.gguf" {
		t.Errorf("precision = %q, want flux-Q4.gguf", d1.Precision)
	}
	if d1.Width != req.Width || d1.Height != req.Height {
		t.Errorf("resolution must not change on the precision step: %dx%d", d1.Width, d1.Height)
	}
	if d1.Frames != req.Frames || d1.Steps != req.Steps {
		t.Error("only one variable per step")
	}

	// 再降一档才动分辨率
	d2, ok2 := degradeMediaRequest(d1, ladder)
	if !ok2 || d2.Width >= d1.Width {
		t.Fatalf("second step should degrade resolution, got %dx%d ok=%v", d2.Width, d2.Height, ok2)
	}
	if d2.Precision != d1.Precision {
		t.Error("precision should stay at the last rung")
	}
}

// TestNextPrecision_UnknownCurrentDoesNotGuess 当前权重不在阶梯里时不动——
// 猜错会把用户指定的模型悄悄换掉，比 OOM 更难查。
func TestNextPrecision_UnknownCurrentDoesNotGuess(t *testing.T) {
	ladder := []string{"a.safetensors", "b.gguf"}
	req := MediaRequest{Kind: "img2img", Precision: "custom-mine.safetensors"}
	if _, ok := nextPrecision(req, ladder); ok {
		t.Fatal("must not guess when current weight is outside the ladder")
	}
}

// TestNextPrecision_BottomOfLadder 走到最底就不再降
func TestNextPrecision_BottomOfLadder(t *testing.T) {
	ladder := []string{"a.safetensors", "b.gguf"}
	req := MediaRequest{Kind: "img2img", Precision: "b.gguf"}
	if _, ok := nextPrecision(req, ladder); ok {
		t.Fatal("bottom of ladder must not degrade further")
	}
}

// TestDegradeNote_MentionsPrecision 降级说明必须写清换了哪一档权重
func TestDegradeNote_MentionsPrecision(t *testing.T) {
	before := MediaRequest{Kind: "img2img", Width: 1024, Height: 1024}
	after := before
	after.Precision = "flux-Q4.gguf"
	note := degradeNote(before, after)
	if !containsSub(note, "精度档") || !containsSub(note, "flux-Q4.gguf") {
		t.Errorf("note should name the precision rung: %s", note)
	}
}

func containsSub(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestBundleStore_Ladder 精度阶梯从配置文件读，且不污染内置默认
func TestBundleStore_Ladder(t *testing.T) {
	store := LoadBundleStore("")
	if got := store.Ladder(RoleT2IPrimary); len(got) != 0 {
		t.Errorf("builtin ladder should be empty, got %v", got)
	}
	store.Merge(RoleT2IPrimary, nil, nil)
	store.Set(RoleT2IPrimary, "ckpt", "flux-fp8.safetensors")
	// 直接写 ladder（Merge 只处理 files/params，这里验证 Ladder 读取路径）
	store.mu.Lock()
	store.file[RoleT2IPrimary].Ladder = []string{"flux-fp8.safetensors", "flux-Q4.gguf"}
	store.mu.Unlock()
	got := store.Ladder(RoleT2IPrimary)
	if len(got) != 2 || got[1] != "flux-Q4.gguf" {
		t.Errorf("ladder = %v", got)
	}
	// 返回值必须是拷贝：外部改它不能影响仓库
	got[0] = "mutated"
	if store.Ladder(RoleT2IPrimary)[0] == "mutated" {
		t.Error("Ladder must return a copy")
	}
}

// TestApplyPrecisionToGraph 精度档真的进了提交给 ComfyUI 的图
func TestApplyPrecisionToGraph(t *testing.T) {
	svc := &Text2ImageService{
		BaseURL: "http://127.0.0.1:8188", H3BaseURL: "http://127.0.0.1:8189",
		Bundles: LoadBundleStore(""),
	}
	graph, err := svc.BuildMediaGraph(MediaRequest{Kind: "h3", Prompt: "x", Precision: "low_vram_h3.safetensors"})
	if err != nil {
		t.Fatal(err)
	}
	unet := graph["1"].(map[string]interface{})["inputs"].(map[string]interface{})["unet_name"]
	if unet != "low_vram_h3.safetensors" {
		t.Errorf("unet_name = %v, want the precision override", unet)
	}
}
