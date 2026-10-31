package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flowedge/backend/model"
)

// gates.vlm 必须能从 film gates 里读出来，并且默认是关的。
// 默认关是刻意的：一次 VLM 调用 10–30 秒，不能悄悄改掉所有人的出片耗时。
func TestFilmFinalRules_VLMGate(t *testing.T) {
	node := &model.NodeInstance{ID: "sl-1", Type: "shotlist", Data: map[string]interface{}{}}

	// 默认关
	r := filmFinalRules(nil, node, 100)
	if r.vlmEnabled {
		t.Error("未声明时应默认关闭 VLM 判定")
	}

	// 显式开
	gates := map[string]interface{}{
		"vlm": map[string]interface{}{"enabled": true, "topic": "人物身份稳定"},
	}
	r = filmFinalRules(gates, node, 100)
	if !r.vlmEnabled {
		t.Fatal("gates.vlm.enabled=true 应开启")
	}
	if r.vlmTopic != "人物身份稳定" {
		t.Errorf("vlm_topic 未透传：%q", r.vlmTopic)
	}

	// 显式关
	gates = map[string]interface{}{"vlm": map[string]interface{}{"enabled": false}}
	if filmFinalRules(gates, node, 100).vlmEnabled {
		t.Error("gates.vlm.enabled=false 应关闭")
	}
}

// 节点级 vlm 字段也要能开（画布用户直接在节点上勾）。
func TestParseGateRules_VLMFromNode(t *testing.T) {
	node := &model.NodeInstance{Data: map[string]interface{}{"vlm": true, "vlm_topic": "无明显畸形"}}
	r := parseGateRules(node)
	if !r.vlmEnabled || r.vlmTopic != "无明显畸形" {
		t.Fatalf("节点级 vlm 未解析：%+v", r)
	}
}

// 三种失败必须分得开：判词问题 / 不可达 / 内容问题。
// 混成一句「质量不佳」等于没报——修复动作完全不同。
func TestAppendVLMCheck_DisabledNoop(t *testing.T) {
	e := &EngineV2{}
	base := []gateCheck{{Name: "min_frames", OK: true, Level: "warn"}}
	got := e.appendVLMCheck(context.Background(), "whatever.mp4", gateRules{vlmEnabled: false}, base)
	if len(got) != len(base) {
		t.Fatalf("未开启时不应追加检查，实际 %d 条", len(got))
	}
}

// 没有视觉模型时必须记 warn 且**明确不是通过**，不能静默放行。
func TestAppendVLMCheck_NoVisionModel(t *testing.T) {
	e := &EngineV2{}
	checks := e.appendVLMCheck(context.Background(), "x.mp4", gateRules{vlmEnabled: true}, nil)
	if len(checks) != 1 {
		t.Fatalf("应追加 1 条检查，实际 %d", len(checks))
	}
	c := checks[0]
	if c.OK {
		t.Error("VLM 没做成不能算通过")
	}
	if c.Level != "warn" {
		t.Errorf("环境问题应为 warn，实际 %s", c.Level)
	}
	if !strings.Contains(c.Detail, "不是通过") {
		t.Errorf("必须点明「不是通过」，实际：%q", c.Detail)
	}
	if !strings.Contains(c.Fix, "ollama list") {
		t.Errorf("修复动作要能照做（检查 Ollama/模型），实际：%q", c.Fix)
	}
}

// 有模型、有产物时，走完整链路：抽帧 → VisionChat → 解析 → 门映射。
// 这里用 httptest 假 Ollama，验证「判词能一路变成 gateCheck」，不依赖真机。
func TestAppendVLMCheck_EndToEndWithFakeOllama(t *testing.T) {
	// 假 Ollama：/api/chat 返回带证据与等级的判词
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"models": []map[string]interface{}{{"name": "qwen2.5vl:7b", "size": 5 << 30}},
			})
		case "/api/chat":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"message": map[string]interface{}{"role": "assistant", "content": "1) 证据：\n- [帧1] 蓝发少女站在码头\n- [帧2] 围裙图案清晰\n2) 缺陷：\n- 无\n3) 等级：fair\n4) 修复：重拍第 2 镜"},
			})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	img := filepath.Join(dir, "frame.png")
	// 最小 PNG（1x1）——isStillImagePath 会按扩展名认成图片，跳过抽帧
	if err := os.WriteFile(img, tinyPNG(), 0o644); err != nil {
		t.Fatal(err)
	}

	e := &EngineV2{llm: NewLLMService(srv.URL, "qwen2.5vl:7b"), visionModel: "qwen2.5vl:7b"}
	// 显式声明后端：否则 ResolveLLMBackend 会把 127.0.0.1 判成 OpenAI 兼容，
	// 请求打到 /v1/chat/completions 而不是 /api/chat（本地 Ollama 是后者）。
	e.llm.Backend = BackendOllama
	checks := e.appendVLMCheck(context.Background(), img, gateRules{vlmEnabled: true, vlmTopic: "身份稳定"}, nil)
	if len(checks) != 1 {
		t.Fatalf("应追加 1 条检查，实际 %d：%+v", len(checks), checks)
	}
	c := checks[0]
	if c.OK {
		t.Error("fair 不应算通过（可交付但要看）")
	}
	if c.Level != "warn" {
		t.Errorf("fair 应为 warn，实际 %s", c.Level)
	}
	if !strings.Contains(c.Detail, "fair") {
		t.Errorf("结论里应体现等级，实际：%q", c.Detail)
	}
}

// tinyPNG 返回一张合法的最小 PNG（1x1 透明）。
func tinyPNG() []byte {
	return []byte{
		0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
		0x89, 0x00, 0x00, 0x00, 0x0A, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00,
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE,
		0x42, 0x60, 0x82,
	}
}
