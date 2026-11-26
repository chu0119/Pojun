package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// BuildWeb is the content digest of the embedded frontend (16 hex chars). It is
// computed from the served dist, not from git: two builds of the same source
// with different toolchains still get different identities, and the value the
// page reads from <meta name="pojun-build"> always describes the JS it is
// actually running. Empty in builds without an embedded frontend.
var BuildWeb string

// webuiClass is the only place that decides which namespace a request belongs
// to. Document routes may fall back to a document (or the 404 page); assets and
// data files must hit exactly or 404 — never an HTML substitute.
type webuiClass int

const (
	classDoc webuiClass = iota
	classAsset
	classBuildID
)

// classifyWebuiPath classifies a cleaned, relative path. Callers reject non
// GET/HEAD methods before asking.
func classifyWebuiPath(p string) webuiClass {
	if p == "_pojun_build.txt" {
		return classBuildID
	}
	if strings.HasPrefix(p, "_next/") || strings.HasPrefix(p, "__next.") {
		return classAsset
	}
	if p == "" || strings.HasSuffix(p, "/") {
		return classDoc
	}
	switch path.Ext(p) {
	case "":
		return classDoc
	case ".html":
		return classDoc
	default:
		return classAsset
	}
}

// webuiHandler serves the exported SPA with explicit HTTP semantics:
//
//   - assets under _next/static/ that really exist: one year immutable;
//   - other assets that exist: no-cache + ETag;
//   - documents (extensionless routes, *.html): no-cache, injected build meta;
//   - any missing asset/data file: 404 + no-store (never index.html);
//   - any missing document: the exported 404.html with status 404 + no-store.
//
// The SPA shell is served only for documents that were actually exported, so
// unknown deep links no longer turn into a 200 homepage.
type webuiHandler struct {
	root  fs.FS
	build string
	docs  map[string][]byte // "index.html" / "login/index.html" -> injected bytes
	// shellFallback restores the old "unknown document → app shell" behaviour for
	// the document namespace only (POJUN_WEBUI_SHELL_FALLBACK=1). Set once, at
	// construction: one process must not serve two different 404 contracts.
	shellFallback bool
}

func newWebuiHandler(root fs.FS) *webuiHandler {
	build, docs := buildWebuiIndex(root)
	BuildWeb = build
	return &webuiHandler{root: root, build: build, docs: docs, shellFallback: shellFallbackRequested()}
}

func shellFallbackRequested() bool { return os.Getenv("POJUN_WEBUI_SHELL_FALLBACK") == "1" }

func buildWebuiIndex(root fs.FS) (string, map[string][]byte) {
	var paths []string
	_ = fs.WalkDir(root, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		paths = append(paths, p)
		return nil
	})
	sort.Strings(paths)

	h := sha256.New()
	rawDocs := map[string][]byte{}
	for _, p := range paths {
		b, err := fs.ReadFile(root, p)
		if err != nil {
			continue
		}
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write(b)
		h.Write([]byte{0})
		if strings.HasSuffix(p, ".html") {
			rawDocs[p] = b
		}
	}
	id := hex.EncodeToString(h.Sum(nil))[:16]
	docs := make(map[string][]byte, len(rawDocs))
	for p, b := range rawDocs {
		docs[p] = injectBuildMeta(b, id)
	}
	return id, docs
}

// injectBuildMeta puts the build identity into <head> before any script runs,
// so the client can compare itself with the server without a separate request.
func injectBuildMeta(html []byte, id string) []byte {
	meta := []byte(`<meta name="pojun-build" content="` + id + `">`)
	insert := func(pos int) []byte {
		out := make([]byte, 0, len(html)+len(meta))
		out = append(out, html[:pos]...)
		out = append(out, meta...)
		out = append(out, html[pos:]...)
		return out
	}
	// 只认真正的 <head> 开始标签：`<header` 也以 `<head` 开头，命中它会插进错误位置。
	if i := bytes.Index(html, []byte("<head")); i >= 0 && i+5 < len(html) {
		switch html[i+5] {
		case '>', ' ', '\t', '\r', '\n':
			if j := bytes.IndexByte(html[i:], '>'); j >= 0 {
				return insert(i + j + 1)
			}
		}
	}
	// 兜底：插在 doctype 之后，绝不能让 meta 排在 <!DOCTYPE> 前面（会进入怪异模式）。
	if bytes.HasPrefix(html, []byte("<!DOCTYPE")) || bytes.HasPrefix(html, []byte("<!doctype")) {
		if k := bytes.IndexByte(html, '>'); k >= 0 {
			return insert(k + 1)
		}
	}
	return append(meta, html...)
}

func (h *webuiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Same three headers the API already gets; the webui previously had none.
	hdr := w.Header()
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("X-Frame-Options", "DENY")
	hdr.Set("Referrer-Policy", "no-referrer")

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		hdr.Set("Allow", "GET, HEAD")
		hdr.Set("Cache-Control", "no-store")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	p := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	switch classifyWebuiPath(p) {
	case classBuildID:
		h.serveBuildID(w, r)
	case classAsset:
		h.serveAsset(w, r, p)
	default:
		h.serveDoc(w, r, p)
	}
}

func (h *webuiHandler) serveBuildID(w http.ResponseWriter, r *http.Request) {
	hdr := w.Header()
	hdr.Set("Content-Type", "text/plain; charset=utf-8")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("ETag", `"`+h.build+`"`)
	if r.Header.Get("If-None-Match") == `"`+h.build+`"` {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write([]byte(h.build + "\n"))
}

func (h *webuiHandler) serveAsset(w http.ResponseWriter, r *http.Request, p string) {
	b, err := fs.ReadFile(h.root, p)
	if err != nil {
		h.notFoundAsset(w)
		return
	}
	if strings.HasPrefix(p, "_next/static/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	if ct := contentTypeFor(p); ct != "" {
		// Explicit types: http.ServeContent falls back to mime.TypeByExtension,
		// which on Windows reads the registry (.js is commonly text/plain there),
		// so the same binary would answer different MIME per host OS.
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("ETag", `"`+assetETag(h.build, p)+`"`)
	http.ServeContent(w, r, path.Base(p), time.Time{}, bytes.NewReader(b))
}

func (h *webuiHandler) serveDoc(w http.ResponseWriter, r *http.Request, p string) {
	// A .html request is a document too; serving it from the injected cache
	// also avoids ServeFileFS's 301 from /x/index.html to ./x/.
	name := ""
	switch {
	case p == "":
		name = "index.html"
	case strings.HasSuffix(p, ".html"):
		name = p
	case strings.HasSuffix(p, "/"):
		name = p + "index.html"
	default:
		if _, ok := h.docs[p+"/index.html"]; ok {
			name = p + "/index.html"
		} else if _, ok := h.docs[p+".html"]; ok {
			name = p + ".html"
		}
	}
	b, ok := h.docs[name]
	if !ok {
		// Transitional escape hatch, off by default and for at most one release:
		// restore the old "unknown document → app shell" behaviour for the
		// document namespace only. Assets never take this path.
		if h.shellFallback {
			if shell, ok := h.docs["index.html"]; ok {
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("Vary", "Accept")
				http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(shell))
				return
			}
		}
		h.serve404Doc(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Vary", "Accept")
	w.Header().Set("ETag", `"`+docETag(h.build, name)+`"`)
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(b))
}

func (h *webuiHandler) serve404Doc(w http.ResponseWriter, r *http.Request) {
	b, ok := h.docs["404.html"]
	hdr := w.Header()
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("Vary", "Accept")
	if !ok {
		hdr.Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("页面不存在\n"))
		return
	}
	hdr.Set("Content-Type", "text/html; charset=utf-8")
	hdr.Set("ETag", `"`+docETag(h.build, "404.html")+`"`)
	w.WriteHeader(http.StatusNotFound)
	if r.Method != http.MethodHead {
		_, _ = w.Write(b)
	}
}

func (h *webuiHandler) notFoundAsset(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("not found\n"))
}

// contentTypeFor pins the types the exported frontend actually ships. Unknown
// extensions return "" so ServeContent's own detection still applies.
func contentTypeFor(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".txt", ".map":
		return "text/plain; charset=utf-8"
	case ".json", ".webmanifest":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".avif":
		return "image/avif"
	case ".ico":
		return "image/x-icon"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".ttf":
		return "font/ttf"
	default:
		return ""
	}
}

func assetETag(build, p string) string {
	sum := sha256.Sum256([]byte(p))
	return fmt.Sprintf("%s-%s", build, hex.EncodeToString(sum[:])[:8])
}

func docETag(build, name string) string {
	sum := sha256.Sum256([]byte(name))
	return fmt.Sprintf("%s-%s", build, hex.EncodeToString(sum[:])[:8])
}
