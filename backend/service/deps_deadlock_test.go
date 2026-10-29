package service

import (
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// 回归：Start 的 spawn 路径在持锁状态下接线日志，所以那条路径必须**无锁**。
//
// 2026-10-26 现场：Start 在 spawn 路径上持有 s.mu，而 attachDepLog 内部
// depLogPath() 也 `s.mu.Lock()`，Go 的互斥锁不可重入 ⇒ 永久死锁。症状极具误导性：
//   - /api/deps 全部挂住（20s+ 不返回）
//   - /api/deps/comfy_h3/start 返回 202，但再也不派生任何进程
//   - /api/health、 /api/runs 却完全正常（它们不碰这把锁）
// 结论：**"服务起不来"要先看是不是锁**，不要往模型/权重上找。
func TestDepLogHandleAt_DoesNotTakeSupervisorLock(t *testing.T) {
	s := NewSupervisor("", "", "")
	dir := t.TempDir()
	path := filepath.Join(dir, "probe.log") // Start 在锁外先算好 path
	s.mu.Lock()                              // 模拟 Start 的持锁状态
	done := make(chan struct{})
	go func() {
		f := depLogHandleAt(exec.Command("cmd", "/c", "echo", "x"), "probe", path)
		if f != nil {
			_ = f.Close()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		s.mu.Unlock()
		t.Fatal("持锁状态下接线日志卡住了 → Start 会永久死锁")
	}
	s.mu.Unlock()
}

// 没配日志目录时也必须能安全调用（不能因为 nil 句柄把调用方搞崩）。
func TestAttachDepLog_NoLogDir(t *testing.T) {
	s := NewSupervisor("", "", "")
	if f := s.attachDepLog(exec.Command("cmd", "/c", "echo", "x"), "probe"); f != nil {
		_ = f.Close()
		t.Fatal("没配 logDir 时不该返回句柄")
	}
}
