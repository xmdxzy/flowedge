package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"flowedge/backend/model"
)

// 热实例（模型已在显存里）不能被预检门判成"装不下"。
//
// 2026-10-26 实测 run 247：H3 已驻留 19GB，空闲只剩 5.3GB，
// 于是"空闲 < 模型 20.5GB"被判 too_small → 409 拦住，可它一次加载都不需要。
// 判据：已用显存 ≥ 需求 60% 就算已驻留。
func TestAlreadyResidentForNodes(t *testing.T) {
	cases := []struct {
		name     string
		freeMB   int64
		totalMB  int64
		needMB   int
		expected bool
	}{
		{"冷实例（只用桌面 2GB）", 22000, 24575, 20480, false},
		{"热实例（已驻留 19GB）", 5300, 24575, 20480, true},
		{"半驻留边界之上", 12000, 24575, 20480, true},  // used=12575 ≥ 12288
		{"半驻留边界之下", 13000, 24575, 20480, false}, // used=11575 < 12288
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"devices": []map[string]int64{{"vram_free": c.freeMB * (1 << 20), "vram_total": c.totalMB * (1 << 20)}},
			})
		}))
		nodes := []model.NodeInstance{{Type: "text2video"}}
		got := AlreadyResidentForNodes(nodes, "", srv.URL, c.needMB)
		srv.Close()
		if got != c.expected {
			t.Fatalf("%s：AlreadyResidentForNodes = %v, want %v", c.name, got, c.expected)
		}
	}
}

// 探不到实例时保守返回 false（宁可不放行，也不假装已驻留）。
func TestAlreadyResidentForNodes_Unreachable(t *testing.T) {
	nodes := []model.NodeInstance{{Type: "text2video"}}
	if AlreadyResidentForNodes(nodes, "", "http://127.0.0.1:1", 20480) {
		t.Fatal("探不到实例时必须返回 false")
	}
}
