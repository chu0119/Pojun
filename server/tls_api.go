package server

// 内置 HTTPS 与证书管理。
//
// 模式：off / selfsigned / import / acme。证书文件在 <data>/tls 下，私钥永不出 API。
// HTTP→HTTPS 跳转由 tlsmgr.SniffListener 在同一端口完成（默认 8787），不占用 80。
// ACME 走 lego（HTTP-01 / TLS-ALPN-01），IP 证书自动使用 shortlived profile；
// 申请/续期期间按需临时放行防火墙 80/443，用完撤销（见 hostd/firewall.go）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chu0119/pojun/tlsmgr"
)

const (
	settingTLSMode         = "tls_mode"
	settingTLSAddr         = "tls_https_addr"
	settingTLSHSTS         = "tls_hsts"
	settingTLSSANs         = "tls_self_signed_sans"
	settingTLSFirewallAuto = "tls_firewall_auto"
	settingTLSACME         = "tls_acme"

	defaultTLSAddr = ":8787"
)

// tlsHSTS 由设置页控制；securityHeaders 在 TLS 连接上据此决定是否下发 HSTS。
// 自签证书默认关闭：一旦下发 HSTS，浏览器会拒绝让用户点过证书警告。
var tlsHSTS atomic.Bool

func init() { tlsHSTS.Store(true) }

// hostdFirewall 通过特权助手临时放行端口。助手不可用时按“没有可管理的防火墙”
// 处理（不阻断 ACME，端口可能本来就通），由申请结果暴露真实问题。
type hostdFirewall struct{}

func (hostdFirewall) Open(ctx context.Context, port int, ttl time.Duration) (string, bool, error) {
	body := map[string]any{"port": port, "ttl_seconds": int(ttl.Seconds())}
	var out struct {
		LeaseID string `json:"lease_id"`
		Changed bool   `json:"changed"`
		Error   string `json:"error"`
	}
	code, err := hostdPost(ctx, "/firewall/lease", body, &out)
	if err != nil {
		if isHostdUnavailable(err) {
			log.Printf("[tls] 宿主助手不可用，跳过防火墙临时放行（%d/tcp）：%v", port, err)
			return "", false, nil
		}
		return "", false, err
	}
	if code == http.StatusNotFound {
		// 老版本 hostd 没有防火墙接口：跳过放行，照常尝试申请（端口可能本来就通）。
		log.Printf("[tls] 宿主助手版本过旧（无 /firewall 接口），跳过临时放行；建议重跑 install.sh 升级助手")
		return "", false, nil
	}
	if code != http.StatusOK {
		if out.Error == "" {
			out.Error = fmt.Sprintf("宿主助手返回 HTTP %d", code)
		}
		return "", false, fmt.Errorf("%s", out.Error)
	}
	return out.LeaseID, out.Changed, nil
}

func (hostdFirewall) Close(ctx context.Context, leaseID string) error {
	_, err := hostdPost(ctx, "/firewall/release", map[string]any{"lease_id": leaseID}, nil)
	return err
}

// initTLS 创建证书管理器并载入持久化设置。失败不阻塞启动（页面会显示原因）。
func (s *Server) initTLS() {
	tm, err := tlsmgr.New(s.dataDir)
	if err != nil {
		log.Printf("[tls] 初始化证书管理器失败：%v", err)
		return
	}
	tm.SetFirewall(hostdFirewall{})
	s.tlsMgr = tm
	s.loadTLSSettings()
}

// loadTLSSettings 从 PG 读取设置。HTTPS 是显式 opt-in：没有 tls_mode 时保持
// off（纯 HTTP），只有设置页/API 明确保存过才启用。这样新装与存量升级都不会
// 因为 301/证书信任打断现有部署（Windows Docker Desktop、非同机反代等）。
func (s *Server) loadTLSSettings() {
	tm := s.tlsMgr
	if tm == nil {
		return
	}
	set := tlsmgr.Settings{Mode: tlsmgr.ModeOff, HTTPSAddr: defaultTLSAddr, FirewallAuto: true}
	if s.m != nil && s.m.pg != nil {
		pg := s.m.pg
		mode, ok, _ := pg.GetSetting(settingTLSMode)
		if !ok || strings.TrimSpace(mode) == "" {
			mode = string(tlsmgr.ModeOff)
		}
		set.Mode = tlsmgr.Mode(strings.TrimSpace(mode))
		if v, ok, _ := pg.GetSetting(settingTLSAddr); ok && strings.TrimSpace(v) != "" {
			set.HTTPSAddr = strings.TrimSpace(v)
			s.tlsAddrSaved.Store(true)
		}
		if v, ok, _ := pg.GetSetting(settingTLSHSTS); ok {
			set.HSTS = v == "1"
		} else {
			// 默认关闭：off 模式没有 TLS；自签下发 HSTS 会让浏览器无法跳过硬性
			// 证书告警。导入/ACME（公信证书）保持历史默认开启。
			set.HSTS = set.Mode == tlsmgr.ModeImport || set.Mode == tlsmgr.ModeACME
		}
		if v, ok, _ := pg.GetSetting(settingTLSFirewallAuto); ok {
			set.FirewallAuto = v == "1"
		}
		if v, ok, _ := pg.GetSetting(settingTLSSANs); ok && strings.TrimSpace(v) != "" {
			_ = json.Unmarshal([]byte(v), &set.SelfSignedSANs)
		}
		if v, ok, _ := pg.GetSetting(settingTLSACME); ok && strings.TrimSpace(v) != "" {
			_ = json.Unmarshal([]byte(v), &set.ACME)
		}
	}
	if err := tm.ApplySettings(set); err != nil {
		// 配置无效时不要直接关 HTTPS：磁盘上可能还有可用证书。
		// 有证书就按导入模式继续提供 TLS，页面显示 last_error 让管理员修正。
		if tm.CertInfo() != nil {
			log.Printf("[tls] 设置无效，保留现有证书并按导入模式启动：%v", err)
			set.Mode = tlsmgr.ModeImport
		} else {
			log.Printf("[tls] 设置无效，回落 off：%v", err)
			set.Mode = tlsmgr.ModeOff
		}
		_ = tm.ApplySettings(set)
	}
	tlsHSTS.Store(set.HSTS)
}

// saveTLSSettings 持久化 TLS 设置。writeAddr=false 时不写监听地址——
// 否则自定义 `-addr` 启动的实例在只改 HSTS 时会被默认值覆盖。
func (s *Server) saveTLSSettings(set tlsmgr.Settings, writeAddr bool) error {
	if s.m == nil || s.m.pg == nil {
		return fmt.Errorf("数据库未就绪")
	}
	pg := s.m.pg
	sans, _ := json.Marshal(set.SelfSignedSANs)
	acme, _ := json.Marshal(set.ACME)
	pairs := [][2]string{
		{settingTLSMode, string(set.Mode)},
		{settingTLSHSTS, boolStr(set.HSTS)},
		{settingTLSSANs, string(sans)},
		{settingTLSFirewallAuto, boolStr(set.FirewallAuto)},
		{settingTLSACME, string(acme)},
	}
	if writeAddr {
		pairs = append(pairs, [2]string{settingTLSAddr, set.HTTPSAddr})
	}
	for _, p := range pairs {
		if err := pg.SetSetting(p[0], p[1]); err != nil {
			return err
		}
	}
	return nil
}

func boolStr(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// TLSManager 暴露给 main 用于启动监听。
func (s *Server) TLSManager() *tlsmgr.Manager { return s.tlsMgr }

// TLSSettings 返回当前 TLS 设置副本（供 main 决定监听方式与续期循环）。
func (s *Server) TLSSettings() tlsmgr.Settings {
	if s.tlsMgr == nil {
		return tlsmgr.Settings{Mode: tlsmgr.ModeOff, HTTPSAddr: defaultTLSAddr}
	}
	return s.tlsMgr.Settings()
}

// OverrideTLSMode 是 main 的 -tls 命令行逃生阀：只覆盖本次进程的内存设置，
// 不写库。反代/容器场景升级后如果被 301 打断、又进不了设置页，可用它临时
// 恢复明文（-tls=off），修好后再从页面切换。
func (s *Server) OverrideTLSMode(mode string) error {
	if s.tlsMgr == nil {
		return fmt.Errorf("证书管理器未初始化")
	}
	set := s.tlsMgr.Settings()
	set.Mode = tlsmgr.Mode(strings.ToLower(strings.TrimSpace(mode)))
	if err := s.tlsMgr.ApplySettings(set); err != nil {
		return err
	}
	if set.Mode == tlsmgr.ModeOff {
		s.tlsActive.Store(false)
	}
	log.Printf("[tls] 命令行覆盖证书模式：%s（不写入设置表）", set.Mode)
	return nil
}

// EnsureTLS 在监听前准备证书（自签生成 / 导入校验 / ACME 首次申请）。
func (s *Server) EnsureTLS(ctx context.Context) {
	if s.tlsMgr == nil {
		return
	}
	if err := s.tlsMgr.Ensure(ctx); err != nil {
		log.Printf("[tls] 证书准备失败：%v", err)
		// 启动期失败必须留痕：main 会回落明文、active=false，状态页否则只能显示
		// “待重启生效”，用户看不到“导入证书过期/私钥不匹配”这类真实原因。
		s.tlsMgr.SetLastError("启动时证书准备失败: " + err.Error())
	}
}

// SetTLSActive 由 main 在 TLS 监听成功后调用，状态页据此区分“已配置”和“正在生效”。
func (s *Server) SetTLSActive(v bool) { s.tlsActive.Store(v) }

// SetTLSRuntime 记录当前进程实际监听的地址，供状态页判断“改了设置但还没重启”。
func (s *Server) SetTLSRuntime(addr string) {
	s.tlsRuntimeAddr.Store(addr)
	s.tlsActive.Store(true)
}

// SetPlainRuntime 记录明文 HTTP 实际监听地址（mode=off 或证书不可用回落）。
// 与 SetTLSRuntime 分开：active 仍为 false，状态页不能显示 https://。
func (s *Server) SetPlainRuntime(addr string) {
	s.tlsRuntimeAddr.Store(addr)
	s.tlsActive.Store(false)
}

func (s *Server) tlsRuntime() (string, bool) {
	v := s.tlsRuntimeAddr.Load()
	if v == nil {
		return "", false
	}
	addr, _ := v.(string)
	return addr, s.tlsActive.Load()
}

// TLSEffectiveAddr 返回实际应监听的地址。
//
// 优先级：设置页保存过 tls_https_addr → 用设置值；否则用启动参数 -addr。
// 这样 systemd 单元里的 -addr 只是默认值，管理员改端口后重启即生效；
// Docker/手工运行不带设置时仍以 -addr 为准。
func (s *Server) TLSEffectiveAddr(fallback string) string {
	if s.tlsMgr == nil {
		return fallback
	}
	set := s.tlsMgr.Settings()
	if set.Mode == tlsmgr.ModeOff {
		return fallback
	}
	if !s.tlsAddrSaved.Load() || strings.TrimSpace(set.HTTPSAddr) == "" {
		return fallback
	}
	return set.HTTPSAddr
}

func (s *Server) tlsStatus(w http.ResponseWriter, r *http.Request) {
	if s.tlsMgr == nil {
		writeErr(w, http.StatusServiceUnavailable, "证书管理器未初始化")
		return
	}
	st := s.tlsMgr.Status()
	set := s.tlsMgr.Settings()
	activeAddr, active := s.tlsRuntime()
	addrSaved := s.tlsAddrSaved.Load()
	listenAddr := set.HTTPSAddr
	if activeAddr != "" {
		// 实际监听地址优先：mode=off 时 TLSEffectiveAddr 用 -addr，可能和设置页
		// 保存的 tls_https_addr 不同；状态页必须显示真实监听。
		listenAddr = activeAddr
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      st,
		"settings":    set,
		"active":      active,
		"active_addr": activeAddr,
		"addr_saved":  addrSaved,
		"listen_addr": listenAddr,
		"urls":        tlsURLsWithScheme(listenAddr, schemeForTLS(set.Mode, active)),
		"firewall":    s.firewallStatus(r.Context()),
	})
}

func (s *Server) tlsSaveSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 3<<20)
	if s.tlsMgr == nil {
		writeErr(w, http.StatusServiceUnavailable, "证书管理器未初始化")
		return
	}
	var in tlsmgr.Settings
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	prev := s.tlsMgr.Settings()
	// 部分更新语义：未提交的字段沿用旧值，避免 API 调用方只改模式时清空
	// 监听地址/ACME/SAN。前端目前发全量，但 API 不能依赖调用方。
	if strings.TrimSpace(string(in.Mode)) == "" {
		in.Mode = prev.Mode
	}
	if strings.TrimSpace(in.HTTPSAddr) == "" {
		in.HTTPSAddr = prev.HTTPSAddr
	}
	if in.SelfSignedSANs == nil {
		in.SelfSignedSANs = prev.SelfSignedSANs
	}
	if len(in.ACME.Identifiers) == 0 && in.ACME.Email == "" {
		in.ACME = prev.ACME
	}
	if err := s.tlsMgr.ApplySettings(in); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	next := s.tlsMgr.Settings()
	writeAddr := prev.HTTPSAddr != next.HTTPSAddr
	if writeAddr && next.Mode != tlsmgr.ModeOff {
		// 保存前试绑一次，避免把端口冲突/无权限的地址写进设置后重启起不来。
		// 但当前进程正监听的地址必然 EADDRINUSE：状态页显示的是运行地址，
		// 用户不动地址直接保存（只改 HSTS 等）也会全量提交该地址，试绑必失败。
		// 与运行地址相同时跳过试绑——重启后会重新监听同一地址，本来就是通的。
		curAddr, curActive := s.tlsRuntime()
		if !curActive || curAddr != next.HTTPSAddr {
			ln, err := net.Listen("tcp", next.HTTPSAddr)
			if err != nil {
				_ = s.tlsMgr.ApplySettings(prev)
				writeErr(w, http.StatusBadRequest, "无法监听 "+next.HTTPSAddr+": "+err.Error())
				return
			}
			_ = ln.Close()
		}
	}
	if err := s.saveTLSSettings(next, writeAddr); err != nil {
		// 回滚内存设置，避免“页面显示已保存但重启后还原”。
		_ = s.tlsMgr.ApplySettings(prev)
		writeErr(w, http.StatusInternalServerError, "保存设置失败: "+err.Error())
		return
	}
	tlsHSTS.Store(next.HSTS)
	if writeAddr {
		s.tlsAddrSaved.Store(true)
	}
	needsRestart := prev.Mode != next.Mode
	if writeAddr {
		if activeAddr, active := s.tlsRuntime(); active && activeAddr != next.HTTPSAddr {
			needsRestart = true
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "needs_restart": needsRestart, "settings": next})
}

func (s *Server) tlsImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 3<<20)
	if s.tlsMgr == nil {
		writeErr(w, http.StatusServiceUnavailable, "证书管理器未初始化")
		return
	}
	var in struct {
		CertPEM string `json:"cert_pem"`
		KeyPEM  string `json:"key_pem"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	info, err := s.tlsMgr.Import([]byte(in.CertPEM), []byte(in.KeyPEM))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	set := s.tlsMgr.Settings()
	set.Mode = tlsmgr.ModeImport
	_ = s.tlsMgr.ApplySettings(set)
	if err := s.saveTLSSettings(set, false); err != nil {
		writeErr(w, http.StatusInternalServerError, "证书已导入但保存模式失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cert": info, "needs_restart": !s.tlsActive.Load()})
}

func (s *Server) tlsSelfSigned(w http.ResponseWriter, r *http.Request) {
	if s.tlsMgr == nil {
		writeErr(w, http.StatusServiceUnavailable, "证书管理器未初始化")
		return
	}
	var in struct {
		SANs []string `json:"sans"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	info, err := s.tlsMgr.GenerateSelfSigned(in.SANs)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	set := s.tlsMgr.Settings()
	set.Mode = tlsmgr.ModeSelfSigned
	set.SelfSignedSANs = in.SANs
	_ = s.tlsMgr.ApplySettings(set)
	if err := s.saveTLSSettings(set, false); err != nil {
		writeErr(w, http.StatusInternalServerError, "证书已生成但保存设置失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "cert": info, "needs_restart": !s.tlsActive.Load()})
}

func (s *Server) tlsACMECreate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 3<<20)
	if s.tlsMgr == nil {
		writeErr(w, http.StatusServiceUnavailable, "证书管理器未初始化")
		return
	}
	var cfg tlsmgr.ACMEConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	cfg = tlsmgr.NormalizeACME(cfg)
	// 挑战端口与面板监听端口相同会抢占失败，提前给明确提示。
	challengePort := "80"
	if cfg.Challenge == "tls-alpn-01" {
		challengePort = "443"
	}
	checkAddr := s.tlsMgr.Settings().HTTPSAddr
	if activeAddr, _ := s.tlsRuntime(); activeAddr != "" {
		checkAddr = activeAddr
	}
	if _, port, err := net.SplitHostPort(checkAddr); err == nil && port == challengePort {
		writeErr(w, http.StatusBadRequest, "面板已监听 "+challengePort+"，ACME 挑战无法再绑定同一端口；请换挑战方式或把面板换到其它端口")
		return
	}
	// 先占单飞锁，再改设置：并发的第二个请求不会覆盖正在使用的配置。
	s.tlsMu.Lock()
	if s.tlsCancel != nil || s.tlsMgr.Status().ACME.Running {
		s.tlsMu.Unlock()
		writeErr(w, http.StatusConflict, "已有一个 ACME 申请在进行中")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	s.tlsCancel = cancel
	s.tlsMu.Unlock()
	abort := func(code int, msg string) {
		cancel()
		s.tlsMu.Lock()
		s.tlsCancel = nil
		s.tlsMu.Unlock()
		writeErr(w, code, msg)
	}

	set := s.tlsMgr.Settings()
	set.Mode = tlsmgr.ModeACME
	set.ACME = cfg
	if err := s.tlsMgr.ApplySettings(set); err != nil {
		abort(http.StatusBadRequest, err.Error())
		return
	}
	if err := s.saveTLSSettings(set, false); err != nil {
		abort(http.StatusInternalServerError, "保存 ACME 配置失败: "+err.Error())
		return
	}
	go func() {
		defer func() {
			cancel()
			s.tlsMu.Lock()
			s.tlsCancel = nil
			s.tlsMu.Unlock()
		}()
		if _, err := s.tlsMgr.IssueACME(ctx, cfg); err != nil {
			log.Printf("[tls] ACME 申请失败：%v", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "running": true, "needs_restart": !s.tlsActive.Load(),
	})
}

func (s *Server) tlsACMECancel(w http.ResponseWriter, r *http.Request) {
	// 启动时 Ensure 和 6 小时续期循环不经过 tlsCancel；统一走 manager 的取消句柄，
	// 否则 UI 看到 running=true 点取消必然 409。
	if s.tlsMgr != nil && s.tlsMgr.CancelACME() {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	s.tlsMu.Lock()
	cancel := s.tlsCancel
	s.tlsMu.Unlock()
	if cancel == nil {
		writeErr(w, http.StatusConflict, "没有正在进行的 ACME 申请")
		return
	}
	cancel()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) tlsCA(w http.ResponseWriter, r *http.Request) {
	if s.tlsMgr == nil {
		writeErr(w, http.StatusServiceUnavailable, "证书管理器未初始化")
		return
	}
	if s.tlsMgr.Settings().Mode != tlsmgr.ModeSelfSigned {
		writeErr(w, http.StatusNotFound, "当前不是自签模式，没有可下载的本地 CA")
		return
	}
	ca := s.tlsMgr.CA()
	if len(ca) == 0 {
		writeErr(w, http.StatusNotFound, "当前没有本地 CA（仅自签模式会生成）")
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="pojun-ca.pem"`)
	_, _ = w.Write(ca)
}

// tlsRestart 让进程以 75 退出，由 start.sh/systemd 重新拉起，使监听地址/模式生效。
func (s *Server) tlsRestart(w http.ResponseWriter, r *http.Request) {
	// 申请进行中重启会直接 cancel ACME goroutine，用户看到的是“申请失败”。
	if s.tlsMgr != nil && s.tlsMgr.Status().ACME.Running {
		writeErr(w, http.StatusConflict, "ACME 申请进行中，完成或取消后再重启")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "restarting": true})
	go func() {
		time.Sleep(300 * time.Millisecond)
		requestRestart()
	}()
}

// firewallStatus 读取宿主助手状态；助手不可用时返回 available=false 与原因。
func (s *Server) firewallStatus(ctx context.Context) map[string]any {
	c, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodGet, "http://hostd/firewall/status", nil)
	if err != nil {
		return map[string]any{"available": false, "error": err.Error()}
	}
	resp, err := hostdClient().Do(req)
	if err != nil {
		return map[string]any{"available": false, "error": "宿主助手不可用（未安装 pojun-hostd 或 Docker 部署）"}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return map[string]any{"available": false, "error": "宿主助手版本过旧（无 /firewall 接口），请重跑 install.sh 升级"}
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return map[string]any{"available": false, "error": "宿主助手响应异常"}
	}
	return out
}

func schemeForTLS(mode tlsmgr.Mode, active bool) string {
	if mode == tlsmgr.ModeOff || !active {
		return "http"
	}
	return "https"
}

func tlsURLs(addr string) []string {
	return tlsURLsWithScheme(addr, "https")
}

func tlsURLsWithScheme(addr, scheme string) []string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(host string) {
		if host == "" || seen[host] {
			return
		}
		seen[host] = true
		h := host
		if strings.Contains(h, ":") && !strings.HasPrefix(h, "[") {
			h = "[" + h + "]"
		}
		out = append(out, scheme+"://"+h+":"+port)
	}
	if host, err := os.Hostname(); err == nil {
		add(host)
	}
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP != nil && !ipnet.IP.IsLoopback() && !ipnet.IP.IsLinkLocalUnicast() {
				add(ipnet.IP.String())
			}
		}
	}
	return out
}

func isHostdUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "connect: no such file or directory") ||
		strings.Contains(msg, "connect: connection refused") ||
		strings.Contains(msg, "dial unix")
}

// hostdPost 向宿主助手发一个 JSON POST（Unix socket）。
func hostdPost(ctx context.Context, path string, in any, out any) (int, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://hostd"+path, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hostdClient().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
	}
	return resp.StatusCode, nil
}
