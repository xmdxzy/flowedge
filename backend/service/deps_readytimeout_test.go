package service

import (
	"testing"
	"time"
)

// readyTimeout 里的 tts 分支为什么是 600s：
//
//	Breeze TTS 2 的冷启动要跑 CUDA Graph 预热 —— 编译缓存热时约 117s，
//	冷编译实测约 245s，而启动瞬间显存被别的实例占着时会拖到 300s 以上
//	（图捕获本身要吃满 24GB 卡）。2026-10-26 实测被卡过两次：服务其实最后起来了，
//	但引擎已经记了一条「启动失败」，面板上就是红的。
//
// 原来 tts 和 asr 共用 300s，只剩 55s 余量。这条测试把结论钉住，
// 防止以后有人「顺手统一一下超时」把它改回去。
func TestReadyTimeoutTTSAllowsSlowWarmup(t *testing.T) {
	if got := readyTimeout("tts"); got < 600*time.Second {
		t.Fatalf("tts 冷启动上限被改小了：%s（实测冷编译 245s，需 ≥600s 才留够余量）", got)
	}
}

// 其余依赖的上限不该被这次改动带着漂。
func TestReadyTimeoutOtherDepsUnchanged(t *testing.T) {
	cases := map[string]time.Duration{
		"asr":        300 * time.Second,
		"comfy_h3":   180 * time.Second,
		"comfy_moss": 300 * time.Second,
		"whatever":   120 * time.Second,
	}
	for name, want := range cases {
		if got := readyTimeout(name); got != want {
			t.Errorf("%s 的超时变了：got %s want %s", name, got, want)
		}
	}
}
