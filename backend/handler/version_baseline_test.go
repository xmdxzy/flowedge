package handler

import (
	"encoding/json"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"flowedge/backend/model"
)

// run 的「版本号」是自进化配对评测的支柱：
// 没有它，改前改后只能靠评测层自己标的 role 区分，而 role 可以标错、标错了也没人发现。
//
// 2026-10-23 实测发现：新建的图跑完，证据里 `workflowVersion` 字段直接消失 ——
// 因为 POST /api/workflows 不建初始版本，latestVersion() 返回 0，而 0 被 omitempty 吞掉。
// 修法：起跑时若这张图从没被快照过，先立一个基线。

func newRunFixture(t *testing.T) (*gorm.DB, *WorkflowHandler, model.Workflow) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	// DSN 唯一：共用内存库在 -count>1 时会互相污染
	dsn := "file:newrun_" + t.Name() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Workflow{}, &model.WorkflowVersion{}, &model.WorkflowRun{}); err != nil {
		t.Fatal(err)
	}
	nodes, _ := json.Marshal([]map[string]interface{}{
		{"id": "s", "type": "start", "data": map[string]interface{}{"prompt": "x"}},
	})
	edges, _ := json.Marshal([]map[string]interface{}{})
	wf := model.Workflow{Name: "基线测试", Nodes: nodes, Edges: edges}
	if err := db.Create(&wf).Error; err != nil {
		t.Fatal(err)
	}
	return db, NewWorkflowHandler(db, nil, nil), wf
}

// 从没被快照过的图 → 起跑时自动立基线，版本号是 1 而不是 0
func TestNewRun_CreatesBaselineWhenNeverSnapshotted(t *testing.T) {
	db, h, wf := newRunFixture(t)

	run := h.newRun(wf.ID, "running", RunOptions{InvokedBy: "manual"})
	if run.WorkflowVersion != 1 {
		t.Fatalf("应从 0 补到 1，实际 %d（0 会在证据里被 omitempty 吞掉）", run.WorkflowVersion)
	}

	var vers []model.WorkflowVersion
	db.Where("workflow_id = ?", wf.ID).Find(&vers)
	if len(vers) != 1 {
		t.Fatalf("应建 1 条基线版本，实际 %d", len(vers))
	}
	if vers[0].Version != 1 {
		t.Fatalf("基线版本号应为 1，实际 %d", vers[0].Version)
	}
	// 说明要写清"为什么会有这一版"，否则人看到 v1 会以为是手存的
	if vers[0].ChangeNote == "" {
		t.Fatal("基线版本要有 change note，说明它是起跑时自动建的")
	}
	// 快照内容必须就是这张图（回滚要能回到跑之前的样子）
	if string(vers[0].Nodes) != string(wf.Nodes) {
		t.Fatalf("基线快照的 nodes 应与图当前内容一致：%s != %s", vers[0].Nodes, wf.Nodes)
	}
}

// 第二次跑不该再建版本（否则每跑一次就多一版，版本历史会被 run 淹掉）
func TestNewRun_DoesNotDuplicateBaseline(t *testing.T) {
	db, h, wf := newRunFixture(t)

	_ = h.newRun(wf.ID, "running", RunOptions{})
	r2 := h.newRun(wf.ID, "running", RunOptions{})

	if r2.WorkflowVersion != 1 {
		t.Fatalf("第二次跑仍是 v1，实际 %d", r2.WorkflowVersion)
	}
	var n int64
	db.Model(&model.WorkflowVersion{}).Where("workflow_id = ?", wf.ID).Count(&n)
	if n != 1 {
		t.Fatalf("版本数应保持 1，实际 %d", n)
	}
}

// 已有更新的版本时用最新版，不另建
func TestNewRun_UsesLatestExistingVersion(t *testing.T) {
	db, h, wf := newRunFixture(t)
	db.Create(&model.WorkflowVersion{WorkflowID: wf.ID, Version: 1, Name: wf.Name, Nodes: wf.Nodes, Edges: wf.Edges})
	db.Create(&model.WorkflowVersion{WorkflowID: wf.ID, Version: 2, Name: wf.Name, Nodes: wf.Nodes, Edges: wf.Edges})

	run := h.newRun(wf.ID, "running", RunOptions{})
	if run.WorkflowVersion != 2 {
		t.Fatalf("应取最新版 2，实际 %d", run.WorkflowVersion)
	}
	var n int64
	db.Model(&model.WorkflowVersion{}).Where("workflow_id = ?", wf.ID).Count(&n)
	if n != 2 {
		t.Fatalf("不该多建版本，实际 %d 条", n)
	}
}

// 显式指定版本（评测跑 baseline 的历史图）→ 原样采用，且不建新版本
func TestNewRun_RespectsExplicitVersion(t *testing.T) {
	db, h, wf := newRunFixture(t)
	db.Create(&model.WorkflowVersion{WorkflowID: wf.ID, Version: 1, Name: wf.Name, Nodes: wf.Nodes, Edges: wf.Edges})

	run := h.newRun(wf.ID, "running", RunOptions{WorkflowVersion: 1})
	if run.WorkflowVersion != 1 {
		t.Fatalf("显式指定的版本要原样用，实际 %d", run.WorkflowVersion)
	}
	var n int64
	db.Model(&model.WorkflowVersion{}).Where("workflow_id = ?", wf.ID).Count(&n)
	if n != 1 {
		t.Fatalf("显式指定版本时不该再建快照，实际 %d 条", n)
	}
}
