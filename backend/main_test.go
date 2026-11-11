package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// 静态资源服务的关键约定：
//   - "/" 直接返回 200 + index.html 内容（不能是 301，否则与 SPA 回退叠成重定向循环）
//   - 未知路由回退到 index.html（前端路由）
//   - 真实静态文件带正确的 Content-Type
//   - /api/* 未命中时返回 JSON 404，不能回退成 HTML
func TestMountStatic(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html><title>FlowEdge 测试页</title>"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("console.log(1)"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STATIC_DIR", dir)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	mountStatic(r)

	cases := []struct {
		name        string
		path        string
		wantCode    int
		wantContain string
		wantJSON    bool
	}{
		{"首页不重定向", "/", http.StatusOK, "FlowEdge 测试页", false},
		{"SPA 深链回退", "/workflows/42", http.StatusOK, "FlowEdge 测试页", false},
		{"静态资源", "/assets/app.js", http.StatusOK, "console.log", false},
		// 带扩展名的资源缺失时必须真 404：回退成 index.html 会让浏览器
		// 把 HTML 当 JS 解析，报 "Unexpected token '<'"，比 404 难查得多。
		{"缺资源不回退成 HTML", "/assets/gone.js", http.StatusNotFound, "not found", false},
		{"API 未命中", "/api/nope", http.StatusNotFound, "not found", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if w.Code != tc.wantCode {
				t.Fatalf("%s 返回 %d，期望 %d（index.html 用 ServeFile 会 301，注意别回归）",
					tc.path, w.Code, tc.wantCode)
			}
			body := w.Body.String()
			if !strings.Contains(body, tc.wantContain) {
				t.Errorf("%s 响应体不含 %q：%s", tc.path, tc.wantContain, body[:min(120, len(body))])
			}
			if tc.wantJSON && !strings.Contains(w.Header().Get("Content-Type"), "json") {
				t.Errorf("%s 的 Content-Type = %s，期望 json", tc.path, w.Header().Get("Content-Type"))
			}
		})
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// 缓存约定：带哈希的产物永久缓存、壳不缓存。
// 反例（真实出现过）：壳被缓存后升级 exe，浏览器仍拿旧壳指向已删除的 hash 产物 → 白屏。
func TestStaticCacheHeaders(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html><title>壳</title>"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "index-abc123.js"), []byte("1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "favicon.svg"), []byte("<svg/>"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STATIC_DIR", dir)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	mountStatic(r)

	cases := []struct {
		path     string
		wantPart string
	}{
		{"/", "no-cache"},
		{"/workflows/42", "no-cache"},            // SPA 回退同样是壳
		{"/assets/index-abc123.js", "immutable"}, // 哈希产物
		{"/favicon.svg", "max-age=3600"},         // 非哈希资源
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if got := w.Header().Get("Cache-Control"); !strings.Contains(got, tc.wantPart) {
			t.Errorf("%s 的 Cache-Control = %q，期望含 %q", tc.path, got, tc.wantPart)
		}
	}
}

// 日志噪声闸门：这些高频成功请求不写日志，否则引擎日志会被轮询淹掉。
func TestQuietRequest(t *testing.T) {
	quiet := [][2]string{
		{"GET", "/api/health"},
		{"POST", "/api/capabilities"},
		{"GET", "/api/ws"},
		{"GET", "/api/deps/tts/wait"},
		{"GET", "/api/artifacts/film-123.mp4"},
		{"GET", "/assets/index-abc.js"},
		{"GET", "/runs"},
	}
	for _, q := range quiet {
		if !quietRequest(q[0], q[1]) {
			t.Errorf("%s %s 应视为噪声请求", q[0], q[1])
		}
	}
	loud := [][2]string{
		{"POST", "/api/produce"},
		{"GET", "/api/runs"},
		{"POST", "/api/deps/tts/start"},
		{"DELETE", "/api/artifacts/film.mp4"},
	}
	for _, l := range loud {
		if quietRequest(l[0], l[1]) {
			t.Errorf("%s %s 是业务调用，不该静音", l[0], l[1])
		}
	}
}
