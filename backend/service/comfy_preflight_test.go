package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// objectInfoStub 造一个最小可用的 /object_info 响应
const objectInfoStub = `{
  "CheckpointLoaderSimple": {"input": {"required": {"ckpt_name": [["flux1-schnell-fp8.safetensors","sdxl_base.safetensors"]]}}},
  "UNETLoader": {"input": {"required": {"unet_name": [["minimax_h3_fl2va_pruned_int8_convrot.safetensors"]]}}},
  "CLIPLoader": {"input": {"required": {"clip_name": [["qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors"]]}}},
  "VAELoader": {"input": {"required": {"vae_name": [["minimax_h3_video_vae_fp16.safetensors","minimax_h3_audio_vae_fp32.safetensors"]]}}},
  "KSampler": {"input": {"required": {"seed": ["INT", {"default": 0}]}}},
  "SaveImage": {"input": {"required": {"images": ["IMAGE"]}}}
}`

func newObjectInfoServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/object_info" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(objectInfoStub))
	}))
	t.Cleanup(srv.Close)
	InvalidateObjectInfoCache()
	return srv
}

func h3Graph(unet, clip, vae string) map[string]interface{} {
	return map[string]interface{}{
		"1": map[string]interface{}{"class_type": "UNETLoader", "inputs": map[string]interface{}{"unet_name": unet}},
		"2": map[string]interface{}{"class_type": "CLIPLoader", "inputs": map[string]interface{}{"clip_name": clip}},
		"3": map[string]interface{}{"class_type": "VAELoader", "inputs": map[string]interface{}{"vae_name": vae}},
		"9": map[string]interface{}{"class_type": "SaveImage", "inputs": map[string]interface{}{"images": []interface{}{"3", 0}}},
	}
}

// TestPreflightGraph_AcceptsKnownWeights 正常图必须零 issue
func TestPreflightGraph_AcceptsKnownWeights(t *testing.T) {
	srv := newObjectInfoServer(t)
	graph := h3Graph(
		"minimax_h3_fl2va_pruned_int8_convrot.safetensors",
		"qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors",
		"minimax_h3_video_vae_fp16.safetensors",
	)
	issues, err := preflightGraph(srv.URL, graph)
	if err != nil {
		t.Fatalf("unexpected probe error: %v", err)
	}
	if got := blockingIssues(issues); got != nil {
		t.Fatalf("expected pass, got %v", got)
	}
}

// TestPreflightGraph_CatchesWrongWeight 权重名不对必须被拦，且给出怎么改
func TestPreflightGraph_CatchesWrongWeight(t *testing.T) {
	srv := newObjectInfoServer(t)
	graph := h3Graph("typo_unet.safetensors",
		"qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors",
		"minimax_h3_video_vae_fp16.safetensors")
	issues, err := preflightGraph(srv.URL, graph)
	if err != nil {
		t.Fatalf("unexpected probe error: %v", err)
	}
	err = blockingIssues(issues)
	if err == nil {
		t.Fatal("expected blocking error for wrong unet name")
	}
	msg := err.Error()
	if !strings.Contains(msg, "typo_unet.safetensors") {
		t.Errorf("error must name the offending value: %s", msg)
	}
	if !strings.Contains(msg, "data/models.json") {
		t.Errorf("error must say where to fix it: %s", msg)
	}
}

// TestPreflightGraph_CatchesMissingNode 节点没装必须被拦
func TestPreflightGraph_CatchesMissingNode(t *testing.T) {
	srv := newObjectInfoServer(t)
	graph := h3Graph(
		"minimax_h3_fl2va_pruned_int8_convrot.safetensors",
		"qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors",
		"minimax_h3_video_vae_fp16.safetensors",
	)
	graph["77"] = map[string]interface{}{"class_type": "MiniMaxH3ReferenceToVideo", "inputs": map[string]interface{}{}}
	issues, err := preflightGraph(srv.URL, graph)
	if err != nil {
		t.Fatal(err)
	}
	berr := blockingIssues(issues)
	if berr == nil {
		t.Fatal("expected blocking error for missing node class")
	}
	if !strings.Contains(berr.Error(), "MiniMaxH3ReferenceToVideo") {
		t.Errorf("must name the missing class: %s", berr.Error())
	}
}

// TestPreflightGraph_UnreachableIsNotPass 探不到 ≠ 通过。
// 这是 AGENTS §0.6 的核心教训：验收被静默跳过比失败更危险。
func TestPreflightGraph_UnreachableIsNotPass(t *testing.T) {
	InvalidateObjectInfoCache()
	graph := h3Graph("a", "b", "c")
	// 用一个必然连不上的端口
	issues, err := preflightGraph("http://127.0.0.1:1", graph)
	if err == nil {
		t.Fatal("unreachable object_info must return an error, not pretend to pass")
	}
	if len(issues) != 0 {
		t.Errorf("no issues should be invented when probe failed, got %v", issues)
	}
}

// TestValidateGraph_NilInfoIsWarnNotError nil info 只能产生 warn，不能变成 error
// （否则一次网络抖动就会让所有出图失败）
func TestValidateGraph_NilInfoIsWarnNotError(t *testing.T) {
	issues := validateGraph(nil, h3Graph("a", "b", "c"))
	if len(issues) != 1 || issues[0].Severity != "warn" {
		t.Fatalf("expected single warn, got %+v", issues)
	}
	if blockingIssues(issues) != nil {
		t.Error("warn must not block execution")
	}
}

// TestParseEnumCandidates_DynamicEnumIgnored 动态枚举（第一项是对象）不做判断，
// 猜错比不猜更糟。
func TestParseEnumCandidates_DynamicEnumIgnored(t *testing.T) {
	spec := []byte(`[{"content": "dynamic"}, {"default": ""}]`)
	if set := parseEnumCandidates(spec); set != nil {
		t.Errorf("dynamic enum should yield nil set, got %v", set)
	}
	spec2 := []byte(`[["a.safetensors","b.safetensors"], {}]`)
	set := parseEnumCandidates(spec2)
	if len(set) != 2 || !set["a.safetensors"] {
		t.Errorf("expected parsed candidates, got %v", set)
	}
}

// TestPreflightGraph_SkipSwitch 逃生开关：畸形 object_info 环境下也能提交
func TestPreflightGraph_SkipSwitch(t *testing.T) {
	t.Setenv("FLOWEDGE_SKIP_PREFLIGHT", "1")
	issues, err := preflightGraph("http://127.0.0.1:1", h3Graph("a", "b", "c"))
	if err != nil || issues != nil {
		t.Fatalf("skip switch must bypass both probe and validation, got (%v, %v)", issues, err)
	}
}
