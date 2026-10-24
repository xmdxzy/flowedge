package service

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"flowedge/backend/model"
)

// RunService 的续跑状态机：这几条以前埋在 handler 里，只能靠 httptest 间接覆盖，
// 而单镜重跑（shots.go）又复用了同一段逻辑——两处入口、一份状态机，最容易写歪。

func newRunTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("内存库打不开：%v", err)
	}
	if err := db.AutoMigrate(&model.Workflow{}, &model.WorkflowRun{}, &model.Job{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func seedRun(t *testing.T, db *gorm.DB, status string) (model.Workflow, model.WorkflowRun) {
	t.Helper()
	wf := model.Workflow{
		Name:  "t",
		Nodes: []byte(`[{"id":"s","type":"start","data":{"prompt":"hi"}},{"id":"e","type":"end","data":{}}]`),
		Edges: []byte(`[{"id":"1","source":"s","target":"e"}]`),
	}
	if err := db.Create(&wf).Error; err != nil {
		t.Fatal(err)
	}
	run := model.WorkflowRun{
		WorkflowID: wf.ID,
		Status:     status,
		NodeStates: []byte(`[{"id":"s","type":"start","status":"success","output":{"text":"hi"}},` +
			`{"id":"e","type":"end","status":"failed","error":"boom"}]`),
	}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	return wf, run
}

func newTestRunService(db *gorm.DB) *RunService {
	eng := NewEngineV2(nil, nil, nil, nil, nil, nil, nil)
	s := NewRunService(db, eng, nil)
	s.SetSynchronous() // 测试里不等 goroutine，否则只能 sleep
	return s
}

func TestResumeRejectsUnknownRun(t *testing.T) {
	db := newRunTestDB(t)
	s := newTestRunService(db)
	_, err := s.Resume(9999, "", nil)
	if !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("应返回 ErrRunNotFound，实际 %v", err)
	}
}

func TestResumeRejectsRunningRun(t *testing.T) {
	db := newRunTestDB(t)
	_, run := seedRun(t, db, "running")
	s := newTestRunService(db)
	if _, err := s.Resume(run.ID, "", nil); !errors.Is(err, ErrRunStillRunning) {
		t.Fatalf("运行中的任务不该允许续跑（会两个执行体改同一份状态），实际 %v", err)
	}
}

func TestResumeRejectsUnknownFromNode(t *testing.T) {
	db := newRunTestDB(t)
	_, run := seedRun(t, db, "failed")
	s := newTestRunService(db)
	_, err := s.Resume(run.ID, "no-such-node", nil)
	var nf *NodeNotFoundError
	if !errors.As(err, &nf) || nf.NodeID != "no-such-node" {
		t.Fatalf("拼错的 node id 必须显式失败，实际 %v", err)
	}
}

func TestResumeSucceedsAndPersistsStates(t *testing.T) {
	db := newRunTestDB(t)
	_, run := seedRun(t, db, "failed")
	s := newTestRunService(db)

	out, err := s.Resume(run.ID, "", nil)
	if err != nil {
		t.Fatalf("续跑失败：%v", err)
	}
	if out.PrevNodes != 2 {
		t.Fatalf("应复用 2 个已完成节点，实际 %d", out.PrevNodes)
	}

	var got model.WorkflowRun
	if err := db.First(&got, run.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != "success" {
		t.Fatalf("续跑后状态应为 success，实际 %s（error=%s）", got.Status, got.Error)
	}
	if got.EndedAt == nil {
		t.Fatal("结束后必须写 EndedAt，否则执行面板会显示成「永远在跑」")
	}
	if len(got.NodeStates) == 0 {
		t.Fatal("节点状态没回写")
	}

	var jobs []model.Job
	db.Find(&jobs)
	if len(jobs) != 2 {
		t.Fatalf("应留下 2 条 job（running + done），实际 %d", len(jobs))
	}
	var done int
	for _, j := range jobs {
		if j.Status == "done" && j.Trigger == "resume" {
			done++
		}
	}
	if done != 1 {
		t.Fatalf("应有一条 trigger=resume 的 done job，实际 %d", done)
	}
}

func TestResumeWithOverrideDoesNotMutateWorkflow(t *testing.T) {
	db := newRunTestDB(t)
	wf, run := seedRun(t, db, "failed")
	s := newTestRunService(db)

	baseNodes := string(wf.Nodes)
	override := []byte(`[{"id":"s","type":"start","data":{"prompt":"改过的"}},` +
		`{"id":"e","type":"end","data":{}}]`)
	if _, err := s.Resume(run.ID, "", override); err != nil {
		t.Fatalf("带覆盖的续跑失败：%v", err)
	}

	var after model.Workflow
	if err := db.First(&after, wf.ID).Error; err != nil {
		t.Fatal(err)
	}
	if string(after.Nodes) != baseNodes {
		t.Fatalf("单镜重跑的临时改图不许写回工作流，否则用户保存的图被永久改掉：\n%s", after.Nodes)
	}
}
