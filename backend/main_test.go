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
