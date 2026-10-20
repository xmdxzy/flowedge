//go:build windows

package service

import "syscall"

// CREATE_NO_WINDOW：子进程完全不分配控制台，连 conhost 都不创建。
//
// 为什么不能只靠 HideWindow：HideWindow 走的是 STARTUPINFO 的
// STARTF_USESHOWWINDOW + SW_HIDE，语义是「先创建控制台窗口再隐藏」。
// 当父进程是没有控制台的 GUI 进程（双击 FlowEdge.exe / Edge 应用模式拉起）时，
// 这个隐藏动作跟不上窗口创建的时序，屏幕上仍会闪一下黑框。
// CREATE_NO_WINDOW 是在 CreateProcess 阶段就不给子进程分配控制台，
// 从源头消除窗口，桌面端体验才干净。
const createNoWindow = 0x08000000

// hiddenWindowAttr 子进程不弹黑窗（powershell/python 在桌面端双击启动时尤其明显）
func hiddenWindowAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
}
