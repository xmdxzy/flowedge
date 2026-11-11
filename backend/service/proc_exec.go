package service

import (
	"context"
	"os/exec"
)

// hiddenExec 等价 exec.Command，但保证子进程不弹控制台黑窗。
//
// 为什么必须有这层：引擎（FlowEdge.exe 是控制台子系统程序）被 detached 拉起时
// （序策工作站 / 登录自启 / 桌面端 spawn，父进程都没有控制台），
// 任何用裸 exec.Command 起的控制台程序（ffmpeg/ffprobe/node/nvidia-smi/powershell）
// 都会让 Windows 给它**新分配一个 conhost 窗口**——标题就是 exe 全路径，
// 内容全黑，长任务期间一直挂着。2026-11-10 的真实弹窗：
// `<用户目录>\AppData\Local\Microsoft\WinGet\Links\ffmpeg.exe`。
// CREATE_NO_WINDOW 是在 CreateProcess 阶段就不分配控制台，从源头消除窗口。
//
// 所以 backend 下的生产代码一律用 hiddenExec / hiddenExecContext，
// 禁裸 exec.Command（proc_exec_test.go 会扫源码钉住这一条）。
func hiddenExec(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = hiddenWindowAttr()
	return cmd
}

// hiddenExecContext 同 hiddenExec，但带 ctx（超时 / 取消）。
func hiddenExecContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = hiddenWindowAttr()
	return cmd
}
