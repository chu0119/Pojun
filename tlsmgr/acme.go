package tlsmgr

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-acme/lego/v5/acme"
	"github.com/go-acme/lego/v5/certcrypto"
	"github.com/go-acme/lego/v5/certificate"
	"github.com/go-acme/lego/v5/challenge/tlsalpn01"
	"github.com/go-acme/lego/v5/lego"
	"github.com/go-acme/lego/v5/registration"
	"golang.org/x/net/idna"
)

// NormalizeACME trims and deduplicates identifiers/email/challenge.
// It is exported so the API layer can persist exactly what will be requested.
func NormalizeACME(c ACMEConfig) ACMEConfig {
	seen := map[string]bool{}
	ids := make([]string, 0, len(c.Identifiers))
	for _, raw := range c.Identifiers {
		id := normalizeIdentifier(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	c.Identifiers = ids
	c.Email = strings.TrimSpace(c.Email)
	c.Challenge = strings.ToLower(strings.TrimSpace(c.Challenge))
	if c.Challenge == "" {
		c.Challenge = "http-01"
	}
	c.HTTPAddr = strings.TrimSpace(c.HTTPAddr)
	c.TLSAddr = strings.TrimSpace(c.TLSAddr)
	return c
}

// normalizeIdentifier 把标识归一成将要提交给 ACME 的形式：去掉尾点、IP 走
// 规范写法、域名转小写 punycode。ACME/CSR 只接受 ASCII IA5String，直接提交
// 中文域名会在签发阶段报 "SAN dNSName is malformed"；大小写/尾点不一致还会
// 让 certMatchesIdentifiers 判定成“标识变了”而反复重新下单。
func normalizeIdentifier(id string) string {
	id = strings.TrimSuffix(strings.TrimSpace(id), ".")
	if id == "" {
		return ""
	}
	if ip := net.ParseIP(id); ip != nil {
		return ip.String()
	}
	if ascii, err := idna.Lookup.ToASCII(id); err == nil && ascii != "" {
		id = ascii
	}
	return strings.ToLower(id)
}

// validateACME checks identifiers, email, challenge and ports.
func validateACME(c ACMEConfig) error {
	if len(c.Identifiers) == 0 {
		return errors.New("至少需要一个 IP 或域名")
	}
	if len(c.Identifiers) > 25 {
		return errors.New("标识最多 25 个")
	}
	ipCount, domainCount := 0, 0
	for _, id := range c.Identifiers {
		id = strings.TrimSpace(id)
		if id == "" {
			return errors.New("标识不能为空")
		}
		if net.ParseIP(id) != nil {
			ipCount++
		} else {
			domainCount++
		}
		if net.ParseIP(id) == nil {
			if numericDotted(id) {
				return fmt.Errorf("无效的 IP 地址: %s", id)
			}
			if strings.HasPrefix(id, "*.") {
				return fmt.Errorf("不支持通配符（HTTP-01/TLS-ALPN-01 无法验证）: %s", id)
			}
			if strings.Contains(id, ":") {
				return fmt.Errorf("标识不要带端口: %s", id)
			}
			if strings.Contains(id, "_") {
				return fmt.Errorf("标识不能含下划线: %s", id)
			}
			if !validDNSName(id) {
				return fmt.Errorf("无效的 IP/域名: %s", id)
			}
		}
	}
	// LE 的 IP 证书走 shortlived profile，与域名证书是两条签发路径；混填会让
	// 整单按 IP 证书处理，域名部分被拒（用户在页面上看到的是"填了合法标识却整单失败"）。
	if ipCount > 0 && domainCount > 0 {
		return errors.New("IP 与域名不能混在同一次申请：请分开申请（IP 证书使用 shortlived，有效期约 6 天）")
	}
	email := strings.TrimSpace(c.Email)
	if email == "" {
		return errors.New("ACME 需要有效邮箱")
	}
	if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
		return fmt.Errorf("ACME 邮箱无效: %s", email)
	}
	switch c.Challenge {
	case "", "http-01":
		if c.HTTPAddr == "" {
			c.HTTPAddr = ":80"
		}
		if h, _, err := net.SplitHostPort(c.HTTPAddr); err != nil {
			return fmt.Errorf("HTTP-01 监听地址无效（需 host:port，如 :80）: %w", err)
		} else if h == "localhost" {
			return errors.New("HTTP-01 不能绑定 localhost/回环地址（Let's Encrypt 从公网回源）")
		} else if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
			return errors.New("HTTP-01 不能绑定回环地址（Let's Encrypt 从公网回源）")
		}
		if p := acmePort(c.HTTPAddr, 80); p != 80 {
			return fmt.Errorf("HTTP-01 只能监听 80 端口（Let's Encrypt 固定从 80 回源），当前 %d", p)
		}
	case "tls-alpn-01":
		if c.TLSAddr == "" {
			c.TLSAddr = ":443"
		}
		if h, _, err := net.SplitHostPort(c.TLSAddr); err != nil {
			return fmt.Errorf("TLS-ALPN-01 监听地址无效（需 host:port，如 :443）: %w", err)
		} else if h == "localhost" {
			return errors.New("TLS-ALPN-01 不能绑定 localhost/回环地址（Let's Encrypt 从公网回源）")
		} else if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
			return errors.New("TLS-ALPN-01 不能绑定回环地址（Let's Encrypt 从公网回源）")
		}
		if p := acmePort(c.TLSAddr, 443); p != 443 {
			return fmt.Errorf("TLS-ALPN-01 只能监听 443 端口，当前 %d", p)
		}
	default:
		return fmt.Errorf("不支持的挑战方式: %s", c.Challenge)
	}
	return nil
}

// acmeProfile returns the required profile: IP identifiers need shortlived.
func acmeProfile(ids []string) string {
	for _, id := range ids {
		if net.ParseIP(strings.TrimSpace(id)) != nil {
			return "shortlived"
		}
	}
	return ""
}

func acmePort(addr string, def int) int {
	if _, p, err := net.SplitHostPort(addr); err == nil {
		if n, err := net.LookupPort("tcp", p); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// IssueACME obtains a new certificate. It starts a temporary challenge listener
// (and optionally opens the host firewall) and closes both before returning.
func (m *Manager) IssueACME(ctx context.Context, cfg ACMEConfig) (CertInfo, error) {
	cfg = NormalizeACME(cfg)
	if err := validateACME(cfg); err != nil {
		return CertInfo{}, err
	}
	m.renewMu.Lock()
	defer m.renewMu.Unlock()
	return m.obtainACME(ctx, cfg)
}

func (m *Manager) obtainACME(ctx context.Context, cfg ACMEConfig) (CertInfo, error) {
	ctx, cancel := context.WithCancel(ctx)
	m.setACMECancel(cancel)
	defer m.clearACMECancel()
	defer cancel()
	m.setRunning(true)
	defer m.setRunning(false)
	provider := newHTTP01Provider()
	stop, err := m.startChallenge(ctx, cfg, provider)
	if err != nil {
		m.setLastErr(err.Error())
		return CertInfo{}, err
	}
	defer stop()

	client, err := m.acmeClient(ctx, cfg)
	if err != nil {
		m.setLastErr(err.Error())
		return CertInfo{}, err
	}
	if cfg.Challenge == "tls-alpn-01" {
		host, port := splitAddr(cfg.TLSAddr, ":443")
		if err := client.Challenge.SetTLSALPN01Provider(tlsalpn01.NewProviderServer(host, port)); err != nil {
			m.setLastErr(err.Error())
			return CertInfo{}, err
		}
	} else {
		if err := client.Challenge.SetHTTP01Provider(provider); err != nil {
			m.setLastErr(err.Error())
			return CertInfo{}, err
		}
	}
	res, err := client.Certificate.Obtain(ctx, acmeObtainRequest(cfg))
	if err != nil {
		m.setLastErr("ACME 申请失败: " + err.Error())
		return CertInfo{}, fmt.Errorf("ACME 申请失败: %w", err)
	}
	info, err := m.installPair(res.Certificate, res.PrivateKey, "acme")
	if err != nil {
		m.setLastErr(err.Error())
		return CertInfo{}, err
	}
	m.setLastErr("")
	m.setNext(nextRenewTime(info))
	return info, nil
}

func acmeObtainRequest(cfg ACMEConfig) certificate.ObtainRequest {
	return certificate.ObtainRequest{
		Domains: cfg.Identifiers,
		KeyType: certcrypto.EC256,
		Profile: acmeProfile(cfg.Identifiers),
		Bundle:  true,
	}
}

// RenewACME renews the current certificate in place.
func (m *Manager) RenewACME(ctx context.Context, cfg ACMEConfig) (CertInfo, error) {
	cfg = NormalizeACME(cfg)
	if err := validateACME(cfg); err != nil {
		return CertInfo{}, err
	}
	// Renew 会从现有证书 SAN 推导域名；自签/导入证书、或标识已变更时
	// 必须重新 Obtain，否则会拿本机自签 SAN 去 ACME 下单并失败。
	if !shouldRenewACME(m.CertInfo(), cfg.Identifiers, cfg.Staging) {
		m.renewMu.Lock()
		defer m.renewMu.Unlock()
		return m.obtainACME(ctx, cfg)
	}
	m.renewMu.Lock()
	defer m.renewMu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	m.setACMECancel(cancel)
	defer m.clearACMECancel()
	defer cancel()
	certPEM, err := os.ReadFile(m.path("cert.pem"))
	if err != nil {
		return m.obtainACME(ctx, cfg)
	}
	keyPEM, err := os.ReadFile(m.path("key.pem"))
	if err != nil {
		return m.obtainACME(ctx, cfg)
	}
	m.setRunning(true)
	defer m.setRunning(false)
	provider := newHTTP01Provider()
	stop, err := m.startChallenge(ctx, cfg, provider)
	if err != nil {
		m.setLastErr(err.Error())
		return CertInfo{}, err
	}
	defer stop()
	client, err := m.acmeClient(ctx, cfg)
	if err != nil {
		m.setLastErr(err.Error())
		return CertInfo{}, err
	}
	if cfg.Challenge == "tls-alpn-01" {
		host, port := splitAddr(cfg.TLSAddr, ":443")
		if err := client.Challenge.SetTLSALPN01Provider(tlsalpn01.NewProviderServer(host, port)); err != nil {
			m.setLastErr(err.Error())
			return CertInfo{}, err
		}
	} else {
		if err := client.Challenge.SetHTTP01Provider(provider); err != nil {
			m.setLastErr(err.Error())
			return CertInfo{}, err
		}
	}
	res, err := client.Certificate.Renew(ctx, certificate.Resource{
		Domains: cfg.Identifiers, PrivateKey: keyPEM, Certificate: certPEM,
	}, &certificate.RenewOptions{Profile: acmeProfile(cfg.Identifiers), Bundle: true})
	if err != nil {
		m.setLastErr("ACME 续期失败: " + err.Error())
		return CertInfo{}, fmt.Errorf("ACME 续期失败: %w", err)
	}
	info, err := m.installPair(res.Certificate, res.PrivateKey, "acme")
	if err != nil {
		m.setLastErr(err.Error())
		return CertInfo{}, err
	}
	m.setLastErr("")
	m.setNext(nextRenewTime(info))
	return info, nil
}

func nextRenewTime(info CertInfo) time.Time {
	lifetime := info.NotAfter.Sub(info.NotBefore)
	if lifetime <= 0 {
		return time.Now().Add(24 * time.Hour)
	}
	return info.NotAfter.Add(-lifetime / 2)
}

// startChallenge starts the temporary HTTP-01 listener and the firewall lease.
// TLS-ALPN uses lego's own listener; only the firewall lease is managed here.
func (m *Manager) startChallenge(ctx context.Context, cfg ACMEConfig, provider *http01Provider) (func(), error) {
	var closers []func()
	closeAll := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}
	if cfg.Challenge != "tls-alpn-01" {
		addr := cfg.HTTPAddr
		if addr == "" {
			addr = ":80"
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("无法监听 ACME HTTP-01 端口 %s: %w（请检查端口占用/权限/防火墙）", addr, err)
		}
		srv := &http.Server{Handler: provider, ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = srv.Serve(ln) }()
		closers = append(closers, func() { _ = srv.Close() })
	}
	s := m.Settings()
	if s.FirewallAuto && m.firewall != nil {
		port := acmePort(cfg.HTTPAddr, 80)
		if cfg.Challenge == "tls-alpn-01" {
			port = acmePort(cfg.TLSAddr, 443)
		}
		lease, changed, err := m.firewall.Open(ctx, port, 15*time.Minute)
		if err != nil {
			closeAll()
			return nil, fmt.Errorf("临时放行防火墙端口 %d 失败: %w", port, err)
		}
		if changed && lease != "" {
			closers = append(closers, func() {
				c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = m.firewall.Close(c, lease)
			})
		}
	}
	return closeAll, nil
}

type acmeUser struct {
	Email string
	Key   *ecdsa.PrivateKey
	Reg   *acme.ExtendedAccount
}

func (u *acmeUser) GetEmail() string                       { return u.Email }
func (u *acmeUser) GetRegistration() *acme.ExtendedAccount { return u.Reg }
func (u *acmeUser) GetPrivateKey() crypto.Signer           { return u.Key }

func (m *Manager) acmeClient(ctx context.Context, cfg ACMEConfig) (*lego.Client, error) {
	key, err := m.acmeAccountKey(cfg.Staging)
	if err != nil {
		return nil, err
	}
	user := &acmeUser{Email: cfg.Email, Key: key}
	config := lego.NewConfig(user)
	if cfg.Staging {
		config.CADirURL = lego.DirectoryURLLetsEncryptStaging
	}
	config.UserAgent = "pojun-tls"
	client, err := lego.NewClient(config)
	if err != nil {
		return nil, err
	}
	reg, err := client.Registration.Register(ctx, registration.RegisterOptions{TermsOfServiceAgreed: true})
	if err != nil {
		return nil, fmt.Errorf("ACME 账户注册失败: %w", err)
	}
	user.Reg = reg
	return client, nil
}

func (m *Manager) acmeAccountKey(staging bool) (*ecdsa.PrivateKey, error) {
	dir := m.path("acme")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	name := "account.key"
	if staging {
		name = "account-staging.key"
	}
	path := filepath.Join(dir, name)
	if b, err := os.ReadFile(path); err == nil {
		if block, _ := pem.Decode(b); block != nil {
			if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
				return k, nil
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := atomicWrite(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

type http01Provider struct {
	mu     sync.RWMutex
	tokens map[string]string
}

func newHTTP01Provider() *http01Provider {
	return &http01Provider{tokens: map[string]string{}}
}

func (p *http01Provider) Present(_ context.Context, _ string, token, keyAuth string) error {
	p.mu.Lock()
	p.tokens[token] = keyAuth
	p.mu.Unlock()
	return nil
}

func (p *http01Provider) CleanUp(_ context.Context, _ string, token, _ string) error {
	p.mu.Lock()
	delete(p.tokens, token)
	p.mu.Unlock()
	return nil
}

func (p *http01Provider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const prefix = "/.well-known/acme-challenge/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		http.NotFound(w, r)
		return
	}
	token := strings.TrimPrefix(r.URL.Path, prefix)
	p.mu.RLock()
	keyAuth, ok := p.tokens[token]
	p.mu.RUnlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(keyAuth))
}

func splitAddr(addr, def string) (string, string) {
	if strings.TrimSpace(addr) == "" {
		addr = def
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", strings.TrimPrefix(addr, ":")
	}
	return host, port
}

// numericDotted 识别“看起来像 IP 但解析失败”的输入（如 999.1.1.1），
// 避免把它当成合法域名提交给 ACME。
func numericDotted(s string) bool {
	if !strings.Contains(s, ".") {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
}
