package tlsmgr

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	selfSignedLeafDays = 397
	selfSignedCADays   = 3650
	rotateBeforeDays   = 30
)

// EnsureSelfSigned generates a leaf if none exists, if it is close to expiry,
// or if the requested SANs are not covered by the current leaf.
func (m *Manager) EnsureSelfSigned(extra []string) error {
	sans, err := collectSANs(extra)
	if err != nil {
		return err
	}
	info := m.CertInfo()
	if info != nil && info.NotAfter.After(time.Now().Add(rotateBeforeDays*24*time.Hour)) && coversSANs(info.SANs, sans) {
		// 叶子还必须由当前 CA 签发；只恢复/替换了 ca.pem 或 ca.key 时，
		// 旧叶子看起来仍在有效期，但客户端用新 CA 验证会失败。
		if ca, _, err := m.loadOrCreateCA(); err == nil && info.Issuer == ca.Subject.String() {
			return nil
		}
	}
	_, err = m.GenerateSelfSigned(extra)
	return err
}

// GenerateSelfSigned creates/loads a local CA and issues a new leaf.
func (m *Manager) GenerateSelfSigned(extra []string) (CertInfo, error) {
	sans, err := collectSANs(extra)
	if err != nil {
		return CertInfo{}, err
	}
	caCert, caKey, err := m.loadOrCreateCA()
	if err != nil {
		return CertInfo{}, err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return CertInfo{}, err
	}
	serial, err := randomSerial()
	if err != nil {
		return CertInfo{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"POJUN"}, CommonName: firstHostname(sans)},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(0, 0, selfSignedLeafDays),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, s := range sans {
		if ip := net.ParseIP(s); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, s)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return CertInfo{}, fmt.Errorf("签发自签证书: %w", err)
	}
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return CertInfo{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	info, err := m.installPair(leafPEM, keyPEM, "selfsigned")
	if err != nil {
		return CertInfo{}, err
	}
	return info, nil
}

func (m *Manager) loadOrCreateCA() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	m.caMu.Lock()
	defer m.caMu.Unlock()
	certPEM, cerr := os.ReadFile(m.path("ca.pem"))
	keyPEM, kerr := os.ReadFile(m.path("ca.key"))
	if cerr == nil && kerr == nil {
		if c, k, err := parseCA(certPEM, keyPEM); err == nil {
			return c, k, nil
		}
	}
	// Missing or unreadable CA: create a new one. Existing clients will need to
	// import the new CA; the status page reports that a new CA was generated.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{Organization: []string{"POJUN"}, CommonName: "POJUN Local CA"},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(0, 0, selfSignedCADays),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	keyPEMOut := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := m.saveCA(caPEM, keyPEMOut); err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

func parseCA(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	cb, _ := pem.Decode(certPEM)
	if cb == nil {
		return nil, nil, fmt.Errorf("CA 证书不是 PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	if !cert.IsCA || time.Now().After(cert.NotAfter.Add(-30*24*time.Hour)) {
		return nil, nil, fmt.Errorf("CA 已过期或不是 CA 证书")
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, nil, fmt.Errorf("CA 私钥不是 PEM")
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, nil, fmt.Errorf("CA 证书与私钥不匹配")
	}
	return cert, key, nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}

func collectSANs(extra []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(strings.TrimSuffix(s, "."))
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	if host, err := os.Hostname(); err == nil {
		add(host)
	}
	add("127.0.0.1")
	add("::1")
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP
			if ip == nil || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
				continue
			}
			add(ip.String())
		}
	}
	for _, s := range extra {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if ip := net.ParseIP(s); ip == nil && !validDNSName(s) {
			return nil, fmt.Errorf("无效的 SAN: %s", s)
		}
		add(s)
	}
	if len(out) > 100 {
		out = out[:100]
	}
	sort.Strings(out)
	return out, nil
}

func coversSANs(have, want []string) bool {
	set := map[string]bool{}
	for _, s := range have {
		set[s] = true
	}
	for _, s := range want {
		if !set[s] {
			return false
		}
	}
	return true
}

func firstHostname(sans []string) string {
	for _, s := range sans {
		if net.ParseIP(s) == nil {
			return s
		}
	}
	if len(sans) > 0 {
		return sans[0]
	}
	return "pojun"
}

func validDNSName(s string) bool {
	if len(s) == 0 || len(s) > 253 || strings.ContainsAny(s, " /\\") {
		return false
	}
	for _, part := range strings.Split(s, ".") {
		if part == "" || len(part) > 63 {
			return false
		}
	}
	return true
}
