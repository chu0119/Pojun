package tlsmgr

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/idna"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	// 不用 t.TempDir()：Windows 上它的 RemoveAll 会与杀毒/索引服务的短暂句柄
	// 竞争，偶发 "The directory is not empty"（与本包实现无关，单跑必过，
	// 与本仓库其它已验证的同类 flaky 一致）。这里改为 best-effort 清理。
	dir, err := os.MkdirTemp("", "pojun-tls-test-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	m, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func TestSelfSignedGeneratesCAAndLeaf(t *testing.T) {
	m := newTestManager(t)
	info, err := m.GenerateSelfSigned([]string{"panel.example.test"})
	if err != nil {
		t.Fatalf("GenerateSelfSigned: %v", err)
	}
	if info.Source != "selfsigned" {
		t.Fatalf("source = %q", info.Source)
	}
	if len(m.CA()) == 0 {
		t.Fatal("CA 为空")
	}
	want := map[string]bool{"127.0.0.1": false, "::1": false, "panel.example.test": false}
	for _, s := range info.SANs {
		if _, ok := want[s]; ok {
			want[s] = true
		}
	}
	for s, seen := range want {
		if !seen {
			t.Fatalf("SAN %q 缺失：%v", s, info.SANs)
		}
	}
	if time.Until(info.NotAfter) < 300*24*time.Hour {
		t.Fatalf("有效期过短：%v", info.NotAfter)
	}
	// Ensure 在已覆盖时不应重复签发。
	first := info.NotBefore
	if err := m.EnsureSelfSigned([]string{"panel.example.test"}); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if again := m.CertInfo(); !again.NotBefore.Equal(first) {
		t.Fatal("Ensure 应复用已有证书")
	}
}

func TestEnsureSelfSignedRotatesWhenSANMissing(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.GenerateSelfSigned(nil); err != nil {
		t.Fatal(err)
	}
	old := m.CertInfo()
	if err := m.EnsureSelfSigned([]string{"new.example.test"}); err != nil {
		t.Fatal(err)
	}
	cur := m.CertInfo()
	if cur.NotAfter.Equal(old.NotAfter) && !coversSANs(cur.SANs, []string{"new.example.test"}) {
		t.Fatal("新增 SAN 后应重新签发")
	}
	if !coversSANs(cur.SANs, []string{"new.example.test"}) {
		t.Fatalf("新 SAN 未生效：%v", cur.SANs)
	}
}

func TestImportRejectsMismatchAndExpired(t *testing.T) {
	m := newTestManager(t)
	certPEM, keyPEM := mustPair(t, time.Now().Add(-2*time.Hour), time.Now().Add(time.Hour))
	otherKey := mustKeyPEM(t)
	if _, err := m.Import(certPEM, otherKey); err == nil {
		t.Fatal("证书与私钥不匹配应被拒绝")
	}
	expiredCert, expiredKey := mustPair(t, time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
	if _, err := m.Import(expiredCert, expiredKey); err == nil || !strings.Contains(err.Error(), "过期") {
		t.Fatalf("过期证书应被拒绝，err=%v", err)
	}
	if _, err := m.Import(certPEM, keyPEM); err != nil {
		t.Fatalf("合法证书应通过：%v", err)
	}
	if m.CertInfo() == nil || m.CertInfo().Source != "import" {
		t.Fatalf("导入后 source=%v", m.CertInfo())
	}
}

func TestImportRejectsOversize(t *testing.T) {
	m := newTestManager(t)
	big := make([]byte, maxImportBytes+1)
	if _, err := m.Import(big, big); err == nil {
		t.Fatal("超大输入应被拒绝")
	}
}

func TestSniffListenerTLSAndLoopbackPlainHTTP(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.GenerateSelfSigned([]string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	srv := &http.Server{Handler: RedirectHandler(app, addr), TLSConfig: &tls.Config{GetCertificate: m.GetCertificate, MinVersion: tls.VersionTLS12}}
	sniff := &SniffListener{Listener: ln, TLSConfig: srv.TLSConfig}
	go func() { _ = srv.Serve(sniff) }()
	defer srv.Close()

	// 本机回环明文直通（兼容 nginx 回源/健康检查）。
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get("http://" + addr + "/foo?x=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("回环明文应直通，status=%d", resp.StatusCode)
	}

	// TLS → 业务处理器。
	tlsClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	tr, err := tlsClient.Get("https://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Body.Close()
	body, _ := io.ReadAll(tr.Body)
	if string(body) != "ok" {
		t.Fatalf("body=%q", body)
	}
}

func TestValidateACMERules(t *testing.T) {
	cases := []struct {
		name string
		cfg  ACMEConfig
		ok   bool
	}{
		{"ip-ok", ACMEConfig{Identifiers: []string{"203.0.113.10"}, Email: "a@example.com", Challenge: "http-01", HTTPAddr: ":80"}, true},
		{"bad-ip", ACMEConfig{Identifiers: []string{"999.1.1.1"}, Email: "a@example.com"}, false},
		{"no-email", ACMEConfig{Identifiers: []string{"example.com"}}, false},
		{"wildcard", ACMEConfig{Identifiers: []string{"*.example.com"}, Email: "a@example.com"}, false},
		{"port", ACMEConfig{Identifiers: []string{"example.com:443"}, Email: "a@example.com"}, false},
		{"underscore", ACMEConfig{Identifiers: []string{"_acme.example.com"}, Email: "a@example.com"}, false},
		{"bad-email", ACMEConfig{Identifiers: []string{"example.com"}, Email: ".@."}, false},
		{"bad-challenge", ACMEConfig{Identifiers: []string{"example.com"}, Email: "a@example.com", Challenge: "dns-01"}, false},
		{"wrong-http-port", ACMEConfig{Identifiers: []string{"example.com"}, Email: "a@example.com", Challenge: "http-01", HTTPAddr: ":8080"}, false},
		{"loopback-http", ACMEConfig{Identifiers: []string{"example.com"}, Email: "a@example.com", Challenge: "http-01", HTTPAddr: "127.0.0.1:80"}, false},
		{"wrong-alpn-port", ACMEConfig{Identifiers: []string{"example.com"}, Email: "a@example.com", Challenge: "tls-alpn-01", TLSAddr: ":8443"}, false},
		{"alpn-ok", ACMEConfig{Identifiers: []string{"example.com"}, Email: "a@example.com", Challenge: "tls-alpn-01", TLSAddr: ":443"}, true},
	}
	for _, c := range cases {
		err := validateACME(c.cfg)
		if c.ok && err != nil {
			t.Errorf("%s: 期望通过，得到 %v", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s: 期望拒绝", c.name)
		}
	}
}

func TestACMECProfileForIP(t *testing.T) {
	if got := acmeProfile([]string{"example.com"}); got != "" {
		t.Fatalf("域名不应使用 shortlived：%q", got)
	}
	if got := acmeProfile([]string{"example.com", "203.0.113.10"}); got != "shortlived" {
		t.Fatalf("含 IP 必须 shortlived：%q", got)
	}
}

// 续期窗口的边界：寿命一半、6 天 IP 证书、NotBefore 缺失、已过期。
func TestACMERenewDueTable(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	cases := []struct {
		name string
		info *CertInfo
		want bool
	}{
		{"nil", nil, false},
		{"90d-at-10d", &CertInfo{NotBefore: now.Add(-10 * day), NotAfter: now.Add(80 * day)}, false},
		{"90d-at-60d", &CertInfo{NotBefore: now.Add(-30 * day), NotAfter: now.Add(60 * day)}, false},
		{"90d-exactly-half", &CertInfo{NotBefore: now.Add(-45 * day), NotAfter: now.Add(45 * day)}, true},
		{"90d-past-half", &CertInfo{NotBefore: now.Add(-60 * day), NotAfter: now.Add(30 * day)}, true},
		{"6d-ip-at-2d", &CertInfo{NotBefore: now.Add(-4 * day), NotAfter: now.Add(2 * day)}, true},
		{"6d-ip-fresh", &CertInfo{NotBefore: now.Add(-6 * time.Hour), NotAfter: now.Add(138 * time.Hour)}, false},
		{"expired", &CertInfo{NotBefore: now.Add(-91 * day), NotAfter: now.Add(-day)}, true},
		// NotBefore 零值时按 90 天估计：剩余 30 天应进入续期窗口，而不是“几千年寿命”永不续。
		{"zero-notbefore-within-window", &CertInfo{NotAfter: now.Add(30 * day)}, true},
		{"zero-notbefore-fresh", &CertInfo{NotAfter: now.Add(80 * day)}, false},
		// 寿命非正（坏数据）时也按 90 天口径，不能用 0 寿命把它算成“已到期”。
		{"nonpositive-lifetime-fresh", &CertInfo{NotBefore: now.Add(day), NotAfter: now.Add(80 * day)}, false},
	}
	for _, c := range cases {
		if got := acmeRenewDue(c.info, now); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestRenewDueUsesTableLogic(t *testing.T) {
	m := newTestManager(t)
	if m.RenewDue(time.Now()) {
		t.Fatal("无证书时不应判定需要续期")
	}
	if _, err := m.GenerateSelfSigned(nil); err != nil {
		t.Fatal(err)
	}
	if m.RenewDue(time.Now()) {
		t.Fatal("刚生成的自签证书不应进入续期窗口")
	}
	// 把内存里的证书改成“已过半寿命”的 ACME 证书，RenewDue 应跟随同一口径。
	m.mu.Lock()
	m.info.NotBefore = time.Now().Add(-200 * 24 * time.Hour)
	m.info.NotAfter = time.Now().Add(100 * 24 * time.Hour)
	m.mu.Unlock()
	if !m.RenewDue(time.Now()) {
		t.Fatal("过半寿命应判定需要续期")
	}
}

func TestPlanACMEStart(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	ids := []string{"panel.example.com"}
	cases := []struct {
		name    string
		info    *CertInfo
		ids     []string
		staging bool
		want    acmeStartAction
	}{
		{"no-cert", nil, ids, false, acmeStartIssue},
		{"selfsigned", &CertInfo{Source: "selfsigned", NotAfter: now.Add(300 * 24 * time.Hour)}, ids, false, acmeStartIssue},
		{"import", &CertInfo{Source: "import", NotAfter: now.Add(300 * 24 * time.Hour)}, ids, false, acmeStartIssue},
		{"acme-valid", &CertInfo{Source: "acme", SANs: ids, Issuer: "Let's Encrypt", NotBefore: now.Add(-30 * 24 * time.Hour), NotAfter: now.Add(60 * 24 * time.Hour)}, ids, false, acmeStartNone},
		{"acme-renamed", &CertInfo{Source: "acme", SANs: []string{"old.example.com"}, Issuer: "Let's Encrypt", NotAfter: now.Add(60 * 24 * time.Hour)}, ids, false, acmeStartIssue},
		{"acme-renew-window", &CertInfo{Source: "acme", SANs: ids, Issuer: "Let's Encrypt", NotBefore: now.Add(-89 * 24 * time.Hour), NotAfter: now.Add(12 * time.Hour)}, ids, false, acmeStartRenew},
		{"acme-half-life", &CertInfo{Source: "acme", SANs: ids, Issuer: "Let's Encrypt", NotBefore: now.Add(-60 * 24 * time.Hour), NotAfter: now.Add(30 * 24 * time.Hour)}, ids, false, acmeStartRenew},
		{"acme-before-half", &CertInfo{Source: "acme", SANs: ids, Issuer: "Let's Encrypt", NotBefore: now.Add(-10 * 24 * time.Hour), NotAfter: now.Add(80 * 24 * time.Hour)}, ids, false, acmeStartNone},
		{"acme-ip-shortlived-renew", &CertInfo{Source: "acme", SANs: []string{"203.0.113.10"}, Issuer: "Let's Encrypt", NotBefore: now.Add(-4 * 24 * time.Hour), NotAfter: now.Add(2 * 24 * time.Hour)}, []string{"203.0.113.10"}, false, acmeStartRenew},
		{"acme-ip-shortlived-fresh", &CertInfo{Source: "acme", SANs: []string{"203.0.113.10"}, Issuer: "Let's Encrypt", NotBefore: now.Add(-6 * time.Hour), NotAfter: now.Add(138 * time.Hour)}, []string{"203.0.113.10"}, false, acmeStartNone},
		{"acme-expired", &CertInfo{Source: "acme", SANs: ids, Issuer: "Let's Encrypt", NotAfter: now.Add(-time.Hour)}, ids, false, acmeStartIssue},
		{"acme-staging-mismatch", &CertInfo{Source: "acme", SANs: ids, Issuer: "(STAGING) Pretend Pear X1", NotAfter: now.Add(60 * 24 * time.Hour)}, ids, false, acmeStartIssue},
		{"acme-staging-match", &CertInfo{Source: "acme", SANs: ids, Issuer: "(STAGING) Pretend Pear X1", NotAfter: now.Add(60 * 24 * time.Hour)}, ids, true, acmeStartNone},
	}
	for _, c := range cases {
		if got := planACMEStart(c.info, c.ids, c.staging, now); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestCertMatchesIdentifiers(t *testing.T) {
	info := &CertInfo{SANs: []string{"Panel.Example.com", "203.0.113.10"}}
	if !certMatchesIdentifiers(info, []string{"panel.example.com.", "203.0.113.10"}) {
		t.Fatal("大小写/尾点应视为同一标识")
	}
	if certMatchesIdentifiers(info, []string{"panel.example.com"}) {
		t.Fatal("SAN 多于请求标识时不应继续 Renew（Renew 会从 SAN 推域名）")
	}
	if certMatchesIdentifiers(info, []string{"panel.example.com", "203.0.113.11"}) {
		t.Fatal("标识变更时不应继续 Renew")
	}
	if !shouldRenewACME(&CertInfo{Source: "acme", SANs: []string{"panel.example.com"}, Issuer: "Let's Encrypt"}, []string{"panel.example.com"}, false) {
		t.Fatal("ACME 证书且标识一致应允许 Renew")
	}
	if shouldRenewACME(&CertInfo{Source: "selfsigned", SANs: []string{"panel.example.com"}}, []string{"panel.example.com"}, false) {
		t.Fatal("自签证书不应走 ACME Renew")
	}
	ascii, err := idna.Lookup.ToASCII("例子.com")
	if err != nil {
		t.Fatal(err)
	}
	if !certMatchesIdentifiers(&CertInfo{SANs: []string{ascii}}, []string{"例子.com"}) {
		t.Fatalf("IDN 应归一为 punycode 后比较: %s", ascii)
	}
}

func TestACMEObtainRequestHasKeyType(t *testing.T) {
	req := acmeObtainRequest(ACMEConfig{Identifiers: []string{"example.com"}, Email: "a@example.com"})
	if req.KeyType == "" {
		t.Fatal("ObtainRequest.KeyType 为空：lego 在 CSR 阶段会报 the key type is missing")
	}
	if len(req.EmailAddresses) != 0 {
		t.Fatalf("账号邮箱不应写进 CSR email SAN: %v", req.EmailAddresses)
	}
	if req.Profile != "" {
		t.Fatalf("纯域名不应使用 shortlived: %q", req.Profile)
	}
	req = acmeObtainRequest(ACMEConfig{Identifiers: []string{"203.0.113.10"}})
	if req.Profile != "shortlived" {
		t.Fatalf("IP 应使用 shortlived: %q", req.Profile)
	}
}

func TestNormalizeACMEDefaultChallenge(t *testing.T) {
	c := NormalizeACME(ACMEConfig{Identifiers: []string{"example.com"}, Email: "a@example.com"})
	if c.Challenge != "http-01" {
		t.Fatalf("默认 challenge=%q，期望 http-01", c.Challenge)
	}
}

func TestSettingsApplyValidation(t *testing.T) {
	m := newTestManager(t)
	if err := m.ApplySettings(Settings{Mode: "bogus"}); err == nil {
		t.Fatal("未知模式应被拒绝")
	}
	if err := m.ApplySettings(Settings{Mode: ModeSelfSigned, HTTPSAddr: "8787"}); err == nil {
		t.Fatal("缺端口应被拒绝")
	}
	if err := m.ApplySettings(Settings{Mode: ModeSelfSigned, HTTPSAddr: "::1"}); err == nil {
		t.Fatal("裸 IPv6 缺端口应被拒绝")
	}
	if err := m.ApplySettings(Settings{Mode: ModeSelfSigned, HTTPSAddr: "[::1]:9443"}); err != nil {
		t.Fatalf("合法 IPv6 监听地址应通过: %v", err)
	}
	if err := m.ApplySettings(Settings{Mode: ModeSelfSigned, HTTPSAddr: ":9443", HSTS: false}); err != nil {
		t.Fatal(err)
	}
	if got := m.Settings().HTTPSAddr; got != ":9443" {
		t.Fatalf("addr=%q", got)
	}
}

func TestStatusDoesNotLeakPrivateKey(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.GenerateSelfSigned(nil); err != nil {
		t.Fatal(err)
	}
	st := m.Status()
	if st.Cert == nil {
		t.Fatal("status.cert 为空")
	}
	if strings.Contains(strings.ToUpper(st.Cert.Subject), "PRIVATE") {
		t.Fatal("status 泄露私钥信息")
	}
}

func TestRedirectHandlerIPv6Host(t *testing.T) {
	h := RedirectHandler(http.NotFoundHandler(), "[::1]:8787")
	req := httptest.NewRequest(http.MethodGet, "http://[::1]:8787/a", nil)
	req.Host = "[::1]:8787"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMovedPermanently {
		t.Fatalf("code=%d", w.Code)
	}
	if got := w.Header().Get("Location"); got != "https://[::1]:8787/a" {
		t.Fatalf("Location=%q", got)
	}
}

// 半写状态（key 已换、cert 还是旧的，或只写了一半）不能让面板起不来：
// New 只记录 last_error，自签模式下的 Ensure 必须重新签发出一对可用的证书。
func TestEnsureRecoversFromHalfWrittenPair(t *testing.T) {
	m := newTestManager(t)
	now := time.Now()
	staleCert, _ := mustPair(t, now.Add(-2*time.Hour), now.Add(time.Hour))
	_, freshKey := mustPair(t, now.Add(-time.Hour), now.Add(48*time.Hour))
	if err := atomicWrite(m.path("cert.pem"), staleCert, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(m.path("key.pem"), freshKey, 0o600); err != nil {
		t.Fatal(err)
	}
	// 重新从磁盘构造：Load 会因证书/私钥不匹配失败，但构造函数不能返回错误。
	m2, err := New(filepath.Dir(m.dir))
	if err != nil {
		t.Fatalf("半写状态不应阻塞启动: %v", err)
	}
	if m2.Status().LastError == "" {
		t.Fatal("半写状态应留下 last_error")
	}
	if m2.CertInfo() != nil {
		t.Fatal("不可用的证书不应被加载")
	}
	if err := m2.ApplySettings(Settings{Mode: ModeSelfSigned, HTTPSAddr: ":8787"}); err != nil {
		t.Fatal(err)
	}
	if err := m2.Ensure(context.Background()); err != nil {
		t.Fatalf("自签模式应能自愈: %v", err)
	}
	info := m2.CertInfo()
	if info == nil || info.Source != "selfsigned" {
		t.Fatalf("应重新签发出自签证书: %+v", info)
	}
	// 自愈后必须能真正握手（私钥与证书匹配）。
	if _, err := m2.GetCertificate(nil); err != nil {
		t.Fatalf("自愈后的证书不可用: %v", err)
	}
	m2.mu.RLock()
	pair := m2.cert
	m2.mu.RUnlock()
	if pair == nil || len(pair.Certificate) == 0 || pair.PrivateKey == nil {
		t.Fatal("证书对不完整")
	}
}

// 导入模式：没有证书 / 证书已过期都必须在监听前报错（由 main 回落明文并留痕）。
func TestEnsureImportRejectsMissingAndExpired(t *testing.T) {
	m := newTestManager(t)
	if err := m.ApplySettings(Settings{Mode: ModeImport, HTTPSAddr: ":8787"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Ensure(context.Background()); err == nil {
		t.Fatal("没有证书时导入模式应报错")
	}
	expiredCert, expiredKey := mustPair(t, time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
	if _, err := m.installPair(expiredCert, expiredKey, "import"); err != nil {
		// installPair 本身不做有效期校验，这里只是确保文件落地；校验在 Ensure。
		t.Fatalf("installPair: %v", err)
	}
	if err := m.Ensure(context.Background()); err == nil || !strings.Contains(err.Error(), "过期") {
		t.Fatalf("过期证书应报错，得到 %v", err)
	}
	// 改成 off 后不应再持有证书。
	if err := m.ApplySettings(Settings{Mode: ModeOff, HTTPSAddr: ":8787"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.CertInfo() != nil {
		t.Fatal("off 模式不应持有证书")
	}
}

func TestEnsureOffClearsCert(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.GenerateSelfSigned(nil); err != nil {
		t.Fatal(err)
	}
	if err := m.ApplySettings(Settings{Mode: ModeOff, HTTPSAddr: ":8787"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.CertInfo() != nil {
		t.Fatal("off 模式不应持有证书")
	}
}

func mustKeyPEM(t *testing.T) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

func mustPair(t *testing.T, notBefore, notAfter time.Time) ([]byte, []byte) {
	t.Helper()
	return mustPairSubject(t, "test", notBefore, notAfter)
}

// mustPairSubject 生成自签证书对，Subject/Issuer 都用 cn——用于测试“meta 丢失时
// 按签发者推断 ACME 来源”等只依赖 Issuer 字符串的分支。
func mustPairSubject(t *testing.T, cn string, notBefore, notAfter time.Time) ([]byte, []byte) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"test.example"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func TestLoopbackProxyPassthrough(t *testing.T) {
	called := false
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	h := RedirectHandler(app, ":8787", "panel.example.com")
	cases := []struct {
		name       string
		remoteAddr string
		host       string
		wantCode   int
		wantCalled bool
	}{
		{"nginx-domain", "127.0.0.1:5555", "panel.example.com", http.StatusOK, true},
		{"nginx-domain-port", "127.0.0.1:5555", "panel.example.com:8787", http.StatusOK, true},
		{"local-localhost", "127.0.0.1:5555", "localhost:8787", http.StatusOK, true},
		{"local-ip", "127.0.0.1:5555", "127.0.0.1:8787", http.StatusOK, true},
		{"local-ipv6", "[::1]:5555", "[::1]:8787", http.StatusOK, true},
		{"remote-domain", "203.0.113.9:5555", "panel.example.com", http.StatusMovedPermanently, false},
	}
	for _, c := range cases {
		called = false
		req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
		req.RemoteAddr = c.remoteAddr
		req.Host = c.host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != c.wantCode || called != c.wantCalled {
			t.Errorf("%s: code=%d called=%v want code=%d called=%v", c.name, w.Code, called, c.wantCode, c.wantCalled)
		}
	}
}

func TestRedirectHandlerRemotePlainHTTP(t *testing.T) {
	h := RedirectHandler(http.NotFoundHandler(), ":8787", "panel.example.com")
	req := httptest.NewRequest(http.MethodGet, "http://panel.example.com/foo?x=1", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	req.Host = "panel.example.com"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMovedPermanently {
		t.Fatalf("status=%d", w.Code)
	}
	if got, want := w.Header().Get("Location"), "https://panel.example.com:8787/foo?x=1"; got != want {
		t.Fatalf("Location=%q want %q", got, want)
	}
}

// 白名单为空时也不能把任意 Host 301 出去（开放重定向）；只有本机 Host 才放行。
func TestRedirectHandlerEmptyAllowlistIsNotOpenRedirect(t *testing.T) {
	h := RedirectHandler(http.NotFoundHandler(), ":8787")
	req := httptest.NewRequest(http.MethodGet, "http://evil.example/steal", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	req.Host = "evil.example"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("空白名单 + 外部 Host 应拒绝，code=%d loc=%q", w.Code, w.Header().Get("Location"))
	}
	if loc := w.Header().Get("Location"); strings.Contains(loc, "evil.example") {
		t.Fatalf("不应重定向到攻击者域名: %q", loc)
	}

	// 本机主机名/回环地址仍然 301（回环来源明文已直通，这里走的是远程来源）。
	for _, host := range []string{"127.0.0.1", "[::1]"} {
		req := httptest.NewRequest(http.MethodGet, "http://x/", nil)
		req.RemoteAddr = "203.0.113.9:5555"
		req.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusMovedPermanently {
			t.Errorf("host %q: 本机地址应 301，code=%d", host, w.Code)
		}
	}
}

func TestRedirectHandlerRejectsUnknownHost(t *testing.T) {
	h := RedirectHandler(http.NotFoundHandler(), ":8787", "panel.example.com")
	req := httptest.NewRequest(http.MethodGet, "http://evil.example/foo", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	req.Host = "evil.example"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("未知 Host 应拒绝，code=%d loc=%q", w.Code, w.Header().Get("Location"))
	}
	req = httptest.NewRequest(http.MethodGet, "http://panel.example.com/foo", nil)
	req.RemoteAddr = "203.0.113.9:5555"
	req.Host = "panel.example.com"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusMovedPermanently {
		t.Fatalf("配置内 Host 应 301，code=%d", w.Code)
	}
}

func TestNormalizeACME(t *testing.T) {
	c := NormalizeACME(ACMEConfig{
		Identifiers: []string{" 203.0.113.10 ", "203.0.113.10", "", "panel.example.com"},
		Email:       " ops@example.com ",
		Challenge:   " HTTP-01 ",
		HTTPAddr:    " :80 ",
	})
	if len(c.Identifiers) != 2 || c.Identifiers[0] != "203.0.113.10" || c.Identifiers[1] != "panel.example.com" {
		t.Fatalf("identifiers=%v", c.Identifiers)
	}
	if c.Email != "ops@example.com" || c.Challenge != "http-01" || c.HTTPAddr != ":80" {
		t.Fatalf("normalize=%+v", c)
	}
}

// 归一必须在提交 ACME 前完成：中文域名要转 punycode（x509 只接受 ASCII SAN），
// 大小写/尾点要收敛，否则 certMatchesIdentifiers 会一直判“标识变了”而重复下单。
func TestNormalizeACMEIdentifierCanonicalization(t *testing.T) {
	ascii, err := idna.Lookup.ToASCII("例子.com")
	if err != nil {
		t.Fatal(err)
	}
	c := NormalizeACME(ACMEConfig{
		Identifiers: []string{"例子.com", strings.ToUpper(ascii), "Panel.Example.COM.", "2001:DB8::1"},
		Email:       "a@example.com",
	})
	want := []string{ascii, "panel.example.com", "2001:db8::1"}
	if len(c.Identifiers) != len(want) {
		t.Fatalf("identifiers=%v want %v", c.Identifiers, want)
	}
	for i := range want {
		if c.Identifiers[i] != want[i] {
			t.Fatalf("identifiers[%d]=%q want %q（全部：%v）", i, c.Identifiers[i], want[i], c.Identifiers)
		}
	}
}

// 面板与 TLS-ALPN-01 挑战都占 443 时保存设置必须被拒绝（重启后每次续期都会 bind 失败）。
func TestApplySettingsRejectsALPNPortConflict(t *testing.T) {
	m := newTestManager(t)
	err := m.ApplySettings(Settings{
		Mode:      ModeACME,
		HTTPSAddr: ":443",
		ACME: ACMEConfig{
			Identifiers: []string{"panel.example.com"},
			Email:       "a@example.com",
			Challenge:   "tls-alpn-01",
			TLSAddr:     ":443",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "443") {
		t.Fatalf("443 冲突应被拒绝，err=%v", err)
	}
	// 端口写法差异（前导零）也应按数值判定。
	if err := m.ApplySettings(Settings{
		Mode:      ModeACME,
		HTTPSAddr: ":0443",
		ACME: ACMEConfig{
			Identifiers: []string{"panel.example.com"},
			Email:       "a@example.com",
			Challenge:   "tls-alpn-01",
			TLSAddr:     ":443",
		},
	}); err == nil {
		t.Fatal(":0443 与 443 数值相同，也应拒绝")
	}
	// HTTP-01 + 443 监听不冲突（挑战走 80）。
	if err := m.ApplySettings(Settings{Mode: ModeACME,
		HTTPSAddr: ":443",
		ACME: ACMEConfig{
			Identifiers: []string{"panel.example.com"},
			Email:       "a@example.com",
			Challenge:   "http-01",
			HTTPAddr:    ":80",
		},
	}); err != nil {
		t.Fatalf("HTTP-01 不应被拒绝: %v", err)
	}
	// ALPN + 其它监听端口不冲突。
	if err := m.ApplySettings(Settings{
		Mode:      ModeACME,
		HTTPSAddr: ":8787",
		ACME: ACMEConfig{
			Identifiers: []string{"panel.example.com"},
			Email:       "a@example.com",
			Challenge:   "tls-alpn-01",
			TLSAddr:     ":443",
		},
	}); err != nil {
		t.Fatalf("ALPN 与非 443 监听不应被拒绝: %v", err)
	}
}

// 新证书加载失败回滚时，meta 的来源必须一起回滚；否则旧的自签证书会被标成
// ACME，planACMEStart 认为“已有有效证书”，再也不会重试签发。
func TestInstallPairRollbackRestoresSource(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.GenerateSelfSigned([]string{"panel.example.test"}); err != nil {
		t.Fatal(err)
	}
	if got := m.CertInfo().Source; got != "selfsigned" {
		t.Fatalf("前置条件：source=%q", got)
	}
	// 模拟 installPair 已把 meta 写成新来源、随后 Load 失败。
	m.writeMeta("acme", "")
	err := m.rollbackPair("selfsigned", errors.New("模拟加载失败"))
	if err == nil || !strings.Contains(err.Error(), "模拟加载失败") {
		t.Fatalf("应返回原始错误，得到 %v", err)
	}
	info := m.CertInfo()
	if info == nil || info.Source != "selfsigned" {
		t.Fatalf("回滚后来源必须恢复为 selfsigned，得到 %+v", info)
	}
	if !strings.Contains(m.Status().LastError, "模拟加载失败") {
		t.Fatalf("回滚原因应写入 last_error：%q", m.Status().LastError)
	}
}

// meta.json 丢失时用签发者推断 ACME 来源，避免重启后重复下单消耗 LE 限额。
func TestLoadInfersACMESourceFromIssuer(t *testing.T) {
	m := newTestManager(t)
	certPEM, keyPEM := mustPairSubject(t, "Let's Encrypt", time.Now().Add(-time.Hour), time.Now().Add(60*24*time.Hour))
	if err := atomicWrite(m.path("cert.pem"), certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(m.path("key.pem"), keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(m.path("meta.json"))
	if err := m.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	info := m.CertInfo()
	if info == nil || info.Source != "acme" {
		t.Fatalf("Let's Encrypt 签发者应推断为 acme，得到 %+v", info)
	}
	if m.Status().ACME.NextRenew.IsZero() {
		t.Fatal("ACME 证书加载后应设置 NextRenew")
	}
}

type temporaryAcceptError struct{}

func (temporaryAcceptError) Error() string   { return "too many open files" }
func (temporaryAcceptError) Timeout() bool   { return false }
func (temporaryAcceptError) Temporary() bool { return true }

type flakyListener struct {
	calls int
	conn  net.Conn
	done  chan struct{}
}

func (f *flakyListener) Accept() (net.Conn, error) {
	f.calls++
	switch f.calls {
	case 1:
		return nil, temporaryAcceptError{}
	case 2:
		return f.conn, nil
	default:
		<-f.done
		return nil, net.ErrClosed
	}
}
func (f *flakyListener) Close() error   { close(f.done); return nil }
func (f *flakyListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1} }

// 首次 Accept 临时错误（fd 耗尽）后，泵必须继续 Accept，而不是让
// http.Server 永久等一个已经没人写的 channel。
func TestSniffListenerTemporaryAcceptErrorRetries(t *testing.T) {
	server, client := net.Pipe()
	fl := &flakyListener{conn: server, done: make(chan struct{})}
	sniff := &SniffListener{Listener: fl, TLSConfig: &tls.Config{}, PeekTimeout: time.Second}
	defer sniff.Close()
	go func() {
		_, _ = client.Write([]byte{0x16}) // TLS handshake record 首字节
	}()
	conn, err := sniff.Accept()
	if err != nil {
		t.Fatalf("临时 Accept 错误后应继续服务: %v", err)
	}
	_ = conn.Close()
	_ = client.Close()
}

func TestSniffListenerSlowClientDoesNotBlock(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.GenerateSelfSigned([]string{"127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	app := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	srv := &http.Server{Handler: RedirectHandler(app, addr), TLSConfig: &tls.Config{GetCertificate: m.GetCertificate}}
	sniff := &SniffListener{Listener: ln, TLSConfig: srv.TLSConfig, PeekTimeout: 2 * time.Second}
	go func() { _ = srv.Serve(sniff) }()
	defer srv.Close()

	// 一个连上但不发数据的连接，不应卡住后续请求。
	slow, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()

	client := &http.Client{Timeout: time.Second}
	resp, err := client.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("慢连接不应阻塞新请求: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
}
