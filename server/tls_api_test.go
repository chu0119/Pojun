package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTLSUrls(t *testing.T) {
	urls := tlsURLs(":8787")
	if len(urls) == 0 {
		t.Fatal("应至少给出一个访问地址")
	}
	for _, u := range urls {
		if !strings.HasPrefix(u, "https://") || !strings.HasSuffix(u, ":8787") {
			t.Fatalf("地址格式错误: %s", u)
		}
		if strings.Contains(u, "127.0.0.1") || strings.Contains(u, "[::1]") {
			t.Fatalf("不应包含回环地址: %s", u)
		}
	}
	if tlsURLs("bad") != nil {
		t.Fatal("无端口应返回 nil")
	}
}

func TestSchemeForTLS(t *testing.T) {
	if got := schemeForTLS("off", true); got != "http" {
		t.Fatalf("off 应为 http，得到 %q", got)
	}
	if got := schemeForTLS("acme", false); got != "http" {
		t.Fatalf("证书未生效（回落明文）应为 http，得到 %q", got)
	}
	if got := schemeForTLS("selfsigned", true); got != "https" {
		t.Fatalf("TLS 已生效应为 https，得到 %q", got)
	}
	if got := tlsURLsWithScheme(":9000", "http"); len(got) == 0 || !strings.HasPrefix(got[0], "http://") || !strings.HasSuffix(got[0], ":9000") {
		t.Fatalf("http urls=%v", got)
	}
}

func TestIsHostdUnavailable(t *testing.T) {
	if isHostdUnavailable(nil) {
		t.Fatal("nil 不应判为不可用")
	}
	if !isHostdUnavailable(errors.New("dial unix /run/pojun/hostd.sock: connect: no such file or directory")) {
		t.Fatal("缺少 socket 应判为不可用")
	}
	if isHostdUnavailable(errors.New("宿主助手返回 HTTP 500")) {
		t.Fatal("业务错误不应被吞掉")
	}
}

func TestSecurityHeadersHSTSGate(t *testing.T) {
	old := tlsHSTS.Load()
	defer tlsHSTS.Store(old)

	h := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.TLS = &tls.ConnectionState{}
	w := httptest.NewRecorder()
	tlsHSTS.Store(true)
	h.ServeHTTP(w, req)
	if w.Header().Get("Strict-Transport-Security") == "" {
		t.Fatal("开启时应下发 HSTS")
	}

	w = httptest.NewRecorder()
	tlsHSTS.Store(false)
	h.ServeHTTP(w, req)
	if got := w.Header().Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("关闭时不应下发 HSTS，得到 %q", got)
	}

	// 明文请求永不下发。
	w = httptest.NewRecorder()
	tlsHSTS.Store(true)
	plain := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(w, plain)
	if w.Header().Get("Strict-Transport-Security") != "" {
		t.Fatal("明文请求不应下发 HSTS")
	}
}

func TestTLSSaveSkipsTrialBindOnRuntimeAddr(t *testing.T) {
	m, err := NewManager(tempDir(t), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	td := tempDir(t)
	s := New(context.Background(), m, td, td, td)

	// 占住一个真实端口并把它声明为“当前运行地址”，模拟 -addr 与已保存设置不一致
	// 时的状态页：输入框显示运行地址，用户只改 HSTS 也会全量提交该地址。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	s.SetTLSRuntime(addr)

	body := fmt.Sprintf(`{"mode":"selfsigned","https_addr":%q}`, addr)
	req := httptest.NewRequest(http.MethodPost, "/api/tls/settings", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.tlsSaveSettings(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("保存设置应跳过对运行地址的试绑，得到 %d: %s", w.Code, w.Body.String())
	}

	// 反向保护：提交一个确实被占用、又不是运行地址的端口，仍必须 400 并回滚。
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen2: %v", err)
	}
	defer ln2.Close()
	before := s.TLSSettings().HTTPSAddr
	body2 := fmt.Sprintf(`{"mode":"selfsigned","https_addr":%q}`, ln2.Addr().String())
	req2 := httptest.NewRequest(http.MethodPost, "/api/tls/settings", strings.NewReader(body2))
	w2 := httptest.NewRecorder()
	s.tlsSaveSettings(w2, req2)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("占用端口应 400，得到 %d: %s", w2.Code, w2.Body.String())
	}
	if after := s.TLSSettings().HTTPSAddr; after != before {
		t.Fatalf("失败后应回滚地址：before=%s after=%s", before, after)
	}
}

// HTTPS 是显式 opt-in：没有 tls_mode 时保持 off，不在升级时自动启用。
// 显式保存 selfsigned（PUT /api/tls/settings 或 POST /api/tls/selfsigned）后
// 重启/Ensure 才生成证书；-tls=off 只覆盖本次进程，不写库。
func TestTLSDefaultsOffUntilExplicitlyEnabled(t *testing.T) {
	m, err := NewManager(tempDir(t), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	// 共享测试库里可能已有 TLS 设置（其他用例建过 Server），先清干净。
	if _, err := m.pg.Exec(`DELETE FROM settings WHERE key IN ($1,$2,$3,$4,$5,$6,$7)`,
		settingTLSMode, settingTLSAddr, settingTLSHSTS, settingTLSSANs,
		settingTLSFirewallAuto, settingTLSACME, "tls_migration_https_v1"); err != nil {
		t.Fatalf("清理设置: %v", err)
	}
	td := tempDir(t)
	s := New(context.Background(), m, td, td, td)
	if got := s.TLSSettings().Mode; got != "off" {
		t.Fatalf("默认应为 off（纯 HTTP），得到 %q", got)
	}
	if _, ok, _ := m.pg.GetSetting("tls_migration_https_v1"); ok {
		t.Fatal("不应再写入自动启用迁移标记")
	}
	if addr := s.TLSEffectiveAddr("127.0.0.1:9999"); addr != "127.0.0.1:9999" {
		t.Fatalf("off 模式应沿用 -addr，得到 %q", addr)
	}

	// 显式 PUT 设置（设置页「保存设置」）→ 落库，重启后仍是 selfsigned。
	req := httptest.NewRequest(http.MethodPut, "/api/tls/settings", strings.NewReader(`{"mode":"selfsigned"}`))
	w := httptest.NewRecorder()
	s.tlsSaveSettings(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("保存 selfsigned 失败: %d %s", w.Code, w.Body.String())
	}
	if got := s.TLSSettings().Mode; got != "selfsigned" {
		t.Fatalf("保存后应为 selfsigned，得到 %q", got)
	}
	s2 := New(context.Background(), m, td, td, td)
	if got := s2.TLSSettings().Mode; got != "selfsigned" {
		t.Fatalf("显式启用后重启应保持 selfsigned，得到 %q", got)
	}
	// 主程序在监听前调用 EnsureTLS：自签证书此时才生成。
	s2.EnsureTLS(context.Background())
	if info := s2.TLSManager().CertInfo(); info == nil || info.Source != "selfsigned" {
		t.Fatalf("显式启用后应生成自签证书，得到 %+v", info)
	}

	// 设置页「启用 HTTPS（自签）」按钮走 POST /api/tls/selfsigned：立即生成并落库。
	s3 := New(context.Background(), m, td, td, td)
	req2 := httptest.NewRequest(http.MethodPost, "/api/tls/selfsigned", strings.NewReader(`{"sans":[]}`))
	w2 := httptest.NewRecorder()
	s3.tlsSelfSigned(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("selfsigned 端点失败: %d %s", w2.Code, w2.Body.String())
	}
	if info := s3.TLSManager().CertInfo(); info == nil || info.Source != "selfsigned" {
		t.Fatalf("一键启用后应有自签证书，得到 %+v", info)
	}

	// -tls=off 逃生阀：只覆盖内存，库里的 selfsigned 保持不变。
	if err := s3.OverrideTLSMode("off"); err != nil {
		t.Fatal(err)
	}
	if got := s3.TLSSettings().Mode; got != "off" {
		t.Fatalf("override 后应为 off，得到 %q", got)
	}
	if v, _, _ := m.pg.GetSetting(settingTLSMode); v != "selfsigned" {
		t.Fatalf("override 不应写库，库中 mode=%q", v)
	}
	if addr := s3.TLSEffectiveAddr("127.0.0.1:9999"); addr != "127.0.0.1:9999" {
		t.Fatalf("override off 后应沿用 -addr，得到 %q", addr)
	}
	// EnsureTLS 在 off 下清空内存证书，main 会走明文监听。
	s3.EnsureTLS(context.Background())
	if info := s3.TLSManager().CertInfo(); info != nil {
		t.Fatalf("off 模式不应持有证书，得到 %+v", info)
	}
}
