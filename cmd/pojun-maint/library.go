package main

// 知识库内容包的离线校验与导入。
//
//	pojun-maint library lint   --pack skills/web-attack-methods/kb-packs/kb-web-api
//	pojun-maint library import --pack <dir> --config /opt/pojun/config.json [--status approved]
//
// lint 不连数据库，可放进 CI；import 走与启动播种同一套 SeedKBEntriesBatch，
// 同一 id+version 只播一次，不覆盖用户已有条目。
//
// 外部导入默认 draft：manifest 里的 review_status 可能是上游自填的，不能作为
// 「已人工审核」的证据。只有显式 --status approved 才直接进默认检索。

import (
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/chu0119/pojun/db"
)

var (
	dangerousCommandRE = regexp.MustCompile(`(?i)(rm\s+-[a-z]*r[a-z]*f?\s+/(\s|$)|mkfs(\.|\s)|dd\s+if=.*of=/dev/|:\(\)\s*\{\s*:\|:&\s*\};:)`)
	secretRE           = regexp.MustCompile(`(AKIA[0-9A-Z]{16}|sk_live_[0-9A-Za-z]{16,}|ghp_[0-9A-Za-z]{20,}|-----BEGIN (RSA |EC |OPENSSH )?PRIVATE KEY-----)`)
	ipv4RE             = regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|1?\d?\d)\b`)
)

// publicIPs 返回正文里的非文档/非内网 IPv4。Go RE2 不支持负向前瞻，故先匹配再过滤。
func publicIPs(s string) []string {
	var out []string
	for _, ip := range ipv4RE.FindAllString(s, -1) {
		switch {
		case strings.HasPrefix(ip, "10."),
			strings.HasPrefix(ip, "127."),
			strings.HasPrefix(ip, "169.254."),
			strings.HasPrefix(ip, "192.168."),
			strings.HasPrefix(ip, "192.0.2."),
			strings.HasPrefix(ip, "198.51.100."),
			strings.HasPrefix(ip, "203.0.113."):
			continue
		}
		if strings.HasPrefix(ip, "172.") {
			var b int
			if _, err := fmt.Sscanf(ip, "172.%d.", &b); err == nil && b >= 16 && b <= 31 {
				continue
			}
		}
		if strings.HasPrefix(ip, "100.") {
			var b int
			if _, err := fmt.Sscanf(ip, "100.%d.", &b); err == nil && b >= 64 && b <= 127 {
				continue // 100.64.0.0/10 运营商级 NAT / 云元数据
			}
		}
		out = append(out, ip)
	}
	return out
}

func runLibrary(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: pojun-maint library <lint|import> --pack <dir> [--config path] [--status draft|approved]")
		os.Exit(2)
	}
	sub := args[0]
	fs := flag.NewFlagSet("library "+sub, flag.ExitOnError)
	packDir := fs.String("pack", "", "内容包目录（含 manifest.json）")
	cfgPath := fs.String("config", "/opt/pojun/config.json", "import: POJUN config.json 路径")
	status := fs.String("status", "", "import: 初始状态（缺省 draft；approved 才进默认检索）")
	_ = fs.Parse(args[1:])
	if strings.TrimSpace(*packDir) == "" {
		fail(fmt.Errorf("必须指定 --pack"))
	}
	switch sub {
	case "lint":
		if err := lintKBPack(*packDir); err != nil {
			fail(err)
		}
	case "import":
		if err := importKBPack(*packDir, *cfgPath, *status); err != nil {
			fail(err)
		}
	default:
		fmt.Fprintln(os.Stderr, "unknown library subcommand:", sub)
		os.Exit(2)
	}
}

func lintKBPack(dir string) error {
	pack, err := db.LoadKBPack(dir)
	if err != nil {
		return err
	}
	errs := db.ValidateKBPack(pack)
	warnings := 0
	for _, e := range pack.Entries {
		prefix := "《" + e.Title + "》"
		for _, c := range e.Commands {
			if dangerousCommandRE.MatchString(c) {
				errs = append(errs, fmt.Errorf("%s 含破坏性命令：%s", prefix, clipForLint(c, 120)))
			}
			if secretRE.MatchString(c) {
				errs = append(errs, fmt.Errorf("%s 命令里疑似硬编码密钥", prefix))
			}
			if ips := publicIPs(c); len(ips) > 0 {
				fmt.Printf("警告：%s 含公网 IP %s（内容包应使用文档占位网段）\n", prefix, strings.Join(ips, ","))
				warnings++
			}
		}
		for _, text := range []string{e.Title, e.Description, e.Detail} {
			if secretRE.MatchString(text) {
				errs = append(errs, fmt.Errorf("%s 正文疑似含密钥", prefix))
				break
			}
			if ips := publicIPs(text); len(ips) > 0 {
				fmt.Printf("警告：%s 正文含公网 IP %s（内容包应使用文档占位网段）\n", prefix, strings.Join(ips, ","))
				warnings++
				break
			}
		}
	}
	for _, e := range errs {
		fmt.Printf("错误：%v\n", e)
	}
	m := pack.Manifest
	fmt.Printf("包 %s@%s：%d 条，错误 %d，警告 %d\n", m.ID, m.Version, len(pack.Entries), len(errs), warnings)
	if len(errs) > 0 {
		return fmt.Errorf("内容包校验未通过（%d 个错误）", len(errs))
	}
	return nil
}

func importKBPack(dir, cfgPath, status string) error {
	pack, err := db.LoadKBPack(dir)
	if err != nil {
		return err
	}
	// 外部导入默认 draft，不信任 manifest 自填的 review_status。
	target := db.KBStatusDraft
	if strings.TrimSpace(status) != "" {
		target = strings.TrimSpace(status)
	}
	if err := db.SetKBPackStatus(pack, target); err != nil {
		return fmt.Errorf("--status 只能是 draft/approved")
	}
	dsn, err := loadDSN(cfgPath)
	if err != nil {
		if env := strings.TrimSpace(os.Getenv("POJUN_PG_DSN")); env != "" {
			dsn, err = env, nil
		}
	}
	if err != nil {
		return err
	}
	pg, err := db.Open(dsn)
	if err != nil {
		return fmt.Errorf("连接数据库失败：%w", err)
	}
	defer pg.Close()
	if err := pg.SeedKBPack(pack); err != nil {
		return err
	}
	fmt.Printf("已导入包 %s@%s：%d 条（状态 %s）。同一版本重复导入会被 flag 跳过。\n",
		pack.Manifest.ID, pack.Manifest.Version, len(pack.Entries), target)
	if target == db.KBStatusDraft {
		fmt.Println("提示：草稿不会进入默认检索；在知识页采纳（approved）后才对 agent 可见。")
	}
	return nil
}

func clipForLint(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
