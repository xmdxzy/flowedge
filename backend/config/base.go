package config

import (
	"os"
	"path/filepath"
	"strings"
)

// BaseDir 返回程序的工作根目录。
//
// 为什么需要它：Windows 用户双击 exe 或从快捷方式启动时，当前工作目录
// 往往是 C:\Windows\System32 之类的位置，相对路径 ./data 会写到那里去。
// 以可执行文件所在目录为基准，才能保证「把 exe 放到哪，数据就在哪」。
// go run / go test 时 exe 位于临时目录，此时回退到当前工作目录。
func BaseDir() string {
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		tmp := os.TempDir()
		if !strings.Contains(dir, "go-build") && !strings.Contains(dir, tmp) {
			return dir
		}
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// Resolve 将相对路径解析到 BaseDir 下；绝对路径原样返回
func Resolve(p string) string {
	if p == "" {
		return BaseDir()
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(BaseDir(), p)
}
