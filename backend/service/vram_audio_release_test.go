package service

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"flowedge/backend/model"
)

// 2026-10-27 实测背景：Breeze TTS 2（3.47B BF16）驻留 15.2GB、冷加载 180s。
// 这三个缺口合在一起曾让「配音跑过之后出片」和「卸载后第一次配音」双双失败：
//
//	① enterSide(sideDiffusion) 只管 Ollama，语音侧没人让位 → H3 只剩 3–4GB
//	② tts 节点吃 defaultNodeTimeout=180s，与冷加载打平 → node timeout after 3m0s
//	③ HTTP 预算 60s 起算，短文本 150s → 连加载都没走完就断了
//
// 下面逐条钉住。

// TestReleaseAudioSideUnloadsViaHTTP 进扩散侧时，语音侧必须主动 /unload。
func TestReleaseAudioSideUnloadsViaHTTP(t *testing.T) {
	orig := ttsOnCPU
	defer func() { ttsOnCPU = orig }()
	ttsOnCPU = func() bool { return false }

	var called int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/unload" {
			atomic.AddInt32(&called, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := NewEngineV2(nil, nil, nil, NewTTSService(srv.URL), nil, nil, nil)
	e.releaseAudioSide("进扩散侧")

	if atomic.LoadInt32(&called) == 0 {
		t.Fatal("releaseAudioSide 没有向 TTS 发 POST /unload：扩散侧仍会被那 15GB 卡住")
	}
}

// TestReleaseAudioSideSkipsCPUTTS CPU 后端零显存占用，不该为它承担 180s 冷加载。
func TestReleaseAudioSideSkipsCPUTTS(t *testing.T) {
	orig := ttsOnCPU
	defer func() { ttsOnCPU = orig }()
	ttsOnCPU = func() bool { return true }

	var called int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&called, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	e := NewEngineV2(nil, nil, nil, NewTTSService(srv.URL), nil, nil, nil)
	e.releaseAudioSide("进扩散侧")

	if atomic.LoadInt32(&called) != 0 {
		t.Fatal("CPU 上的 TTS 被无谓地卸载了：下次合成要白等一次冷加载")
	}
}

// TestTTSTimeoutCoversColdLoad 节点超时必须大于冷加载实测值。
//
// 旧实现没给 Timeout，吃 defaultNodeTimeout=180s —— 与实测冷加载 180s 打平，
// 「服务被卸载后第一次请求」必然超时（run #288：node timeout after 3m0s）。
func TestTTSTimeoutCoversColdLoad(t *testing.T) {
	const coldLoadObserved = 180 * time.Second // 2026-10-27 逐 10s 采样实测

	n := &model.NodeInstance{Type: "tts", Data: map[string]interface{}{"text": "一句短台词"}}
	got := nodeTimeoutFor(n)
	if got <= coldLoadObserved {
		t.Fatalf("tts 超时 = %s，没有超过冷加载实测 %s：卸载后再用仍会超时", got, coldLoadObserved)
	}

	// 长台词要有更多预算（自回归，长文本容易跑飞）。
	long := &model.NodeInstance{Type: "tts", Data: map[string]interface{}{
		"text": string(make([]rune, 120)),
	}}
	if nodeTimeoutFor(long) <= got {
		t.Fatal("长台词的超时没有比短台词更长")
	}
}

// TestTTSHTTPBudgetCoversColdLoad HTTP 层预算同样必须覆盖冷加载。
func TestTTSHTTPBudgetCoversColdLoad(t *testing.T) {
	const coldLoadObserved = 180 * time.Second
	if ttsFirstCallBudget <= coldLoadObserved {
		t.Fatalf("ttsFirstCallBudget = %s，不足以覆盖冷加载 %s", ttsFirstCallBudget, coldLoadObserved)
	}
}
