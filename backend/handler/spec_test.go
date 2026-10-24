package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

// specHandlerFixture 建一个内存库 + 一条三节点工作流。
// DSN 用唯一值：共用 file::memory:?cache=shared 在 -count>1 重跑时会互相污染。
func specHandlerFixture(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dsn := "file:spec_handler_" + t.Name() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Workflow{}); err != nil {
		t.Fatal(err)
	}

	nodes := []map[string]interface{}{
		{"id": "s", "type": "start", "data": map[string]interface{}{"prompt": "雪夜归城"}},
		{"id": "b", "type": "if_else", "data": map[string]interface{}{"condition_type": "contains", "compare_value": "雪"}},
		{"id": "i", "type": "text2image", "data": map[string]interface{}{"prompt": "snowy night", "width": 768}},
	}
	edges := []map[string]interface{}{
		{"id": "e1", "source": "s", "target": "b"},
		{"id": "e2", "source": "b", "target": "i"},
	}
	nj, _ := json.Marshal(nodes)
	ej, _ := json.Marshal(edges)
	if err := db.Create(&model.Workflow{Name: "任务书样例", Nodes: nj, Edges: ej}).Error; err != nil {
		t.Fatal(err)
	}

	h := NewWorkflowHandler(db, nil, nil)
	r := gin.New()
	r.GET("/api/workflows/:id/spec", h.Spec)
	mh := NewModuleHandler()
	r.GET("/api/modules", mh.List)
	return r, db
}

func TestSpecEndpointMarkdown(t *testing.T) {
	r, _ := specHandlerFixture(t)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/workflows/1/spec", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/markdown") {
		t.Errorf("Content-Type 应为 markdown，实际 %q", ct)
	}
	body := w.Body.String()
	for _, want := range []string{"# 工作流任务书", "执行顺序", "验收门", "产物清单", "开始", "条件分支", "文生图"} {
		if !strings.Contains(body, want) {
			t.Errorf("任务书缺少 %q\n---\n%s", want, body)
		}
	}
}

func TestSpecEndpointJSON(t *testing.T) {
	r, _ := specHandlerFixture(t)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/workflows/1/spec?format=json", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%s", w.Code, w.Body.String())
	}

	var doc service.SpecDoc
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("JSON 解析失败：%v", err)
	}
	if len(doc.Modules) != 3 {
		t.Fatalf("期望 3 个模块，实际 %d", len(doc.Modules))
	}
	if doc.Markdown == "" {
		t.Error("json 格式也应带 markdown，方便一次取两种形态")
	}
	// 参数只报真实写的值
	var sawWidth bool
	for _, p := range doc.Modules[2].Params {
		if p.Key == "width" && p.Value == "768" {
			sawWidth = true
		}
	}
	if !sawWidth {
		t.Errorf("width=768 没进任务书：%+v", doc.Modules[2].Params)
	}
}

func TestSpecEndpointErrors(t *testing.T) {
	r, _ := specHandlerFixture(t)

	// 不存在的工作流
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/workflows/999/spec", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("不存在的工作流应 404，实际 %d", w.Code)
	}

	// 非法 format
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/workflows/1/spec?format=xml", nil))
	if w.Code != http.StatusBadRequest {
		t.Errorf("非法 format 应 400，实际 %d", w.Code)
	}
}

// 模块目录必须与契约表完全一致：前端/智能体靠它知道"有哪些积木"。
func TestModulesEndpointMatchesContract(t *testing.T) {
	r, _ := specHandlerFixture(t)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/modules", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", w.Code)
	}

	var resp struct {
		Count   int          `json:"count"`
		Modules []ModuleView `json:"modules"`
		Kinds   []string     `json:"port_kinds"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Count != len(service.Modules) {
		t.Errorf("count=%d，契约表有 %d 条", resp.Count, len(service.Modules))
	}
	for _, m := range resp.Modules {
		if m.Title == "" {
			t.Errorf("%s 没有 title", m.Type)
		}
		if _, ok := service.NodeSpecOf(m.Type); !ok {
			t.Errorf("%s 不在 NodeSpecs 里", m.Type)
		}
	}
	if len(resp.Kinds) == 0 {
		t.Error("port_kinds 为空：模态类型必须在目录里可见")
	}
}

// 运行时要记录「谁在调」，并在与开始节点声明的入口不一致时给出提示（不阻断）。
func TestRunRecordsInvokerAndWarnsMismatch(t *testing.T) {
	r, db := specHandlerFixture(t)
	// 运行记录要落库：fixture 只建了 workflows 表，这里补上 runs/jobs
	if err := db.AutoMigrate(&model.WorkflowRun{}, &model.Job{}); err != nil {
		t.Fatal(err)
	}
	// 真引擎（空壳可懒装配内置节点）：start→end 不碰显存，跑得动
	h := NewWorkflowHandler(db, &service.EngineV2{}, nil)
	r.POST("/api/workflows", h.Create)
	r.POST("/api/workflows/:id/run", h.Run)

	wf := `{"name":"入口","nodes":[{"id":"s","type":"start","data":{"prompt":"x","entry_agent":"workbuddy"}},{"id":"e","type":"end","data":{}}],"edges":[{"id":"x","source":"s","target":"e"}]}`
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/workflows", strings.NewReader(wf))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("建工作流失败：%d %s", w.Code, w.Body.String())
	}
	var created model.Workflow
	_ = json.Unmarshal(w.Body.Bytes(), &created)

	// 1) 不认识的调用方 → 400
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/workflows/%d/run", created.ID), strings.NewReader(`{"invoked_by":"随便谁"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法 invoked_by 应被拒，实际 %d", w.Code)
	}

	// 2) dsh 调一条声明给 workbuddy 的流水线 → 202 + warning + 记录实际调用方
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/workflows/%d/run", created.ID), strings.NewReader(`{"invoked_by":"dsh"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("应受理，实际 %d：%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "warning") {
		t.Errorf("调用方不一致应给 warning：%s", w.Body.String())
	}

	var run model.WorkflowRun
	if err := db.Order("id desc").First(&run).Error; err != nil {
		t.Fatal(err)
	}
	if run.InvokedBy != "dsh" {
		t.Errorf("运行记录应记为 dsh，实际 %q", run.InvokedBy)
	}
}
