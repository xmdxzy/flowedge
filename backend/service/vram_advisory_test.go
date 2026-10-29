package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"flowedge/backend/model"
)

// 三档的边界必须钉死：这不是审美问题，判错一档就是「以为没问题然后白等半小时」。
func TestJudgeVRAM_Boundaries(t *testing.T) {
	const need, reserve = 20480, 4096
	cases := []struct {
		name  string
		free  int
		level string
	}{
		{"刚好够装下还要预留", need + reserve, VRAMLevelOK},
		{"远大于需求", 24576, VRAMLevelOK},
		{"装得下但少 1MB 预留", need + reserve - 1, VRAMLevelReserve},
		{"刚好只够装下", need, VRAMLevelReserve},
		{"少 1MB 装不下", need - 1, VRAMLevelTooSmall},
		{"只剩桌面那点", 752, VRAMLevelTooSmall},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := JudgeVRAM(c.free, need, reserve)
			if got.Level != c.level {
				t.Fatalf("free=%d：档位 = %s，want %s（msg=%s）", c.free, got.Level, c.level, got.Message)
			}
			if got.Message == "" {
				t.Fatal("任何档位都必须有可读说明，不能只给一个枚举值")
			}
		})
	}
}

// 不吃扩散显存的图（例如纯 tts）不该报警。
func TestJudgeVRAM_NoDiffusionIsOK(t *testing.T) {
	got := JudgeVRAM(100, 0, vramAdvisoryReserveMB)
	if got.Level != VRAMLevelOK {
		t.Fatalf("不吃显存的流水线应为 ok，实际 %s", got.Level)
	}
	if s := ShortVRAMAdvisory(got); s != "" {
		t.Fatalf("ok 档不该写日志，实际 %q", s)
	}
}

// reserve 档必须把「差多少」算给用户看，而不是只丢一句"偏紧"。
// 依据是 ComfyUI 源码：全量加载要求「模型 + extra_reserved_memory」同时空闲。
func TestJudgeVRAM_ReserveExplainsGap(t *testing.T) {
	const need, reserve = 20480, 4096
	free := 21000 // 装得下模型，但保不住预留
	got := JudgeVRAM(free, need, reserve)
	if got.Level != VRAMLevelReserve {
		t.Fatalf("该档位应为 reserve，实际 %s", got.Level)
	}
	gap := need + reserve - free // 3576
	if gap != 3576 {
		t.Fatalf("测试假设有误：gap 应为 3576，实际 %d", gap)
	}
	for _, want := range []string{"差 3576MB", "部分加载", "model_management.py"} {
		if !strings.Contains(got.Message, want) {
			t.Fatalf("说明里应有 %q，实际：%s", want, got.Message)
		}
	}
}

// 「装不下」档同样不能断言 OOM。
//
// 2026-10-26 实测：空闲 19028MB < 模型 20480MB，走的正是这一档，
// 而片子照样 82–112s 出来了。说成"大概率 OOM"就是在吓人，用户会不再信告警。
func TestJudgeVRAM_TooSmallIsNotDoom(t *testing.T) {
	const need, reserve = 20480, 4096
	got := JudgeVRAM(19028, need, reserve)
	if got.Level != VRAMLevelTooSmall {
		t.Fatalf("空闲 < 模型需求时应为 too_small，实际 %s", got.Level)
	}
	if strings.Contains(got.Message, "OOM") {
		t.Fatalf("不该断言 OOM（实测能出片）：%s", got.Message)
	}
	for _, want := range []string{"差 1452MB", "部分加载", "1.3 倍"} {
		if !strings.Contains(got.Message, want) {
			t.Fatalf("说明里应有 %q，实际：%s", want, got.Message)
		}
	}
}

// 需求取图里最重的那个节点：混合图不许按最轻的算。
func TestDiffusionNeedMB_TakesHeaviest(t *testing.T) {
	nodes := []model.NodeInstance{
		{Type: "t2i"},
		{Type: "shotlist"}, // H3 最重
		{Type: "tts"},
	}
	if got := diffusionNeedMB(nodes); got != 20480 {
		t.Fatalf("应取 H3 的 20480MB，实际 %d", got)
	}
	if got := diffusionNeedMB([]model.NodeInstance{{Type: "tts"}}); got != 0 {
		t.Fatalf("纯语音图应吃 0MB，实际 %d", got)
	}
}

// 探哪个端口由节点决定：H3 的图去 8189，出图去 8188。
func TestFreeVRAMForNodes_PicksEndpoint(t *testing.T) {
	mk := func(freeMB int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/system_stats" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(`{"devices":[{"vram_free":` +
				itoa(int64(freeMB)<<20) + `}]}`))
		}))
	}
	mainSrv := mk(1234)
	defer mainSrv.Close()
	h3Srv := mk(1111)
	defer h3Srv.Close()

	if got := FreeVRAMForNodes([]model.NodeInstance{{Type: "t2i"}}, mainSrv.URL, h3Srv.URL); got != 1234 {
		t.Fatalf("出图图应探主实例 1234MB，实际 %d", got)
	}
	if got := FreeVRAMForNodes([]model.NodeInstance{{Type: "text2video"}}, mainSrv.URL, h3Srv.URL); got != 1111 {
		t.Fatalf("视频图应探 H3 实例 1111MB，实际 %d", got)
	}
	// 服务没起：返回 0，让调用方跳过前瞻（依赖预检已经会报「没起」）
	if got := FreeVRAMForNodes([]model.NodeInstance{{Type: "t2i"}}, "http://127.0.0.1:1", ""); got != 0 {
		t.Fatalf("探不到应返回 0，实际 %d", got)
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
