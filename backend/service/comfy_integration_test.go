package service

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// ComfyUI 集成验证（不碰真 GPU）
//
// 为什么用 mock 而不是真跑：真跑一张图要占 17GB 显存和 3 分钟，而这里要验证的是
// 「引擎侧的决策是否正确」——降了几档、换了哪个权重、请求发到了哪个实例、
// 互斥组有没有真的串行。这些全部可以在 HTTP 层观测，不需要模型参与。
//
// 局限（必须承认）：mock 只能证明「我们发出的请求符合预期」，
// 不能证明「ComfyUI 会接受它」。后者需要真实实例，见 CHANGELOG「未验证」节。

type mockComfyServer struct {
	srv *httptest.Server

	mu       sync.Mutex
	prompts  []map[string]interface{} // 每次 /prompt 收到的图
	oomLeft  int                      // 前 N 次 /prompt 返回 OOM
	hits     []string                 // 收到的路径（带纳秒时间戳）
	inFlight int
	maxInFl  int
	delay    time.Duration // 每次 /prompt 的人为耗时（用于互斥检测）
}

func newMockComfyServer(t *testing.T) *mockComfyServer {
	t.Helper()
	m := &mockComfyServer{}
	mux := http.NewServeMux()

	mux.HandleFunc("/object_info", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"UNETLoader": {"input":{"required":{"unet_name":[["wan2.1_t2v_1.3B_fp16.safetensors","minimax_h3_fl2va_pruned_int8_convrot.safetensors","low_precision_h3.safetensors"]]}}},
			"CLIPLoader": {"input":{"required":{"clip_name":[["umt5_xxl_fp8_e4m3fn_scaled.safetensors","qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors"]]}}},
			"VAELoader": {"input":{"required":{"vae_name":[["wan_2.1_vae.safetensors","minimax_h3_video_vae_fp16.safetensors","minimax_h3_audio_vae_fp32.safetensors"]]}}},
			"KSampler": {"input":{"required":{"seed":["INT",{"default":0}],"steps":["INT",{"default":20}]}}},
			"CheckpointLoaderSimple": {"input":{"required":{"ckpt_name":[["flux1-schnell-fp8.safetensors"]]}}},
			"EmptyLatentImage": {"input":{"required":{"width":["INT",{}],"height":["INT",{}],"batch_size":["INT",{}]}}},
			"CLIPTextEncode": {"input":{"required":{"text":["STRING",{}],"clip":["CLIP",{}]}}},
			"VAEDecode": {"input":{"required":{"samples":["LATENT",{}],"vae":["VAE",{}]}}},
			"SaveImage": {"input":{"required":{"images":["IMAGE",{}],"filename_prefix":["STRING",{}]}}},
			"ModelSamplingSD3": {"input":{"required":{"model":["MODEL",{}],"shift":["FLOAT",{}]}}},
			"EmptyHunyuanLatentVideo": {"input":{"required":{"width":["INT",{}],"height":["INT",{}],"length":["INT",{}],"batch_size":["INT",{}]}}},
			"CreateVideo": {"input":{"required":{"images":["IMAGE",{}],"fps":["FLOAT",{}]}}},
			"SaveVideo": {"input":{"required":{"video":["VIDEO",{}],"filename_prefix":["STRING",{}],"format":["STRING",{}],"codec":["STRING",{}]}}},
			"MiniMaxH3ImageToVideo": {"input":{"required":{"clip":["CLIP",{}],"vae":["VAE",{}],"prompt":["STRING",{}],"width":["INT",{}],"height":["INT",{}],"length":["INT",{}]}}},
			"MiniMaxH3ReferenceToVideo": {"input":{"required":{"clip":["CLIP",{}],"vae":["VAE",{}],"audio_vae":["VAE",{}],"prompt":["STRING",{}]}}},
			"MiniMaxH3SigmaShift": {"input":{"required":{"model":["MODEL",{}],"shift_video":["FLOAT",{}],"shift_audio":["FLOAT",{}]}}},
			"MiniMaxH3AddGuide": {"input":{"required":{"positive":["COND",{}]}}},
			"MiniMaxLowVRAMAttention": {"input":{"required":{"model":["MODEL",{}],"head_chunks":["INT",{}]}}},
			"MiniMaxChunkFeedForward": {"input":{"required":{"model":["MODEL",{}],"chunks":["INT",{}]}}},
			"LoraLoaderModelOnly": {"input":{"required":{"model":["MODEL",{}],"lora_name":[["h3_turbo.safetensors"]]}}},
			"RandomNoise": {"input":{"required":{"noise_seed":["INT",{}]}}},
			"BasicGuider": {"input":{"required":{"model":["MODEL",{}],"conditioning":["COND",{}]}}},
			"KSamplerSelect": {"input":{"required":{"sampler_name":["STRING",{}]}}},
			"BasicScheduler": {"input":{"required":{"model":["MODEL",{}],"scheduler":["STRING",{}],"steps":["INT",{}],"denoise":["FLOAT",{}]}}},
			"SamplerCustomAdvanced": {"input":{"required":{"noise":["NOISE",{}],"guider":["GUIDER",{}],"sampler":["SAMPLER",{}],"sigmas":["SIGMAS",{}],"latent_image":["LATENT",{}]}}},
			"VAEDecodeAudio": {"input":{"required":{"samples":["LATENT",{}],"vae":["VAE",{}]}}}
		}`))
	})

	// 关键：返回 20GB 空闲让 releaseDiffusionAndWait 立即判定「本来就没驻留」，
	// 否则每次降级要空等 12 秒（真机行为，测试里没必要）。
	mux.HandleFunc("/system_stats", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"devices":[{"vram_free":20971520000,"vram_total":25757220864}]}`))
	})
	mux.HandleFunc("/free", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("/interrupt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})

	mux.HandleFunc("/prompt", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Prompt map[string]interface{} `json:"prompt"`
		}
		_ = json.Unmarshal(raw, &body)

		m.mu.Lock()
		m.hits = append(m.hits, time.Now().Format(time.RFC3339Nano)+" /prompt")
		m.prompts = append(m.prompts, body.Prompt)
		oom := m.oomLeft > 0
		if oom {
			m.oomLeft--
		}
		m.inFlight++
		if m.inFlight > m.maxInFl {
			m.maxInFl = m.inFlight
		}
		delay := m.delay
		m.mu.Unlock()

		if delay > 0 {
			time.Sleep(delay)
		}
		m.mu.Lock()
		m.inFlight--
		m.mu.Unlock()

		if oom {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"type":"prompt_outputs_failed_validation"},"message":"CUDA out of memory. Tried to allocate 2.00 GiB"}`))
			return
		}
		_, _ = w.Write([]byte(`{"prompt_id":"p1","number":1}`))
	})

	mux.HandleFunc("/history/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"p1":{"status":{"status_str":"success","completed":true},
			"outputs":{"9":{"images":[{"filename":"mock_out.png","subfolder":"","type":"output"}]}}}}`))
	})

	mux.HandleFunc("/view", func(w http.ResponseWriter, r *http.Request) {
		// collectOutputsAt 会丢弃小于 1KB 的产物（防 0 字节假文件）
		buf := make([]byte, 2048)
		for i := range buf {
			buf[i] = 0x89
		}
		_, _ = w.Write(buf)
	})

	mux.HandleFunc("/upload/image", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"uploaded.png","subfolder":"","type":"input"}`))
	})

	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockComfyServer) promptCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.prompts)
}

func (m *mockComfyServer) promptAt(i int) map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 0 || i >= len(m.prompts) {
		return nil
	}
	return m.prompts[i]
}

// nodeInputs 取图里某个节点 id 的 inputs
func nodeInputs(graph map[string]interface{}, id string) map[string]interface{} {
	n, ok := graph[id].(map[string]interface{})
	if !ok {
		return nil
	}
	in, _ := n["inputs"].(map[string]interface{})
	return in
}

func newTestT2I(baseURL, outputDir string) *Text2ImageService {
	return &Text2ImageService{
		BaseURL:   baseURL,
		H3BaseURL: baseURL,
		OutputDir: outputDir,
		Bundles:   LoadBundleStore(""),
	}
}

// TestIntegration_WanChain 正常链路：构图 → 提交 → 轮询 → 下载 → 落盘
func TestIntegration_WanChain(t *testing.T) {
	m := newMockComfyServer(t)
	out := t.TempDir()
	svc := newTestT2I(m.srv.URL, out)

	path, kind, err := svc.GenerateMedia(MediaRequest{Kind: "t2v", Prompt: "a cat", Width: 832, Height: 480, Frames: 33})
	if err != nil {
		t.Fatalf("GenerateMedia: %v", err)
	}
	if kind != "video" && kind != "image" {
		t.Errorf("unexpected kind %q", kind)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("output file missing: %v", statErr)
	}
	if m.promptCount() != 1 {
		t.Fatalf("expected 1 prompt submission, got %d", m.promptCount())
	}
	g := m.promptAt(0)
	// 权重来自 bundle，不是代码常量
	if got := nodeInputs(g, "1")["unet_name"]; got != "wan2.1_t2v_1.3B_fp16.safetensors" {
		t.Errorf("unet_name = %v", got)
	}
	if got := nodeInputs(g, "6")["width"]; got != float64(832) {
		t.Errorf("width = %v (%T), want 832", got, got)
	}
	// 保存节点必须在，否则 ComfyUI 空跑
	if nodeInputs(g, "11") == nil {
		t.Error("missing SaveVideo node")
	}
}

// TestIntegration_OOMDegradesResolution OOM 后必须真的降档重试，且只降分辨率
// （h3 没有配精度阶梯时，第一档就该动分辨率）
func TestIntegration_OOMDegradesResolution(t *testing.T) {
	m := newMockComfyServer(t)
	m.oomLeft = 1
	out := t.TempDir()
	svc := newTestT2I(m.srv.URL, out)

	e := &EngineV2{t2i: svc}
	path, _, level, note, err := e.generateMediaWithDegrade(MediaRequest{
		Kind: "h3", Prompt: "x", Width: 608, Height: 352, Frames: 124, Steps: 20,
	})
	if err != nil {
		t.Fatalf("degrade path failed: %v", err)
	}
	if level != 1 {
		t.Errorf("degrade level = %d, want 1", level)
	}
	if note == "" || !containsSub(note, "分辨率") {
		t.Errorf("note must disclose the degradation: %q", note)
	}
	if path == "" {
		t.Error("expected an output path")
	}
	if m.promptCount() != 2 {
		t.Fatalf("expected 2 submissions (fail + retry), got %d", m.promptCount())
	}
	// 第一次原始分辨率，第二次降 0.75 并对齐 8
	first := nodeInputs(m.promptAt(0), "5")
	second := nodeInputs(m.promptAt(1), "5")
	if first["width"] != float64(608) {
		t.Errorf("first attempt width = %v, want 608", first["width"])
	}
	want := align8(int(608 * 0.75)) // 456
	if second["width"] != float64(want) {
		t.Errorf("second attempt width = %v, want %d", second["width"], want)
	}
	// 帧数不该在这一档被动
	if first["length"] != second["length"] {
		t.Errorf("frames changed on the resolution step: %v -> %v", first["length"], second["length"])
	}
}

// TestIntegration_PrecisionLadderTakesPriority 配了精度阶梯时先换权重、不动分辨率
func TestIntegration_PrecisionLadderTakesPriority(t *testing.T) {
	m := newMockComfyServer(t)
	m.oomLeft = 1
	out := t.TempDir()
	svc := newTestT2I(m.srv.URL, out)
	// 声明 H3 的两档精度：fp8 → 更低精度
	svc.Bundles.Merge(RoleVideoH3Fl2va, map[string]string{
		"unet": "minimax_h3_fl2va_pruned_int8_convrot.safetensors",
	}, nil)
	svc.Bundles.mu.Lock()
	svc.Bundles.file[RoleVideoH3Fl2va].Ladder = []string{
		"minimax_h3_fl2va_pruned_int8_convrot.safetensors",
		"low_precision_h3.safetensors",
	}
	svc.Bundles.mu.Unlock()

	e := &EngineV2{t2i: svc}
	_, _, level, note, err := e.generateMediaWithDegrade(MediaRequest{
		Kind: "h3", Prompt: "x", Width: 608, Height: 352, Frames: 124, Steps: 20,
	})
	if err != nil {
		t.Fatalf("degrade path failed: %v", err)
	}
	if level != 1 {
		t.Errorf("level = %d", level)
	}
	if !containsSub(note, "精度档") {
		t.Errorf("note must mention precision: %q", note)
	}
	if m.promptCount() != 2 {
		t.Fatalf("expected 2 submissions, got %d", m.promptCount())
	}
	u1 := nodeInputs(m.promptAt(0), "1")["unet_name"]
	u2 := nodeInputs(m.promptAt(1), "1")["unet_name"]
	if u1 != "minimax_h3_fl2va_pruned_int8_convrot.safetensors" {
		t.Errorf("first unet = %v", u1)
	}
	if u2 != "low_precision_h3.safetensors" {
		t.Errorf("second unet = %v, want the lower rung", u2)
	}
	// 构图不能变
	if nodeInputs(m.promptAt(0), "5")["width"] != nodeInputs(m.promptAt(1), "5")["width"] {
		t.Error("precision step must not change resolution")
	}
}

// TestIntegration_ExhaustedDegradeReportsOriginal 降到底还 OOM 必须如实报原始错误，
// 不能伪装成成功，也不能无限重试。
func TestIntegration_ExhaustedDegradeReportsOriginal(t *testing.T) {
	m := newMockComfyServer(t)
	m.oomLeft = 99 // 一直 OOM
	out := t.TempDir()
	svc := newTestT2I(m.srv.URL, out)
	e := &EngineV2{t2i: svc}

	_, _, level, _, err := e.generateMediaWithDegrade(MediaRequest{
		Kind: "h3", Prompt: "x", Width: 608, Height: 352, Frames: 124, Steps: 20,
	})
	if err == nil {
		t.Fatal("must fail after exhausting degradation")
	}
	if !IsOOMError(err) {
		t.Errorf("original OOM error must surface: %v", err)
	}
	if level < 1 || level > maxOOMDegrade {
		t.Errorf("level = %d, want 1..%d", level, maxOOMDegrade)
	}
	// 尝试次数 = 1 初始 + level 次重试
	if m.promptCount() != level+1 {
		t.Errorf("submissions = %d, want %d", m.promptCount(), level+1)
	}
}

// TestIntegration_MultiInstanceRouting 按权重名路由到不同实例
func TestIntegration_MultiInstanceRouting(t *testing.T) {
	mainComfy := newMockComfyServer(t)
	h3Comfy := newMockComfyServer(t)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "instances.json")
	cfg := `{"default":"main","instances":[
		{"name":"main","base_url":"` + mainComfy.srv.URL + `","models":["flux*","wan*"],"enabled":true},
		{"name":"h3","base_url":"` + h3Comfy.srv.URL + `","models":["minimax_h3*"],"enabled":true}
	]}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := newTestT2I(mainComfy.srv.URL, t.TempDir())
	svc.H3BaseURL = "http://127.0.0.1:1" // 故意给错的：路由必须按配置走，不能靠这个字段
	svc.Instances = LoadInstanceRegistry(cfgPath, "")

	if _, _, err := svc.GenerateMedia(MediaRequest{Kind: "t2v", Prompt: "x"}); err != nil {
		t.Fatalf("wan path: %v", err)
	}
	if _, _, err := svc.GenerateMedia(MediaRequest{Kind: "h3", Prompt: "y", Frames: 124}); err != nil {
		t.Fatalf("h3 path: %v", err)
	}
	if mainComfy.promptCount() != 1 {
		t.Errorf("main instance got %d prompts, want 1", mainComfy.promptCount())
	}
	if h3Comfy.promptCount() != 1 {
		t.Errorf("h3 instance got %d prompts, want 1", h3Comfy.promptCount())
	}
	// 路由错了会打到 127.0.0.1:1 并超时/拒绝，所以上面的成功本身就是证据
}

// TestIntegration_ExclusiveGroupSerializesCalls 同一互斥组的两个实例不能同时跑
func TestIntegration_ExclusiveGroupSerializesCalls(t *testing.T) {
	a := newMockComfyServer(t)
	b := newMockComfyServer(t)
	a.delay = 200 * time.Millisecond
	b.delay = 200 * time.Millisecond

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "instances.json")
	cfg := `{"default":"a","instances":[
		{"name":"a","base_url":"` + a.srv.URL + `","models":["flux*"],"exclusive_group":"gpu","enabled":true},
		{"name":"b","base_url":"` + b.srv.URL + `","models":["minimax_h3*"],"exclusive_group":"gpu","enabled":true}
	]}`
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := LoadInstanceRegistry(cfgPath, "")

	// 直接测锁：a 持有期间 b 必须等
	start := time.Now()
	unlockA := reg.LockForBase(a.srv.URL)
	var bEnter time.Time
	var mu sync.Mutex
	done := make(chan struct{})
	go func() {
		u := reg.LockForBase(b.srv.URL)
		mu.Lock()
		bEnter = time.Now()
		mu.Unlock()
		u()
		close(done)
	}()
	time.Sleep(150 * time.Millisecond)
	unlockA()
	<-done

	mu.Lock()
	waited := bEnter.Sub(start)
	mu.Unlock()
	if waited < 140*time.Millisecond {
		t.Errorf("second instance entered after %v — it should wait for the group lock", waited)
	}
}
