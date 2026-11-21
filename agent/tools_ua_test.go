package agent

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
)

func TestNormalizeToolUserAgent(t *testing.T) {
	long1024 := strings.Repeat("A", 1024)
	long1025 := strings.Repeat("A", 1025)

	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
		errHas  []string
	}{
		{name: "空串", in: "", want: ""},
		{name: "纯空白", in: " \t \r\n ", want: ""},
		{name: "去首尾空白", in: "  MyUA/1.0  ", want: "MyUA/1.0"},
		{name: "Chrome", in: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36", want: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"},
		{name: "iPhone", in: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1", want: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"},
		{name: "Googlebot", in: "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)", want: "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)"},
		{name: "1024 字节", in: long1024, want: long1024},
		{name: "1025 字节", in: long1025, wantErr: true, errHas: []string{"1025", "1024", "过长"}},
		{name: "CRLF 注入", in: "a\r\nX-Injected: 1", wantErr: true, errHas: []string{"0-based", "0x0D", "user_agent"}},
		{name: "LF", in: "a\nb", wantErr: true, errHas: []string{"0-based", "0x0A"}},
		{name: "CR", in: "a\rb", wantErr: true, errHas: []string{"0-based", "0x0D"}},
		{name: "NUL", in: "a\x00b", wantErr: true, errHas: []string{"0-based", "0x00"}},
		{name: "DEL", in: "a\x7fb", wantErr: true, errHas: []string{"0-based", "0x7F"}},
		{name: "控制字符", in: "a\x1bb", wantErr: true, errHas: []string{"0-based", "0x1B"}},
		{name: "中文", in: "中文UA", wantErr: true, errHas: []string{"0-based", "0xE4"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := normalizeToolUserAgent(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际得到 %q", got)
				}
				msg := err.Error()
				for _, want := range c.errHas {
					if !strings.Contains(msg, want) {
						t.Errorf("错误信息应含 %q，实际 %q", want, msg)
					}
				}
				// 错误信息不得回显完整 UA 原文，避免把恶意内容写进日志。
				if len(c.in) > 0 && strings.Contains(msg, c.in) {
					t.Errorf("错误信息不应回显 UA 原文：%q", msg)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错：%v", err)
			}
			if got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func FuzzNormalizeToolUserAgent(f *testing.F) {
	f.Add("")
	f.Add("MyUA/1.0")
	f.Add("a b\tc")
	f.Add("a\r\nX-Injected: 1")
	f.Add("中文")
	f.Fuzz(func(t *testing.T, s string) {
		got, err := normalizeToolUserAgent(s)
		if err != nil {
			return
		}
		if len(got) > toolUserAgentMaxBytes {
			t.Fatalf("接受的值超过上限：%d", len(got))
		}
		for i := 0; i < len(got); i++ {
			if got[i] < 0x20 || got[i] > 0x7e {
				t.Fatalf("接受的值第 %d 字节为 0x%02X，超出可打印 ASCII", i, got[i])
			}
		}
	})
}

// 跨工具引号注入回归（定稿第 12 条）：恶意但合法（可打印 ASCII）的 UA 在
// 四个工具的生成物里都必须被 quoteShell 包成单个 argv——单引号按 quoteShell
// 的断开转义形态（反斜杠 + 单引号）出现——且整条命令是合法 bash。
func TestUserAgentQuoteInjectionAcrossTools(t *testing.T) {
	sh, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("未找到 bash")
	}
	bashNSyntax := func(t *testing.T, cmd string) {
		t.Helper()
		// 不做 \→/ 替换：那会撕开 quoteShell 的 '\'' 形态（'x'\''$()' 变 'x'/''$()'，
		// 引号不配对）。bash -n 只查语法，路径里的反斜杠无害。
		if b, err := exec.Command(sh, "-n", "-c", cmd).CombinedOutput(); err != nil {
			t.Fatalf("bash -n 不通过: %v\n%s\n命令: %s", err, b, cmd)
		}
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "x.py")

	// 四个工具各自的生成物。
	generators := map[string]func(ua string) string{
		"http_probe": func(ua string) string {
			d := t.TempDir()
			ts := NewToolSet(nil, "worker")
			ts.SetRunDir(d)
			tool := ts.httpProbe()
			in, _ := json.Marshal(map[string]any{"urls": []string{"https://a.example.com/x"}, "user_agent": ua})
			res, err := tool.Call(context.Background(), in, nil)
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			if res.IsError {
				t.Fatalf("http_probe 对 %q 不应报错: %s", ua, toolResultText(t, res))
			}
			return toolResultJSON(t, res)["run_with_bash"].(string)
		},
		"api_contract": func(ua string) string {
			return buildContractCmd("https://t.example.com", []string{"/openapi.json"}, script, dir, ua)
		},
		"diff_responses": func(ua string) string {
			return buildDiffScript(diffArgs{
				URLA: "https://a.test/x", URLB: "https://a.test/y", UserAgent: ua,
			}, script, dir)
		},
		"js_audit": func(ua string) string {
			d := t.TempDir()
			ts := NewToolSet(nil, "worker")
			ts.SetRunDir(d)
			tool := ts.jsAudit()
			in, _ := json.Marshal(map[string]any{"urls": []string{"https://a.example.com/"}, "user_agent": ua})
			res, err := tool.Call(context.Background(), in, nil)
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
			if res.IsError {
				t.Fatalf("js_audit 对 %q 不应报错: %s", ua, toolResultText(t, res))
			}
			return toolResultJSON(t, res)["run_with_bash"].(string)
		},
	}

	for _, ua := range []string{`x'$(id)`, "a b", "a`id`", "a;id", `a\nb`, `x'$(curl evil)`} {
		for name, gen := range generators {
			got := gen(ua)
			want := "'" + strings.ReplaceAll(ua, "'", `'\''`) + "'"
			if !strings.Contains(got, want) {
				t.Fatalf("%s 对 UA %q 的生成物应含 quoteShell 形态 %q:\n%s", name, ua, want, got)
			}
			bashNSyntax(t, got)
		}
	}
}

// schema 断言（定稿第 14 条）：四工具的 user_agent 都是可选 string、描述按事实分写、
// 不进 required。
func TestUserAgentSchemaProperties(t *testing.T) {
	ts := NewToolSet(nil, "")
	byName := map[string]actool.CoreTool{}
	for _, tl := range ts.WorkerTools() {
		byName[tl.Name()] = tl
	}
	tools := []struct{ name, mustNotContain string }{
		{"http_probe", "Chrome/131"},
		{"api_contract", "Chrome/131"},
		{"diff_responses", "Chrome/131"},
		{"js_audit", ""}, // js_audit 的默认确实是平台 Chrome/131，描述允许提。
	}
	for _, tt := range tools {
		tl, ok := byName[tt.name]
		if !ok {
			t.Fatalf("WorkerTools 里找不到 %s", tt.name)
		}
		schema := tl.InputSchema()
		props, _ := schema["properties"].(map[string]any)
		ua, ok := props["user_agent"].(map[string]any)
		if !ok {
			t.Fatalf("%s 的 schema 缺 properties.user_agent: %v", tt.name, schema)
		}
		if ua["type"] != "string" {
			t.Fatalf("%s 的 user_agent.type = %v，期望 string", tt.name, ua["type"])
		}
		desc, _ := ua["description"].(string)
		if tt.mustNotContain != "" && strings.Contains(desc, tt.mustNotContain) {
			t.Fatalf("%s 的 user_agent 描述不得承诺 %q（默认是 curl 自报 UA）: %s",
				tt.name, tt.mustNotContain, desc)
		}
		if !strings.Contains(desc, "User-Agent") {
			t.Fatalf("%s 的 user_agent 描述应说明只改 User-Agent 头: %s", tt.name, desc)
		}
		if req, ok := schema["required"].([]string); ok {
			for _, r := range req {
				if r == "user_agent" {
					t.Fatalf("%s 的 user_agent 不得是必填", tt.name)
				}
			}
		}
	}
}
