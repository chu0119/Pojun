package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeCodeFixture(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// codeAuditToolSet 造一个 tasks/<id>/i1 的 runDir 与 tasks/<id>/code 源码目录。
func codeAuditToolSet(t *testing.T, files map[string]string) *ToolSet {
	t.Helper()
	base := t.TempDir()
	taskDir := filepath.Join(base, "tasks", "1")
	runDir := filepath.Join(taskDir, "i1")
	codeDir := filepath.Join(taskDir, "code")
	for _, d := range []string{runDir, codeDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeCodeFixture(t, codeDir, files)
	ts := NewToolSet(nil, "")
	ts.SetRunDir(runDir)
	return ts
}

func TestRepoMapDetectsFrameworkRoutesAndSecrets(t *testing.T) {
	ts := codeAuditToolSet(t, map[string]string{
		"package.json":            `{"dependencies":{"express":"4.18.2"}}`,
		"src/app.js":              "router.get('/admin/users', handler)\n",
		"src/index.js":            "console.log('entry')\n",
		"src/db.js":               `const password = "hunter2secret";` + "\n",
		".env":                    "API_KEY=abcdef123456\n",
		"node_modules/ignored.js": "router.get('/should-not-appear', x)\n",
	})
	res, err := ts.repoMap().Call(context.Background(), json.RawMessage(`{"dir":"code"}`), nil)
	if err != nil || res.IsError {
		t.Fatalf("repo_map: err=%v result=%v", err, res)
	}
	var out struct {
		Frameworks []string `json:"frameworks"`
		Routes     []struct {
			Path string `json:"path"`
		} `json:"routes"`
		Entrypoints []string `json:"entrypoints"`
		SecretSpots []struct {
			File string `json:"file"`
		} `json:"secret_spots"`
		IndexFile string `json:"index_file"`
	}
	if err := json.Unmarshal([]byte(res.Flatten()), &out); err != nil {
		t.Fatalf("parse repo_map: %v\n%s", err, res.Flatten())
	}
	if len(out.Frameworks) == 0 || out.Frameworks[0] != "Express" {
		t.Fatalf("framework = %v", out.Frameworks)
	}
	var routes []string
	for _, r := range out.Routes {
		routes = append(routes, r.Path)
	}
	joined := strings.Join(routes, ",")
	if !strings.Contains(joined, "/admin/users") {
		t.Fatalf("missing route: %v", routes)
	}
	if strings.Contains(joined, "should-not-appear") {
		t.Fatalf("node_modules must be skipped: %v", routes)
	}
	if !strings.Contains(strings.Join(out.Entrypoints, ","), "src/index.js") {
		t.Fatalf("entrypoints = %v", out.Entrypoints)
	}
	var secrets []string
	for _, s := range out.SecretSpots {
		secrets = append(secrets, s.File)
	}
	if !strings.Contains(strings.Join(secrets, ","), ".env") {
		t.Fatalf("secret hotspots = %v", secrets)
	}
	if out.IndexFile == "" {
		t.Fatal("index_file not written")
	}
	if _, err := os.Stat(out.IndexFile); err != nil {
		t.Fatalf("index file missing: %v", err)
	}
}

func TestRepoMapRejectsPathEscape(t *testing.T) {
	ts := codeAuditToolSet(t, map[string]string{"a.js": "1\n"})
	escape := "/etc/passwd"
	if runtime.GOOS == "windows" {
		escape = `C:\Windows`
	}
	for _, dir := range []string{"../..", escape} {
		in, _ := json.Marshal(map[string]any{"dir": dir})
		res, err := ts.repoMap().Call(context.Background(), in, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError || !strings.Contains(res.Flatten(), "任务工作目录") {
			t.Fatalf("%s 应被拒绝: %s", dir, res.Flatten())
		}
	}
}

func TestRenderTaintRule(t *testing.T) {
	yaml, err := renderTaintRule("pojun-test", "java", `含 "引号" 的说明`, "ERROR",
		[]string{`(HttpServletRequest $R).getParameter(...)`},
		[]string{`(Runtime $R).exec(...)`, `it's-a-pattern`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"id: pojun-test", "mode: taint", "languages: [java]", "severity: ERROR", "pattern-sinks:", "it''s-a-pattern", `\"引号\"`} {
		if !strings.Contains(yaml, want) {
			t.Fatalf("yaml missing %q:\n%s", want, yaml)
		}
	}
	cases := []struct {
		name string
		fn   func() error
		want string
	}{
		{"newline", func() error {
			_, err := renderTaintRule("ok-id", "java", "", "ERROR", []string{"a\nb"}, []string{"c"}, nil)
			return err
		}, "换行"},
		{"no-sink", func() error {
			_, err := renderTaintRule("ok-id", "java", "", "ERROR", []string{"a"}, nil, nil)
			return err
		}, "sinks"},
		{"bad-lang", func() error {
			_, err := renderTaintRule("ok-id", "cobol", "", "ERROR", []string{"a"}, []string{"b"}, nil)
			return err
		}, "不支持的语言"},
		{"bad-sev", func() error {
			_, err := renderTaintRule("ok-id", "java", "", "FATAL", []string{"a"}, []string{"b"}, nil)
			return err
		}, "severity"},
		{"bad-id", func() error {
			_, err := renderTaintRule("bad id!", "java", "", "ERROR", []string{"a"}, []string{"b"}, nil)
			return err
		}, "rule_id"},
	}
	for _, c := range cases {
		if err := c.fn(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err=%v, want contains %q", c.name, err, c.want)
		}
	}
}

func TestTaintSpecWritesRuleForPreset(t *testing.T) {
	ts := codeAuditToolSet(t, map[string]string{"a.js": "1\n"})
	res, err := ts.taintSpec().Call(context.Background(), json.RawMessage(`{"language":"javascript","preset":"node-cmd","rule_id":"node-js-cmd"}`), nil)
	if err != nil || res.IsError {
		t.Fatalf("taint_spec: err=%v result=%v", err, res)
	}
	var out struct {
		ConfigPath string `json:"config_path"`
		RuleYAML   string `json:"rule_yaml"`
	}
	if err := json.Unmarshal([]byte(res.Flatten()), &out); err != nil {
		t.Fatal(err)
	}
	if out.ConfigPath == "" {
		t.Fatal("config_path empty")
	}
	body, err := os.ReadFile(out.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "$CP.exec") || !strings.Contains(string(body), "pattern-sinks") {
		t.Fatalf("rule missing preset sinks:\n%s", body)
	}
}

func TestCodeTraceFindsCallerChain(t *testing.T) {
	ts := codeAuditToolSet(t, map[string]string{
		"handler.go": "package main\n\nimport \"os/exec\"\n\nfunc Handle(x string) {\n\texec.Command(x)\n}\n",
		"router.go":  "package main\n\nfunc Serve(h string) {\n\tHandle(h)\n}\n\nfunc main() {\n\tServe(\"x\")\n}\n",
	})
	raw := json.RawMessage(`{"dir":"code","sink":"exec\\.Command","source":"func Handle","max_depth":3}`)
	res, err := ts.codeTrace().Call(context.Background(), raw, nil)
	if err != nil || res.IsError {
		t.Fatalf("code_trace: err=%v result=%v", err, res)
	}
	var out struct {
		Hits  int `json:"hits"`
		Paths []struct {
			Confidence string `json:"confidence"`
			Depth      int    `json:"depth"`
			Nodes      []struct {
				File string `json:"file"`
				Func string `json:"func"`
			} `json:"nodes"`
		} `json:"paths"`
		TraceFile string `json:"trace_file"`
	}
	if err := json.Unmarshal([]byte(res.Flatten()), &out); err != nil {
		t.Fatalf("parse: %v\n%s", err, res.Flatten())
	}
	if out.Hits == 0 || len(out.Paths) == 0 {
		t.Fatalf("no trace paths: %s", res.Flatten())
	}
	if out.Paths[0].Confidence != "high" {
		t.Fatalf("sink 与 source 同文件的路径应为 high: %+v", out.Paths[0])
	}
	if out.TraceFile == "" {
		t.Fatal("trace_file not written")
	}
	if _, err := os.Stat(out.TraceFile); err != nil {
		t.Fatal(err)
	}
}

func TestParseSemgrepOutput(t *testing.T) {
	raw := []byte(`{
	  "results": [
	    {"check_id":"r1","path":"/repo/src/A.java","start":{"line":10},"extra":{"severity":"ERROR","message":"cmd injection","lines":"Runtime.getRuntime().exec(x)"}},
	    {"check_id":"r1","path":"/repo/src/A.java","start":{"line":10},"extra":{"severity":"ERROR","message":"dup"}},
	    {"check_id":"r2","path":"/repo/src/B.java","start":{"line":3},"extra":{"severity":"INFO","message":"note"}}
	  ],
	  "errors": [{"type":"Warn","message":"parse timeout"}]
	}`)
	findings, byRule, ruleErrors, err := parseSemgrepOutput(raw, "/repo", "WARNING")
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].CheckID != "r1" || findings[0].Path != "src/A.java" || findings[0].Line != 10 {
		t.Fatalf("findings = %+v", findings)
	}
	if byRule["r1"] != 1 || byRule["r2"] != 0 {
		t.Fatalf("by_rule = %v", byRule)
	}
	if len(ruleErrors) != 1 || !strings.Contains(ruleErrors[0], "parse timeout") {
		t.Fatalf("rule_errors = %v", ruleErrors)
	}
	if _, _, _, err := parseSemgrepOutput([]byte("not json"), "", ""); err == nil {
		t.Fatal("invalid json must fail")
	}
}

func TestEnclosingCodeFunc(t *testing.T) {
	lines := strings.Split("func A() {\n\tx := 1\n\tcall(x)\n}\n", "\n")
	fn, line := enclosingCodeFunc(lines, 2, "go")
	if fn != "A" || line != 1 {
		t.Fatalf("got %q:%d", fn, line)
	}
	js := strings.Split("const handler = (req) => {\n  exec(req)\n}\n", "\n")
	if fn, _ := enclosingCodeFunc(js, 1, "javascript"); fn != "handler" {
		t.Fatalf("js func = %q", fn)
	}
}

func TestCodeAuditToolsRegistered(t *testing.T) {
	byKey := map[string]ToolSeed{}
	for _, s := range BuiltinToolSeeds() {
		byKey[s.Key] = s
	}
	for _, key := range []string{"repo_map", "taint_spec", "sast_scan", "code_trace"} {
		seed, ok := byKey[key]
		if !ok {
			t.Fatalf("%s 未注册到 BuiltinToolSeeds（工具页/绑定不会出现）", key)
		}
		hasWorker := false
		for _, a := range seed.Agents {
			if a == "worker" {
				hasWorker = true
			}
		}
		if !hasWorker {
			t.Errorf("%s agents = %v, want worker", key, seed.Agents)
		}
	}
	if seed := byKey["repo_map"]; len(seed.Agents) == 0 {
		t.Errorf("repo_map 应至少给 planner 一个绑定: %v", seed.Agents)
	}
}

// TestSastScanEndToEnd 真跑 semgrep（需要本机安装）。默认跳过，避免 CI 依赖
// 外部二进制；本地/发布前用 POJUN_SEMGREP_E2E=1 跑一次，验证 taint_spec 产出的
// 规则能被 sast_scan 命中（规则语法错会在这里暴露）。
func TestSastScanEndToEnd(t *testing.T) {
	if os.Getenv("POJUN_SEMGREP_E2E") != "1" {
		t.Skip("set POJUN_SEMGREP_E2E=1 to run semgrep end-to-end")
	}
	if _, err := findSemgrep(); err != nil {
		t.Skip(err)
	}
	ts := codeAuditToolSet(t, map[string]string{
		"src/T.java": `import javax.servlet.http.HttpServletRequest;

public class T {
    public void run(HttpServletRequest req) throws Exception {
        String c = req.getParameter("cmd");
        Runtime.getRuntime().exec(c);
    }
}
`,
	})
	specRes, err := ts.taintSpec().Call(context.Background(), json.RawMessage(`{"language":"java","preset":"java-cmd","rule_id":"java-cmd-e2e"}`), nil)
	if err != nil || specRes.IsError {
		t.Fatalf("taint_spec: %v %v", err, specRes)
	}
	var spec struct {
		ConfigPath string `json:"config_path"`
	}
	if err := json.Unmarshal([]byte(specRes.Flatten()), &spec); err != nil {
		t.Fatal(err)
	}
	in, _ := json.Marshal(map[string]any{"dir": "code", "config": spec.ConfigPath, "timeout_sec": 300})
	res, err := ts.sastScan().Call(context.Background(), in, nil)
	if err != nil || res.IsError {
		t.Fatalf("sast_scan: err=%v result=%v", err, res)
	}
	var out struct {
		Total    int `json:"total"`
		Findings []struct {
			Path string `json:"path"`
			Line int    `json:"line"`
		} `json:"findings"`
	}
	if err := json.Unmarshal([]byte(res.Flatten()), &out); err != nil {
		t.Fatalf("parse: %v\n%s", err, res.Flatten())
	}
	if out.Total == 0 || len(out.Findings) == 0 {
		t.Fatalf("taint rule did not hit: %s", res.Flatten())
	}
	if out.Findings[0].Path != "src/T.java" || out.Findings[0].Line == 0 {
		t.Fatalf("bad finding location: %+v", out.Findings[0])
	}
}

func TestParseUnifiedDiff(t *testing.T) {
	diff := `diff --git a/src/A.java b/src/A.java
index 111..222 100644
--- a/src/A.java
+++ b/src/A.java
@@ -10,3 +10,3 @@ class A {
-        Runtime.getRuntime().exec(req.getParameter("c"));
+        safeExec(req.getParameter("c"));
 }
`
	got, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Files) != 1 || got.Files[0] != "src/A.java" {
		t.Fatalf("files=%v", got.Files)
	}
	if len(got.Removed) != 1 || got.Removed[0].Line != 10 || !strings.Contains(got.Removed[0].Text, "Runtime.getRuntime().exec") {
		t.Fatalf("removed=%+v", got.Removed)
	}
	if len(got.Added) != 1 || !strings.Contains(got.Added[0].Text, "safeExec") {
		t.Fatalf("added=%+v", got.Added)
	}
	if _, err := parseUnifiedDiff("not a diff"); err == nil {
		t.Fatal("non-diff must fail")
	}
}

func TestPatchVariantsFindsCopyPasteAndSameCall(t *testing.T) {
	ts := codeAuditToolSet(t, map[string]string{
		"src/A.java": "class A {\n    void run(String c) throws Exception {\n        Runtime.getRuntime().exec(c);\n    }\n}\n",
		"src/B.java": "class B {\n    void other(String c) throws Exception {\n        Runtime.getRuntime().exec(c);\n    }\n}\n",
		"src/C.java": "class C {\n    void later(String c) throws Exception {\n        helper(c);\n    }\n}\n",
		"src/D.java": "class D {\n    void third(String env) throws Exception {\n        Runtime.getRuntime().exec(env);\n    }\n}\n",
	})
	diff := "--- a/src/A.java\n+++ b/src/A.java\n@@ -1,4 +1,4 @@\n class A {\n     void run(String c) throws Exception {\n-        Runtime.getRuntime().exec(c);\n+        safe(c);\n     }\n }\n"
	in, _ := json.Marshal(map[string]any{"dir": "code", "diff": diff})
	res, err := ts.patchVariants().Call(context.Background(), in, nil)
	if err != nil || res.IsError {
		t.Fatalf("patch_variants: err=%v result=%v", err, res)
	}
	var out struct {
		CopyPaste []struct {
			File string `json:"file"`
			Line int    `json:"line"`
		} `json:"copy_paste"`
		SameCall []struct {
			File string `json:"file"`
		} `json:"same_call"`
		ReportFile string `json:"report_file"`
	}
	if err := json.Unmarshal([]byte(res.Flatten()), &out); err != nil {
		t.Fatalf("parse: %v\n%s", err, res.Flatten())
	}
	if len(out.CopyPaste) != 1 || out.CopyPaste[0].File != "src/B.java" {
		t.Fatalf("copy_paste=%+v", out.CopyPaste)
	}
	// D.java 用不同参数调同一 sink：copy_paste 不命中，same_call 应命中且不重复报 B。
	if len(out.SameCall) != 1 || out.SameCall[0].File != "src/D.java" {
		t.Fatalf("same_call=%+v", out.SameCall)
	}
	if out.ReportFile == "" {
		t.Fatal("report_file empty")
	}
}

// worker 的相对路径落在 tasks/<id>/iN/（Bash 的 CWD），规划者落在 tasks/<id>/；
// 同一个 dir=code 必须两种布局都能命中。
func TestCodeAuditDirResolvesRunDirRelative(t *testing.T) {
	base := t.TempDir()
	taskDir := filepath.Join(base, "tasks", "1")
	runDir := filepath.Join(taskDir, "i1")
	codeDir := filepath.Join(runDir, "code")
	if err := os.MkdirAll(filepath.Join(codeDir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCodeFixture(t, codeDir, map[string]string{"src/app.js": "router.get('/in-run-dir', h)\n"})
	ts := NewToolSet(nil, "")
	ts.SetRunDir(runDir)
	res, err := ts.repoMap().Call(context.Background(), json.RawMessage(`{"dir":"code"}`), nil)
	if err != nil || res.IsError {
		t.Fatalf("repo_map: err=%v result=%v", err, res)
	}
	if !strings.Contains(res.Flatten(), "/in-run-dir") {
		t.Fatalf("未在 runDir 相对路径下找到源码: %s", res.Flatten())
	}
}

// 删除的行本身可能以 "-- " 开头（SQL 注释，diff 行前缀后变成 "--- "）。
// 旧解析器按前缀把它当文件头，导致删除行丢失、后续行挂错文件。
func TestParseUnifiedDiffKeepsSQLCommentRemoval(t *testing.T) {
	diff := "" +
		"--- a/db/init.sql\n" +
		"+++ b/db/init.sql\n" +
		"@@ -1,2 +1,1 @@\n" +
		"--- legacy comment\n" +
		" SELECT 1;\n"
	got, err := parseUnifiedDiff(diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Removed) != 1 || got.Removed[0].Text != "-- legacy comment" {
		t.Fatalf("removed=%+v", got.Removed)
	}
	if len(got.Added) != 0 || len(got.Files) != 1 || got.Files[0] != "db/init.sql" {
		t.Fatalf("files=%v added=%+v", got.Files, got.Added)
	}
}
