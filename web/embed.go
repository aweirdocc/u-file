// Package web 内嵌前端构建产物（web/dist）。
// go:embed 不能引用包外目录，故 embed 声明必须放在 web/ 内。
// 构建顺序：先 cd web && pnpm build（或 npm run build）产出 dist/，再 go build。
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// FS 返回前端静态资源（根为 dist/ 内部）。
func FS() (fs.FS, error) {
	return fs.Sub(distFS, "dist")
}
