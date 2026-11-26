// Command pojun runs the POJUN backend: the dual SQLite graph stores,
// the event-driven exploration engine, and the JSON HTTP API consumed by the
// shadcn/ui frontend.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/chu0119/pojun/agent"
	"github.com/chu0119/pojun/config"
	"github.com/chu0119/pojun/resource"
	"github.com/chu0119/pojun/selfupdate"
	"github.com/chu0119/pojun/server"
	"github.com/chu0119/pojun/tlsmgr"
	"github.com/chu0119/pojun/traffic"
	"golang.org/x/net/http2"
)

// version is the build version, injected at release time via
// -ldflags "-X main.version=<tag>". Defaults to "dev" for local builds.
var version = "dev"

const banner = `
    _    ____ _____ _______  __
   / \  |  _ \_   _| ____\ \/ /
  / _ \ | |_) || | |  _|  \  /
 / ___ \|  _ < | | | |___ /  \
/_/   \_\_| \_\|_| |_____/_/\_\
`

// defaultListenAddr is the control-plane bind address for a bare binary.
// Loopback by default: the console executes commands on the host, so exposing it
// must be an explicit choice (0.0.0.0:8787 / reverse proxy).
const defaultListenAddr = "127.0.0.1:8787"

// isLoopbackListenAddr reports whether addr binds to a loopback-only host.
// An unparsable address returns false (nothing to promise about it).
func isLoopbackListenAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// warnIfExposed logs a loud warning when the console is bound to anything other
// than loopback. A malformed address is left to ListenAndServe to report.
func warnIfExposed(addr string) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return
	}
	if isLoopbackListenAddr(addr) {
		return
	}
	desc := host
	if desc == "" {
		desc = "所有接口"
	}
	log.Printf("WARN: HTTP 控制台监听 %s 非回环（%s），将对其他主机可达；如非必要请使用 127.0.0.1:8787 或置于反向代理之后", addr, desc)
}

// printBanner writes the startup banner + version/runtime info to stdout.
func printBanner(addr string) {
	fmt.Print(banner)
	fmt.Println("  AI 自主渗透测试系统")
	fmt.Printf("  版本 %s  ·  %s/%s  ·  %s  ·  监听 %s\n\n",
		version, runtime.GOOS, runtime.GOARCH, runtime.Version(), addr)
}

// main only maps run's result onto the process exit code. The exit code is part
// of the update protocol — the supervising start script reads it to decide
// whether to relaunch us (see selfupdate.ExitRestart) — so the body has to live
// in a function that can *return* rather than os.Exit past its own defers.
func main() {
	os.Exit(run())
}

func run() int {
	var (
		addr    = flag.String("addr", defaultListenAddr, "HTTP listen address (default loopback; use 0.0.0.0:8787 to expose)")
		dataDir = flag.String("data", filepath.Join(config.BaseDir(), "data"), "data directory for SQLite stores (default: data/ next to the executable)")
		proxy   = flag.String("proxy", "127.0.0.1:8788", "traffic recording proxy address (empty to disable)")
		// -tls 是恢复用逃生阀：override 本次进程的证书模式但不写库。
		// 反代/容器升级后被 301 打断且进不了设置页时，用 -tls=off 临时恢复明文。
		tlsMode = flag.String("tls", "auto", "certificate mode override for this run: auto|off|selfsigned|import|acme (auto = persisted settings)")
	)
	flag.Parse()

	// 非回环绑定时喊一声：默认是回环，改成 0.0.0.0/:PORT 属于显式选择，
	// 但开机后日志里必须留下暴露面的痕迹。
	warnIfExposed(*addr)

	// hand the build version to the server package so GET /api/health can report it
	// to the frontend top bar.
	server.BuildVersion = version

	// capture backend logs into the in-memory sink (still to stderr) so the /logs
	// page can show a live log stream. Do this first, to catch startup logs too.
	server.StartLogCapture()

	// 资源自适应：启动早期只做只读容量探测（CPU/内存/负载/cgroup/磁盘），装上的
	// 是中性计划——不改 worker/作业/扫描/Go 运行时。设置页开启后 server 会按库
	// 里的设置重新推导并应用；探测失败只降级对应维度，不阻断启动。
	resource.Init(*dataDir)
	c, _, _ := resource.Snapshot()
	log.Printf("[resource] 容量: CPU=%.2f 核 内存=%dMB 可用=%dMB 磁盘剩余=%dMB 负载=%.2f cgroup_v2=%v 容器=%v",
		c.CPUCores, c.MemTotal>>20, c.MemAvailable>>20, c.DiskFree>>20, c.Load1, c.CgroupV2, c.InContainer)
	if len(c.Warnings) > 0 {
		log.Printf("[resource] 探测降级（不影响启动）: %s", strings.Join(c.Warnings, "；"))
	}

	// Self-update bootstrap: swap in a staged binary, or count a post-swap boot
	// attempt and roll back if the new build keeps dying. Must run before we open
	// the stores or bind a port — this may end with "exit and let the start script
	// relaunch me", and there is no point paying for either first.
	action, upState := selfupdate.Bootstrap()
	server.SetBootUpdateState(upState)
	if action == selfupdate.Restart {
		return selfupdate.ExitRestart
	}

	// surface which config file the binary reads (absolute, so `go run`'s relative
	// "config.json" — resolved against the CWD — is unambiguous).
	cfgPath := config.Path()
	if abs, e := filepath.Abs(cfgPath); e == nil {
		cfgPath = abs
	}
	if _, e := os.Stat(cfgPath); e == nil {
		log.Printf("[config] 配置文件: %s", cfgPath)
	} else {
		log.Printf("[config] 配置文件: %s (不存在 — 将仅尝试环境变量 POJUN_PG_DSN)", cfgPath)
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, shutdown := shutdownContext(sigCtx)
	defer shutdown(agent.AbortShutdown)

	// MITM 代理日志与主日志分离（CONNECT ERROR 不再刷屏 journal）。
	traffic.SetupProxyLog(*dataDir)
	mgr, err := server.NewManager(*dataDir, *proxy)
	if err != nil {
		log.Fatalf("open stores: %v", err)
	}
	defer mgr.Close()

	// Surviving this long means a freshly swapped-in build actually works, so drop
	// the upgrade marker and stop counting attempts. Until it fires, every boot
	// increments the count and a build that keeps dying gets rolled back.
	settle := time.AfterFunc(selfupdate.SettleDelay, selfupdate.Settle)
	defer settle.Stop()

	skillDir := config.SkillDir()
	if abs, err := filepath.Abs(skillDir); err == nil {
		skillDir = abs
	}
	log.Printf("[config] skill 目录: %s", skillDir)
	srv := server.New(ctx, mgr, skillDir, *dataDir, config.BaseDir())
	defer srv.Close()

	// 命令行恢复开关：只覆盖本次进程的内存设置，不写库。
	if m := strings.ToLower(strings.TrimSpace(*tlsMode)); m != "" && m != "auto" {
		if err := srv.OverrideTLSMode(m); err != nil {
			log.Fatalf("tls override %q: %v", m, err)
		}
	}

	// 监听地址：设置页保存过 tls_https_addr 就用它，否则用 -addr（默认回环）。
	// TLS 开启时同一个端口既收 TLS 也收明文 HTTP（远程 301，本机回环直通），不占用 80。
	listenAddr := srv.TLSEffectiveAddr(*addr)
	printBanner(listenAddr)
	srv.EnsureTLS(ctx)

	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	tm := srv.TLSManager()
	tlsModeValue := tlsmgr.ModeOff
	if tm != nil {
		tlsModeValue = tm.Settings().Mode
	}
	serveErr := func(err error) {
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}

	startedTLS := false
	if tm != nil && tlsModeValue != tlsmgr.ModeOff {
		if _, err := tm.GetCertificate(nil); err != nil {
			// 证书不可用（例如导入文件被删）时宁可明文启动也不让面板打不开；
			// 状态页会显示 last_error，修好后点“重启”即可切回 HTTPS。
			log.Printf("[tls] 证书不可用，回落明文 HTTP：%v", err)
		} else {
			httpSrv.Handler = tlsmgr.RedirectHandler(srv.Handler(), listenAddr, tlsRedirectHosts(tm)...)
			httpSrv.TLSConfig = &tls.Config{
				GetCertificate: tm.GetCertificate,
				MinVersion:     tls.VersionTLS12,
				NextProtos:     []string{"h2", "http/1.1"},
			}
			ln, actualAddr, err := listenTCP(listenAddr, *addr)
			if err != nil {
				log.Fatalf("listen %s: %v", listenAddr, err)
			}
			if actualAddr != listenAddr {
				log.Printf("[tls] 监听 %s 失败（被占用/无权限），回退到启动参数 %s", listenAddr, actualAddr)
				listenAddr = actualAddr
				httpSrv.Handler = tlsmgr.RedirectHandler(srv.Handler(), listenAddr, tlsRedirectHosts(tm)...)
			}
			sniff := &tlsmgr.SniffListener{Listener: ln, TLSConfig: httpSrv.TLSConfig}
			if err := http2.ConfigureServer(httpSrv, &http2.Server{}); err != nil {
				log.Printf("[tls] HTTP/2 配置失败（继续用 HTTP/1.1）：%v", err)
			}
			srv.SetTLSRuntime(listenAddr)
			tm.StartRenewLoop(ctx, func() (tlsmgr.Settings, bool) {
				return srv.TLSSettings(), true // 自签 SAN 刷新与 ACME 续期都走这条循环
			})
			log.Printf("POJUN %s backend listening on %s (HTTPS+HTTP 同端口, data=%s, workers=%d)", version, listenAddr, *dataDir, mgr.Workers())
			go func() { serveErr(httpSrv.Serve(sniff)) }()
			startedTLS = true
		}
	}

	if !startedTLS {
		// TLS 配置了但证书不可用：不要在保存端口上静默起明文，
		// 回退到启动参数端口（默认 8787），状态页会显示 tls_active=false。
		if tm != nil && tlsModeValue != tlsmgr.ModeOff && *addr != "" && *addr != listenAddr {
			log.Printf("[tls] 证书不可用，明文回退到启动参数 %s（避免在 %s 上明文提供服务）", *addr, listenAddr)
			listenAddr = *addr
		}
		ln, actualAddr, err := listenTCP(listenAddr, *addr)
		if err != nil {
			log.Fatalf("listen %s: %v", listenAddr, err)
		}
		if actualAddr != listenAddr {
			log.Printf("[tls] 监听 %s 失败（被占用/无权限），回退到启动参数 %s", listenAddr, actualAddr)
			listenAddr = actualAddr
		}
		srv.SetPlainRuntime(listenAddr)
		log.Printf("POJUN %s backend listening on %s (HTTP, data=%s, workers=%d)", version, listenAddr, *dataDir, mgr.Workers())
		go func() { serveErr(httpSrv.Serve(ln)) }()
	}

	// Two ways out: a signal (normal stop → exit 0, the start script stops looping)
	// or a staged update / rollback (→ exit 75, the script relaunches us and the
	// bootstrap above installs the new build).
	code := 0
	select {
	case <-ctx.Done():
	case <-server.RestartRequested():
		code = selfupdate.ExitRestart
		shutdown(agent.AbortShutdown)
	}

	log.Println("shutting down...")
	srv.Close()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	return code
}

// tlsRedirectHosts 收集允许 301 跳转的主机名：ACME 标识、自签额外 SAN 与当前
// 证书 SAN。白名单之外的外部 Host 会被拒绝，避免开放重定向。
func tlsRedirectHosts(tm *tlsmgr.Manager) []string {
	if tm == nil {
		return nil
	}
	s := tm.Settings()
	out := append([]string(nil), s.ACME.Identifiers...)
	out = append(out, s.SelfSignedSANs...)
	if info := tm.CertInfo(); info != nil {
		out = append(out, info.SANs...)
	}
	return out
}

// listenTCP 先试主地址；失败且 fallback 不同则回退到 -addr。
// 这样设置页保存了被占用端口时不会把整个面板挡在系统启动之外。
func listenTCP(primary, fallback string) (net.Listener, string, error) {
	ln, err := net.Listen("tcp", primary)
	if err == nil {
		return ln, primary, nil
	}
	if fallback != "" && fallback != primary {
		if ln2, err2 := net.Listen("tcp", fallback); err2 == nil {
			return ln2, fallback, nil
		}
	}
	return nil, primary, err
}

// shutdownContext deliberately does not derive from signalCtx. If it did, the
// parent's plain context.Canceled could win the race before AbortShutdown was
// attached to the child, losing the diagnostic cause in every running Agent.
func shutdownContext(signalCtx context.Context) (context.Context, context.CancelCauseFunc) {
	ctx, shutdown := context.WithCancelCause(context.Background())
	go func() {
		select {
		case <-signalCtx.Done():
			shutdown(agent.AbortShutdown)
		case <-ctx.Done():
		}
	}()
	return ctx, shutdown
}
