package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"flowedge/backend/model"
)

// 晋升台账端点的单测。重点在三件事：
//  1. 坏 action 必须 400（台账里混进看不懂的记录，审计时就要猜）
//  2. 缺理由必须 400（"当时凭什么"这一栏不许空）
//  3. 落下去的记录必须能原样查回来

func newPromoHandlerTest(t *testing.T) (*gin.Engine, *gorm.DB, *EvolutionHandler) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Workflow{}, &model.WorkflowVersion{}, &model.Promotion{}); err != nil {
		t.Fatal(err)
	}
	h := NewEvolutionHandler(db, nil)
	r := gin.New()
	r.POST("/api/evolution/promote", h.Promote)
	r.GET("/api/evolution/promotions", h.Promotions)
	return r, db, h
}

func TestPromote_RejectsBadAction(t *testing.T) {
	r, _, _ := newPromoHandlerTest(t)

	// action 不在 {promote, keep} 里：端点只收人按的两个键
	w := postJSON(r, "/api/evolution/promote", `{"workflowId":1,"action":"rollback","reason":"x"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("action=rollback 应 400，got %d: %s", w.Code, w.Body.String())
	}
	w = postJSON(r, "/api/evolution/promote", `{"workflowId":1,"action":"archive","reason":"x"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("action=archive（引擎专用）应 400，got %d", w.Code)
	}
	w = postJSON(r, "/api/evolution/promote", `{"workflowId":1,"reason":"x"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("缺 action 应 400，got %d", w.Code)
	}
}

func TestPromote_RequiresReason(t *testing.T) {
	r, _, _ := newPromoHandlerTest(t)
	w := postJSON(r, "/api/evolution/promote", `{"workflowId":1,"action":"keep"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("缺 reason 应 400，got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "reason") {
		t.Errorf("错误信息应点名 reason，收到：%s", w.Body.String())
	}
}

// keep 落库 + 查回来：一条记录从写到读的完整往返。
func TestPromote_KeepRecordedAndListed(t *testing.T) {
	r, db, _ := newPromoHandlerTest(t)
	if err := db.Create(&model.Workflow{Name: "wf"}).Error; err != nil {
		t.Fatal(err)
	}

	w := postJSON(r, "/api/evolution/promote",
		`{"workflowId":1,"action":"keep","reason":"token 涨幅超上限，维持基线","actor":"张越"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("keep 应成功，got %d: %s", w.Code, w.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/evolution/promotions?workflowId=1", nil)
	lw := httptest.NewRecorder()
	r.ServeHTTP(lw, req)
	if lw.Code != http.StatusOK {
		t.Fatalf("查询应成功，got %d", lw.Code)
	}
	var resp struct {
		Count  int               `json:"count"`
		Latest model.Promotion   `json:"latest"`
		Recs   []model.Promotion `json:"records"`
	}
	if err := json.Unmarshal(lw.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count != 1 || len(resp.Recs) != 1 {
		t.Fatalf("应有 1 条记录，count=%d", resp.Count)
	}
	got := resp.Recs[0]
	if got.Action != "keep" || got.Actor != "张越" || got.Source != "promote" {
		t.Errorf("记录字段不对：%+v", got)
	}
	if !strings.Contains(got.Reason, "token 涨幅超上限") {
		t.Errorf("理由应原样读回，收到 %q", got.Reason)
	}
}

// promote 到不存在的版本：404，且不许落库（没有目标的晋升是伪记录）。
func TestPromote_UnknownVersionRejected(t *testing.T) {
	r, db, _ := newPromoHandlerTest(t)
	if err := db.Create(&model.Workflow{Name: "wf"}).Error; err != nil {
		t.Fatal(err)
	}
	w := postJSON(r, "/api/evolution/promote",
		`{"workflowId":1,"action":"promote","toVersion":99,"reason":"x"`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("坏 JSON 应 400，got %d", w.Code)
	}

	w = postJSON(r, "/api/evolution/promote",
		`{"workflowId":1,"action":"promote","toVersion":99,"reason":"门分提升"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("不存在的版本应 404，got %d: %s", w.Code, w.Body.String())
	}
	var count int64
	db.Model(&model.Promotion{}).Count(&count)
	if count != 0 {
		t.Errorf("失败的晋升不许落库，台账却有 %d 条", count)
	}
}

// promote apply=true 但 workflow 层未装配：503 且不落库。
// 没有快照路径就不许写图 —— 这是 promote 与 version rollback 的关键区别。
func TestPromote_ApplyWithoutWiringRefuses(t *testing.T) {
	r, db, _ := newPromoHandlerTest(t)
	if err := db.Create(&model.Workflow{Name: "wf"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.WorkflowVersion{
		WorkflowID: 1, Version: 4, Name: "v4",
		Nodes: json.RawMessage(`[]`), Edges: json.RawMessage(`[]`),
	}).Error; err != nil {
		t.Fatal(err)
	}
	w := postJSON(r, "/api/evolution/promote",
		`{"workflowId":1,"action":"promote","toVersion":4,"reason":"门分提升","apply":true}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("未装配写图路径应 503，got %d: %s", w.Code, w.Body.String())
	}
	var count int64
	db.Model(&model.Promotion{}).Count(&count)
	if count != 0 {
		t.Errorf("拒绝写图时不许落库，台账却有 %d 条", count)
	}
}

// 台账查询按 workflowId 隔离：别的工作流的记录不许混进来。
func TestPromotions_ScopedByWorkflow(t *testing.T) {
	r, db, _ := newPromoHandlerTest(t)
	for _, id := range []uint{1, 2} {
		if err := db.Create(&model.Workflow{Name: "wf"}).Error; err != nil {
			t.Fatal(err)
		}
		_ = id
	}
	w := postJSON(r, "/api/evolution/promote", `{"workflowId":1,"action":"keep","reason":"甲"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("wf1 keep: %d", w.Code)
	}
	w = postJSON(r, "/api/evolution/promote", `{"workflowId":2,"action":"keep","reason":"乙"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("wf2 keep: %d", w.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/evolution/promotions?workflowId=2", nil)
	lw := httptest.NewRecorder()
	r.ServeHTTP(lw, req)
	var resp struct {
		Count int               `json:"count"`
		Recs  []model.Promotion `json:"records"`
	}
	if err := json.Unmarshal(lw.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count != 1 || resp.Recs[0].WorkflowID != 2 {
		t.Errorf("workflowId=2 只应看到自己的 1 条，收到 count=%d", resp.Count)
	}

	// 缺 workflowId：400，不给"全库台账"这种含糊响应
	req = httptest.NewRequest(http.MethodGet, "/api/evolution/promotions", nil)
	lw = httptest.NewRecorder()
	r.ServeHTTP(lw, req)
	if lw.Code != http.StatusBadRequest {
		t.Errorf("缺 workflowId 应 400，got %d", lw.Code)
	}
}

// decide record=true：引擎结论归档为 archive（人未按键），
// 且 verdict 原样进台账 —— 复核时能看出"引擎当时怎么判、人最后按了什么"。
func TestDecide_RecordArchivesEngineVerdict(t *testing.T) {
	// 隔离证据目录：不让真实 data/evidence 影响判定
	t.Setenv("EVIDENCE_DIR", t.TempDir())
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Workflow{}, &model.Promotion{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Workflow{Name: "wf"}).Error; err != nil {
		t.Fatal(err)
	}
	h := NewEvolutionHandler(db, nil)
	r := gin.New()
	r.POST("/api/evolution/decide", h.Decide)

	// 无证据 → inconclusive；record=true 也要归档（"测了，没测够"同样是事实）
	w := postJSON(r, "/api/evolution/decide", `{"workflowId":1,"record":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("decide 应成功，got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Recorded bool `json:"recorded"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Recorded {
		t.Errorf("recorded 应为 true")
	}

	var rec model.Promotion
	if err := db.Order("id desc").First(&rec).Error; err != nil {
		t.Fatalf("decide 归档应落库：%v", err)
	}
	if rec.Action != "archive" || rec.Source != "decide" {
		t.Errorf("decide 归档口径不对：action=%s source=%s", rec.Action, rec.Source)
	}
	if rec.Verdict != "inconclusive" {
		t.Errorf("verdict 应原样归档，收到 %q", rec.Verdict)
	}
	if rec.Actor != "engine" {
		t.Errorf("引擎归档 actor 应是 engine，收到 %q", rec.Actor)
	}
	if rec.Reason == "" {
		t.Errorf("归档不许空理由")
	}

	// 不传 record：不落库（归档是显式动作，不是 decide 的副作用）
	w = postJSON(r, "/api/evolution/decide", `{"workflowId":1}`)
	if w.Code != http.StatusOK {
		t.Fatalf("decide 应成功，got %d", w.Code)
	}
	var count int64
	db.Model(&model.Promotion{}).Count(&count)
	if count != 1 {
		t.Errorf("不带 record 的 decide 不该落库，台账却有 %d 条", count)
	}
}
