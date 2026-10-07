// Package web 内嵌管理面板（go:embed，零 CDN、可离线）。
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed panel.html
var panelHTML []byte

//go:embed all:static
var staticFS embed.FS

// PanelHTML 返回面板页面字节。
func PanelHTML() []byte { return panelHTML }

// StaticHandler 提供 /static/ 资源（打赏二维码等）。
func StaticHandler() http.HandlerFunc {
	sub, _ := fs.Sub(staticFS, "static")
	fileServer := http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "..") {
			http.NotFound(w, r)
			return
		}
		fileServer.ServeHTTP(w, r)
	}
}
