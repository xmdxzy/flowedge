package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"flowedge/backend/service"
)

func postJSON(r *gin.Engine, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func newEstimateRouter(h *EstimateHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/estimate", h.Estimate)
	r.GET("/api/vram/profiles", h.ListProfiles)
	return r
}

func seededProfiles() *service.ProfileStore {
	s := service.LoadProfileStore("", 24576)
	s.Record(service.VRAMProfile{Kind: "image", Model: "flux", Width: 1024, Height: 1024, Steps: 4,
		PeakMB: 17000, WallMS: 3000, Samples: 3, Source: "sampled"})
	s.Record(service.VRAMProfile{Kind: "h3", Width: 608, Height: 352, Frames: 124, Steps: 20,
		PeakMB: 21800, WallMS: 120000, Samples: 5, Source: "sampled"})
	return s
}

type estimateResp struct {
	Items []struct {
		Kind       string `json:"kind"`
		PeakMB     int    `json:"peak_mb"`
		Confidence string `json:"confidence"`
		Basis      string `json:"basis"`
	} `json:"items"`
	PeakMB     int      `json:"peak_mb"`
	WallMinMS  int64    `json:"wall_min_ms"`
	WallMaxMS  int64    `json:"wall_max_ms"`
	Fits       bool     `json:"fits"`
	Confidence string   `json:"confidence"`
	Notes      []string `json:"notes"`
	Warning    string   `json:"warning"`
}

// TestEstimate_PeakIsMaxWallIsSum 口径：显存取 max（同一张卡串行），时间求和
func TestEstimate_PeakIsMaxWallIsSum(t *testing.T) {
	h := NewEstimateHandler(nil, seededProfiles(), 24576)
	r := newEstimateRouter(h)
	body := `{"nodes":[
		{"type":"text2image","data":{"width":1024,"height":1024,"steps":4,"model":"flux"}},
		{"type":"text2image","data":{"width":1024,"height":1024,"steps":4,"model":"flux"}}
	]}`
	w := postJSON(r, "/api/estimate", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var resp estimateResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.PeakMB != 17000 {
		t.Errorf("peak = %d, want max(17000,17000)=17000", resp.PeakMB)
	}
	if resp.WallMinMS < 5000 {
		t.Errorf("wall should be summed over 2 nodes, got %d", resp.WallMinMS)
	}
	if !resp.Fits {
		t.Error("17GB should fit in 24GB")
	}
}

// TestEstimate_ShotlistMultipliesByShotCount 分镜按镜数串行累加，并给出风险提示
func TestEstimate_ShotlistMultipliesByShotCount(t *testing.T) {
	h := NewEstimateHandler(nil, seededProfiles(), 24576)
	r := newEstimateRouter(h)
	body := `{"nodes":[{"type":"shotlist","data":{"width":608,"height":352,"frames":124,"shots":[{},{},{},{}]}}]}`
	w := postJSON(r, "/api/estimate", body)
	var resp estimateResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// 单镜 120s × 4 镜 → 至少 8 分钟
	if resp.WallMinMS < 8*60*1000 {
		t.Errorf("4 shots should take >= 8min, got %d ms", resp.WallMinMS)
	}
	joined := strings.Join(resp.Notes, " ")
	if !strings.Contains(joined, "4 镜") {
		t.Errorf("notes should state the shot count: %v", resp.Notes)
	}
}

// TestEstimate_NoSamplesSaysNone 没有样本时必须报 none，不要给一个编出来的数
func TestEstimate_NoSamplesSaysNone(t *testing.T) {
	empty := service.LoadProfileStore("", 24576)
	h := NewEstimateHandler(nil, empty, 24576)
	r := newEstimateRouter(h)
	w := postJSON(r, "/api/estimate", `{"nodes":[{"type":"text2video","data":{"width":832,"height":480,"frames":33}}]}`)
	var resp estimateResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 1 || resp.Items[0].Confidence != "none" {
		t.Fatalf("expected confidence none, got %+v", resp.Items)
	}
	if resp.Items[0].PeakMB != 0 {
		t.Errorf("must not invent numbers, got peak=%d", resp.Items[0].PeakMB)
	}
	if resp.Confidence != "none" {
		t.Errorf("overall confidence = %q", resp.Confidence)
	}
}

// TestEstimate_RequiresInput 空请求必须 400
func TestEstimate_RequiresInput(t *testing.T) {
	h := NewEstimateHandler(nil, seededProfiles(), 24576)
	r := newEstimateRouter(h)
	if w := postJSON(r, "/api/estimate", `{}`); w.Code != http.StatusBadRequest {
		t.Errorf("empty request: status = %d, want 400", w.Code)
	}
}

// TestEstimate_NonMediaNodesIgnored 非媒体节点不该产生估算条目
func TestEstimate_NonMediaNodesIgnored(t *testing.T) {
	h := NewEstimateHandler(nil, seededProfiles(), 24576)
	r := newEstimateRouter(h)
	w := postJSON(r, "/api/estimate", `{"nodes":[{"type":"start"},{"type":"llm"},{"type":"tts"}]}`)
	var resp estimateResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Items) != 0 {
		t.Errorf("expected no media predictions, got %+v", resp.Items)
	}
	if len(resp.Notes) == 0 {
		t.Error("should explain why nothing was estimated")
	}
}

// TestListProfiles_EmptyOK 未启用档案时也要能查询（返回空 + 说明）
func TestListProfiles_EmptyOK(t *testing.T) {
	h := NewEstimateHandler(nil, nil, 24576)
	r := newEstimateRouter(h)
	req := httptest.NewRequest(http.MethodGet, "/api/vram/profiles", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "未启用") {
		t.Errorf("should explain why it is empty: %s", w.Body.String())
	}
}
