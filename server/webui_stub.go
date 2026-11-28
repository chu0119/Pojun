//go:build !embedui

package server

import (
	"net/http"
	"os"
)

// webuiHandler is the no-embed stub (default build). The frontend is NOT bundled
// into this binary — run it separately with `cd web && npm run dev` during
// development. Build the bundled single-binary with:
//
//	cd web && npm run build:static     # produces web/out
//	cp -r web/out server/webui/dist    # (or use the build script)
//	go build -tags embedui ./cmd/pojun
//
// Dev-only escape hatch: POJUN_WEBUI_DIST_DIR + POJUN_DEV=1 serves an exported
// dist directory with the same handler semantics, so E2E tests can swap builds
// without re-embedding. Both gates are required; never set them in production.
func (s *Server) webuiHandler() http.Handler {
	if dir := os.Getenv("POJUN_WEBUI_DIST_DIR"); dir != "" && os.Getenv("POJUN_DEV") == "1" {
		return newWebuiHandler(os.DirFS(dir))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "前端未内嵌到此二进制（开发用 next dev；发布用 -tags embedui 构建）", http.StatusNotFound)
	})
}
