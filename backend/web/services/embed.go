// Package services 内嵌可选的本地语音服务脚本
//
// 单文件部署的用户手里没有源码仓库，因此把「语音转写 / 语音合成」这两个
// Python 脚本一起打进 exe：运行 FlowEdge.exe --export-services 就能把它们
// 导到磁盘上执行。装不装 Python 只影响这两项能力，不影响引擎其它功能。
package services

import (
	"embed"
	"io/fs"
)

//go:embed all:files
var filesFS embed.FS

// Files 返回内嵌的脚本集合；没有产物时返回 nil
func Files() fs.FS {
	sub, err := fs.Sub(filesFS, "files")
	if err != nil {
		return nil
	}
	entries, err := fs.ReadDir(sub, ".")
	if err != nil || len(entries) == 0 {
		return nil
	}
	return sub
}
