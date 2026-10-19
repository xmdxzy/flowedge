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

// 回归：PUT 只带 name 时绝不能把 nodes/edges 清空。
// 旧实现无条件写三个字段，一次重命名就会静默清空整张工作流。
func TestUpdateWorkflowPartialDoesNotWipeGraph(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Workflow{}); err != nil {
		t.Fatal(err)
	}

	nodes := json.RawMessage(`[{"id":"s","type":"start","data":{}}]`)
	edges := json.RawMessage(`[]`)
	wf := model.Workflow{Name: "原名", Nodes: nodes, Edges: edges}
	if err := db.Create(&wf).Error; err != nil {
		t.Fatal(err)
	}

	h := NewWorkflowHandler(db, nil, nil)
	r := gin.New()
	r.PUT("/api/workflows/:id", h.Update)

	// 只改名
	req := httptest.NewRequest(http.MethodPut, "/api/workflows/1", strings.NewReader(`{"name":"新名"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("改名应成功，got %d: %s", w.Code, w.Body.String())
	}

	var got model.Workflow
	if err := db.First(&got, wf.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Name != "新名" {
		t.Fatalf("名字没更新：%s", got.Name)
	}
	if len(got.Nodes) == 0 || string(got.Nodes) == "null" {
		t.Fatalf("改名把节点清空了：%s", string(got.Nodes))
	}
	if string(got.Edges) == "null" {
		t.Fatalf("改名把连线清空了：%s", string(got.Edges))
	}

	// 空 body 应明确拒绝，而不是静默清空
	req = httptest.NewRequest(http.MethodPut, "/api/workflows/1", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("空更新应 400，got %d", w.Code)
	}
}
