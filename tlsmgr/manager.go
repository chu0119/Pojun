// Package tlsmgr manages POJUN's TLS certificates: self-signed CA, imported
// PEM certificates, and Let's Encrypt ACME (including IP certificates).
//
// The manager owns files under <data>/tls and never exposes private key bytes
// through Status or any API. It is safe for concurrent GetCertificate calls.
package tlsmgr

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Mode selects how the HTTPS listener obtains its certificate.
type Mode string

const (
	ModeOff        Mode = "off"
	ModeSelfSigned Mode = "selfsigned"
	ModeImport     Mode = "import"
	ModeACME       Mode = "acme"
)

// ACMEConfig is the persisted ACME request configuration.
type ACMEConfig struct {
	Identifiers []string `json:"identifiers"`
	Email       string   `json:"email"`
	Staging     bool     `json:"staging"`
	Challenge   string   `json:"challenge"` // http-01 | tls-alpn-01
	HTTPAddr    string   `json:"http_addr"`
	TLSAddr     string   `json:"tls_addr"`
}

// Settings is the TLS-related subset of system settings.
type Settings struct {
	Mode           Mode       `json:"mode"`
	HTTPSAddr      string     `json:"https_addr"`
	HSTS           bool       `json:"hsts"`
	SelfSignedSANs []string   `json:"self_signed_sans"`
	FirewallAuto   bool       `json:"firewall_auto"`
	ACME           ACMEConfig `json:"acme"`
}

// CertInfo is the non-secret description of the active certificate.
type CertInfo struct {
	Subject   string    `json:"subject"`
	Issuer    string    `json:"issuer"`
	SANs      []string  `json:"sans"`
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
	Source    string    `json:"source"`
}

// Status is returned to the settings UI.
type Status struct {
	Mode         Mode       `json:"mode"`
	HTTPSAddr    string     `json:"https_addr"`
	Cert         *CertInfo  `json:"cert,omitempty"`
	CAvailable   bool       `json:"ca_available"`
	LastError    string     `json:"last_error,omitempty"`
	NeedsRestart bool       `json:"needs_restart"`
	ACME         ACMEStatus `json:"acme"`
}

// ACMEStatus is the live ACME state.
type ACMEStatus struct {
	Running     bool      `json:"running"`
	LastError   string    `json:"last_error,omitempty"`
	NextRenew   time.Time `json:"next_renew,omitempty"`
	Identifiers []string  `json:"identifiers,omitempty"`
	Email       string    `json:"email,omitempty"`
	Staging     bool      `json:"staging,omitempty"`
	Challenge   string    `json:"challenge,omitempty"`
}

// Firewall is implemented by the hostd-backed firewall helper. A nil firewall
// means "do not touch the host firewall".
type Firewall interface {
	// Open temporarily allows port (TCP) for ttl and returns a lease id.
	// changed is false when the port was already allowed.
	Open(ctx context.Context, port int, ttl time.Duration) (leaseID string, changed bool, err error)
	Close(ctx context.Context, leaseID string) error
}

// Manager owns certificate state for one data directory.
type Manager struct {
	dir string

	mu       sync.RWMutex
	cert     *tls.Certificate
	info     *CertInfo
	caPEM    []byte
	settings Settings
	lastErr  string
	running  bool
	nextRun  time.Time
	acmeStop context.CancelFunc
	renewMu  sync.Mutex
	// installMu 串行化 Import/GenerateSelfSigned/ACME 的证书写入，避免 key/cert
	// 交叉写与 .bak 回滚互相覆盖；caMu 单独保护本地 CA 的首次创建。
	installMu sync.Mutex
	caMu      sync.Mutex

	firewall Firewall
}

// New creates a manager rooted at <dataDir>/tls.
func New(dataDir string) (*Manager, error) {
	dir := filepath.Join(dataDir, "tls")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建 TLS 目录: %w", err)
	}
	_ = os.Chmod(dir, 0o700)
	m := &Manager{dir: dir, settings: Settings{Mode: ModeOff, HTTPSAddr: ":8787"}}
	if err := m.Load(); err != nil {
		// A corrupt file is reported through Status, not fatal: the operator can
		// regenerate/import from the UI.
		m.setLastErr(err.Error())
	}
	m.loadMetaLastErr()
	return m, nil
}

// SetFirewall installs the optional host firewall helper.
func (m *Manager) SetFirewall(f Firewall) { m.firewall = f }

// CancelACME cancels the in-flight ACME request (API-triggered, startup or
// renewal loop). Returns false when nothing is running.
func (m *Manager) CancelACME() bool {
	m.mu.Lock()
	cancel := m.acmeStop
	m.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

func (m *Manager) setACMECancel(cancel context.CancelFunc) {
	m.mu.Lock()
	m.acmeStop = cancel
	m.mu.Unlock()
}

func (m *Manager) clearACMECancel() {
	m.mu.Lock()
	m.acmeStop = nil
	m.mu.Unlock()
}

// Dir returns the TLS storage directory.
func (m *Manager) Dir() string { return m.dir }

// Settings returns a copy of the current settings.
func (m *Manager) Settings() Settings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.settings
}

// ApplySettings validates and stores settings. Certificate generation/loading is
// done by Ensure; changing mode or HTTPSAddr requires a restart.
func (m *Manager) ApplySettings(s Settings) error {
	s.Mode = Mode(strings.ToLower(strings.TrimSpace(string(s.Mode))))
	switch s.Mode {
	case ModeOff, ModeSelfSigned, ModeImport, ModeACME:
	default:
		return fmt.Errorf("未知 TLS 模式: %s", s.Mode)
	}
	if strings.TrimSpace(s.HTTPSAddr) == "" {
		s.HTTPSAddr = ":8787"
	}
	if _, _, err := splitHostPort(s.HTTPSAddr); err != nil {
		return fmt.Errorf("HTTPS 监听地址无效: %w", err)
	}
	// TLS-ALPN-01 由 lego 自己独占 443 起挑战监听；面板若也监听 443，重启后
	// 每次续期都会 bind 失败（6 小时一轮，静默消耗重试）。保存时就拒绝，
	// 让用户改用 HTTP-01 或把面板换到其它端口。
	if s.Mode == ModeACME && strings.EqualFold(strings.TrimSpace(s.ACME.Challenge), "tls-alpn-01") {
		if _, port, err := splitHostPort(s.HTTPSAddr); err == nil {
			if n, convErr := strconv.Atoi(port); convErr == nil && n == 443 {
				return errors.New("TLS-ALPN-01 需要独占 443 端口，但 HTTPS 监听也配置为 443；请改用 HTTP-01 或把 HTTPS 监听换到其它端口")
			}
		}
	}
	// ACME 允许先保存空配置（页面切到 ACME 但还没填标识），此时不申请、
	// 用自签兜底；真正申请时 IssueACME 会完整校验。
	if s.Mode == ModeACME && len(s.ACME.Identifiers) > 0 {
		if err := validateACME(s.ACME); err != nil {
			return err
		}
	}
	m.mu.Lock()
	m.settings = s
	m.mu.Unlock()
	return nil
}

// Load reads cert.pem/key.pem from disk if present. A missing pair is not an
// error (Status.Cert stays nil); a malformed pair is.
func (m *Manager) Load() error {
	certPEM, err := os.ReadFile(m.path("cert.pem"))
	if errors.Is(err, os.ErrNotExist) {
		m.mu.Lock()
		m.cert, m.info = nil, nil
		m.mu.Unlock()
		return nil
	}
	if err != nil {
		return err
	}
	keyPEM, err := os.ReadFile(m.path("key.pem"))
	if err != nil {
		return fmt.Errorf("读取私钥: %w", err)
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("证书/私钥不匹配或格式错误: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return fmt.Errorf("解析证书: %w", err)
	}
	cert.Leaf = leaf
	source := "file"
	if b, err := os.ReadFile(m.path("meta.json")); err == nil {
		var mt meta
		if json.Unmarshal(b, &mt) == nil {
			if mt.Source != "" {
				source = mt.Source
			}
			if mt.LastError != "" {
				m.setLastErr(mt.LastError)
			}
		}
	}
	if source == "file" && strings.Contains(strings.ToLower(leaf.Issuer.String()), "let's encrypt") {
		// meta.json 丢失/从备份恢复时，用签发者推断 ACME 来源，避免每次重启
		// 都重新下单消耗 Let's Encrypt 限额。
		source = "acme"
	}
	info := certInfoFromLeaf(leaf, source)
	ca, _ := os.ReadFile(m.path("ca.pem"))
	m.mu.Lock()
	m.cert, m.info, m.caPEM = &cert, &info, ca
	m.mu.Unlock()
	if source == "acme" {
		m.setNext(nextRenewTime(info))
	}
	return nil
}

// Ensure prepares the active certificate according to settings. It is called
// before the HTTPS listener starts.
func (m *Manager) Ensure(ctx context.Context) error {
	s := m.Settings()
	switch s.Mode {
	case ModeOff:
		m.mu.Lock()
		m.cert, m.info = nil, nil
		m.mu.Unlock()
		return nil
	case ModeSelfSigned:
		return m.EnsureSelfSigned(s.SelfSignedSANs)
	case ModeImport:
		if err := m.Load(); err != nil {
			return err
		}
		info := m.CertInfo()
		if info == nil {
			return errors.New("导入模式没有证书，请先上传证书与私钥")
		}
		if info.NotAfter.Before(time.Now()) {
			return fmt.Errorf("导入证书已过期（NotAfter=%s），请重新导入", info.NotAfter.Format(time.RFC3339))
		}
		return nil
	case ModeACME:
		// 已有 ACME 证书且不在续期窗口：直接起服务。自签/导入证书只是兜底，
		// 不能被当成“ACME 已完成”——否则切换模式或上次申请失败后，重启永远
		// 不会再发 ACME 请求。
		_ = m.Load()
		now := time.Now()
		info := m.CertInfo()
		plan := planACMEStart(info, s.ACME.Identifiers, s.ACME.Staging, now)
		if plan == acmeStartNone {
			return nil
		}
		// 没有证书或已过期时先自签，保证面板能起来；随后仍按 plan 发 ACME。
		if info == nil || info.NotAfter.Before(now) {
			if err := m.EnsureSelfSigned(s.SelfSignedSANs); err != nil {
				return err
			}
		}
		if len(s.ACME.Identifiers) > 0 {
			go func(plan acmeStartAction) {
				var err error
				if plan == acmeStartRenew {
					_, err = m.RenewACME(ctx, s.ACME)
				} else {
					_, err = m.IssueACME(ctx, s.ACME)
				}
				if err != nil {
					log.Printf("[tls] 启动时 ACME 申请/续期失败（已用现有证书继续服务）：%v", err)
				}
			}(plan)
		}
		return nil
	default:
		return fmt.Errorf("未知 TLS 模式: %s", s.Mode)
	}
}

// acmeStartAction 是启动时对 ACME 的处理：none=已有有效 ACME 证书；
// issue=需要新申请（无证书/自签/导入/过期）；renew=续期现有 ACME 证书。
type acmeStartAction int

const (
	acmeStartNone acmeStartAction = iota
	acmeStartIssue
	acmeStartRenew
)

// planACMEStart 只看证书来源、标识集合与剩余寿命，不发网络请求，便于单测。
func planACMEStart(info *CertInfo, ids []string, staging bool, now time.Time) acmeStartAction {
	if info == nil || info.Source != "acme" {
		return acmeStartIssue
	}
	if !certMatchesIdentifiers(info, ids) || !certMatchesStaging(info, staging) {
		return acmeStartIssue
	}
	if info.NotAfter.Before(now) {
		return acmeStartIssue
	}
	if acmeRenewDue(info, now) {
		return acmeStartRenew
	}
	return acmeStartNone
}

// acmeRenewDue 按“剩余寿命 ≤ 寿命一半”判定续期（LE 与设计文档口径）：90 天
// 证书第 45 天续、6 天 IP 证书第 3 天续。旧实现只在最后 24 小时才续，6 小时
// 一轮最多 4 次机会，任一失败就直接过期；HSTS 已下发时浏览器无法回退。
// 同时保留 24h 内的“迫切”判定，避免证书寿命异常时不续。
//
// NotBefore 缺失（旧 meta/零值）时按 LE 常规 90 天估计——直接算 NotAfter-零值
// 会得到几千年寿命，续期永远不触发。
func acmeRenewDue(info *CertInfo, now time.Time) bool {
	if info == nil {
		return false
	}
	lifetime := info.NotAfter.Sub(info.NotBefore)
	if info.NotBefore.IsZero() || lifetime <= 0 {
		lifetime = 90 * 24 * time.Hour
	}
	left := info.NotAfter.Sub(now)
	return left <= lifetime/2 || left <= 24*time.Hour
}

// certMatchesIdentifiers 判断证书 SAN 与本次 ACME 标识是否完全一致。
// Renew 会从证书 SAN 重新推导域名；集合不一致时继续 Renew 会申请旧/多余标识。
func certMatchesIdentifiers(info *CertInfo, ids []string) bool {
	if info == nil {
		return false
	}
	want := map[string]bool{}
	for _, id := range ids {
		id = normalizeIdentifier(id)
		if id != "" {
			want[id] = true
		}
	}
	if len(want) == 0 {
		return false
	}
	have := map[string]bool{}
	for _, s := range info.SANs {
		have[normalizeIdentifier(s)] = true
	}
	if len(have) != len(want) {
		return false
	}
	for id := range want {
		if !have[id] {
			return false
		}
	}
	return true
}

// shouldRenewACME 决定能否走 lego 的 Renew；自签/导入证书或标识变更必须重新 Obtain。
func shouldRenewACME(info *CertInfo, ids []string, staging bool) bool {
	return info != nil && info.Source == "acme" && certMatchesIdentifiers(info, ids) && certMatchesStaging(info, staging)
}

func certMatchesStaging(info *CertInfo, staging bool) bool {
	if info == nil {
		return false
	}
	isStaging := strings.Contains(strings.ToUpper(info.Issuer), "STAGING")
	return isStaging == staging
}

// GetCertificate implements tls.Config.GetCertificate.
func (m *Manager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cert == nil {
		return nil, errors.New("TLS 证书尚未就绪")
	}
	return m.cert, nil
}

// CertInfo returns a copy of the active certificate description.
func (m *Manager) CertInfo() *CertInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.info == nil {
		return nil
	}
	cp := *m.info
	cp.SANs = append([]string(nil), m.info.SANs...)
	return &cp
}

// CA returns the self-signed CA certificate PEM, if present.
func (m *Manager) CA() []byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]byte(nil), m.caPEM...)
}

// Status returns the UI-facing status.
func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st := Status{
		Mode:       m.settings.Mode,
		HTTPSAddr:  m.settings.HTTPSAddr,
		CAvailable: m.settings.Mode == ModeSelfSigned && len(m.caPEM) > 0,
		LastError:  m.lastErr,
		ACME: ACMEStatus{
			Running:     m.running,
			LastError:   m.lastErr,
			NextRenew:   m.nextRun,
			Identifiers: append([]string(nil), m.settings.ACME.Identifiers...),
			Email:       m.settings.ACME.Email,
			Staging:     m.settings.ACME.Staging,
			Challenge:   m.settings.ACME.Challenge,
		},
	}
	if m.info != nil {
		cp := *m.info
		cp.SANs = append([]string(nil), m.info.SANs...)
		st.Cert = &cp
	}
	return st
}

// RenewDue reports whether the active certificate is past half of its lifetime.
func (m *Manager) RenewDue(now time.Time) bool {
	return acmeRenewDue(m.CertInfo(), now)
}

// StartRenewLoop runs a 6-hour renewal check for ACME mode. settingsFn returns
// the current settings and whether ACME is enabled.
func (m *Manager) StartRenewLoop(ctx context.Context, settingsFn func() (Settings, bool)) {
	go func() {
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s, ok := settingsFn()
				if !ok {
					continue
				}
				switch s.Mode {
				case ModeSelfSigned:
					// 覆盖主机名/网卡 IP 变化；未到轮换期且 SAN 未变时是 no-op。
					if err := m.EnsureSelfSigned(s.SelfSignedSANs); err != nil {
						m.setLastErr("自签证书检查失败: " + err.Error())
					}
				case ModeACME:
					// 首次申请失败后如果停在自签兜底，RenewDue 对 397 天
					// 自签永远为 false；这里按来源/标识/寿命统一决定 issue/renew，
					// 让 6 小时循环继续重试。
					plan := planACMEStart(m.CertInfo(), s.ACME.Identifiers, s.ACME.Staging, time.Now())
					if plan == acmeStartNone {
						continue
					}
					var err error
					if plan == acmeStartRenew {
						_, err = m.RenewACME(ctx, s.ACME)
					} else {
						_, err = m.IssueACME(ctx, s.ACME)
					}
					if err != nil {
						m.setLastErr("ACME 申请/续期失败: " + err.Error())
					}
				}
			}
		}
	}()
}

// SetLastError 供启动流程记录“证书准备失败”的真实原因（重启后状态页可见）。
// 与内部 setLastErr 同源：内存 + meta 持久化。
func (m *Manager) SetLastError(msg string) { m.setLastErr(msg) }

func (m *Manager) setLastErr(msg string) {
	m.mu.Lock()
	m.lastErr = msg
	source := ""
	if m.info != nil {
		source = m.info.Source
	}
	m.mu.Unlock()
	// 持久化上次错误，重启后状态页还能看到失败原因（meta 只含诊断信息）。
	// 还没有任何证书时也要落盘：首次申请就失败（端口占用/校验失败）是最需要
	// 重启后仍能看到原因的场景；"none" 只是占位，后续 installPair 会改写。
	if source == "" {
		source = "none"
	}
	m.writeMeta(source, msg)
}

func (m *Manager) setRunning(v bool) {
	m.mu.Lock()
	m.running = v
	m.mu.Unlock()
}

func (m *Manager) setNext(t time.Time) {
	m.mu.Lock()
	m.nextRun = t
	m.mu.Unlock()
}

func (m *Manager) path(name string) string { return filepath.Join(m.dir, name) }

func certInfoFromLeaf(leaf *x509.Certificate, source string) CertInfo {
	sans := append([]string(nil), leaf.DNSNames...)
	for _, ip := range leaf.IPAddresses {
		sans = append(sans, ip.String())
	}
	if len(sans) == 0 && leaf.Subject.CommonName != "" {
		sans = []string{leaf.Subject.CommonName}
	}
	return CertInfo{
		Subject:   leaf.Subject.String(),
		Issuer:    leaf.Issuer.String(),
		SANs:      sans,
		NotBefore: leaf.NotBefore,
		NotAfter:  leaf.NotAfter,
		Source:    source,
	}
}

func splitHostPort(addr string) (host, port string, err error) {
	host, port, err = net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return "", "", err
	}
	if port == "" {
		return "", "", errors.New("端口为空")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", "", fmt.Errorf("端口无效: %s", port)
	}
	if host != "" {
		if ip := net.ParseIP(host); ip == nil && !validDNSName(host) {
			return "", "", fmt.Errorf("监听地址主机无效: %s", host)
		}
	}
	return host, port, nil
}

// meta is persisted next to the certificate for diagnostics. It never contains
// private key material.
type meta struct {
	Source    string    `json:"source"`
	UpdatedAt time.Time `json:"updated_at"`
	LastError string    `json:"last_error,omitempty"`
}

// loadMetaLastErr 在还没有证书（cert.pem 不存在，Load 提前返回）时也能把
// 上次失败原因带回 Status，避免用户重启后只看到“无错误”。
func (m *Manager) loadMetaLastErr() {
	m.mu.RLock()
	existing := m.lastErr
	m.mu.RUnlock()
	if existing != "" {
		return
	}
	b, err := os.ReadFile(m.path("meta.json"))
	if err != nil {
		return
	}
	var mt meta
	if json.Unmarshal(b, &mt) != nil || mt.LastError == "" {
		return
	}
	m.mu.Lock()
	if m.lastErr == "" {
		m.lastErr = mt.LastError
	}
	m.mu.Unlock()
}

func (m *Manager) writeMeta(source string, lastErr string) {
	b, _ := json.MarshalIndent(meta{Source: source, UpdatedAt: time.Now().UTC(), LastError: lastErr}, "", "  ")
	_ = atomicWrite(m.path("meta.json"), b, 0o600)
}
