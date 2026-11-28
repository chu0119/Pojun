//go:build embedui

package server

import (
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// 用真实的静态导出产物（server/webui/dist）冒烟：MapFS 能证明语义，但证明不了
// 真实产物的命名空间形状（`__next.*.txt`、`_next/static/` 下的哈希文件名、
// 每个路由目录的 index.html）。
func TestWebuiEmbeddedDistSmoke(t *testing.T) {
	root, err := fs.Sub(webuiDist, "webui/dist")
	if err != nil {
		t.Fatal(err)
	}
	h := newWebuiHandler(root)

	var realChunk string
	if err := fs.WalkDir(root, "_next/static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || realChunk != "" {
			return nil
		}
		if strings.HasSuffix(p, ".js") {
			realChunk = p
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if realChunk == "" {
		t.Fatal("内嵌产物里没有 _next/static/*.js，dist 可能没拷进去")
	}

	buildRe := regexp.MustCompile(`<meta name="pojun-build" content="([0-9a-f]{16})">`)

	for _, target := range []string{"/", "/login/", "/system/agents/chain/"} {
		w := serveReq(h, http.MethodGet, target, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", target, w.Code)
		}
		m := buildRe.FindStringSubmatch(w.Body.String())
		if m == nil {
			t.Fatalf("GET %s 缺少 pojun-build meta", target)
		}
		if m[1] != h.build || BuildWeb != h.build {
			t.Fatalf("GET %s 身份不一致：meta=%s handler=%s BuildWeb=%s", target, m[1], h.build, BuildWeb)
		}
	}

	w := serveReq(h, http.MethodGet, "/"+realChunk, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /%s = %d", realChunk, w.Code)
	}
	if !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("真实 chunk 不是 immutable：%q", w.Header().Get("Cache-Control"))
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/javascript") {
		t.Errorf("真实 chunk Content-Type = %q", w.Header().Get("Content-Type"))
	}

	// 换装后旧标签页最可能请求的那种 URL：同目录、已不存在的哈希文件名。
	gone := "/" + strings.TrimSuffix(realChunk, ".js") + "-gone.js"
	w = serveReq(h, http.MethodGet, gone, nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("GET %s = %d, 缺失 chunk 必须 404", gone, w.Code)
	}
	if strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("缺失 chunk 带了 immutable：%q", w.Header().Get("Cache-Control"))
	}

	// Next 16 的 RSC 载荷是实体文件，必须按 text/plain 精确命中。
	w = serveReq(h, http.MethodGet, "/system/agents/chain/index.txt", nil)
	if w.Code != http.StatusOK {
		t.Errorf("RSC 载荷 = %d", w.Code)
	}
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("RSC Content-Type = %q", w.Header().Get("Content-Type"))
	}

	w = serveReq(h, http.MethodGet, "/no-such-route/", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("未知文档 = %d, 应为 404", w.Code)
	}
	if !strings.Contains(w.Body.String(), `name="pojun-build"`) {
		t.Error("404 页缺少 pojun-build meta")
	}

	w = serveReq(h, http.MethodGet, "/_pojun_build.txt", nil)
	if got := strings.TrimSpace(w.Body.String()); got != h.build {
		t.Errorf("_pojun_build.txt = %q, want %q", got, h.build)
	}
}
