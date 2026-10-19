package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"flowedge/backend/model"
)

// 镜头级作业的回归测试（包 B）。
//
// 这些断言锁住的是三条曾经真实发生过的事故：
//  1. 第 5 镜失败 → 前 4 镜成果一起丢（现在靠 Record 落盘 + Lookup 复用）
//  2. 12 镜长片被 30 分钟整节点上限杀掉（现在按帧预算）
//  3. 复用判据太宽 → 改了提示词却拿到旧画面（现在哈希含提示词与关键参数）

func testShotDB(t *testing.T) *ShotJobStore {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.ShotJob{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewShotJobStore(db)
}

// 复用判据必须「同参同哈希、异参异哈希」——否则会出现「改了提示词拿到旧画面」。
func TestShotPromptHashSensitiveToInputs(t *testing.T) {
	base := shotPromptHash("a monk draws his sword", 124, 608, 352, 12, true)
	cases := []struct {
		name string
		got  string
	}{
		{"same", shotPromptHash("a monk draws his sword", 124, 608, 352, 12, true)},
		{"prompt", shotPromptHash("a monk sheathes his sword", 124, 608, 352, 12, true)},
		{"frames", shotPromptHash("a monk draws his sword", 141, 608, 352, 12, true)},
		{"width", shotPromptHash("a monk draws his sword", 124, 736, 352, 12, true)},
		{"steps", shotPromptHash("a monk draws his sword", 124, 608, 352, 20, true)},
		{"turbo", shotPromptHash("a monk draws his sword", 124, 608, 352, 12, false)},
	}
	if cases[0].got != base {
		t.Fatalf("same params must hash equally")
	}
	for _, c := range cases[1:] {
		if c.got == base {
			t.Errorf("%s: hash must differ from base (got %s)", c.name, c.got)
		}
	}
}

func TestParseReroll(t *testing.T) {
	mk := func(v interface{}) *model.NodeInstance {
		return &model.NodeInstance{Data: map[string]interface{}{"reroll": v}}
	}
	cases := []struct {
		in   *model.NodeInstance
		want []int
	}{
		{nil, nil},
		{mk(""), nil},
		{mk("3"), []int{2}},
		{mk(3.0), []int{2}},
		{mk("1,3"), []int{0, 2}},
		{mk("1-3"), []int{0, 1, 2}},
		{mk(" 2 , 4 "), []int{1, 3}},
	}
	for i, c := range cases {
		got := parseReroll(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("case %d: want %v, got %v", i, c.want, got)
		}
		for _, w := range c.want {
			if !got[w] {
				t.Fatalf("case %d: expected index %d in %v", i, w, got)
			}
		}
	}
}

// 12 镜长片的预算必须显著超过旧的 30 分钟封顶——这正是「必然被超时杀掉」的回归点。
func TestShotListBudgetBeatsOldCap(t *testing.T) {
	single := ShotBudget(124)
	if single < 2*time.Minute || single > 6*time.Minute {
		t.Fatalf("single shot budget out of plausible range: %v", single)
	}
	if ShotBudget(362) <= ShotBudget(124) {
		t.Fatalf("budget must scale with frames")
	}
	// 12 镜、每镜 300 帧（§5.10 的交锋段量级）
	twelveShots := ShotListBudget(12, 12*300)
	if twelveShots <= maxNodeTimeout {
		t.Fatalf("12-shot budget %v must exceed the legacy %v cap", twelveShots, maxNodeTimeout)
	}
	if twelveShots > maxShotlistTimeout {
		t.Fatalf("budget %v should stay under the shotlist ceiling %v", twelveShots, maxShotlistTimeout)
	}
}

// nil 仓库必须完全可用（单测与「独立执行节点」路径没有数据库）。
func TestShotJobStoreNilSafe(t *testing.T) {
	var s *ShotJobStore
	if _, ok := s.Lookup(1, "shotlist-1", 0, "h"); ok {
		t.Fatal("nil store must not report reusable job")
	}
	s.Record(model.ShotJob{RunID: 1, NodeID: "n", ShotIndex: 0}) // 不能 panic
	if got := s.ListByNode(1, "n"); got != nil {
		t.Fatal("nil store must return nil list")
	}
	if NewShotJobStore(nil) != nil {
		t.Fatal("nil db must yield nil store")
	}
}

// 核心闭环：成功落盘 → 可复用；失败不留可复用记录；产物被删后不再复用。
func TestShotJobRecordAndReuse(t *testing.T) {
	store := testShotDB(t)
	dir := t.TempDir()
	clip := filepath.Join(dir, "shot1.mp4")
	if err := os.WriteFile(clip, []byte("fake-mp4"), 0o644); err != nil {
		t.Fatal(err)
	}

	hash := shotPromptHash("p", 124, 608, 352, 12, true)

	// 先记一次失败：不可复用
	store.Record(model.ShotJob{
		RunID: 7, NodeID: "shotlist-1", ShotIndex: 0, PromptHash: hash,
		ShotID: 1, Status: "failed", Error: "CUDA out of memory",
	})
	if _, ok := store.Lookup(7, "shotlist-1", 0, hash); ok {
		t.Fatal("failed job must not be reusable")
	}

	// 再记成功：可复用，文件仍在
	store.Record(model.ShotJob{
		RunID: 7, NodeID: "shotlist-1", ShotIndex: 0, PromptHash: hash,
		ShotID: 1, File: clip, Frames: 124, Seconds: 124.0 / 24.0,
		Transition: "cut", Status: "success",
	})
	job, ok := store.Lookup(7, "shotlist-1", 0, hash)
	if !ok {
		t.Fatal("successful job must be reusable")
	}
	if job.File != clip || job.Frames != 124 || job.Transition != "cut" {
		t.Fatalf("reused job mismatch: %+v", job)
	}

	// 同 run 同节点同镜号再记一次 → 覆盖而不是新增（否则列表会越滚越长）
	store.Record(model.ShotJob{
		RunID: 7, NodeID: "shotlist-1", ShotIndex: 0, PromptHash: hash,
		ShotID: 1, File: clip, Frames: 124, Status: "success", Note: "第二次",
	})
	if n := len(store.ListByNode(7, "shotlist-1")); n != 1 {
		t.Fatalf("expected 1 row per (run,node,index), got %d", n)
	}

	// 提示词变了 → 哈希变 → 不复用（必须重生成）
	if _, ok := store.Lookup(7, "shotlist-1", 0, shotPromptHash("p2", 124, 608, 352, 12, true)); ok {
		t.Fatal("changed prompt must invalidate reuse")
	}

	// 产物被清理 → 不复用（否则会装配出空片段）
	if err := os.Remove(clip); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Lookup(7, "shotlist-1", 0, hash); ok {
		t.Fatal("missing artifact must invalidate reuse")
	}

	// 独立执行（runID=0）不落盘
	store.Record(model.ShotJob{RunID: 0, NodeID: "shotlist-1", ShotIndex: 0, Status: "success"})
	if got := len(store.ListByNode(0, "shotlist-1")); got != 0 {
		t.Fatalf("runID=0 must not persist, got %d rows", got)
	}
}
