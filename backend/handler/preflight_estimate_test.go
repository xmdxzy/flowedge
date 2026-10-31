package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// 出片入口必须能回答「这条片子大概多久」。
//
// 之前 EstimateGraph 只挂在画布的 /graph/dry-run 上，流水线入口返回的 preflight
// 只有 missing/warnings —— 于是用户问「30 秒片要多久」，产品里没有任何地方能答。
// 这条测试钉住：preflight 必须带 estimate，且是**区间**（p50/p90/worst 都在）。
func TestPreflightReturnsEstimate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	p := &Producer{db: db, pipes: newTestPipelines(t), budgets: map[uint]*budgetState{}}

	r := gin.New()
	r.POST("/api/preflight", p.PreflightPipelines)

	body := `{"pipeline":"film","params":{"prompt":"一个人在海边散步","seconds":10}}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/preflight", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("preflight 应 200，实际 %d：%s", w.Code, w.Body.String())
	}
	var resp struct {
		Estimate *struct {
			P50Sec   float64 `json:"p50_sec"`
			P90Sec   float64 `json:"p90_sec"`
			WorstSec float64 `json:"worst_sec"`
			Caveat   string  `json:"caveat"`
		} `json:"estimate"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON：%v", err)
	}
	if resp.Estimate == nil {
		t.Fatalf("preflight 必须返回 estimate（区间估算），实际响应：%s", w.Body.String())
	}
	if resp.Estimate.P50Sec <= 0 || resp.Estimate.P90Sec <= 0 {
		t.Errorf("估算必须给出正数区间：p50=%.1f p90=%.1f", resp.Estimate.P50Sec, resp.Estimate.P90Sec)
	}
	if resp.Estimate.P90Sec < resp.Estimate.P50Sec {
		t.Errorf("p90 不应小于 p50：p50=%.1f p90=%.1f", resp.Estimate.P50Sec, resp.Estimate.P90Sec)
	}
	if resp.Estimate.Caveat == "" {
		t.Error("估算必须带 caveat（区间估算的适用边界），否则会被当成承诺")
	}
}
