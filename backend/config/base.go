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
// go run 时 exe 位于 %TEMP%\go-buildXXXX 下（路径含 "go-build"），此时回退到当前工作目录。
//
// 注意：只认 "go-build" 这个特征，**不能**用「路径含 Temp 目录」做判断——
// 那会把所有放在临时目录里的真实 exe 误判成 go run，导致数据写到 CWD
// （实测：从 C:// 启动放在 Temp 下的 FlowEdge.exe，data 落到了 C://data）。
func BaseDir() string {
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		if !strings.Contains(dir, "go-build") {
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
