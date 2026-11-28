//go:build embedui

package server

import (
	"embed"
	"io/fs"
	"net/http"
	"sync"
)

// webuiDist holds the statically-exported frontend (built with
// `cd web && npm run build:static`, then copied into server/webui/dist). Compiled
// into the binary only when building with `-tags embedui`. The `all:` prefix is
// required so Next's `_next/` asset dir (leading underscore) is included.
//
//go:embed all:webui/dist
var webuiDist embed.FS

var (
	webuiOnce          sync.Once
	webuiHandlerCached http.Handler
)

// webuiHandler serves the embedded SPA. Public (no JWT) — auth is enforced
// client-side and on /api. HTTP semantics (404 for missing assets, build meta
// injection, cache matrix) live in webui_handler.go, shared with the dev stub.
func (s *Server) webuiHandler() http.Handler {
	webuiOnce.Do(func() {
		root, err := fs.Sub(webuiDist, "webui/dist")
		if err != nil {
			panic(err)
		}
		webuiHandlerCached = newWebuiHandler(root)
	})
	return webuiHandlerCached
}
