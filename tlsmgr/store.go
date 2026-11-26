package tlsmgr

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// atomicWrite writes data to path through a temp file + fsync + rename, so a
// crash can never leave a half-written certificate or key.
func atomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var suffix [8]byte
	_, _ = rand.Read(suffix[:])
	tmp := path + ".tmp-" + hex.EncodeToString(suffix[:])
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	cerr := f.Close()
	if werr != nil {
		_ = os.Remove(tmp)
		return werr
	}
	if cerr != nil {
		_ = os.Remove(tmp)
		return cerr
	}
	if err := os.Chmod(tmp, mode); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// backupFile copies an existing file to <path>.bak (best effort).
func backupFile(path string) {
	src, err := os.Open(path)
	if err != nil {
		return
	}
	defer src.Close()
	dst, err := os.OpenFile(path+".bak", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	_, _ = io.Copy(dst, src)
	_ = dst.Sync()
	_ = dst.Close()
}

// installPair atomically stores a certificate/key pair and reloads it.
func (m *Manager) installPair(certPEM, keyPEM []byte, source string) (CertInfo, error) {
	m.installMu.Lock()
	defer m.installMu.Unlock()
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		return CertInfo{}, fmt.Errorf("证书或私钥为空")
	}
	// 记录安装前的来源：新证书加载失败要回滚，meta 也必须一起回滚（见 rollbackPair）。
	prevSource := ""
	if info := m.CertInfo(); info != nil {
		prevSource = info.Source
	}
	backupFile(m.path("cert.pem"))
	backupFile(m.path("key.pem"))
	// Write the key first: a crash between the two leaves the old cert with the
	// new key, which Load rejects, rather than a new cert with a missing key.
	if err := atomicWrite(m.path("key.pem"), keyPEM, 0o600); err != nil {
		return CertInfo{}, fmt.Errorf("写入私钥: %w", err)
	}
	if err := atomicWrite(m.path("cert.pem"), certPEM, 0o644); err != nil {
		return CertInfo{}, fmt.Errorf("写入证书: %w", err)
	}
	// 先写 meta 再 Load：Load 会按 meta 还原来源（selfsigned/import/acme）。
	m.writeMeta(source, "")
	if err := m.Load(); err != nil {
		// 新证书加载失败：回滚到 .bak，保证服务还能用旧证书继续跑。
		return CertInfo{}, m.rollbackPair(prevSource, err)
	}
	m.setLastErr("")
	info := m.CertInfo()
	if info == nil {
		return CertInfo{}, fmt.Errorf("证书加载后不可用")
	}
	return *info, nil
}

// rollbackPair 把 cert/key 恢复到 .bak，并把 meta 的来源写回安装前的值。
// meta 在 installPair 里已先写成新来源；只恢复文件不恢复 meta 会把旧的自签/
// 导入证书标记成 ACME，planACMEStart 随后会认为“已有有效 ACME 证书”，
// 失败后再也不重试（一直服务自签证书却显示 ACME）。
// 返回 nil 表示回滚后的证书可用；否则返回原始错误。
func (m *Manager) rollbackPair(prevSource string, cause error) error {
	restoreBackup(m.path("key.pem"))
	restoreBackup(m.path("cert.pem"))
	m.writeMeta(prevSource, cause.Error())
	if rerr := m.Load(); rerr != nil {
		return cause
	}
	return fmt.Errorf("%w（已回滚到上一份证书）", cause)
}

func (m *Manager) saveCA(certPEM, keyPEM []byte) error {
	backupFile(m.path("ca.key"))
	backupFile(m.path("ca.pem"))
	if err := atomicWrite(m.path("ca.key"), keyPEM, 0o600); err != nil {
		return err
	}
	if err := atomicWrite(m.path("ca.pem"), certPEM, 0o644); err != nil {
		restoreBackup(m.path("ca.key"))
		return err
	}
	m.mu.Lock()
	m.caPEM = append([]byte(nil), certPEM...)
	m.mu.Unlock()
	return nil
}

// restoreBackup 把 <path>.bak 复制回 <path>（存在时）。
func restoreBackup(path string) {
	b, err := os.ReadFile(path + ".bak")
	if err != nil {
		return
	}
	_ = atomicWrite(path, b, 0o600)
}
