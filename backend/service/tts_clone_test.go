package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// 克隆参数必须原样透传给 TTS 服务。
// 反面案例：2026-10-23 之前 TTSOptions 只有 Voice/Rate/Speed，节点就算读了
// ref_audio 也发不出去，用户会以为「填了参考音频」但其实用的是内置音色。
func TestTTSRefAudioPassthrough(t *testing.T) {
	var got map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tts" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("bad body: %v", err)
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Write(make([]byte, 4096)) // >1000 字节，避免被「音频太小」判失败
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "a.wav")
	backend, err := NewTTSService(srv.URL).SynthesizeWith("测试一句台词", out, TTSOptions{
		Voice: "should-be-ignored", RefAudio: "C:/refs/narrator.wav", RefText: "参考录音的逐字稿",
	})
	if err != nil {
		t.Fatalf("合成失败: %v", err)
	}
	if backend != "service" {
		t.Fatalf("backend = %q，期望 service", backend)
	}
	if got["ref_audio"] != "C:/refs/narrator.wav" {
		t.Errorf("ref_audio 未透传，body=%v", got)
	}
	if got["ref_text"] != "参考录音的逐字稿" {
		t.Errorf("ref_text 未透传，body=%v", got)
	}
	if got["text"] != "测试一句台词" {
		t.Errorf("text 被改动，body=%v", got)
	}
}

// 克隆请求失败时绝不能回退 SAPI：换了后端等于换了个人在说话，
// 节点却会报「成功」，用户拿到的是完全无关的音色。
func TestTTSCloneNeverFallsBackToSAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "a.wav")
	backend, err := NewTTSService(srv.URL).SynthesizeWith("测试", out, TTSOptions{
		RefAudio: "C:/refs/narrator.wav", RefText: "逐字稿",
	})
	if err == nil {
		t.Fatalf("克隆请求失败必须报错，实际返回 backend=%q", backend)
	}
	if backend != "" {
		t.Errorf("失败时不应有 backend 标记，得到 %q", backend)
	}
}

// 不带参考音频时，服务失败仍可回退（这是既有契约，别被上面的改动连带删掉）。
func TestTTSNoRefStillAllowsFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	out := filepath.Join(t.TempDir(), "a.wav")
	backend, err := NewTTSService(srv.URL).SynthesizeWith("测试", out, TTSOptions{})
	if !SAPIAvailable() {
		t.Skip("本机没有 Windows SAPI，跳过回退路径")
	}
	if err != nil {
		t.Fatalf("SAPI 可用时不该失败: %v", err)
	}
	if backend != "sapi-fallback" {
		t.Errorf("backend = %q，期望 sapi-fallback", backend)
	}
}
