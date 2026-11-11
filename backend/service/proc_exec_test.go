package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoBareExecCommand 钉住一条桌面端体验契约：backend 生产代码不许裸 exec.Command。
//
// 原因（2026-11-10 真实弹窗）：引擎被 detached 拉起时父进程没有控制台，
// 裸 exec.Command 起 ffmpeg/ffprobe/node/nvidia-smi 会让 Windows 新分配一个
// 黑窗（标题 = exe 全路径，例 `...\WinGet\Links\ffmpeg.exe`），长任务期间一直挂着。
// 必须走 hiddenExec / hiddenExecContext（proc_exec.go），它设了 CREATE_NO_WINDOW。
func TestNoBareExecCommand(t *testing.T) {
	root := ".."
	// proc_exec.go：唯一允许出现裸 exec.Command 的地方（包装器本体）
	// main.go：openBrowser 起 rundll32（GUI 子系统，不分配控制台），且 main 是独立 package
	allowed := map[string]bool{"proc_exec.go": true, "main.go": true}

	var offenders []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == "web" || name == "node_modules" || name == "dist" || (strings.HasPrefix(name, ".") && name != ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		if allowed[filepath.Base(p)] {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		s := string(b)
		for _, pat := range []string{"exec.Command(", "exec.CommandContext("} {
			if i := strings.Index(s, pat); i >= 0 {
				line := 1 + strings.Count(s[:i], "\n")
				offenders = append(offenders, fmt.Sprintf("%s:%d %s", p, line, pat))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历源码失败：%v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("生产代码出现裸 exec.Command（会弹控制台黑窗），改用 hiddenExec/hiddenExecContext：\n  %s",
			strings.Join(offenders, "\n  "))
	}
}
