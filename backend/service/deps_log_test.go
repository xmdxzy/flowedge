package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// shellCmd 造一条「同时往 stdout 与 stderr 各写一行」的命令。
// 用 shell 而不是写临时 exe：要验的是「两个流都被接住」，不是进程本身。
func shellCmd() *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd", "/c", "echo out-line & echo err-line 1>&2")
	}
	return exec.Command("sh", "-c", "echo out-line; echo err-line 1>&2")
}

// 托管进程的 stdout 与 stderr 都必须落到 logs/<name>.log。
//
// 这条不是为了「有日志」，而是为了 §0.6：没有依赖自身日志时，
// 「服务没起」与「跑了但结果为空」在被观测上完全一样 —— H3 到底有没有
// 加载权重、ASR 是不是根本连不上，都只能靠反复花 GPU 去试。
func TestAttachDepLog_CapturesBothStreams(t *testing.T) {
	dir := t.TempDir()
	s := NewSupervisor("", "", "")
	s.SetLogDir(dir)

	cmd := shellCmd()
	// attachDepLog 现在只接线、不登记（登记在 Start 的锁内做，避免自锁），
	// 所以测试要自己关句柄，否则 Windows 上临时目录删不掉。
	f := s.attachDepLog(cmd, "fake")
	if err := cmd.Run(); err != nil {
		t.Fatalf("子进程运行失败：%v", err)
	}
	if f != nil {
		_ = f.Close()
	}

	raw, err := os.ReadFile(filepath.Join(dir, "fake.log"))
	if err != nil {
		t.Fatalf("日志文件没生成：%v", err)
	}
	text := string(raw)
	for _, want := range []string{"out-line", "err-line", "=== fake 启动"} {
		if !strings.Contains(text, want) {
			t.Fatalf("日志里缺少 %q，实际内容：\n%s", want, text)
		}
	}
}

// 超过 8MB 滚一份 .1：与 engine.log 同一把尺子，免得依赖日志无限长大。
func TestAttachDepLog_RotatesAt8MB(t *testing.T) {
	dir := t.TempDir()
	s := NewSupervisor("", "", "")
	s.SetLogDir(dir)

	big := filepath.Join(dir, "fake.log")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", 9<<20)), 0o644); err != nil {
		t.Fatalf("造旧日志失败：%v", err)
	}

	cmd := shellCmd()
	f := s.attachDepLog(cmd, "fake")
	if err := cmd.Run(); err != nil {
		t.Fatalf("子进程运行失败：%v", err)
	}
	if f != nil {
		_ = f.Close()
	}

	if st, err := os.Stat(big + ".1"); err != nil || st.Size() < 8<<20 {
		t.Fatalf("旧日志应被滚到 .1 且保留原大小，实际 err=%v", err)
	}
	st, err := os.Stat(big)
	if err != nil {
		t.Fatalf("新日志应重新创建：%v", err)
	}
	if st.Size() > 1<<20 {
		t.Fatalf("新日志不该继承旧内容，实际 %d 字节", st.Size())
	}
}

// 没配日志目录时保持改造前行为：输出丢弃，不 panic、不建文件。
func TestAttachDepLog_NoDirIsGraceful(t *testing.T) {
	s := NewSupervisor("", "", "")
	cmd := shellCmd()
	if f := s.attachDepLog(cmd, "fake"); f != nil {
		_ = f.Close()
		t.Fatal("未配置日志目录时不该返回句柄")
	}
	if cmd.Stdout != nil || cmd.Stderr != nil {
		t.Fatal("未配置日志目录时不该接管子进程输出")
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("未接管输出时子进程仍应正常跑完：%v", err)
	}
}

// Stop 之后再 Stop / closeDepLog 不能重复关句柄出错。
func TestCloseDepLog_Idempotent(t *testing.T) {
	dir := t.TempDir()
	s := NewSupervisor("", "", "")
	s.SetLogDir(dir)
	cmd := shellCmd()
	f := s.attachDepLog(cmd, "fake")
	if f != nil {
		s.mu.Lock()
		s.procLogs["fake"] = append(s.procLogs["fake"], f)
		s.mu.Unlock()
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("子进程运行失败：%v", err)
	}
	s.closeDepLog("fake")
	s.closeDepLog("fake") // 第二次应为空操作
}
