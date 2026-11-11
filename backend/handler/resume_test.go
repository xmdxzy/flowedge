package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"flowedge/backend/model"
	"flowedge/backend/service"
)

// 续跑接口的语义边界必须钉住：能续的续、不能续的明确拒绝并给替代路径。
func TestResumeRun_Rejects(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	p := &Producer{db: db, budgets: map[uint]*budgetState{}}
	r := gin.New()
	r.POST("/api/runs/:runId/resume", p.ResumeRun)

	do := func(id string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/runs/"+id+"/resume", nil)
		r.ServeHTTP(w, req)
		return w
	}

	// 非法 id
	if w := do("abc"); w.Code != http.StatusBadRequest {
		t.Errorf("非法 id 应 400，实际 %d", w.Code)
	}
	// 不存在的 run
	if w := do("9999"); w.Code != http.StatusNotFound {
		t.Errorf("不存在应 404，实际 %d", w.Code)
	}

	// 正在跑的不能续（避免同一条 run 两个执行体）
	running := model.WorkflowRun{Status: "running"}
	db.Create(&running)
	if w := do(itoaU(running.ID)); w.Code != http.StatusConflict {
		t.Errorf("running 应 409，实际 %d：%s", w.Code, w.Body.String())
	}

	// 成功完成的也不该续（语义上是重跑，用 rerun）
	ok := model.WorkflowRun{Status: "success"}
	db.Create(&ok)
	if w := do(itoaU(ok.ID)); w.Code != http.StatusConflict {
		t.Errorf("success 应 409，实际 %d", w.Code)
	}

	// 被中断但工作流已被清理（qc 临时图跑完即删）→ 必须提示改用 rerun，而不是空跑
	stale := model.WorkflowRun{Status: "interrupted", WorkflowID: 424242}
	db.Create(&stale)
	w := do(itoaU(stale.ID))
	if w.Code != http.StatusNotFound {
		t.Errorf("工作流缺失应 404，实际 %d：%s", w.Code, w.Body.String())
	}
	if !containsStr(w.Body.String(), "rerun") {
		t.Errorf("必须提示改用 rerun，实际：%s", w.Body.String())
	}
}

// 中断状态 + 图还在 + 引擎缺失 → 503（不能假装受理）
func TestResumeRun_NoEngine(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	p := &Producer{db: db, budgets: map[uint]*budgetState{}}

	nodes := `[{"id":"start-1","type":"start","data":{},"status":"success"},{"id":"t2v-1","type":"text2video","data":{},"status":"failed"}]`
	wf := model.Workflow{Name: "resume-test", Nodes: json.RawMessage(nodes), Edges: json.RawMessage(`[]`)}
	db.Create(&wf)
	run := model.WorkflowRun{
		Status:     "interrupted",
		WorkflowID: wf.ID,
		NodeStates: json.RawMessage(nodes),
	}
	db.Create(&run)

	r := gin.New()
	r.POST("/api/runs/:runId/resume", p.ResumeRun)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/runs/"+itoaU(run.ID)+"/resume", nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("引擎缺失应 503，实际 %d：%s", w.Code, w.Body.String())
	}
}

// RecoverInterrupted 的语义：run 标 interrupted（不是 failed），且错误文案点名可续跑。
// 这条是「被杀一次就白跑」的根因修复，必须钉住。
func TestRecoverInterrupted_RunBecomesInterrupted(t *testing.T) {
	db := newTestDB(t)
	q := service.NewJobQueue(db, 1)
	defer q.Close()

	run := model.WorkflowRun{Status: "running"}
	db.Create(&run)
	job := model.Job{RunID: run.ID, Status: "running"}
	db.Create(&job)

	n := q.RecoverInterrupted()
	if n < 2 {
		t.Errorf("应回收 run+job 至少 2 条，实际 %d", n)
	}

	var got model.WorkflowRun
	db.First(&got, run.ID)
	if got.Status != "interrupted" {
		t.Errorf("run 应标 interrupted（可续跑），实际 %q", got.Status)
	}
	if !containsStr(got.Error, "resume") {
		t.Errorf("错误文案应点名 resume 入口，实际：%q", got.Error)
	}
}
