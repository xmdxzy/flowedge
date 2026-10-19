//go:build windows

package service

import "syscall"

// hiddenWindowAttr 子进程不弹黑窗（powershell/python 在桌面端双击启动时尤其明显）
func hiddenWindowAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true}
}
