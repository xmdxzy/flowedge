package service

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 准入闸只有两种方向会出错，两种都很贵：
//   ① 该拦没拦 → 机器被打死，一次 OOM 就是几十分钟白跑
//   ② 不该拦却拦了 → 出片被一个"没起的服务"卡死，比没有这个功能更糟
// 所以测试必须同时覆盖两侧，尤其 ②（fail-open 的全部路径）。

// 治理器说可以 → 放行，并带回权重/压力级
func TestGovernor_AllowsWhenAdmitted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/flow/governor/preflight" || r.Method != http.MethodPost {
			t.Errorf("路径/方法不对：%s %s", r.Method, r.URL.Path)
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["kind"] != "flowedge" {
			t.Errorf("kind 应透传 flowedge，实际 %v", body["kind"])
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"kind":"flowedge","weight":3,"level":"ok","effectiveMax":6,"usedWeight":0}`))
	}))
	defer srv.Close()

	t.Setenv("GOVERNOR_BASE_URL", srv.URL)
	v := NewGovernorGate().Check(context.Background())

	if !v.Allowed || v.Source != "governor" {
		t.Fatalf("应放行且来源 governor，实际 allowed=%v source=%s", v.Allowed, v.Source)
	}
	if v.Weight != 3 || v.Level != "ok" {
		t.Fatalf("weight/level 未透传：%d %s", v.Weight, v.Level)
	}
}

// 治理器明确拒绝（429）→ 必须拦住，并把原因带出去
func TestGovernor_DeclinesWhenRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"ok":false,"kind":"flowedge","weight":3,"level":"critical",
			"effectiveMax":2,"usedWeight":2,"advice":"压力过高，先释放显存",
			"reasons":["gpu_reserve","cpu_pressure"]}`))
	}))
	defer srv.Close()

	t.Setenv("GOVERNOR_BASE_URL", srv.URL)
	v := NewGovernorGate().Check(context.Background())

	if v.Allowed {
		t.Fatal("治理器明确拒绝时必须拦住 —— 这是准入闸唯一的存在理由")
	}
	if v.Level != "critical" {
		t.Fatalf("level = %q", v.Level)
	}
	if v.Advice == "" || len(v.Reasons) != 2 {
		t.Fatalf("拒绝原因必须带出来（否则用户不知道为什么被拦）：advice=%q reasons=%v", v.Advice, v.Reasons)
	}
}

// 治理器 5xx → fail-open。拿一个坏掉的服务去否决出片是本末倒置。
func TestGovernor_ServerErrorIsFailOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	t.Setenv("GOVERNOR_BASE_URL", srv.URL)
	v := NewGovernorGate().Check(context.Background())

	if !v.Allowed || v.Source != "unreachable" {
		t.Fatalf("5xx 应放行，实际 allowed=%v source=%s", v.Allowed, v.Source)
	}
	if v.Detail == "" {
		t.Fatal("不可达要留下原因，便于排查「为什么这次没拦」")
	}
}

// 端口没人监听 → fail-open（序策没起是常态，不能因此拒绝出片）
func TestGovernor_UnreachableIsFailOpen(t *testing.T) {
	// 占一个端口再放掉，拿到一个几乎必然没人监听的地址
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	t.Setenv("GOVERNOR_BASE_URL", "http://"+addr)
	v := NewGovernorGate().Check(context.Background())

	if !v.Allowed || v.Source != "unreachable" {
		t.Fatalf("不可达应放行，实际 allowed=%v source=%s", v.Allowed, v.Source)
	}
}

// 响应不是 JSON（代理错误页 / 别的服务占了端口）→ fail-open
func TestGovernor_GarbageResponseIsFailOpen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>proxy error</html>"))
	}))
	defer srv.Close()

	t.Setenv("GOVERNOR_BASE_URL", srv.URL)
	v := NewGovernorGate().Check(context.Background())

	if !v.Allowed || v.Source != "unreachable" {
		t.Fatalf("无法解析应放行，实际 allowed=%v source=%s", v.Allowed, v.Source)
	}
}

// 开关关闭 → 一律放行，且一次请求都不发
func TestGovernor_DisabledSkipsRequest(t *testing.T) {
	hit := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	t.Setenv("GOVERNOR_BASE_URL", srv.URL)
	t.Setenv("FLOWEDGE_GOVERNOR", "off")
	v := NewGovernorGate().Check(context.Background())

	if !v.Allowed || v.Source != "disabled" {
		t.Fatalf("关闭时应放行，实际 allowed=%v source=%s", v.Allowed, v.Source)
	}
	if hit != 0 {
		t.Fatalf("关闭时不该发请求，实际发了 %d 次", hit)
	}
}

// 本机常驻 http_proxy 时必须直连 127.0.0.1。
//
// 这是 AGENTS §0.6 那个坑的同款：走默认 Transport 会把 localhost 请求
// 发给代理，拿回假 502，于是"治理器没起"和"治理器拒绝"分不清。
// 这里把 HTTP_PROXY 指到一个必然失败的地址，仍应成功直连。
func TestGovernor_BypassesHTTPProxy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"weight":3,"level":"ok"}`))
	}))
	defer srv.Close()

	t.Setenv("GOVERNOR_BASE_URL", srv.URL)
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")

	v := NewGovernorGate().Check(context.Background())
	if v.Source != "governor" {
		t.Fatalf("设了 HTTP_PROXY 也必须直连（否则拿到假 502）：source=%s detail=%s", v.Source, v.Detail)
	}
}
