package tlsmgr

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"
	"time"
)

const maxImportBytes = 1 << 20 // 1 MiB per file

// Import validates and stores an unencrypted PEM certificate chain + key.
func (m *Manager) Import(certPEM, keyPEM []byte) (CertInfo, error) {
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return CertInfo{}, fmt.Errorf("证书和私钥都不能为空")
	}
	if len(certPEM) > maxImportBytes || len(keyPEM) > maxImportBytes {
		return CertInfo{}, fmt.Errorf("证书或私钥超过 1 MiB 上限")
	}
	if block, _ := pem.Decode(keyPEM); block == nil {
		return CertInfo{}, fmt.Errorf("私钥不是 PEM 格式")
	} else if strings.Contains(strings.ToUpper(block.Type), "ENCRYPTED") {
		return CertInfo{}, fmt.Errorf("不支持加密私钥，请上传未加密的 PEM 私钥")
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return CertInfo{}, fmt.Errorf("证书与私钥不匹配或格式错误: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return CertInfo{}, fmt.Errorf("解析证书: %w", err)
	}
	now := time.Now()
	if leaf.NotAfter.Before(now) {
		return CertInfo{}, fmt.Errorf("证书已过期（NotAfter=%s）", leaf.NotAfter.Format(time.RFC3339))
	}
	if leaf.NotBefore.After(now.Add(5 * time.Minute)) {
		return CertInfo{}, fmt.Errorf("证书尚未生效（NotBefore=%s），请检查系统时间", leaf.NotBefore.Format(time.RFC3339))
	}
	// 常见误传：把 CA/根证书当站点证书导入。浏览器只接受叶子证书，
	// 这样的“导入成功”现场表现为“证书应用不上”。
	if leaf.IsCA {
		return CertInfo{}, fmt.Errorf("这是一张 CA 证书（IsCA=true），请导入站点叶子证书（含中间证书的 fullchain 也可）")
	}
	if len(leaf.ExtKeyUsage) > 0 {
		serverAuth := false
		for _, u := range leaf.ExtKeyUsage {
			if u == x509.ExtKeyUsageServerAuth || u == x509.ExtKeyUsageAny {
				serverAuth = true
				break
			}
		}
		if !serverAuth {
			return CertInfo{}, fmt.Errorf("证书用途不含 TLS 服务器认证（ExtKeyUsage），不能用于 HTTPS 监听")
		}
	}
	// 只给叶子、缺中间证书：不是硬错误（自建 CA 的客户端已信任根时可接受），
	// 但要在状态里留痕，否则用户只会看到浏览器“无法验证第一张证书”。
	missingIntermediate := len(cert.Certificate) == 1 && leaf.Issuer.String() != leaf.Subject.String()
	cert.Leaf = leaf
	info, err := m.installPair(certPEM, keyPEM, "import")
	if err != nil {
		return CertInfo{}, err
	}
	if missingIntermediate {
		m.setLastErr("导入的证书只有叶子、没有中间证书；若浏览器报“无法验证第一张证书”，请粘贴含中间证书的完整链")
	}
	return info, nil
}
