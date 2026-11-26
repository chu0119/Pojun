package tlsmgr

// 补充测试：ACME 挑战的 HTTP-01 服务与防火墙租约（全部离线，不访问真实 CA）、
// 证书热加载。核心行为（自签 SAN/导入校验/嗅探/重定向/续期判定）在 tlsmgr_test.go。

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHTTP01ProviderServesToken(t *testing.T) {
	p := newHTTP01Provider()
	if err := p.Present(context.Background(), "panel.example.com", "tok-1", "key-auth-1"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/.well-known/acme-challenge/tok-1", nil)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "key-auth-1" {
		t.Fatalf("code=%d body=%q", w.Code, w.Body.String())
	}
	// 未知 token / 其它路径一律 404，不能把别处的内容当挑战响应。
	for _, path := range []string{"/.well-known/acme-challenge/missing", "/api/health"} {
		w = httptest.NewRecorder()
		p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s: code=%d", path, w.Code)
		}
	}
	_ = p.CleanUp(context.Background(), "panel.example.com", "tok-1", "key-auth-1")
	w = httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/.well-known/acme-challenge/tok-1", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("清理后 token 应失效，code=%d", w.Code)
	}
}

type fakeFirewall struct {
	mu     sync.Mutex
	opens  []int
	closed []string
	fail   error
}

func (f *fakeFirewall) Open(_ context.Context, port int, _ time.Duration) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return "", false, f.fail
	}
	f.opens = append(f.opens, port)
	return "lease-1", true, nil
}

func (f *fakeFirewall) Close(_ context.Context, leaseID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, leaseID)
	return nil
}

func TestStartChallengeOpensAndClosesFirewallLease(t *testing.T) {
	m := newTestManager(t)
	fw := &fakeFirewall{}
	m.SetFirewall(fw)
	if err := m.ApplySettings(Settings{Mode: ModeACME, HTTPSAddr: ":8787", FirewallAuto: true}); err != nil {
		t.Fatal(err)
	}
	// 监听 127.0.0.1:0：不抢占真实 80 端口，防火墙按 80 记账。
	cfg := NormalizeACME(ACMEConfig{
		Identifiers: []string{"panel.example.com"}, Email: "a@example.com",
		Challenge: "http-01", HTTPAddr: "127.0.0.1:0",
	})
	stop, err := m.startChallenge(context.Background(), cfg, newHTTP01Provider())
	if err != nil {
		t.Fatalf("startChallenge: %v", err)
	}
	fw.mu.Lock()
	if len(fw.opens) != 1 || fw.opens[0] != 80 {
		t.Fatalf("应按 80 端口申请放行，得到 %v", fw.opens)
	}
	fw.mu.Unlock()
	stop()
	fw.mu.Lock()
	defer fw.mu.Unlock()
	if len(fw.closed) != 1 || fw.closed[0] != "lease-1" {
		t.Fatalf("stop 应撤销租约，得到 %v", fw.closed)
	}
}

// 防火墙放行失败必须让申请在改规则前中止：否则会带着「端口可能仍被拦」的
// 错误假设去下单，Let's Encrypt 回源超时后用户只看到笼统的验证失败。
func TestStartChallengeFirewallErrorAborts(t *testing.T) {
	m := newTestManager(t)
	m.SetFirewall(&fakeFirewall{fail: errors.New("permission denied")})
	if err := m.ApplySettings(Settings{Mode: ModeACME, HTTPSAddr: ":8787", FirewallAuto: true}); err != nil {
		t.Fatal(err)
	}
	cfg := NormalizeACME(ACMEConfig{
		Identifiers: []string{"panel.example.com"}, Email: "a@example.com",
		Challenge: "http-01", HTTPAddr: "127.0.0.1:0",
	})
	if _, err := m.startChallenge(context.Background(), cfg, newHTTP01Provider()); err == nil ||
		!strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("防火墙失败应中止申请，得到 %v", err)
	}
}

// Host 白名单外的远程明文请求不能 301 到攻击者域名；显式配置的通配符要生效。
func TestRedirectHandlerWildcardAllowlist(t *testing.T) {
	h := RedirectHandler(http.NotFoundHandler(), ":8787", "*.example.com")
	req := httptest.NewRequest(http.MethodGet, "http://panel.example.com/x", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	req.Host = "panel.example.com"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "https://panel.example.com:8787/x" {
		t.Fatalf("通配符主机应 301，code=%d loc=%q", w.Code, w.Header().Get("Location"))
	}
	// 后缀伪装（evilexample.com）不能命中 *.example.com。
	req = httptest.NewRequest(http.MethodGet, "http://evilexample.com/x", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	req.Host = "evilexample.com"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("后缀伪装应拒绝，code=%d loc=%q", w.Code, w.Header().Get("Location"))
	}
}

// 导入新证书后立即生效（热加载），无需重启；key.pem 必须 0600。
func TestImportHotReloadsAndKeyIsRestrictive(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.GenerateSelfSigned([]string{"old.example.test"}); err != nil {
		t.Fatal(err)
	}
	old := m.CertInfo()
	certPEM, keyPEM := mustPair(t, time.Now().Add(-time.Hour), time.Now().Add(48*time.Hour))
	info, err := m.Import(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if old.NotAfter.Equal(info.NotAfter) {
		t.Fatal("导入后应换成新证书")
	}
	pair, err := m.GetCertificate(nil)
	if err != nil || pair == nil || pair.Leaf == nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if !pair.Leaf.NotAfter.Equal(info.NotAfter) {
		t.Fatalf("热加载未生效：leaf=%v info=%v", pair.Leaf.NotAfter, info.NotAfter)
	}
	if os.PathSeparator == '\\' {
		return // Windows 不维护 POSIX 文件模式
	}
	fi, err := os.Stat(m.path("key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("私钥权限过宽: %v", fi.Mode().Perm())
	}
}

// 秒级检查：证书/私钥能真正完成 TLS 握手（GetCertificate 返回的 pair 可用）。
func TestGetCertificateCompletesHandshake(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.GenerateSelfSigned([]string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		tc := tls.Server(conn, &tls.Config{GetCertificate: m.GetCertificate, MinVersion: tls.VersionTLS12})
		_ = tc.Handshake()
		_ = tc.Close()
	}()
	client, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if len(client.ConnectionState().PeerCertificates) == 0 {
		t.Fatal("服务端未出示证书")
	}
	_ = client.Close()
}
