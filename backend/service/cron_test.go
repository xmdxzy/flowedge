package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"flowedge/backend/model"
)

// memDB 每个用例一份独立的内存库。
//
// 之前用的是 `file::memory:?cache=shared` —— 这个名字在整个进程内共享，
// 于是同一次运行里所有测试（以及 -count>1 的重跑）都在同一张库上互相看见：
// 别人的工作流会被本用例的调度器扫到，fired 计数凭空多出几次。
// 用用例名做 DSN 让每份测试互不干扰，重复跑结果才稳。
func memDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s-%d?mode=memory&cache=shared", t.Name(), time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	return db
}

func TestCronParseAndMatch(t *testing.T) {
	base := time.Date(2026, 10, 18, 20, 5, 0, 0, time.Local) // 周日 20:05

	cases := []struct {
		expr string
		at   time.Time
		want bool
	}{
		{"*/5 * * * *", base, true},
		{"*/5 * * * *", base.Add(2 * time.Minute), false},
		{"5 20 * * *", base, true},
		{"5 20 * * *", base.Add(time.Hour), false},
		{"5 20 * * 0", base, true},       // 周日
		{"5 20 * * 1-5", base, false},    // 工作日
		{"*/10,15 * * * *", base, false}, // 5 不在 0,10,15 里
		{"0-10/5 20 * * *", base, true},  // 0/5/10
	}
	for _, c := range cases {
		spec, err := ParseCron(c.expr)
		if err != nil {
			t.Fatalf("parse %q: %v", c.expr, err)
		}
		if got := spec.Match(c.at); got != c.want {
			t.Errorf("%q @ %s = %v, want %v", c.expr, c.at.Format("15:04"), got, c.want)
		}
	}

	// 非法表达式必须被拒
	for _, bad := range []string{"", "* * *", "* * * * * *", "60 * * * *", "* 24 * * *", "*/0 * * * *"} {
		if _, err := ParseCron(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}

	// 人话描述
	if d, _ := ValidateCron("*/5 * * * *"); d != "每 5 分钟" {
		t.Errorf("describe = %q", d)
	}
	if d, _ := ValidateCron("30 9 * * *"); d != "每天 09:30" {
		t.Errorf("describe daily = %q", d)
	}
}

// 调度器：同一分钟内多次 Tick 只触发一次；cron 不命中则不触发
func TestCronSchedulerFiresOncePerMinute(t *testing.T) {
	db := memDB(t)
	if err := db.AutoMigrate(&model.Workflow{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	wf := model.Workflow{
		Name: "定时测试", Nodes: []byte("[]"), Edges: []byte("[]"),
		TriggerEnabled: true, CronExpr: "* * * * *",
	}
	db.Create(&wf)

	var fired int
	sched := NewCronScheduler(db, func(id uint) error {
		fired++
		return nil
	})

	now := time.Date(2026, 10, 18, 20, 30, 10, 0, time.Local)
	sched.Tick(now)
	sched.Tick(now.Add(5 * time.Second)) // 同一分钟
	if fired != 1 {
		t.Errorf("fired %d times within one minute, want 1 (dedup by minute)", fired)
	}

	sched.Tick(now.Add(time.Minute))
	if fired != 2 {
		t.Errorf("fired %d times after next minute, want 2", fired)
	}

	// 关闭后不再触发
	db.Model(&model.Workflow{}).Where("id = ?", wf.ID).Update("trigger_enabled", false)
	sched.Tick(now.Add(2 * time.Minute))
	if fired != 2 {
		t.Errorf("disabled workflow still fired (total %d)", fired)
	}

	// 概览能列出已启用的计划
	db.Model(&model.Workflow{}).Where("id = ?", wf.ID).Update("trigger_enabled", true)
	if summary := sched.CronSummary(); len(summary) == 0 {
		t.Error("CronSummary should list enabled schedule")
	}
}
