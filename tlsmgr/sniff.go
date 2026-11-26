package tlsmgr

import (
	"bufio"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// SniffListener serves TLS and plain HTTP on the same TCP port. The first byte
// of a connection is inspected: 0x16 (TLS handshake record) is handed to
// tls.Server; everything else is treated as HTTP so the caller can redirect it.
//
// 分类在独立 goroutine 里做：慢/半开连接只会占住自己那条，不会卡住 Accept
// （否则攻击者开一堆不发数据的连接就能把新连接饿死）。
type SniffListener struct {
	net.Listener
	TLSConfig   *tls.Config
	PeekTimeout time.Duration

	once      sync.Once
	results   chan sniffResult
	doneOnce  sync.Once
	done      chan struct{}
	closeOnce sync.Once
}

type sniffResult struct {
	conn  net.Conn
	fatal error
}

// Accept implements net.Listener.
func (l *SniffListener) Accept() (net.Conn, error) {
	l.start()
	select {
	case r := <-l.results:
		if r.fatal != nil {
			return nil, r.fatal
		}
		return r.conn, nil
	case <-l.doneCh():
		return nil, net.ErrClosed
	}
}

// Close 关闭底层监听并唤醒所有等待中的 goroutine。
func (l *SniffListener) Close() error {
	err := l.Listener.Close()
	l.closeOnce.Do(func() { close(l.doneCh()) })
	return err
}

func (l *SniffListener) doneCh() chan struct{} {
	l.doneOnce.Do(func() { l.done = make(chan struct{}) })
	return l.done
}

func (l *SniffListener) deliver(r sniffResult) {
	select {
	case l.results <- r:
	case <-l.doneCh():
		if r.conn != nil {
			_ = r.conn.Close()
		}
	}
}

func (l *SniffListener) start() {
	l.once.Do(func() {
		l.results = make(chan sniffResult, 128)
		go func() {
			var backoff time.Duration
			for {
				c, err := l.Listener.Accept()
				if err != nil {
					// 关闭是终态；临时错误（EMFILE/ENFILE 等）继续重试，
					// 不能把泵退出后让 http.Server 永远等一个没人写的 channel。
					if errors.Is(err, net.ErrClosed) {
						l.deliver(sniffResult{fatal: err})
						return
					}
					if isTemporaryAcceptError(err) {
						if backoff == 0 {
							backoff = 5 * time.Millisecond
						} else if backoff < time.Second {
							backoff *= 2
						}
						time.Sleep(backoff)
						continue
					}
					l.deliver(sniffResult{fatal: err})
					return
				}
				backoff = 0
				go func(c net.Conn) {
					if conn := l.classify(c); conn != nil {
						l.deliver(sniffResult{conn: conn})
					}
				}(c)
			}
		}()
	})
}

// classify 读取首字节决定协议；失败只丢弃该连接（绝不能返回给 http.Server）。
func (l *SniffListener) classify(c net.Conn) net.Conn {
	timeout := l.PeekTimeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	br := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(timeout))
	first, err := br.Peek(1)
	_ = c.SetReadDeadline(time.Time{})
	if err != nil {
		_ = c.Close()
		return nil
	}
	bc := &bufferedConn{Conn: c, r: br}
	if first[0] == 0x16 { // TLS handshake record
		return tls.Server(bc, l.TLSConfig)
	}
	return bc
}

func isTemporaryAcceptError(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Temporary() {
		return true
	}
	return errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.ENOBUFS) || errors.Is(err, syscall.ENOMEM)
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// RedirectHandler wraps app: TLS requests go to app; plain HTTP requests get a
// 301 to the same host/port over HTTPS. It never proxies or downgrades.
func RedirectHandler(app http.Handler, httpsAddr string, allowedHosts ...string) http.Handler {
	port := httpsPort(httpsAddr)
	allowed := map[string]bool{}
	for _, h := range allowedHosts {
		if h = normalizeHost(h); h != "" {
			allowed[h] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil {
			app.ServeHTTP(w, r)
			return
		}
		// 本机回环明文直通：nginx 用 proxy_pass http://127.0.0.1:8787 时是明文回源，
		// 若统一 301 会把反代部署打断（且 nginx 默认 Host 是 127.0.0.1:8787）。
		// 回环属于本机信任边界，直通不会把远程明文流量放进来；远程明文仍 301。
		if isLoopbackRequest(r) {
			app.ServeHTTP(w, r)
			return
		}
		host := normalizeHost(r.Host)
		if host == "" {
			http.Error(w, "需要 HTTPS", http.StatusBadRequest)
			return
		}
		// 只跳转到本机/已配置的主机名：否则 Host: evil.example 会把 301
		// 变成开放重定向（并可能被缓存）。白名单为空时同样只允许本机——
		// “没配置”不等于“任何 Host 都可信”。
		if !hostAllowed(host, allowed) && !isLocalRedirectHost(host) {
			http.Error(w, "需要 HTTPS", http.StatusBadRequest)
			return
		}
		if strings.Contains(host, ":") { // IPv6 字面量必须加回方括号
			host = "[" + host + "]"
		}
		target := "https://" + host
		if port != "" && port != "443" {
			target += ":" + port
		}
		target += r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	})
}

func normalizeHost(host string) string {
	host = strings.TrimSpace(host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

func hostAllowed(host string, allowed map[string]bool) bool {
	if allowed[host] {
		return true
	}
	for p := range allowed {
		if strings.HasPrefix(p, "*.") {
			suffix := p[1:] // ".example.com"
			if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
				return true
			}
		}
	}
	return false
}

func isLocalRedirectHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsUnspecified() {
			return true
		}
		ifaces, _ := net.Interfaces()
		for _, iface := range ifaces {
			addrs, _ := iface.Addrs()
			for _, a := range addrs {
				if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP.Equal(ip) {
					return true
				}
			}
		}
		return false
	}
	if h, err := os.Hostname(); err == nil && strings.EqualFold(strings.TrimSuffix(h, "."), host) {
		return true
	}
	return false
}

func httpsPort(addr string) string {
	if _, port, err := net.SplitHostPort(addr); err == nil {
		return port
	}
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i+1:]
	}
	return ""
}

// isLoopbackRequest 判断请求是否来自本机回环（反代/健康检查）。
func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
