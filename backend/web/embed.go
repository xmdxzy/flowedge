// Package web 内嵌前端构建产物，使单个 exe 自带 Web 界面
//
// 设计要点：
//   - 干净 clone 时目录里只有 .placeholder，Dist() 返回 nil，程序退化为
//     「仅 API」模式（或使用 STATIC_DIR 指定的外部目录）
//   - 运行 scripts/build-windows.py 后，dist 产物被复制进来并打进 exe，
//     用户双击 exe 即可用完整界面，不需要装 Node
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:static
var staticFS embed.FS

// Dist 返回内嵌的前端资源；没有构建产物时返回 nil
func Dist() fs.FS {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil
	}
	return sub
}
