package vulnintel

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// newNucleiTree 造一个最小的模板树（.git + 一个 CVE 模板），让 BuildNucleiCmd
// 的可用性 os.Stat 与 FindNucleiTemplates 都能通过；不克隆真实模板，测试不碰网络、
// 不执行 nuclei。
func newNucleiTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	tmplDir := filepath.Join(dir, "http", "cves", "2021")
	if err := os.MkdirAll(tmplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmplDir, "CVE-2021-41773.yaml"), []byte("id: CVE-2021-41773\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// CountTemplates 是模板树自愈的判定基准：clone/pull 失败后「目录里一个 yaml
// 都没有」= 损坏残留，清理重 clone；有模板 = 只网络抖动，保留目录下次 pull。
func TestCountTemplates(t *testing.T) {
	dir := t.TempDir()
	if n := CountTemplates(dir); n != 0 {
		t.Fatalf("empty dir: got %d, want 0", n)
	}
	// 造一个 nuclei-templates 形状的树：子目录 + .yaml，外加一个不该数的 .yml/.txt
	y := filepath.Join(dir, "http", "cves", "2026")
	if err := os.MkdirAll(y, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(y, "a.yaml"), []byte("id: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(y, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.yaml"), []byte("id: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nope.yml"), []byte("id: y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n := CountTemplates(dir); n != 2 {
		t.Fatalf("got %d, want 2（.yml 与非 yaml 不计）", n)
	}
}

// TestNormalizeNucleiProxy 锁住归一化规则：空=直连、缺 scheme 才补 http://、
// 白名单 http/https/socks5、逗号 fail-closed、凭据与严格出口哨兵逐字节不变。
func TestNormalizeNucleiProxy(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr string // 非空：错误信息必须包含该子串
	}{
		{"空串", "", "", ""},
		{"纯空白", "   ", "", ""},
		{"裸 host:port 补 http", "127.0.0.1:8788", "http://127.0.0.1:8788", ""},
		{"已带 http 原样", "http://127.0.0.1:8788", "http://127.0.0.1:8788", ""},
		{"socks5 带凭据原样", "socks5://u:p@h:1080", "socks5://u:p@h:1080", ""},
		{"严格出口哨兵逐字节不变", "http://127.0.0.1:1", "http://127.0.0.1:1", ""},
		{"首尾空白被裁剪", "  http://h:1  ", "http://h:1", ""},
		{"不支持协议", "ftp://h:21", "", "协议"},
		{"缺主机", "http://", "", "主机"},
		{"缺端口(带 scheme)", "http://proxy.internal", "", "端口"},
		{"缺端口(裸主机)", "proxy.internal", "", "端口"},
		{"含逗号", "http://a,b:1", "", "逗号"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := normalizeNucleiProxy(c.in)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("normalizeNucleiProxy(%q) err=%v, want 含 %q", c.in, err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeNucleiProxy(%q) 意外报错: %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("normalizeNucleiProxy(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestEffectiveTimeout 锁住整轮超时的归一化：0/负值→10m，>30m 钳到 30m，
// 中间值原样。agent 侧 GNU timeout 的秒数由它推导，两处必须同一套规则。
func TestEffectiveTimeout(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want time.Duration
	}{
		{0, 10 * time.Minute},
		{-time.Minute, 10 * time.Minute},
		{5 * time.Minute, 5 * time.Minute},
		{10 * time.Minute, 10 * time.Minute},
		{30 * time.Minute, 30 * time.Minute},
		{45 * time.Minute, 30 * time.Minute},
	}
	for _, c := range cases {
		if got := EffectiveTimeout(c.in); got != c.want {
			t.Fatalf("EffectiveTimeout(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestBuildNucleiCmdProxy 证明 -proxy 真的作为两个独立 argv 元素发射，
// 且无代理时默认命令逐项不变（零回归）。
func TestBuildNucleiCmdProxy(t *testing.T) {
	dir := newNucleiTree(t)
	const target = "https://example.com"
	base := []string{"nuclei", "-target", target, "-t", dir, "-jsonl", "-silent", "-no-color", "-disable-update-check"}

	// 无代理：命令逐项等于基准，且不出现 -proxy。
	cmd, cancel, err := BuildNucleiCmd(context.Background(), dir, ScanArgs{Target: target})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cmd.Args, base) {
		t.Fatalf("无代理 args = %v, want %v", cmd.Args, base)
	}
	cancel()

	// 有代理：末尾连出 -proxy 与其值两个独立元素。
	cmd, cancel, err = BuildNucleiCmd(context.Background(), dir, ScanArgs{Target: target, Proxy: "http://127.0.0.1:8788"})
	if err != nil {
		t.Fatal(err)
	}
	n := len(cmd.Args)
	if n < 2 || cmd.Args[n-2] != "-proxy" || cmd.Args[n-1] != "http://127.0.0.1:8788" {
		t.Fatalf("有代理 args 末尾 = %v", cmd.Args)
	}
	cancel()

	// CVE/severity 分支与 -proxy 组合：-proxy 仍恰好一次、值紧随其后，
	// -t 为全树目录 + 命中 CVE 的模板两条。
	cmd, cancel, err = BuildNucleiCmd(context.Background(), dir, ScanArgs{
		Target:   target,
		Proxy:    "http://127.0.0.1:8788",
		CVEID:    "CVE-2021-41773",
		Severity: "critical,high",
	})
	if err != nil {
		t.Fatal(err)
	}
	proxyCount, sevCount, tmplCount := 0, 0, 0
	for i, a := range cmd.Args {
		switch a {
		case "-proxy":
			proxyCount++
			if i+1 >= len(cmd.Args) || cmd.Args[i+1] != "http://127.0.0.1:8788" {
				t.Fatalf("-proxy 后应紧跟代理值, args=%v", cmd.Args)
			}
		case "-severity":
			sevCount++
			if i+1 >= len(cmd.Args) || cmd.Args[i+1] != "critical,high" {
				t.Fatalf("-severity 后应紧跟严重度, args=%v", cmd.Args)
			}
		case "-t":
			tmplCount++
		}
	}
	if proxyCount != 1 || sevCount != 1 {
		t.Fatalf("-proxy/-severity 应各出现一次, got proxy=%d sev=%d: %v", proxyCount, sevCount, cmd.Args)
	}
	if tmplCount != 2 {
		t.Fatalf("-t 应为全树 + CVE 模板两条, got %d: %v", tmplCount, cmd.Args)
	}
	cancel()

	// 含逗号：拒绝构造被拆坏的 argv（cmd 为 nil，不返回半成品）。
	badCmd, badCancel, badErr := BuildNucleiCmd(context.Background(), dir, ScanArgs{Target: target, Proxy: "http://a,b:1"})
	if badCancel != nil {
		badCancel()
	}
	if badErr == nil || badCmd != nil {
		t.Fatalf("含逗号代理应返回 err 且 cmd==nil, got cmd=%v err=%v", badCmd, badErr)
	}

	// target 含 & 仍是单个 argv 元素（注入面不因代理改动而回归）。
	cmd, cancel, err = BuildNucleiCmd(context.Background(), dir, ScanArgs{Target: "https://t/api?a=1&b=2"})
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Args[2] != "https://t/api?a=1&b=2" {
		t.Fatalf("target 应保持单元素, got %q", cmd.Args[2])
	}
	cancel()
}

// TestBuildNucleiCmdRateLimit：资源自适应降速时追加 -rate-limit；0 时不出现，
// 保证默认命令与升级前逐项一致。
func TestBuildNucleiCmdRateLimit(t *testing.T) {
	dir := newNucleiTree(t)
	const target = "https://example.com"
	base := []string{"nuclei", "-target", target, "-t", dir, "-jsonl", "-silent", "-no-color", "-disable-update-check"}

	cmd, cancel, err := BuildNucleiCmd(context.Background(), dir, ScanArgs{Target: target})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cmd.Args, base) {
		t.Fatalf("rate=0 args = %v, want untouched %v", cmd.Args, base)
	}
	cancel()

	cmd, cancel, err = BuildNucleiCmd(context.Background(), dir, ScanArgs{Target: target, RateLimit: 75})
	if err != nil {
		t.Fatal(err)
	}
	n := len(cmd.Args)
	if n < 2 || cmd.Args[n-2] != "-rate-limit" || cmd.Args[n-1] != "75" {
		t.Fatalf("rate-limited args 末尾 = %v", cmd.Args)
	}
	cancel()
}
