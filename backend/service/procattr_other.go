//go:build !windows

package service

import "syscall"

// hiddenWindowAttr 非 Windows 平台没有隐藏窗口的概念，返回 nil 即可
func hiddenWindowAttr() *syscall.SysProcAttr {
	return nil
}
