package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestBuiltinBundlesMatchLegacyHardcode 锁死「零配置启动行为不变」。
//
// 这些值必须与 2026-10-21 改造前 comfy_media.go 里的硬编码逐字相同；
// 任何一次「顺手换个默认模型」都会让升级变成静默换模型，必须在这里拦住。
func TestBuiltinBundlesMatchLegacyHardcode(t *testing.T) {
	cases := []struct{ role, slot, want string }{
		{RoleT2IPrimary, "ckpt", "flux1-schnell-fp8.safetensors"},
		{RoleUpscale, "model", "RealESRGAN_x4plus.pth"},
		{RoleVideoWan, "unet", "wan2.1_t2v_1.3B_fp16.safetensors"},
		{RoleVideoWan, "clip", "umt5_xxl_fp8_e4m3fn_scaled.safetensors"},
		{RoleVideoWan, "vae", "wan_2.1_vae.safetensors"},
		{RoleVideoH3Fl2va, "unet", "minimax_h3_fl2va_pruned_int8_convrot.safetensors"},
		{RoleVideoH3Ref2va, "unet", "minimax_h3_ref2va_pruned_int8_convrot.safetensors"},
		{RoleVideoH3Fl2va, "clip", "qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors"},
		{RoleVideoH3Fl2va, "video_vae", "minimax_h3_video_vae_fp16.safetensors"},
		{RoleVideoH3Fl2va, "audio_vae", "minimax_h3_audio_vae_fp32.safetensors"},
	}
	var nilStore *BundleStore
	for _, c := range cases {
		if got := nilStore.File(c.role, c.slot); got != c.want {
			t.Errorf("nil store %s/%s = %q, want %q", c.role, c.slot, got, c.want)
		}
		if got := LoadBundleStore("").File(c.role, c.slot); got != c.want {
			t.Errorf("empty store %s/%s = %q, want %q", c.role, c.slot, got, c.want)
		}
	}
	// 数值默认同样是契约
	if got := nilStore.ParamInt(RoleT2IPrimary, "steps", 0); got != 4 {
		t.Errorf("t2i steps = %d, want 4", got)
	}
	if got := nilStore.Param(RoleT2IPrimary, "cfg", 0); got != 1.0 {
		t.Errorf("t2i cfg = %v, want 1.0", got)
	}
	if got := nilStore.ParamInt(RoleVideoWan, "frames", 0); got != 33 {
		t.Errorf("wan frames = %d, want 33", got)
	}
	if got := nilStore.Param(RoleVideoWan, "cfg", 0); got != 6.0 {
		t.Errorf("wan cfg = %v, want 6.0", got)
	}
}

// TestBundleStore_PartialOverride 只写想改的字段，其余必须继承内置默认。
// 这是「不用抄一整份配置」的契约。
func TestBundleStore_PartialOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	raw := `{"bundles":{"video.h3.fl2va":{"files":{"unet":"my_h3.safetensors"}}}}`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	s := LoadBundleStore(path)
	if err := s.LoadErr(); err != nil {
		t.Fatalf("unexpected load err: %v", err)
	}
	if got := s.File(RoleVideoH3Fl2va, "unet"); got != "my_h3.safetensors" {
		t.Errorf("overridden unet = %q", got)
	}
	if got := s.File(RoleVideoH3Fl2va, "clip"); got != "qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors" {
		t.Errorf("clip should inherit builtin, got %q", got)
	}
	if got := s.ParamInt(RoleVideoH3Fl2va, "frames", 0); got != 124 {
		t.Errorf("params should inherit builtin, got %d", got)
	}
}

// TestBundleStore_ConfiguredVsResolved 区分「文件里配了」与「回落到默认」。
// main.go 用它决定是否覆盖 COMFYUI_CHECKPOINT。
func TestBundleStore_ConfiguredVsResolved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	if err := os.WriteFile(path, []byte(`{"bundles":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := LoadBundleStore(path)
	if got := s.ConfiguredFile(RoleT2IPrimary, "ckpt"); got != "" {
		t.Errorf("ConfiguredFile should be empty, got %q", got)
	}
	if got := s.File(RoleT2IPrimary, "ckpt"); got != DefaultCheckpoint {
		t.Errorf("File should fall back to builtin, got %q", got)
	}
}

// TestLoadBundleStore_MissingFileWritesDefaults 首次运行必须落一份可编辑的文件，
// 否则用户永远不知道模型名在哪里改。
func TestLoadBundleStore_MissingFileWritesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "models.json")
	s := LoadBundleStore(path)
	if err := s.LoadErr(); err != nil {
		t.Fatalf("missing file must not be an error: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected defaults written to %s: %v", path, err)
	}
	var parsed bundleFile
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("written defaults must be valid JSON: %v", err)
	}
	if len(parsed.Bundles) < 8 {
		t.Errorf("expected at least 8 roles, got %d", len(parsed.Bundles))
	}
	if parsed.Bundles[RoleVideoH3Fl2va].Files["unet"] == "" {
		t.Error("written defaults missing H3 unet")
	}
}

// TestLoadBundleStore_BrokenJSONDoesNotBlock 坏配置不能让引擎起不来：
// 记 loadErr 并继续用内置默认。
func TestLoadBundleStore_BrokenJSONDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := LoadBundleStore(path)
	if s.LoadErr() == nil {
		t.Fatal("expected load error to be recorded")
	}
	if got := s.File(RoleT2IPrimary, "ckpt"); got != DefaultCheckpoint {
		t.Errorf("should fall back to builtin, got %q", got)
	}
}

// TestBuildMediaGraph_UsesBundle 端到端证明「改 JSON 就能换模型」：
// 权重名真的进了提交给 ComfyUI 的图。
func TestBuildMediaGraph_UsesBundle(t *testing.T) {
	store := LoadBundleStore("")
	store.Merge(RoleVideoH3Fl2va, map[string]string{
		"unet":      "custom_h3_unet.safetensors",
		"clip":      "custom_h3_clip.safetensors",
		"video_vae": "custom_v.safetensors",
		"audio_vae": "custom_a.safetensors",
	}, map[string]float64{"width": 512, "frames": 107})

	svc := &Text2ImageService{BaseURL: "http://127.0.0.1:8188", H3BaseURL: "http://127.0.0.1:8189", Bundles: store}
	// 107 = 17*6+5，合法网格
	graph, err := svc.BuildMediaGraph(MediaRequest{Kind: "h3", Prompt: "a cat"})
	if err != nil {
		t.Fatalf("build graph: %v", err)
	}
	unet := graph["1"].(map[string]interface{})["inputs"].(map[string]interface{})["unet_name"]
	if unet != "custom_h3_unet.safetensors" {
		t.Errorf("unet_name = %v, want custom", unet)
	}
	clip := graph["2"].(map[string]interface{})["inputs"].(map[string]interface{})["clip_name"]
	if clip != "custom_h3_clip.safetensors" {
		t.Errorf("clip_name = %v, want custom", clip)
	}
	vae := graph["3"].(map[string]interface{})["inputs"].(map[string]interface{})["vae_name"]
	if vae != "custom_v.safetensors" {
		t.Errorf("video vae = %v, want custom", vae)
	}
	// 宽度与帧数也应来自 bundle
	w := graph["5"].(map[string]interface{})["inputs"].(map[string]interface{})["width"]
	if w != 512 {
		t.Errorf("width = %v, want 512 from bundle", w)
	}
	frames := graph["5"].(map[string]interface{})["inputs"].(map[string]interface{})["length"]
	if frames != 107 {
		t.Errorf("frames = %v, want 107 from bundle", frames)
	}
}

// TestBuildMediaGraph_Ref2vaUsesOwnCheckpoint ref2va 与 fl2va 必须用各自的 unet，
// 这是「念稿链路」的核心前提，混用会让参考音频被当成风格提示重新生成。
func TestBuildMediaGraph_Ref2vaUsesOwnCheckpoint(t *testing.T) {
	store := LoadBundleStore("")
	store.Merge(RoleVideoH3Ref2va, map[string]string{"unet": "ref_unet.safetensors"}, nil)
	store.Merge(RoleVideoH3Fl2va, map[string]string{"unet": "fl_unet.safetensors"}, nil)

	svc := &Text2ImageService{BaseURL: "http://127.0.0.1:8188", H3BaseURL: "http://127.0.0.1:8189", Bundles: store}
	// 用 h3 非 r2v 路径即可（r2v 需要真实参考图文件上传，单测里不碰 IO）
	g, err := svc.BuildMediaGraph(MediaRequest{Kind: "h3", Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if got := g["1"].(map[string]interface{})["inputs"].(map[string]interface{})["unet_name"]; got != "fl_unet.safetensors" {
		t.Errorf("fl2va unet = %v", got)
	}
}

// TestBundleStore_SaveRoundTrip 保存后重新加载必须一致
func TestBundleStore_SaveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	s := LoadBundleStore(path)
	s.Merge(RoleUpscale, map[string]string{"model": "4x-UltraSharp.pth"}, nil)
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	again := LoadBundleStore(path)
	if got := again.File(RoleUpscale, "model"); got != "4x-UltraSharp.pth" {
		t.Errorf("round trip lost override: %q", got)
	}
	// 未覆盖的角色仍然继承内置默认
	if got := again.File(RoleVideoWan, "unet"); got != "wan2.1_t2v_1.3B_fp16.safetensors" {
		t.Errorf("inherited value lost: %q", got)
	}
}

// TestBundleStore_Roles 角色清单必须包含全部内置角色
func TestBundleStore_Roles(t *testing.T) {
	roles := LoadBundleStore("").Roles()
	want := map[string]bool{
		RoleT2IPrimary: true, RoleT2IBatch: true, RoleT2IMultiPrompt: true,
		RoleImg2Img: true, RoleInpaint: true, RoleUpscale: true,
		RoleVideoWan: true, RoleVideoH3Fl2va: true, RoleVideoH3Ref2va: true,
	}
	got := map[string]bool{}
	for _, r := range roles {
		got[r] = true
	}
	for r := range want {
		if !got[r] {
			t.Errorf("missing role %s", r)
		}
	}
}
