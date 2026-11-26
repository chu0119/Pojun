package hostd

// 防火墙临时放行（ACME HTTP-01/TLS-ALPN-01 用）。
//
// 设计要点：
//   - 只碰 80/443/TCP，且只在「原本没有放行」时添加，用完立刻撤销；
//     原本就放行的端口一律不动（changed=false，不产生 lease）。
//   - firewalld 用 runtime 规则（不 --permanent），重启即消失；
//     ufw 的 allow 是持久的，所以把 lease 落盘，hostd 重启后先把残留规则撤掉。
//   - 云厂商安全组不在本机，改不了；状态接口给出提示，由用户自行放行。
//   - hostd 的 systemd 单元必须带 CAP_NET_ADMIN 与 AF_NETLINK，否则 ufw/nft 一律失败。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	fwLeaseTTLMax   = 30 * time.Minute
	fwLeaseTTLMin   = time.Minute
	fwLeaseStateDir = "/var/lib/pojun-hostd"
	fwLeaseFileName = "firewall-leases.json"
)

type fwLease struct {
	ID        string    `json:"id"`
	Port      int       `json:"port"`
	Proto     string    `json:"proto"`
	Backend   string    `json:"backend"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// State 为 opening 表示规则可能已写入但尚未确认；恢复时同样要撤销。
	State string `json:"state"`
}

type firewallManager struct {
	mu      sync.Mutex
	leases  map[string]*fwLease
	path    string
	backend string // ufw | firewalld | ""
	active  bool
	// runCmd 可注入（测试用）：默认执行本机的 ufw/firewall-cmd。
	runCmd func(ctx context.Context, name string, args ...string) (string, error)
}

func newFirewallManager() *firewallManager {
	f := &firewallManager{leases: map[string]*fwLease{}}
	dir := fwLeaseStateDir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		dir = "/run/pojun"
	}
	f.path = filepath.Join(dir, fwLeaseFileName)
	f.detect()
	f.recover()
	return f
}

// detect 识别本机防火墙后端与是否处于启用状态。
func (f *firewallManager) detect() {
	f.backend, f.active = "", false
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ufwInstalled := false
	if _, err := exec.LookPath("ufw"); err == nil {
		ufwInstalled = true
		if out, err := f.run(ctx, "ufw", "status"); err == nil && strings.Contains(out, "Status: active") {
			f.backend, f.active = "ufw", true
			return
		}
	}
	// ufw 装了但未启用时继续看 firewalld；否则同时装两者的机器会误判
	// “没有启用中的防火墙”，ACME 80/443 永远不会被临时放行。
	if _, err := exec.LookPath("firewall-cmd"); err == nil {
		if out, err := f.run(ctx, "firewall-cmd", "--state"); err == nil && strings.TrimSpace(out) == "running" {
			f.backend, f.active = "firewalld", true
			return
		}
	}
	if ufwInstalled {
		f.backend, f.active = "ufw", false
	}
}

func (f *firewallManager) run(ctx context.Context, name string, args ...string) (string, error) {
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if f.runCmd != nil {
		return f.runCmd(c, name, args...)
	}
	cmd := exec.CommandContext(c, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// status 返回给主服务/前端的状态。
func (f *firewallManager) status() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	leases := make([]map[string]any, 0, len(f.leases))
	for _, l := range f.leases {
		leases = append(leases, map[string]any{
			"id": l.ID, "port": l.Port, "proto": l.Proto,
			"backend": l.Backend, "expires_at": l.ExpiresAt,
		})
	}
	sort.Slice(leases, func(i, j int) bool { return leases[i]["port"].(int) < leases[j]["port"].(int) })
	hint := ""
	if f.backend == "" {
		hint = "未检测到 ufw/firewalld：本机没有可管理的防火墙，若云厂商安全组拦截请自行放行 80/443"
	} else if !f.active {
		hint = f.backend + " 未启用：端口默认可达，程序不会改动防火墙"
	}
	return map[string]any{
		"available": f.backend != "",
		"backend":   f.backend,
		"active":    f.active,
		"leases":    leases,
		"hint":      hint,
	}
}

func validFirewallPort(port int) error {
	if port != 80 && port != 443 {
		return fmt.Errorf("只允许临时放行 80/443（ACME 挑战端口），收到 %d", port)
	}
	return nil
}

// open 临时放行 port/tcp。changed=false 表示原本已放行，未做任何改动。
func (f *firewallManager) open(ctx context.Context, port int, ttl time.Duration) (string, bool, error) {
	if err := validFirewallPort(port); err != nil {
		return "", false, err
	}
	if ttl < fwLeaseTTLMin {
		ttl = fwLeaseTTLMin
	}
	if ttl > fwLeaseTTLMax {
		ttl = fwLeaseTTLMax
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.backend == "" || !f.active {
		// 启动时可能还没装/没启用防火墙，申请前再探测一次（例如刚启用 ufw）。
		f.detect()
	}
	if f.backend == "" || !f.active {
		// 没有启用中的防火墙：端口本来就是通的，不需要改。
		return "", false, nil
	}
	allowed, err := f.allowedLocked(ctx, port)
	if err != nil {
		return "", false, err
	}
	if allowed {
		// 同一端口已有我们自己的临时租约时续租，避免先到期的 lease
		// 把后一个 ACME 挑战仍在使用的规则撤掉；管理员手动放行的规则没有
		// lease，不在这里动。
		for _, l := range f.leases {
			if l.Port == port && l.Proto == "tcp" && l.State == "open" {
				l.ExpiresAt = time.Now().UTC().Add(ttl)
				_ = f.persistLocked()
				return l.ID, false, nil
			}
		}
		return "", false, nil
	}
	lease := &fwLease{
		ID:        newID(),
		Port:      port,
		Proto:     "tcp",
		Backend:   f.backend,
		CreatedAt: time.Now().UTC(),
		ExpiresAt: time.Now().UTC().Add(ttl),
		State:     "opening",
	}
	// 先落盘再改规则：崩溃后重启时才能知道要撤销哪条。
	f.leases[lease.ID] = lease
	if err := f.persistLocked(); err != nil {
		delete(f.leases, lease.ID)
		return "", false, fmt.Errorf("记录防火墙租约失败（未改动规则）: %w", err)
	}
	if err := f.addLocked(ctx, port); err != nil {
		// add 可能“部分生效”（v4 成功/v6 失败、命令超时），先尝试撤掉；
		// 撤不掉就保留租约让 recover/TTL 继续处理。
		if rmErr := f.removeBackendLocked(ctx, lease.Backend, port); rmErr != nil {
			lease.State = "opening"
			lease.ExpiresAt = time.Now().UTC().Add(5 * time.Minute)
			_ = f.persistLocked()
			time.AfterFunc(5*time.Minute, func() { f.expire(context.Background(), lease.ID) })
		} else {
			delete(f.leases, lease.ID)
			_ = f.persistLocked()
		}
		if fwPermissionError(err) {
			return "", false, fmt.Errorf("%w（助手缺少 CAP_NET_ADMIN/AF_NETLINK：请重启 hostd；仍未生效则重跑 install.sh 更新单元）", err)
		}
		return "", false, err
	}
	lease.State = "open"
	_ = f.persistLocked()
	f.logf("临时放行 %d/tcp（%s，%s 到期）lease=%s", port, f.backend, lease.ExpiresAt.Format(time.RFC3339), lease.ID)
	time.AfterFunc(time.Until(lease.ExpiresAt), func() { f.expire(context.Background(), lease.ID) })
	return lease.ID, true, nil
}

// release 撤销一个 lease。幂等：不存在的 lease 返回 nil。
func (f *firewallManager) release(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	lease, ok := f.leases[id]
	if !ok {
		return nil
	}
	if lease.State == "open" || lease.State == "opening" {
		backend := lease.Backend
		if backend == "" {
			backend = f.backend
		}
		if err := f.removeBackendLocked(ctx, backend, lease.Port); err != nil {
			// 删除规则失败就不能删租约：ufw allow 是持久规则，丢了记录
			// 会永久敞开且无人追踪。保留并 5 分钟后重试。
			if lease.ExpiresAt.Before(time.Now().UTC()) || lease.ExpiresAt.Equal(time.Now().UTC()) {
				lease.ExpiresAt = time.Now().UTC().Add(5 * time.Minute)
			}
			_ = f.persistLocked()
			time.AfterFunc(5*time.Minute, func() { f.expire(context.Background(), lease.ID) })
			f.logf("撤销 %d/tcp 失败（保留租约重试）：%v", lease.Port, err)
			return err
		}
	}
	delete(f.leases, id)
	_ = f.persistLocked()
	f.logf("已撤销临时放行 %d/tcp lease=%s", lease.Port, id)
	return nil
}

// expire 处理 TTL 到期：租约可能因同端口续租被延长，旧 timer 到期时
// 不能直接 release，否则会提前撤掉仍在使用的规则。
func (f *firewallManager) expire(ctx context.Context, id string) {
	f.mu.Lock()
	lease, ok := f.leases[id]
	if !ok {
		f.mu.Unlock()
		return
	}
	wait := time.Until(lease.ExpiresAt)
	if wait > 0 {
		f.mu.Unlock()
		// 重新计时用独立 ctx：调用方（HTTP 请求/旧租约续期）的 ctx 可能已取消。
		time.AfterFunc(wait, func() { f.expire(context.Background(), id) })
		return
	}
	f.mu.Unlock()
	_ = f.release(ctx, id)
}

// recover 处理 hostd 重启：把落盘的 lease 全部撤销（规则可能仍在）。
func (f *firewallManager) recover() {
	b, err := os.ReadFile(f.path)
	if err != nil {
		return
	}
	var saved []*fwLease
	if json.Unmarshal(b, &saved) != nil {
		return
	}
	ctx := context.Background()
	now := time.Now()
	for _, l := range saved {
		if l == nil || l.Port == 0 || l.ID == "" {
			continue
		}
		// 只处理 80/443：租约文件被篡改或来自其它版本时，不能拿任意端口去
		// ufw delete / firewall-cmd --remove-port，否则会删掉管理员自己的规则。
		if validFirewallPort(l.Port) != nil {
			f.logf("恢复：忽略非法端口的租约 %s（port=%d）", l.ID, l.Port)
			continue
		}
		// 未过期且规则已确认写入：接管租约继续生效，到期再撤销。
		// 这样 hostd 自身重启（例如单元修补）不会把正在进行的 ACME 挑战打断。
		if l.State == "open" && l.ExpiresAt.After(now) {
			f.mu.Lock()
			f.leases[l.ID] = l
			f.mu.Unlock()
			time.AfterFunc(time.Until(l.ExpiresAt), func() { f.expire(context.Background(), l.ID) })
			f.logf("恢复：接管未过期的 %d/tcp 临时放行（%s 到期）", l.Port, l.ExpiresAt.Format(time.RFC3339))
			continue
		}
		if f.backend != "" && f.active {
			backend := l.Backend
			if backend == "" {
				backend = f.backend
			}
			if err := f.removeBackendLocked(ctx, backend, l.Port); err != nil {
				// 撤销失败：留在内存里等 TTL 再试，避免规则永久残留。
				f.mu.Lock()
				l.State = "open"
				l.ExpiresAt = time.Now().UTC().Add(5 * time.Minute)
				f.leases[l.ID] = l
				f.mu.Unlock()
				f.logf("恢复时撤销 %d/tcp 失败：%v", l.Port, err)
				time.AfterFunc(5*time.Minute, func() { f.expire(context.Background(), l.ID) })
				continue
			}
			f.logf("恢复：已撤销上次遗留的 %d/tcp 临时放行", l.Port)
		}
	}
	f.mu.Lock()
	// 只有确认后端可用且没有待撤销项时才删记录；后端探测失败时保留文件，
	// 等下次启动再试，避免把“可能仍开着的端口”永久遗忘。
	// 后端未启用时不能删文件：规则可能仍是持久规则（ufw），等下次
	// 后端可用时再撤；只有确认租约清空且后端 active 才清理。
	if len(f.leases) == 0 && f.backend != "" && f.active {
		_ = os.Remove(f.path)
	}
	f.mu.Unlock()
}

func (f *firewallManager) allowedLocked(ctx context.Context, port int) (bool, error) {
	switch f.backend {
	case "ufw":
		out, err := f.run(ctx, "ufw", "status")
		if err != nil {
			return false, err
		}
		return ufwAllows(out, port), nil
	case "firewalld":
		_, err := f.run(ctx, "firewall-cmd", fmt.Sprintf("--query-port=%d/tcp", port))
		if err == nil {
			return true, nil
		}
		// query 退出码 1 = 未放行，不是故障。
		if strings.Contains(err.Error(), "exit status 1") {
			return false, nil
		}
		return false, err
	}
	return false, fmt.Errorf("不支持的防火墙后端")
}

func (f *firewallManager) addLocked(ctx context.Context, port int) error {
	switch f.backend {
	case "ufw":
		_, err := f.run(ctx, "ufw", "allow", fmt.Sprintf("%d/tcp", port))
		return err
	case "firewalld":
		_, err := f.run(ctx, "firewall-cmd", fmt.Sprintf("--add-port=%d/tcp", port))
		return err
	}
	return fmt.Errorf("不支持的防火墙后端")
}

func (f *firewallManager) removeLocked(ctx context.Context, port int) error {
	return f.removeBackendLocked(ctx, f.backend, port)
}

func (f *firewallManager) removeBackendLocked(ctx context.Context, backend string, port int) error {
	switch backend {
	case "ufw":
		_, err := f.run(ctx, "ufw", "delete", "allow", fmt.Sprintf("%d/tcp", port))
		if err != nil && strings.Contains(err.Error(), "Could not delete non-existent rule") {
			return nil
		}
		return err
	case "firewalld":
		_, err := f.run(ctx, "firewall-cmd", fmt.Sprintf("--remove-port=%d/tcp", port))
		if err != nil && strings.Contains(err.Error(), "NOT_ENABLED") {
			return nil
		}
		return err
	}
	return fmt.Errorf("不支持的防火墙后端")
}

func (f *firewallManager) persistLocked() error {
	if len(f.leases) == 0 {
		_ = os.Remove(f.path)
		return nil
	}
	list := make([]*fwLease, 0, len(f.leases))
	for _, l := range f.leases {
		list = append(list, l)
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		f.logf("写 lease 文件失败：%v", err)
		return err
	}
	if err := os.Rename(tmp, f.path); err != nil {
		f.logf("替换 lease 文件失败：%v", err)
		return err
	}
	return nil
}

func (f *firewallManager) logf(format string, args ...any) {
	log.Printf("[hostd/firewall] "+format, args...)
}

// ufwAllows 解析 `ufw status` 输出，判断 port/tcp 是否已被放行。
// 兼容 `80/tcp`、`80`、`80,443/tcp`、`8000:8100/tcp`、`80/tcp (v6)`。
func ufwAllows(status string, port int) bool {
	for _, line := range strings.Split(status, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Status:") || strings.HasPrefix(line, "To ") || strings.HasPrefix(line, "--") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		to, action := fields[0], fields[1]
		if !strings.EqualFold(action, "ALLOW") {
			continue
		}
		// 只把“来源 Anywhere”的放行算作已开：限制来源的规则挡不住 Let's Encrypt。
		if len(fields) < 3 || !strings.HasPrefix(fields[2], "Anywhere") {
			continue
		}
		// 协议后缀作用于整个逗号列表（`9000,9001/udp` 是两个 UDP 端口），
		// 必须先摘掉再按逗号切分，否则第一项会被误判成 TCP。
		proto := ""
		if i := strings.Index(to, "/"); i >= 0 {
			proto = to[i+1:]
			to = to[:i]
		}
		if proto != "" && proto != "tcp" {
			continue
		}
		for _, item := range strings.Split(to, ",") {
			item = strings.TrimSpace(item)
			if strings.Contains(item, ":") {
				parts := strings.SplitN(item, ":", 2)
				lo, err1 := strconv.Atoi(parts[0])
				hi, err2 := strconv.Atoi(parts[1])
				if err1 == nil && err2 == nil && port >= lo && port <= hi {
					return true
				}
				continue
			}
			if n, err := strconv.Atoi(item); err == nil && n == port {
				return true
			}
		}
	}
	return false
}

// fwPermissionError 判断是否因为能力/地址族限制导致防火墙命令失败。
func fwPermissionError(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "operation not permitted") ||
		strings.Contains(m, "permission denied") ||
		strings.Contains(m, "address family not supported") ||
		strings.Contains(m, "eafnosupport")
}
