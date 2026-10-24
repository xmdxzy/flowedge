package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "test.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&model.Workflow{}, &model.WorkflowRun{}, &model.Job{}, &model.RunEvent{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// sqlite 的文件句柄不关，TempDir 清理会报 "being used by another process"
	if sqlDB, err := db.DB(); err == nil {
		t.Cleanup(func() { _ = sqlDB.Close() })
	}
	return db
}

func newTestPipelines(t *testing.T) *service.PipelineStore {
	t.Helper()
	s, err := service.LoadPipelines(filepath.Join("..", "..", "data", "pipelines.json"))
	if err != nil {
		t.Fatalf("load pipelines: %v", err)
	}
	return s
}

// 参数错误必须在入口就变成 400 + 人话，而不是先建一条 run 再失败。
func TestProduceRejectsBadParams(t *testing.T) {
	gin.SetMode(gin.TestMode)
	p := NewProducer(newTestDB(t), nil, nil, newTestPipelines(t))

	cases := []struct {
		name string
		body string
		want string
	}{
		{"缺必填", `{"pipeline":"image","params":{}}`, "画面描述"},
		{"未知字段", `{"pipeline":"image","params":{"propmt":"x"}}`, "propmt"},
		{"没有这条流水线", `{"pipeline":"nope","params":{}}`, "没有这条流水线"},
		{"非法 JSON", `{`, "请求体"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := gin.New()
			r.POST("/api/produce", p.Produce)
			req := httptest.NewRequest(http.MethodPost, "/api/produce", strings.NewReader(c.body))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d，期望 400（body=%s）", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), c.want) {
				t.Errorf("响应应包含 %q，实际 %s", c.want, w.Body.String())
			}
		})
	}
}

// 质量循环包壳：外层必须是 start → quality_loop → end，
// 内层图要落成一条临时工作流（quality_loop 按 workflow_id 取图）。
func TestWrapQualityLoopShape(t *testing.T) {
	db := newTestDB(t)
	p := NewProducer(db, nil, nil, newTestPipelines(t))

	built, err := p.pipes.Build("image", map[string]interface{}{"prompt": "一只白鹿", "qc": true})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	outer, innerID, err := p.wrapQualityLoop(built, 0)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	if innerID == 0 {
		t.Fatal("没有落内层临时工作流")
	}

	var inner model.Workflow
	if err := db.First(&inner, innerID).Error; err != nil {
		t.Fatalf("内层工作流不存在：%v", err)
	}
	if !strings.HasPrefix(inner.Name, "qc:") {
		t.Errorf("内层工作流名字应可被清扫（qc: 前缀），实际 %q", inner.Name)
	}
	var innerNodes []model.NodeInstance
	if err := json.Unmarshal(inner.Nodes, &innerNodes); err != nil {
		t.Fatalf("内层图不合法：%v", err)
	}
	if len(innerNodes) != len(built.Nodes) {
		t.Errorf("内层图应保留原始节点（%d），实际 %d", len(built.Nodes), len(innerNodes))
	}

	if len(outer.Nodes) != 3 {
		t.Fatalf("外层应有 3 个节点，实际 %d", len(outer.Nodes))
	}
	if outer.Nodes[0].Type != "start" || outer.Nodes[1].Type != "quality_loop" || outer.Nodes[2].Type != "end" {
		t.Fatalf("外层顺序应为 start→quality_loop→end，实际 %s→%s→%s",
			outer.Nodes[0].Type, outer.Nodes[1].Type, outer.Nodes[2].Type)
	}
	ql := outer.Nodes[1].Data
	if got, _ := ql["workflow_id"].(float64); uint(got) != innerID {
		t.Errorf("quality_loop.workflow_id = %v，期望 %d", ql["workflow_id"], innerID)
	}
	if got, _ := ql["input"].(string); got != "一只白鹿" {
		t.Errorf("quality_loop.input 应带上提示词，实际 %q", got)
	}
	if len(outer.Edges) != 2 {
		t.Errorf("外层应有 2 条边，实际 %d", len(outer.Edges))
	}
}

// 崩溃残留的 qc: 临时图必须能被清掉（否则库里会无限长）。
func TestSweepTransient(t *testing.T) {
	db := newTestDB(t)
	p := NewProducer(db, nil, nil, newTestPipelines(t))
	db.Create(&model.Workflow{Name: "qc:image", Nodes: []byte("[]"), Edges: []byte("[]")})
	db.Create(&model.Workflow{Name: "keep:me", Nodes: []byte("[]"), Edges: []byte("[]")})

	if n := p.SweepTransient(); n != 1 {
		t.Fatalf("应清掉 1 条，实际 %d", n)
	}
	var left int64
	db.Model(&model.Workflow{}).Count(&left)
	if left != 1 {
		t.Errorf("不该动非 qc: 记录，剩余 %d", left)
	}
}
