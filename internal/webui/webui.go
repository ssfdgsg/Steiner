// Package webui 把 React 控制台的构建产物嵌入网关二进制，
// 由 server 挂载到 /admin/ui/，运维无需额外部署静态服务器。
//
// 产物由 `make web` 生成（web/ 工程 Vite 构建输出到本包的 dist/）。
// dist/ 已提交到仓库：保证 `go build` 不依赖 Node 工具链——
// 没装 Node 的机器（如生产构建机、CI 镜像）也能构建出带控制台的完整二进制。
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// 注：go:embed 不允许目录不存在，dist/.gitkeep 保证空产物时也能编译。
//
//go:embed all:dist
var dist embed.FS

// Available 控制台产物是否已构建（未构建时 server 返回提示而非 404，
// 避免运维误以为端点没实现）。
func Available() bool {
	entries, err := fs.ReadDir(dist, "dist")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name() == "index.html" {
			return true
		}
	}
	return false
}

// Handler 返回控制台静态资源处理器，需挂载在 prefix 路径下（如 /admin/ui/）。
// 未命中具体文件的路径一律回退到 index.html——单页应用的 hash 路由需要，
// 且避免刷新页面时 404。
func Handler(prefix string) http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "控制台资源不可用", http.StatusInternalServerError)
		})
	}
	files := http.FileServer(http.FS(sub))
	return http.StripPrefix(prefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := strings.TrimPrefix(r.URL.Path, "/")
		if clean == "" {
			clean = "index.html"
		}
		if _, err := fs.Stat(sub, clean); err != nil {
			// 单页回退：非资源路径交给 index.html 自行按 hash 渲染。
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	}))
}
