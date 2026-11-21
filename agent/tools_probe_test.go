package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// v0.4.27：http_probe 批量探活工具——只落脚本+组命令，不自己发请求。
func TestHTTPProbeComposesCommand(t *testing.T) {
	dir := t.TempDir()
	ts := NewToolSet(nil, "worker")
	ts.SetRunDir(dir)
	tool := ts.httpProbe()
	in, _ := json.Marshal(map[string]any{"urls": []string{
		"https://a.example.com/x", "https://b.example.com/y?q=1", "not-a-url", "https://a.example.com/x",
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
	if !strings.Contains(out.RunWithBash, "probe.sh") || !strings.Contains(out.RunWithBash, "https://b.example.com/y?q=1") {
		t.Fatalf("command=%q", out.RunWithBash)
	}
	// 脚本已落盘（调用方式是 `bash <script>`，不依赖可执行位——Windows 无 exec 位）。
	script := filepath.Join(dir, "probe.sh")
	info, err := os.Stat(script)
	if err != nil {
		t.Fatalf("script missing: %v", err)
	}
	if info.Size() == 0 {
		t.Fatalf("script empty")
	}
	// 第二次调用复用同一脚本（幂等落盘）。
	res2, _ := tool.Call(context.Background(), in, nil)
	var out2 struct {
		RunWithBash string `json:"run_with_bash"`
	}
	_ = json.Unmarshal([]byte(toolResultText(t, res2)), &out2)
	if out2.RunWithBash != out.RunWithBash {
		t.Fatalf("command not stable across calls:\n%s\n%s", out.RunWithBash, out2.RunWithBash)
	}
}

// 没有 runDir（如 reporter chat 工具集）时明确报错，而不是静默成功。
func TestHTTPProbeRequiresRunDir(t *testing.T) {
	ts := NewToolSet(nil, "reporter")
	tool := ts.httpProbe()
	in, _ := json.Marshal(map[string]any{"urls": []string{"https://a.example.com/"}})
	res, err := tool.Call(context.Background(), in, nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(toolResultText(t, res), "未启用") {
		t.Fatalf("expected 未启用 error, got: %s", toolResultText(t, res))
	}
}

// user_agent 三态：省略/空串/纯空白必须与现状逐字节一致；非空才注入 --ua。
func TestHTTPProbeUserAgentCommand(t *testing.T) {
	dir := t.TempDir()
	ts := NewToolSet(nil, "worker")
	ts.SetRunDir(dir)
	tool := ts.httpProbe()

	call := func(extra map[string]any) (string, bool) {
		in := map[string]any{"urls": []string{"https://a.example.com/x"}}
		for k, v := range extra {
			in[k] = v
		}
		raw, _ := json.Marshal(in)
		res, err := tool.Call(context.Background(), raw, nil)
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		if res.IsError {
			return toolResultText(t, res), true
		}
		var out struct {
			RunWithBash string `json:"run_with_bash"`
		}
		if err := json.Unmarshal([]byte(toolResultText(t, res)), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out.RunWithBash, false
	}

	base, isErr := call(nil)
	if isErr {
		t.Fatalf("默认调用不应报错: %s", base)
	}
	if strings.Contains(base, "--ua") {
		t.Fatalf("默认命令不应含 --ua: %s", base)
	}
	for _, v := range []string{"", "   "} {
		got, isErr := call(map[string]any{"user_agent": v})
		if isErr {
			t.Fatalf("user_agent=%q 不应报错: %s", v, got)
		}
		if got != base {
			t.Fatalf("user_agent=%q 的命令应与默认逐字节一致:\n%s\n%s", v, base, got)
		}
	}

	// 合法 UA 的命令形态；选项样值必须被当作一个完整 argv，而不是拆成 curl 选项。
	//（放在「非法值不落脚本」之前：ensureProbeScript 在首个合法调用就会落盘，
	//  非法用例必须在脚本尚未存在的目录里断言「没写脚本」。）
	for _, v := range []string{"MyUA/1.0", "x -o /tmp/pwn", "-A", "--ua", "--out", "-Mozilla/5.0"} {
		got, isErr := call(map[string]any{"user_agent": v})
		if isErr {
			t.Fatalf("user_agent=%q 不应报错: %s", v, got)
		}
		if !strings.Contains(got, "--ua '"+v+"'") {
			t.Fatalf("UA 应被单引号包成单个参数（--ua '%s'）: %s", v, got)
		}
	}
	if strings.Count(callOnce(t, ts, "https://a.example.com/x", "MyUA/1.0"), "--ua") != 1 {
		t.Fatalf("--ua 只应出现一次（flag），URL 里的 --ua 不应参与解析")
	}

	// 非法值：报错、不落脚本。换全新目录——本测试前面的合法调用已把 probe.sh
	// 写进 dir，「脚本不存在」只有在一个还没被合法调用污染过的目录里才能断言。
	badDir := t.TempDir()
	badTS := NewToolSet(nil, "worker")
	badTS.SetRunDir(badDir)
	badTool := badTS.httpProbe()
	badIn, _ := json.Marshal(map[string]any{
		"urls":       []string{"https://a.example.com/x"},
		"user_agent": "a\r\nX-Injected: 1",
	})
	badRes, err := badTool.Call(context.Background(), badIn, nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !badRes.IsError {
		t.Fatalf("CRLF UA 应被拒绝，实际: %s", toolResultText(t, badRes))
	}
	bad := toolResultText(t, badRes)
	if !strings.Contains(bad, "0-based") || !strings.Contains(bad, "0x0D") {
		t.Fatalf("错误信息应带偏移与字节值: %s", bad)
	}
	if _, err := os.Stat(filepath.Join(badDir, probeScriptName)); !os.IsNotExist(err) {
		t.Fatalf("非法 UA 不应写出脚本")
	}

	// 脚本正文包含惰性分支与带引号的数组展开；且必须是合法 bash（bash -n）。
	_, _ = call(map[string]any{"user_agent": "MyUA/1.0"})
	body, err := os.ReadFile(filepath.Join(dir, probeScriptName))
	if err != nil {
		t.Fatalf("读脚本: %v", err)
	}
	for _, want := range []string{"ua_args=()", `"${ua_args[@]}"`, `[ "$1" = "--ua" ]`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("脚本应含 %q", want)
		}
	}
	if bash, lookErr := exec.LookPath("bash"); lookErr == nil {
		if out, runErr := exec.Command(bash, "-n", filepath.Join(dir, probeScriptName)).CombinedOutput(); runErr != nil {
			t.Fatalf("脚本语法检查失败: %v\n%s", runErr, out)
		}
	}
}

// callOnce 一次性的工具调用（测试内部辅助）。
func callOnce(t *testing.T, ts *ToolSet, url, ua string) string {
	t.Helper()
	in, _ := json.Marshal(map[string]any{"urls": []string{url}, "user_agent": ua})
	res, err := ts.httpProbe().Call(context.Background(), in, nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.IsError {
		t.Fatalf("不应报错: %s", toolResultText(t, res))
	}
	return toolResultText(t, res)
}

// 真跑：用假 curl 记录 argv，确认 -A 后恰好是完整 UA，URL 仍是位置参数。
func TestHTTPProbeUserAgentPassesSingleArgv(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("未找到 bash")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "argv.txt")
	script := "#!/usr/bin/env bash\nprintf '%s\\n' \"$@\" > " + quoteShell(record) + "\necho '200 0.001'\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	ts := NewToolSet(nil, "worker")
	ts.SetRunDir(dir)
	tool := ts.httpProbe()
	in, _ := json.Marshal(map[string]any{
		"urls":       []string{"https://a.example.com/x"},
		"user_agent": "My UA/1.0 -o /tmp/pwn",
	})
	res, err := tool.Call(context.Background(), in, nil)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		RunWithBash string `json:"run_with_bash"`
	}
	if err := json.Unmarshal([]byte(toolResultText(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, "-c", strings.ReplaceAll(out.RunWithBash, `\`, `/`))
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("命令执行失败: %v\n%s", err, b)
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("假 curl 未记录 argv: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")
	found := false
	for i, a := range args {
		if a == "-A" {
			if i+1 >= len(args) || args[i+1] != "My UA/1.0 -o /tmp/pwn" {
				t.Fatalf("-A 后应是完整 UA，实际 argv=%q", args)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("argv 里没有 -A: %q", args)
	}
	if args[len(args)-1] != "https://a.example.com/x" {
		t.Fatalf("URL 应是最后一个位置参数: %q", args)
	}
}
