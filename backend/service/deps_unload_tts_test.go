package service

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPostEmptyDistinguishesMissingEndpoint 钉住「后端不支持」与「请求失败」的分别。
//
// 由来（2026-10-28）：界面上点「卸载 TTS」曾答「该依赖没有卸载接口」，
// 那对 GPU 上的 Breeze 是错的（它驻留 16274 MiB 且有 /unload）。
// 补上分支之后，必须保证三种状态不被混成一种：
//
//	200  → 成功
//	404  → 该后端没这个端点（能力缺失，如实说，别报成失败）
//	500  → 真错误（要冒泡，别静默成「不支持」）
//
// 这正是 §0.6「不可达 ≠ 空结果」的同型要求。
func TestPostEmptyDistinguishesMissingEndpoint(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		wantErr  bool
		isMissin bool
	}{
		{"ok", http.StatusOK, false, false},
		{"not-implemented", http.StatusNotFound, true, true},
		{"server-error", http.StatusInternalServerError, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("期望 POST，实际 %s", r.Method)
				}
				w.WriteHeader(c.status)
			}))
			defer srv.Close()

			err := postEmpty(srv.URL, "/unload")
			if c.wantErr && err == nil {
				t.Fatalf("状态 %d 应当返回错误", c.status)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("状态 %d 不该返回错误：%v", c.status, err)
			}
			gotMissing := errors.Is(err, errDepEndpointMissing)
			if gotMissing != c.isMissin {
				t.Fatalf("404 判定错误：期望 errDepEndpointMissing=%v，实际 err=%v", c.isMissin, err)
			}
		})
	}
}

// TestPostEmptyRejectsUnreachable 连不上必须是错误，不能装成成功。
func TestPostEmptyRejectsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // 立刻关掉，制造不可达

	if err := postEmpty(url, "/unload"); err == nil {
		t.Fatal("不可达时应当返回错误")
	} else if errors.Is(err, errDepEndpointMissing) {
		t.Fatalf("不可达不该被判成「端点缺失」：%v", err)
	}
}
