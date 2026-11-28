package server

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

func testWebuiFS() fstest.MapFS {
	doc := func(title, body string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte(`<!DOCTYPE html><html><head><meta charset="utf-8"><title>` + title + `</title></head><body>` + body + `</body></html>`)}
	}
	return fstest.MapFS{
		"index.html":                     doc("pojun", "root"),
		"404.html":                       doc("404", "not-found-page"),
		"login/index.html":               doc("login", "login"),
		"system/agents/chain/index.html": doc("chain", "chain"),
		"dashboard/index.txt":            {Data: []byte(`self.__next_f.push([1,"rsc"])`)},
		"robots.txt":                     {Data: []byte("User-agent: *\n")},
		"_next/static/chunks/a.js":       {Data: []byte("console.log(1)\n")},
		"_next/static/css/a.css":         {Data: []byte("body{margin:0}\n")},
		"_next/static/media/f.woff2":     {Data: []byte("FONT")},
	}
}

func serveReq(h http.Handler, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestClassifyWebuiPath(t *testing.T) {
	cases := []struct {
		p    string
		want webuiClass
	}{
		{"", classDoc},
		{"_pojun_build.txt", classBuildID},
		{"system/agents/chain/", classDoc},
		{"system/agents/chain", classDoc},
		{"login/index.html", classDoc},
		{"404.html", classDoc},
		{"_next/static/chunks/a.js", classAsset},
		{"__next._tree.txt", classAsset},
		{"dashboard/index.txt", classAsset},
		{"robots.txt", classAsset},
		{"favicon.ico", classAsset},
		{"x/_next/static/y.js", classAsset},
	}
	for _, c := range cases {
		if got := classifyWebuiPath(c.p); got != c.want {
			t.Errorf("classifyWebuiPath(%q) = %v, want %v", c.p, got, c.want)
		}
	}
}

func TestWebuiDocuments(t *testing.T) {
	h := newWebuiHandler(testWebuiFS())
	buildRe := regexp.MustCompile(`<meta name="pojun-build" content="([0-9a-f]{16})">`)

	for _, target := range []string{"/", "/login/", "/login", "/system/agents/chain", "/system/agents/chain/", "/login/index.html"} {
		w := serveReq(h, http.MethodGet, target, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", target, w.Code)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("GET %s Cache-Control = %q, want no-cache", target, cc)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET %s Content-Type = %q", target, ct)
		}
		m := buildRe.FindStringSubmatch(w.Body.String())
		if m == nil {
			t.Fatalf("GET %s body missing pojun-build meta: %q", target, w.Body.String())
		}
		if m[1] != h.build {
			t.Errorf("GET %s meta build %q != handler build %q", target, m[1], h.build)
		}
		if n := strings.Count(w.Body.String(), `name="pojun-build"`); n != 1 {
			t.Errorf("GET %s injected %d times, want 1", target, n)
		}
		if !strings.Contains(w.Body.String(), "<meta charset") {
			t.Errorf("GET %s lost the original head content", target)
		}
	}
	if got := BuildWeb; got != h.build {
		t.Errorf("BuildWeb = %q, want handler identity %q", got, h.build)
	}
}

// A stale tab asks for a chunk the new build no longer ships. Before the fix this
// was 200 text/html + one year immutable; it must now be a non-cacheable 404.
func TestWebuiMissingAssetIsStrict(t *testing.T) {
	h := newWebuiHandler(testWebuiFS())
	for _, target := range []string{
		"/_next/static/chunks/gone.js",
		"/_next/static/css/gone.css",
		"/x/_next/static/y.js",
		"/missing.png",
	} {
		w := serveReq(h, http.MethodGet, target, nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", target, w.Code)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("GET %s Cache-Control = %q, want no-store", target, cc)
		}
		if strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
			t.Errorf("GET %s must not be immutable", target)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Errorf("GET %s Content-Type = %q, want text/plain", target, ct)
		}
		if strings.Contains(w.Body.String(), "<html") {
			t.Errorf("GET %s returned an HTML document", target)
		}
	}
}

func TestWebuiExistingAssets(t *testing.T) {
	h := newWebuiHandler(testWebuiFS())

	w := serveReq(h, http.MethodGet, "/_next/static/chunks/a.js", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("immutable asset Cache-Control = %q", cc)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("js Content-Type = %q", ct)
	}
	if !strings.HasPrefix(w.Header().Get("ETag"), `"`) {
		t.Errorf("missing ETag: %q", w.Header().Get("ETag"))
	}
	if w.Body.String() != "console.log(1)\n" {
		t.Errorf("body = %q", w.Body.String())
	}

	// Non-_next assets are addressable but not immutable. Next content-addresses
	// everything under _next/static/ (fonts/ images/ included), so those stay
	// immutable even outside chunks/.
	for _, tc := range []struct {
		target, wantCT string
		immutable      bool
	}{
		{"/robots.txt", "text/plain", false},
		{"/dashboard/index.txt", "text/plain", false},
		{"/_next/static/media/f.woff2", "font/woff2", true},
	} {
		w := serveReq(h, http.MethodGet, tc.target, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", tc.target, w.Code)
		}
		gotImmutable := strings.Contains(w.Header().Get("Cache-Control"), "immutable")
		if gotImmutable != tc.immutable {
			t.Errorf("GET %s immutable = %v, want %v (Cache-Control %q)", tc.target, gotImmutable, tc.immutable, w.Header().Get("Cache-Control"))
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, tc.wantCT) {
			t.Errorf("GET %s Content-Type = %q, want %s", tc.target, ct, tc.wantCT)
		}
	}
}

func TestWebuiMissingDataFileIs404(t *testing.T) {
	h := newWebuiHandler(testWebuiFS())
	w := serveReq(h, http.MethodGet, "/other/index.txt", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if strings.Contains(w.Header().Get("Content-Type"), "html") {
		t.Errorf("data file miss must not be HTML: %q", w.Header().Get("Content-Type"))
	}
}

func TestWebuiUnknownDocumentServes404Page(t *testing.T) {
	h := newWebuiHandler(testWebuiFS())
	for _, target := range []string{"/no-such-route/", "/no-such-route", "/deep/link/"} {
		w := serveReq(h, http.MethodGet, target, nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", target, w.Code)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("GET %s Cache-Control = %q", target, cc)
		}
		if !strings.Contains(w.Body.String(), "not-found-page") {
			t.Errorf("GET %s did not serve 404.html: %q", target, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `name="pojun-build"`) {
			t.Errorf("GET %s 404 page missing build meta", target)
		}
	}
}

func TestWebuiMethodsAndHeaders(t *testing.T) {
	h := newWebuiHandler(testWebuiFS())

	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		w := serveReq(h, m, "/", nil)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s / = %d, want 405", m, w.Code)
		}
		if allow := w.Header().Get("Allow"); allow != "GET, HEAD" {
			t.Errorf("%s / Allow = %q", m, allow)
		}
	}

	w := serveReq(h, http.MethodGet, "/", nil)
	for _, k := range []string{"X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy"} {
		if w.Header().Get(k) == "" {
			t.Errorf("missing security header %s", k)
		}
	}
}

func TestWebuiHeadAndRange(t *testing.T) {
	h := newWebuiHandler(testWebuiFS())

	w := serveReq(h, http.MethodHead, "/_next/static/chunks/a.js", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD body = %q, want empty", w.Body.String())
	}

	w = serveReq(h, http.MethodGet, "/_next/static/chunks/a.js", map[string]string{"Range": "bytes=0-3"})
	if w.Code != http.StatusPartialContent {
		t.Fatalf("Range status = %d, want 206", w.Code)
	}
	if w.Body.String() != "cons" {
		t.Errorf("Range body = %q", w.Body.String())
	}
}

func TestWebuiConditionalRequests(t *testing.T) {
	h := newWebuiHandler(testWebuiFS())

	w := serveReq(h, http.MethodGet, "/", nil)
	docETag := w.Header().Get("ETag")
	if docETag == "" {
		t.Fatal("document has no ETag")
	}
	if w := serveReq(h, http.MethodGet, "/", map[string]string{"If-None-Match": docETag}); w.Code != http.StatusNotModified {
		t.Errorf("document If-None-Match = %d, want 304", w.Code)
	}

	w = serveReq(h, http.MethodGet, "/_next/static/chunks/a.js", nil)
	if w := serveReq(h, http.MethodGet, "/_next/static/chunks/a.js", map[string]string{"If-None-Match": w.Header().Get("ETag")}); w.Code != http.StatusNotModified {
		t.Errorf("asset If-None-Match = %d, want 304", w.Code)
	}
}

func TestWebuiBuildIDFile(t *testing.T) {
	h := newWebuiHandler(testWebuiFS())
	w := serveReq(h, http.MethodGet, "/_pojun_build.txt", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q", ct)
	}
	body := strings.TrimSpace(w.Body.String())
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(body) {
		t.Fatalf("build id = %q, want 16 hex chars", body)
	}
	if body != h.build {
		t.Errorf("build file %q != handler %q", body, h.build)
	}
	if strings.Contains(body, "pojun") {
		t.Errorf("build id leaks product name: %q", body)
	}
	if w := serveReq(h, http.MethodGet, "/_pojun_build.txt", map[string]string{"If-None-Match": w.Header().Get("ETag")}); w.Code != http.StatusNotModified {
		t.Errorf("If-None-Match = %d, want 304", w.Code)
	}
}

// The identity must describe the served bytes, not the process or the clock.
func TestWebuiBuildIDTracksContent(t *testing.T) {
	base := testWebuiFS()
	a := newWebuiHandler(base)
	b := newWebuiHandler(base)
	if a.build != b.build {
		t.Fatalf("same content produced different identities: %q vs %q", a.build, b.build)
	}

	changed := fstest.MapFS{}
	for k, v := range base {
		changed[k] = v
	}
	changed["_next/static/chunks/a.js"] = &fstest.MapFile{Data: []byte("console.log(2)\n")}
	c := newWebuiHandler(changed)
	if c.build == a.build {
		t.Fatalf("changed asset kept identity %q", a.build)
	}
}

// POJUN_WEBUI_SHELL_FALLBACK=1 is a one-release escape hatch: it may bring back
// the app shell for unknown documents, but never for assets — that would restore
// exactly the defect being fixed.
func TestWebuiShellFallbackOnlyCoversDocuments(t *testing.T) {
	t.Setenv("POJUN_WEBUI_SHELL_FALLBACK", "1")
	h := newWebuiHandler(testWebuiFS())
	if !h.shellFallback {
		t.Fatal("fallback flag not picked up at construction")
	}

	w := serveReq(h, http.MethodGet, "/no-such-route/", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("document fallback status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), ">root<") {
		t.Errorf("document fallback did not serve the shell: %q", w.Body.String())
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("document fallback Cache-Control = %q", cc)
	}

	w = serveReq(h, http.MethodGet, "/_next/static/chunks/gone.js", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("asset under fallback = %d, want 404", w.Code)
	}
	if strings.Contains(w.Body.String(), "<html") {
		t.Error("asset miss fell back to HTML")
	}
}

func TestWebuiPathTraversalIsCleaned(t *testing.T) {
	h := newWebuiHandler(testWebuiFS())
	for _, target := range []string{"/../etc/passwd", "/_next/static/../../etc/passwd", "/%2e%2e/etc/passwd"} {
		w := serveReq(h, http.MethodGet, target, nil)
		if w.Code == http.StatusOK && strings.Contains(w.Body.String(), "root:") {
			t.Fatalf("GET %s escaped the dist root", target)
		}
	}
}
