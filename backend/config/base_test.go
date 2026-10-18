package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBaseDir_NotCwd 编译后的 exe 必须以其所在目录为工作根，
// 即使该目录位于系统临时目录下（曾因用「路径含 Temp」判断 go run 而误伤）
func TestBaseDir_NotCwd(t *testing.T) {
	dir := BaseDir()
	exe, err := os.Executable()
	if err != nil {
		t.Skip("no executable path")
	}
	want := filepath.Dir(exe)
	if strings.Contains(want, "go-build") {
		t.Skip("running under go run; BaseDir intentionally falls back to CWD")
	}
	if filepath.Clean(dir) != filepath.Clean(want) {
		t.Fatalf("BaseDir = %q, want exe dir %q", dir, want)
	}
}

// TestResolve 相对路径拼到 BaseDir，绝对路径原样返回
func TestResolve(t *testing.T) {
	rel := Resolve("data/x.db")
	if !filepath.IsAbs(rel) {
		t.Fatalf("resolved path should be absolute: %q", rel)
	}
	if !strings.HasSuffix(filepath.Clean(rel), filepath.Join("data", "x.db")) {
		t.Fatalf("unexpected resolve result: %q", rel)
	}
	abs := filepath.Join(BaseDir(), "keep", "me")
	if got := Resolve(abs); got != abs {
		t.Fatalf("absolute path must be preserved: got %q want %q", got, abs)
	}
	if got := Resolve(""); got != BaseDir() {
		t.Fatalf("empty path should resolve to BaseDir, got %q", got)
	}
}
