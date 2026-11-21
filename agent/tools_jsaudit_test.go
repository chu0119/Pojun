package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

// v0.4.59：js_audit SPA/webpack 源码泄露审计——只落脚本+组命令，不自己发请求。
func TestJsAuditComposesCommand(t *testing.T) {
	dir := t.TempDir()
	ts := NewToolSet(nil, "worker")
	ts.SetRunDir(dir)
	tool := ts.jsAudit()
	in, _ := json.Marshal(map[string]any{"urls": []string{
		"https://a.example.com/", "https://a.example.com/static/js/app.9f2c1d.js", "bad", "https://a.example.com/",
	}})
	res, err := tool.Call(context.Background(), in, nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var out struct {
		RunWithBash string `json:"run_with_bash"`
		Count       int    `json:"count"`
	}
	if err := json.Unmarshal([]byte(toolResultText(t, res)), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, toolResultText(t, res))
	}
	if out.Count != 2 {
		t.Fatalf("count=%d want 2（非法 URL 与重复项应剔除）", out.Count)
	}
	if !strings.Contains(out.RunWithBash, "js_audit.py") ||
		!strings.Contains(out.RunWithBash, "--out") ||
		!strings.Contains(out.RunWithBash, "app.9f2c1d.js") {
		t.Fatalf("command=%q", out.RunWithBash)
	}
	// 脚本落盘且是合法 python（py_compile 由 CI 外部验证；这里只查非空）。
	p := filepath.Join(dir, "js_audit.py")
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("script missing: %v", err)
	}
	if info.Size() < 2000 {
		t.Fatalf("script too small: %d", info.Size())
	}
}

// 没有 runDir 时明确报错，而不是静默成功。
func TestJsAuditRequiresRunDir(t *testing.T) {
	ts := NewToolSet(nil, "reporter")
	tool := ts.jsAudit()
	in, _ := json.Marshal(map[string]any{"urls": []string{"https://a.example.com/"}})
	res, err := tool.Call(context.Background(), in, nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(toolResultText(t, res), "未启用") {
		t.Fatalf("expected 未启用 error, got: %s", toolResultText(t, res))
	}
}

// js_audit 的 user_agent：默认命令 golden、空串等价、--ua 注入、选项样值、非法拒绝。
func TestJsAuditUserAgentCommand(t *testing.T) {
	// 等价类共用同一个 dir（脚本每次覆盖写，互不影响）；命令里的绝对路径才可比。
	runTool := func(t *testing.T, dir string, ua any) (map[string]any, actool.Result) {
		t.Helper()
		ts := NewToolSet(nil, "worker")
		ts.SetRunDir(dir)
		tool := ts.jsAudit()
		in := map[string]any{"urls": []string{"https://a.example.com/"}}
		if ua != nil {
			in["user_agent"] = ua
		}
		raw, _ := json.Marshal(in)
		res, err := tool.Call(context.Background(), raw, nil)
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		if res.IsError {
			return nil, res // 错误结果是纯文本，不是 JSON
		}
		return toolResultJSON(t, res), res
	}

	// 默认（不传）：不含 --ua；显式空串与纯空白与不传完全等价。
	dir := t.TempDir()
	base, baseRes := runTool(t, dir, nil)
	if baseRes.IsError {
		t.Fatalf("默认调用不应报错: %s", toolResultText(t, baseRes))
	}
	baseCmd, _ := base["run_with_bash"].(string)
	if strings.Contains(baseCmd, "--ua") {
		t.Fatalf("默认命令不应含 --ua: %s", baseCmd)
	}

	for _, name := range []string{"empty", "blank"} {
		v := ""
		if name == "blank" {
			v = "   "
		}
		m, res := runTool(t, dir, v)
		if res.IsError {
			t.Fatalf("%s 不应报错: %s", name, toolResultText(t, res))
		}
		cmd, _ := m["run_with_bash"].(string)
		if cmd != baseCmd {
			t.Fatalf("%s 的命令应与默认完全一致:\n got: %s\nwant: %s", name, cmd, baseCmd)
		}
	}

	// 非空：--ua 出现一次且值被 quoteShell 包住；脚本仍落盘。
	m, res := runTool(t, dir, "MyUA/1.0")
	if res.IsError {
		t.Fatalf("合法 UA 不应报错: %s", toolResultText(t, res))
	}
	cmd, _ := m["run_with_bash"].(string)
	if !strings.Contains(cmd, "--ua 'MyUA/1.0'") {
		t.Fatalf("命令应含 --ua 'MyUA/1.0': %s", cmd)
	}
	if n := strings.Count(cmd, "--ua"); n != 1 {
		t.Fatalf("--ua 只应出现一次（实际 %d 次）: %s", n, cmd)
	}
	if !strings.Contains(cmd, "https://a.example.com/") {
		t.Fatalf("URL 不应被吞: %s", cmd)
	}
	if _, err := os.Stat(filepath.Join(dir, jsAuditScriptName)); err != nil {
		t.Fatalf("合法 UA 应照常落脚本: %v", err)
	}

	// 选项样值：--ua 后面的值以 - 开头或与选项同名，仍被当作值消费、URL 不被错位。
	for _, v := range []string{"--out", "--ua", "-A", "-Mozilla/5.0"} {
		m, res := runTool(t, dir, v)
		if res.IsError {
			t.Fatalf("选项样值 %q 不应报错: %s", v, toolResultText(t, res))
		}
		cmd, _ := m["run_with_bash"].(string)
		if !strings.Contains(cmd, "--ua '"+v+"' 'https://a.example.com/'") {
			t.Fatalf("选项样值 %q 应作为完整 argv 出现在 URL 之前: %s", v, cmd)
		}
	}

	// 非法值：全新 dir，报错含 user_agent 与偏移、脚本不落盘。
	badDir := t.TempDir()
	_, res = runTool(t, badDir, "a\r\nX-Injected: 1")
	if !res.IsError {
		t.Fatal("CRLF UA 应被拒绝")
	}
	if msg := toolResultText(t, res); !strings.Contains(msg, "user_agent") || !strings.Contains(msg, "0-based") {
		t.Fatalf("错误应含 user_agent 与偏移: %s", msg)
	}
	if _, err := os.Stat(filepath.Join(badDir, jsAuditScriptName)); !os.IsNotExist(err) {
		t.Fatal("非法 UA 不应写出脚本")
	}
}
