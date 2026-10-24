package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newTestSupervisor 造一个空规格表（测试直接塞 spec，绕开真配置）。
func newTestSupervisor() *Supervisor {
	return &Supervisor{
		specs:   map[string]*DepSpec{},
		procs:   map[string]*exec.Cmd{},
		started: map[string]bool{},
		lastErr: map[string]string{},
	}
}

// 回归：`ollama serve` 的 serve 是**子命令**不是脚本文件。
// 旧 Startable 一律 os.Stat(Args[0])，于是判成"脚本不存在"、进程根本不派生；
// 而 handler 又把错误丢了（`_ = err`），用户点「一键修复」只看到 202、什么也没发生。
func TestStartableAcceptsSubcommandArgs(t *testing.T) {
	dir := t.TempDir()
	fakeBin := filepath.Join(dir, "fake-service.exe")
	if err := os.WriteFile(fakeBin, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestSupervisor()
	s.specs["svc"] = &DepSpec{Name: "svc", Binary: fakeBin, Args: []string{"serve"}}
	if !s.Startable("svc") {
		t.Fatalf("子命令参数不该被当成脚本路径：%s", s.unstartableReason("svc"))
	}
}

// 脚本类依赖仍然要检查脚本是否存在（别把真缺文件也放过去）。
func TestStartableRejectsMissingScript(t *testing.T) {
	dir := t.TempDir()
	fakePython := filepath.Join(dir, "python.exe")
	if err := os.WriteFile(fakePython, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestSupervisor()
	s.specs["svc"] = &DepSpec{Name: "svc", Binary: fakePython, Args: []string{"missing_server.py"}}
	if s.Startable("svc") {
		t.Fatal("脚本不存在时必须判为不可启动")
	}
	if reason := s.unstartableReason("svc"); !strings.Contains(reason, "脚本") {
		t.Errorf("原因要指明缺脚本，实际：%s", reason)
	}
}

// 相对路径的脚本要按 WorkDir 解析（引擎 chdir 后 CWD 不等于脚本目录）。
func TestStartableResolvesScriptInWorkDir(t *testing.T) {
	dir := t.TempDir()
	fakePython := filepath.Join(dir, "python.exe")
	if err := os.WriteFile(fakePython, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "server.py"), []byte("print(1)"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestSupervisor()
	s.specs["svc"] = &DepSpec{Name: "svc", Binary: fakePython, Args: []string{"server.py"}, WorkDir: dir}
	if !s.Startable("svc") {
		t.Fatalf("WorkDir 下的脚本应可启动：%s", s.unstartableReason("svc"))
	}
}

// 不可启动时必须留下 last_error，否则「点了没反应」无处可查。
func TestStartRecordsLastErrorWhenUnstartable(t *testing.T) {
	s := newTestSupervisor()
	s.specs["svc"] = &DepSpec{Name: "svc", Binary: filepath.Join(t.TempDir(), "nope.exe"), Args: []string{"serve"}}

	if err := s.Start("svc"); err == nil {
		t.Fatal("不可启动时 Start 必须报错")
	}
	s.mu.Lock()
	last := s.lastErr["svc"]
	s.mu.Unlock()
	if last == "" {
		t.Fatal("last_error 为空：界面与 /api/deps 都无法解释失败原因")
	}
	if !strings.Contains(last, "找不到可执行文件") {
		t.Errorf("原因要说清缺什么，实际：%s", last)
	}
}
