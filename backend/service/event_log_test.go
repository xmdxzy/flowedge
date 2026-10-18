package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"flowedge/backend/model"
)

func newTestEventLogger(t *testing.T) (*EventLogger, *gorm.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite failed: %v", err)
	}
	if err := db.AutoMigrate(&model.RunEvent{}); err != nil {
		t.Fatalf("migrate failed: %v", err)
	}
	l := NewEventLogger(db)
	// 先停日志协程再关库句柄：否则 Windows 上临时目录清理会因句柄未释放失败
	t.Cleanup(func() {
		l.Close()
		if sqlDB, err := db.DB(); err == nil {
			sqlDB.Close()
		}
	})
	return l, db
}

// TestEventLogger_PersistsAndSequences 事件必须落库且序号递增（回放顺序）
func TestEventLogger_PersistsAndSequences(t *testing.T) {
	l, db := newTestEventLogger(t)

	l.Emit(7, Event{Type: "node_started", NodeID: "a"})
	l.Emit(7, Event{Type: "node_finished", NodeID: "a", Data: map[string]interface{}{"output": map[string]interface{}{"text": "hi"}}})
	l.Emit(7, Event{Type: "node_failed", NodeID: "b", Message: "boom"})
	l.Flush()

	var rows []model.RunEvent
	if err := db.Order("seq ASC").Find(&rows).Error; err != nil {
		t.Fatalf("query failed: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 persisted events, got %d", len(rows))
	}
	for i, want := range []string{"node_started", "node_finished", "node_failed"} {
		if rows[i].Type != want {
			t.Fatalf("event %d: got %q want %q", i, rows[i].Type, want)
		}
		if rows[i].Seq != i+1 {
			t.Fatalf("event %d: seq got %d want %d", i, rows[i].Seq, i+1)
		}
		if rows[i].RunID != 7 {
			t.Fatalf("event %d: run_id got %d", i, rows[i].RunID)
		}
	}
}

// TestEventLogger_SkipsNodeToken 高频 token 事件不得落库
func TestEventLogger_SkipsNodeToken(t *testing.T) {
	l, db := newTestEventLogger(t)

	l.Emit(7, Event{Type: "node_token", NodeID: "a", Data: map[string]interface{}{"delta": "你"}})
	l.Emit(7, Event{Type: "node_token", NodeID: "a", Data: map[string]interface{}{"delta": "好"}})
	l.Emit(7, Event{Type: "node_started", NodeID: "a"})
	l.Flush()

	var count int64
	db.Model(&model.RunEvent{}).Count(&count)
	if count != 1 {
		t.Fatalf("node_token must not be persisted, got %d rows", count)
	}
}

// TestEventLogger_RunScopedSequence 不同 run 的序号各自独立，ListByRun 只返回本 run
func TestEventLogger_RunScopedSequence(t *testing.T) {
	l, _ := newTestEventLogger(t)

	l.Emit(1, Event{Type: "node_started", NodeID: "a"})
	l.Emit(1, Event{Type: "node_started", NodeID: "b"})
	l.Emit(2, Event{Type: "node_started", NodeID: "a"})
	l.Emit(0, Event{Type: "node_started", NodeID: "x"}) // runID=0：独立执行，不入库
	l.Flush()

	run1, err := l.ListByRun(1, 100)
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(run1) != 2 {
		t.Fatalf("run 1 should have 2 events, got %d", len(run1))
	}
	if run1[0].Seq != 1 || run1[1].Seq != 2 {
		t.Fatalf("run 1 seq should restart at 1, got %d,%d", run1[0].Seq, run1[1].Seq)
	}

	run2, err := l.ListByRun(2, 100)
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(run2) != 1 || run2[0].Seq != 1 {
		t.Fatalf("run 2 should have its own seq=1, got %d rows", len(run2))
	}
}

// TestUsageOfEvents_Accumulates token 用量必须从 node_finished 的 _usage 累加
func TestUsageOfEvents_Accumulates(t *testing.T) {
	events := []model.RunEvent{
		{Type: "node_finished", Data: []byte(`{"output":{"text":"a","_usage":{"prompt_tokens":10,"completion_tokens":20}}}`)},
		{Type: "node_finished", Data: []byte(`{"output":{"text":"b","_usage":{"prompt_tokens":5,"completion_tokens":7}}}`)},
		{Type: "node_started"},              // 不计入
		{Type: "node_finished", Data: nil},  // 无 usage
		{Type: "node_finished", Data: []byte(`not-json`)}, // 坏数据不得 panic
	}

	u := UsageOfEvents(events)
	if u.PromptTokens != 15 || u.CompletionTokens != 27 {
		t.Fatalf("usage mismatch: got %+v want {15 27}", u)
	}
}

// TestRunIDFromContext runID 通过 context 透传
func TestRunIDFromContext(t *testing.T) {
	ctx := withRunID(context.Background(), 42)
	if got := runIDFrom(ctx); got != 42 {
		t.Fatalf("got %d want 42", got)
	}
	if got := runIDFrom(context.Background()); got != 0 {
		t.Fatalf("missing runID should be 0, got %d", got)
	}
	if got := runIDFrom(nil); got != 0 {
		t.Fatalf("nil ctx should be 0, got %d", got)
	}
}
