package hostd

// 防火墙租约状态机测试：注入假后端（不执行真实 ufw/firewall-cmd）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeFirewall struct {
	mu     sync.Mutex
	calls  []string
	rules  map[string]bool // "80/tcp" 是否已放行
	failOn map[string]error
}

func newFakeFirewall() *fakeFirewall {
	return &fakeFirewall{rules: map[string]bool{}, failOn: map[string]error{}}
}

func (f *fakeFirewall) run(_ context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, key)
	if err, ok := f.failOn[key]; ok {
		return "", err
	}
	switch {
	case name == "ufw" && len(args) == 1 && args[0] == "status":
		// 与真实 ufw 一致：allow 过的端口会出现在 status 里。
		var b strings.Builder
		b.WriteString("Status: active\n\nTo                         Action      From\n--                         ------      ----\n")
		ports := make([]string, 0, len(f.rules))
		for port := range f.rules {
			ports = append(ports, port)
		}
		sort.Strings(ports)
		for _, port := range ports {
			fmt.Fprintf(&b, "%-26s ALLOW       Anywhere\n", port)
		}
		return b.String(), nil
	case name == "ufw" && len(args) == 2 && args[0] == "allow":
		f.rules[args[1]] = true
		return "", nil
	case name == "ufw" && len(args) == 3 && args[0] == "delete" && args[1] == "allow":
		if !f.rules[args[2]] {
			return "", errors.New("Could not delete non-existent rule")
		}
		delete(f.rules, args[2])
		return "", nil
	case name == "firewall-cmd" && strings.HasPrefix(args[0], "--query-port="):
		if f.rules[strings.TrimPrefix(args[0], "--query-port=")] {
			return "yes\n", nil
		}
		return "", errors.New("firewall-cmd --query-port: exit status 1")
	case name == "firewall-cmd" && strings.HasPrefix(args[0], "--add-port="):
		f.rules[strings.TrimPrefix(args[0], "--add-port=")] = true
		return "success\n", nil
	case name == "firewall-cmd" && strings.HasPrefix(args[0], "--remove-port="):
		port := strings.TrimPrefix(args[0], "--remove-port=")
		if !f.rules[port] {
			return "", errors.New("NOT_ENABLED: port not in public")
		}
		delete(f.rules, port)
		return "success\n", nil
	}
	return "", fmt.Errorf("未预期的命令: %s", key)
}

func (f *fakeFirewall) has(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

func newTestFirewall(t *testing.T, backend string) (*firewallManager, *fakeFirewall, string) {
	t.Helper()
	ff := newFakeFirewall()
	path := filepath.Join(t.TempDir(), "firewall-leases.json")
	f := &firewallManager{
		leases:  map[string]*fwLease{},
		path:    path,
		backend: backend,
		active:  true,
		runCmd:  ff.run,
	}
	return f, ff, path
}

func TestFirewallOpenAlreadyAllowedIsNoop(t *testing.T) {
	f, ff, path := newTestFirewall(t, "ufw")
	ff.rules["80/tcp"] = true
	id, changed, err := f.open(context.Background(), 80, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if id != "" || changed {
		t.Fatalf("原本已放行不应产生租约: id=%q changed=%v", id, changed)
	}
	if ff.has("ufw allow 80/tcp") {
		t.Fatal("不应对已放行端口再次 allow")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("不应留下租约文件: %v", err)
	}
}

func TestFirewallOpenReleaseRoundTrip(t *testing.T) {
	f, ff, path := newTestFirewall(t, "ufw")
	id, changed, err := f.open(context.Background(), 443, 10*time.Minute)
	if err != nil || !changed || id == "" {
		t.Fatalf("open: id=%q changed=%v err=%v", id, changed, err)
	}
	if !ff.rules["443/tcp"] {
		t.Fatal("后端规则未添加")
	}
	if b, err := os.ReadFile(path); err != nil || !strings.Contains(string(b), "443") {
		t.Fatalf("租约必须先落盘: %v %s", err, string(b))
	}
	if err := f.release(context.Background(), id); err != nil {
		t.Fatalf("release: %v", err)
	}
	if ff.rules["443/tcp"] {
		t.Fatal("规则未撤销")
	}
	f.mu.Lock()
	_, alive := f.leases[id]
	f.mu.Unlock()
	if alive {
		t.Fatal("租约应删除")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("租约应清空文件: %v", err)
	}
	// 幂等：重复 release 不报错。
	if err := f.release(context.Background(), id); err != nil {
		t.Fatalf("release 应幂等: %v", err)
	}
}

func TestFirewallOpenRenewsExistingLease(t *testing.T) {
	f, _, _ := newTestFirewall(t, "ufw")
	id1, changed1, err := f.open(context.Background(), 80, 10*time.Minute)
	if err != nil || !changed1 {
		t.Fatalf("first open: %v %v", id1, err)
	}
	f.mu.Lock()
	firstExpiry := f.leases[id1].ExpiresAt
	f.mu.Unlock()
	// 第二次 open 时端口已放行：应续租而不是新建，也不应再改规则。
	id2, changed2, err := f.open(context.Background(), 80, 20*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id1 || changed2 {
		t.Fatalf("同端口应续租: id1=%q id2=%q changed=%v", id1, id2, changed2)
	}
	f.mu.Lock()
	newExpiry := f.leases[id1].ExpiresAt
	f.mu.Unlock()
	if !newExpiry.After(firstExpiry) {
		t.Fatalf("续租后到期时间应变晚: %v → %v", firstExpiry, newExpiry)
	}
}

func TestFirewallOpenFailureRollsBackLease(t *testing.T) {
	f, ff, path := newTestFirewall(t, "ufw")
	ff.failOn["ufw allow 80/tcp"] = errors.New("Operation not permitted")
	_, _, err := f.open(context.Background(), 80, 10*time.Minute)
	if err == nil {
		t.Fatal("add 失败应返回错误")
	}
	if !strings.Contains(err.Error(), "CAP_NET_ADMIN") {
		t.Fatalf("权限类错误应提示能力缺失: %v", err)
	}
	f.mu.Lock()
	n := len(f.leases)
	f.mu.Unlock()
	if n != 0 {
		t.Fatalf("撤销成功时不应保留租约，得到 %d", n)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("租约文件应清理: %v", err)
	}
}

func TestFirewallOpenRejectsNonACMEPort(t *testing.T) {
	f, ff, _ := newTestFirewall(t, "ufw")
	if _, _, err := f.open(context.Background(), 22, time.Minute); err == nil {
		t.Fatal("非 80/443 必须拒绝")
	}
	ff.mu.Lock()
	calls := len(ff.calls)
	ff.mu.Unlock()
	if calls != 0 {
		t.Fatalf("拒绝前不应执行任何后端命令")
	}
}

// TTL 到期但租约被续租延长时，旧的到期回调不能提前撤销规则。
func TestFirewallExpireHonorsExtendedExpiry(t *testing.T) {
	f, ff, _ := newTestFirewall(t, "ufw")
	id, _, err := f.open(context.Background(), 80, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.leases[id].ExpiresAt = time.Now().UTC().Add(time.Hour)
	f.mu.Unlock()
	f.expire(context.Background(), id)
	f.mu.Lock()
	_, alive := f.leases[id]
	f.mu.Unlock()
	if !alive || !ff.rules["80/tcp"] {
		t.Fatal("未到期（已续租）的租约不应被撤销")
	}
}

func TestFirewallReleaseFailureKeepsLeaseForRetry(t *testing.T) {
	f, ff, _ := newTestFirewall(t, "ufw")
	id, _, err := f.open(context.Background(), 80, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ff.failOn["ufw delete allow 80/tcp"] = errors.New("permission denied")
	if err := f.release(context.Background(), id); err == nil {
		t.Fatal("撤销失败应返回错误")
	}
	f.mu.Lock()
	lease, alive := f.leases[id]
	f.mu.Unlock()
	if !alive {
		t.Fatal("撤销失败必须保留租约（否则持久规则会永久敞开且无人追踪）")
	}
	if !lease.ExpiresAt.After(time.Now().UTC()) {
		t.Fatalf("失败后应安排重试时间: %v", lease.ExpiresAt)
	}
	delete(ff.failOn, "ufw delete allow 80/tcp")
	if err := f.release(context.Background(), id); err != nil {
		t.Fatalf("重试应成功: %v", err)
	}
	if ff.rules["80/tcp"] {
		t.Fatal("重试后规则应撤销")
	}
}

// hostd 重启恢复：未过期的 open 租约接管，过期租约撤销，非法端口一律不碰
// （否则被篡改的租约文件会让 ufw delete 删掉管理员自己的规则）。
func TestFirewallRecoverHandlesLeases(t *testing.T) {
	f, ff, path := newTestFirewall(t, "ufw")
	now := time.Now().UTC()
	saved := []*fwLease{
		{ID: "live", Port: 80, Proto: "tcp", Backend: "ufw", State: "open", ExpiresAt: now.Add(time.Hour)},
		{ID: "stale", Port: 443, Proto: "tcp", Backend: "ufw", State: "open", ExpiresAt: now.Add(-time.Minute)},
		{ID: "evil", Port: 22, Proto: "tcp", Backend: "ufw", State: "open", ExpiresAt: now.Add(time.Hour)},
	}
	b, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	ff.rules["443/tcp"] = true
	f.recover()

	f.mu.Lock()
	_, liveOK := f.leases["live"]
	_, staleOK := f.leases["stale"]
	_, evilOK := f.leases["evil"]
	f.mu.Unlock()
	if !liveOK {
		t.Fatal("未过期的 open 租约应被接管")
	}
	if staleOK {
		t.Fatal("过期租约撤销后不应留在内存")
	}
	if evilOK {
		t.Fatal("非法端口租约不应被接管")
	}
	for _, bad := range []string{"ufw delete allow 22/tcp", "ufw allow 22/tcp"} {
		if ff.has(bad) {
			t.Fatalf("绝不应触碰非 80/443 端口: %s", bad)
		}
	}
	if ff.rules["443/tcp"] {
		t.Fatal("过期租约的规则应撤销")
	}
}

// firewalld 后端：query 退出码 1 表示未放行而不是故障；remove 的 NOT_ENABLED 视为已撤销。
func TestFirewallFirewalldBackend(t *testing.T) {
	f, ff, _ := newTestFirewall(t, "firewalld")
	id, changed, err := f.open(context.Background(), 80, 10*time.Minute)
	if err != nil || !changed || id == "" {
		t.Fatalf("open: id=%q changed=%v err=%v", id, changed, err)
	}
	if !ff.rules["80/tcp"] {
		t.Fatal("firewalld 规则未添加")
	}
	if err := f.release(context.Background(), id); err != nil {
		t.Fatalf("release: %v", err)
	}
	if ff.rules["80/tcp"] {
		t.Fatal("firewalld 规则未移除")
	}
	// 规则其实已经不在了：remove 报 NOT_ENABLED，release 仍应成功并清租约。
	f.mu.Lock()
	f.leases["ghost"] = &fwLease{ID: "ghost", Port: 443, Proto: "tcp", Backend: "firewalld", State: "open", ExpiresAt: time.Now().UTC().Add(time.Hour)}
	f.mu.Unlock()
	if err := f.release(context.Background(), "ghost"); err != nil {
		t.Fatalf("NOT_ENABLED 应视为已撤销: %v", err)
	}
}
