package api

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// staticHandler 托管前端构建产物，并为前端路由做 SPA 回退。
//
// 存在的理由：让部署保持「一个二进制 + 一个数据库」。若把前端单独发布，
// 至少还要多一个静态服务器与一套跨域配置。
//
// 路径安全依赖两件事：先用 path.Clean 消除 ".."，再用 filepath.Join 拼到根目录下。
// 不直接拼接用户输入，也不信任 URL 里的相对路径。
func staticHandler(dir string) http.Handler {
	fileServer := http.FileServer(http.Dir(dir))
	indexFile := filepath.Join(dir, "index.html")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /api/* 的未匹配路径不能回退到 index.html——那会让客户端拿到 200 和
		// 一段 HTML，把「接口不存在」误读成「接口返回了奇怪的东西」。
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeError(w, http.StatusNotFound, "not_found", "unknown API endpoint")
			return
		}

		cleaned := path.Clean("/" + r.URL.Path)
		candidate := filepath.Join(dir, filepath.FromSlash(cleaned))

		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			fileServer.ServeHTTP(w, r)
			return
		}

		// 其余路径一律交给前端路由处理。
		http.ServeFile(w, r, indexFile)
	})
}
