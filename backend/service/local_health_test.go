package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"flowedge/backend/model"
)

// 本地语音服务健康探测验收。
//
// 三种状态必须分开，混成一种就会给出错误的修复动作：
//   ① 就绪            → OK=true
//   ② 进程在跑但内部坏 → OK=false + Degraded=true（一键启动没用，要装包）
//   ③ 根本没起         → OK=false + Degraded=false（该去启动服务）
//
// 历史故障：TTS /health 返回 200、真调 /tts 才 500（缺 ordered_set），
// 一次完整运行白等一百多秒。深度自检就是为了把这句话提前到入口。

func TestProbeLocalHealth_DegradedWhenDependenciesMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("deep") == "1" {
			_, _ = w.Write([]byte(`{"status":"degraded","service":"flowedge-tts","missing":["ordered_set"],"hint":"缺 Python 依赖：pip install ordered_set"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	c := probeLocalHealth(srv.URL, "TTS")
	if c.OK {
		t.Fatal("深度自检报缺依赖时不应标记就绪")
	}
	if !c.Degraded {
		t.Fatal("应标记 Degraded（服务在跑但不可用），否则前端会给一个没用的「一键启动」")
	}
	if !strings.Contains(c.Hint, "pip install") {
		t.Fatalf("修复提示应给出装包命令，实际：%q", c.Hint)
	}
	if !strings.Contains(c.Detail, "ordered_set") {
		t.Fatalf("详情应点名缺失的包，实际：%q", c.Detail)
	}
}

func TestProbeLocalHealth_FallsBackForOldService(t *testing.T) {
	// 旧版脚本用 self.path == "/health" 精确匹配，带 query 会 404。
	// 必须回落，不能把「旧版服务」误判成「不可用」。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path != "/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","service":"flowedge-asr"}`))
	}))
	defer srv.Close()

	c := probeLocalHealth(srv.URL, "ASR")
	if !c.OK {
		t.Fatalf("旧版服务应回落普通 /health 并判为就绪，实际 ok=%v detail=%q", c.OK, c.Detail)
	}
	if c.Degraded {
		t.Fatal("旧版服务不应被标记 Degraded")
	}
}

func TestProbeLocalHealth_NotRunning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	url := srv.URL
	srv.Close() // 关掉：模拟端口不通

	c := probeLocalHealth(url, "TTS")
	if c.OK || c.Degraded {
		t.Fatalf("服务没起时既不该就绪也不该 Degraded，实际 ok=%v degraded=%v", c.OK, c.Degraded)
	}
}

// TestPreflight_DegradedNotFixable 服务跑着但坏了，不能再给「一键修复」——
// 那个按钮只会重启一个本来就活着的进程，用户点十次也不会好。
func TestPreflight_DegradedNotFixable(t *testing.T) {
	nodes := []model.NodeInstance{{ID: "q", Type: "tts"}}
	caps := Capabilities{
		TTS: Capability{
			OK:       false,
			Degraded: true,
			Detail:   "TTS 服务在跑但不可用（缺 ordered_set）",
			Hint:     "缺 Python 依赖：pip install ordered_set",
		},
	}
	missing := PreflightDeps(nodes, caps, nil)
	if len(missing) != 1 {
		t.Fatalf("应报 1 项缺失，实际 %d", len(missing))
	}
	m := missing[0]
	if m.Fixable {
		t.Fatal("Degraded 的依赖不该标记为可一键修复（重启活着的进程解决不了缺包）")
	}
	if m.FixDep != "" {
		t.Fatalf("不该给出要启动的依赖名，实际 %q", m.FixDep)
	}
	if !strings.Contains(m.Fix, "pip install") {
		t.Fatalf("修复动作应落到装包命令，实际：%q", m.Fix)
	}
}

// TestPreflight_NotRunningIsFixable 服务没起时才该给一键启发。
func TestPreflight_NotRunningIsFixable(t *testing.T) {
	nodes := []model.NodeInstance{{ID: "q", Type: "tts"}}
	caps := Capabilities{TTS: Capability{OK: false, Hint: "未配置"}}
	missing := PreflightDeps(nodes, caps, nil)
	if len(missing) != 1 {
		t.Fatalf("应报 1 项缺失，实际 %d", len(missing))
	}
	if !missing[0].Fixable || missing[0].FixDep != "tts" {
		t.Fatalf("没起的服务应可一键拉起，实际 fixable=%v fix_dep=%q",
			missing[0].Fixable, missing[0].FixDep)
	}
}
