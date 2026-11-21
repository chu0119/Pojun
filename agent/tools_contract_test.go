package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	actool "github.com/Autumn-27/norma/tool"

	"github.com/chu0119/pojun/intercept"
)

// 真实 OpenAPI 3 契约样本（结构照 Swagger/OpenAPI 3 规范写）。
//
// 用真实结构而不是"好看的"样本：解析器要处理的是规范里的实际形态——
// parameters 在操作级和 path 级都有、requestBody 在 content 下、
// security 在操作级覆盖全局。
const openapi3Sample = `{
  "openapi": "3.0.0",
  "info": {"title": "Demo API", "version": "1.0.0"},
  "components": {
    "securitySchemes": {
      "bearerAuth": {"type": "http", "scheme": "bearer"},
      "apiKeyAuth": {"type": "apiKey", "name": "X-API-Key", "in": "header"}
    }
  },
  "security": [{"bearerAuth": []}],
  "paths": {
    "/api/users": {
      "get": {
        "summary": "List users",
        "parameters": [
          {"name": "page", "in": "query", "required": false, "schema": {"type": "integer"}},
          {"name": "size", "in": "query", "required": true, "schema": {"type": "integer"}}
        ],
        "responses": {"200": {"description": "ok"}}
      },
      "post": {
        "summary": "Create user",
        "requestBody": {
          "required": true,
          "content": {
            "application/json": {
              "schema": {
                "type": "object",
                "properties": {
                  "name": {"type": "string"},
                  "email": {"type": "string", "required": true}
                }
              }
            }
          }
        },
        "responses": {"201": {"description": "created"}}
      }
    },
    "/api/users/{id}": {
      "parameters": [
        {"name": "id", "in": "path", "required": true, "schema": {"type": "integer"}}
      ],
      "get": {
        "summary": "Get user",
        "security": [],
        "responses": {"200": {"description": "ok"}}
      },
      "delete": {
        "summary": "Delete user",
        "responses": {"204": {"description": "deleted"}}
      }
    },
    "/api/health": {
      "get": {"summary": "Health", "responses": {"200": {"description": "ok"}}}
    }
  }
}`

// 解析器要能读出接口清单、参数、必填标记。
func TestContractScriptParsesOpenAPI3(t *testing.T) {
	python := pythonCommand(t)
	dir := t.TempDir()
	scriptPath, err := WriteContractScript(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "contract.json")
	if err := os.WriteFile(f, []byte(openapi3Sample), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(python, scriptPath, f).Output()
	if err != nil {
		t.Fatalf("脚本执行失败: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("输出不是 JSON: %v\n%s", err, raw)
	}
	if out["found"] != true {
		t.Fatalf("应识别出契约: %v", out)
	}
	if n, _ := out["api_count"].(float64); n != 5 {
		t.Errorf("接口数应为 5（2+2+1），实际 %v", out["api_count"])
	}
	eps, _ := out["endpoints"].([]any)
	if len(eps) != 5 {
		t.Fatalf("endpoints 应有 5 条，实际 %d", len(eps))
	}

	// 按 path+method 找具体接口。
	find := func(path, method string) map[string]any {
		for _, e := range eps {
			em, _ := e.(map[string]any)
			if em["path"] == path && em["method"] == method {
				return em
			}
		}
		return nil
	}

	// GET /api/users：query 参数 + 必填标记。
	e := find("/api/users", "GET")
	if e == nil {
		t.Fatal("应找到 GET /api/users")
	}
	params, _ := e["params"].([]any)
	if len(params) != 2 {
		t.Errorf("应有 2 个 query 参数，实际 %d", len(params))
	}
	var sawSize bool
	for _, p := range params {
		pm, _ := p.(map[string]any)
		if pm["name"] == "size" {
			sawSize = true
			if pm["required"] != true {
				t.Error("size 应标记为必填")
			}
		}
	}
	if !sawSize {
		t.Error("应识别出 size 参数")
	}

	// path 级参数要合并进操作（/api/users/{id} 的 id）。
	e2 := find("/api/users/{id}", "GET")
	if e2 == nil {
		t.Fatal("应找到 GET /api/users/{id}")
	}
	p2, _ := e2["params"].([]any)
	if len(p2) != 1 {
		t.Errorf("path 级参数应合并进操作，实际 %d 个", len(p2))
	}

	// 全局 security 生效 → 需要鉴权。
	if e["auth_required"] != true {
		t.Error("全局 security 生效时该接口应需鉴权")
	}
	// 操作级 security: [] → 显式公开。
	if e2["auth_required"] != false {
		t.Error("操作级 security:[] 表示公开，不应标为需鉴权")
	}
	// 需要鉴权的接口要单独列出（越权挖掘的入口）。
	authEps, _ := out["auth_endpoints"].([]any)
	if len(authEps) != 4 {
		t.Errorf("需鉴权接口应为 4 条（/api/health 不需要），实际 %d", len(authEps))
	}
	// 鉴权方式要列出来。
	schemes, _ := out["auth_schemes"].([]any)
	if len(schemes) != 2 {
		t.Errorf("应列出 2 种鉴权方式，实际 %v", schemes)
	}
}

// Swagger 2.0 也要能读（它的 parameters 在顶层、definitions 不叫 components）。
func TestContractScriptParsesSwagger2(t *testing.T) {
	python := pythonCommand(t)
	dir := t.TempDir()
	scriptPath, err := WriteContractScript(dir)
	if err != nil {
		t.Fatal(err)
	}
	swagger2 := `{
      "swagger": "2.0",
      "info": {"title": "Old API", "version": "1.0"},
      "securityDefinitions": {"apiKey": {"type": "apiKey", "name": "key", "in": "header"}},
      "paths": {
        "/v1/items": {
          "get": {
            "summary": "List items",
            "parameters": [{"name": "q", "in": "query", "type": "string", "required": true}],
            "responses": {"200": {"description": "ok"}}
          }
        }
      }
    }`
	f := filepath.Join(dir, "swagger.json")
	if err := os.WriteFile(f, []byte(swagger2), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(python, scriptPath, f).Output()
	if err != nil {
		t.Fatalf("脚本执行失败: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("输出不是 JSON: %v\n%s", err, raw)
	}
	if out["found"] != true {
		t.Fatalf("应识别出 Swagger 2.0 契约: %v", out)
	}
	if n, _ := out["api_count"].(float64); n != 1 {
		t.Errorf("接口数应为 1，实际 %v", out["api_count"])
	}
	// Swagger 2.0 的参数在顶层（不在 schema 里），类型要能取到。
	eps, _ := out["endpoints"].([]any)
	e, _ := eps[0].(map[string]any)
	params, _ := e["params"].([]any)
	if len(params) != 1 {
		t.Fatalf("应有 1 个参数，实际 %d", len(params))
	}
	pm, _ := params[0].(map[string]any)
	if pm["type"] != "string" {
		t.Errorf("Swagger2 的参数类型应取顶层 type，实际 %v", pm["type"])
	}
	if pm["required"] != true {
		t.Error("必填标记应保留")
	}
}

// Postman collection 做尽力解析。
func TestContractScriptParsesPostman(t *testing.T) {
	python := pythonCommand(t)
	dir := t.TempDir()
	scriptPath, err := WriteContractScript(dir)
	if err != nil {
		t.Fatal(err)
	}
	postman := `{
      "info": {"name": "Demo", "schema": "https://schema.getpostman.com/json/collection/v2.1.0/collection.json"},
      "item": [
        {"name": "Get user", "request": {"method": "GET", "url": {"raw": "https://t.example.com/api/user/1?x=1"}}}
      ]
    }`
	f := filepath.Join(dir, "postman.json")
	if err := os.WriteFile(f, []byte(postman), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(python, scriptPath, f).Output()
	if err != nil {
		t.Fatalf("脚本执行失败: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("输出不是 JSON: %v\n%s", err, raw)
	}
	if n, _ := out["api_count"].(float64); n != 1 {
		t.Errorf("接口数应为 1，实际 %v", out["api_count"])
	}
	eps, _ := out["endpoints"].([]any)
	e, _ := eps[0].(map[string]any)
	// 查询串要去掉。
	if e["path"] != "/api/user/1" {
		t.Errorf("路径应去掉查询串，实际 %v", e["path"])
	}
	if e["method"] != "GET" {
		t.Errorf("方法应为 GET，实际 %v", e["method"])
	}
}

// 非 JSON（如 YAML 契约）要如实报错，不能假装解析成功。
func TestContractScriptReportsNonJSON(t *testing.T) {
	python := pythonCommand(t)
	dir := t.TempDir()
	scriptPath, err := WriteContractScript(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "contract.yaml")
	if err := os.WriteFile(f, []byte("openapi: 3.0.0\npaths: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err := exec.Command(python, scriptPath, f).Output()
	if err != nil {
		t.Fatalf("脚本执行失败: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("输出不是 JSON: %v\n%s", err, raw)
	}
	if out["found"] == true {
		t.Error("非 JSON 不应报告 found=true")
	}
	if out["error"] == nil {
		t.Error("应给出错误说明")
	}
}

// 工具行为：URL 模式要有工作目录；命令要探测契约路径；契约文件落工作目录。
func TestApiContractTool(t *testing.T) {
	dir := t.TempDir()
	ts := &ToolSet{}
	tool := ts.apiContractTool()
	ctx := intercept.WithReviewContext(context.Background(), dir, intercept.ReviewBackground{})

	in, _ := json.Marshal(map[string]any{"base_url": "https://t.example.com/"})
	res, err := tool.Call(ctx, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("工具报错: %v", res.Content)
	}
	out := toolResultJSON(t, res)
	cmd, _ := out["run_with_bash"].(string)
	if cmd == "" {
		t.Fatalf("应返回命令，实际 %v", out)
	}
	// 常见契约路径要出现在探测列表里。
	for _, p := range []string{"/openapi.json", "/v3/api-docs", "/swagger.json"} {
		if !strings.Contains(cmd, p) {
			t.Errorf("命令应探测 %s: %s", p, cmd)
		}
	}
	// 契约文件落在工作目录，不是固定 /tmp 路径。
	if !strings.Contains(cmd, "api_contract.json") {
		t.Errorf("命令应引用契约文件: %s", cmd)
	}
	for _, fixed := range []string{"/tmp/api_contract.json", "/tmp/api_contract.py"} {
		if strings.Contains(cmd, fixed) {
			t.Errorf("中间产物不应落在固定路径 %s: %s", fixed, cmd)
		}
	}
	// 脚本要已写出。
	scriptPath, _ := out["script_path"].(string)
	if scriptPath == "" {
		t.Fatal("应返回脚本路径")
	}
	if _, err := os.Stat(scriptPath); err != nil {
		t.Fatalf("脚本应已写出: %v", err)
	}
}

// 没有工作目录时要明确报错并给替代方案。
func TestApiContractToolRequiresWorkingDir(t *testing.T) {
	ts := &ToolSet{}
	tool := ts.apiContractTool()
	in, _ := json.Marshal(map[string]any{"base_url": "https://t.example.com"})
	res, err := tool.Call(context.Background(), in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("没有工作目录时应报错，实际 %v", res.Content)
	}
	if !strings.Contains(toolResultText(t, res), "工作目录") {
		t.Errorf("错误提示应说明工作目录问题，实际 %q", toolResultText(t, res))
	}
}

// base_url 非法要报错。
func TestApiContractToolValidatesURL(t *testing.T) {
	ts := &ToolSet{}
	tool := ts.apiContractTool()
	ctx := intercept.WithReviewContext(context.Background(), t.TempDir(), intercept.ReviewBackground{})
	for _, bad := range []string{"", "t.example.com", "ftp://t.example.com"} {
		in, _ := json.Marshal(map[string]any{"base_url": bad})
		res, err := tool.Call(ctx, in, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			t.Errorf("base_url=%q 应报错", bad)
		}
	}
}

// 自定义路径：不给就用默认清单，给了就用自定义的。
func TestResolveContractPaths(t *testing.T) {
	if len(resolveContractPaths("")) != len(contractPaths) {
		t.Error("不给自定义路径时应用默认清单")
	}
	got := resolveContractPaths("/a.json, b.json ,")
	if len(got) != 2 {
		t.Fatalf("应解析出 2 个路径，实际 %d: %v", len(got), got)
	}
	if got[0] != "/a.json" || got[1] != "/b.json" {
		t.Errorf("路径应规范化（补前导斜杠、trim），实际 %v", got)
	}
}

// 工具要注册到三个工具集。
func TestApiContractRegisteredInAllToolSets(t *testing.T) {
	ts := NewToolSet(nil, "")
	has := func(list []actool.CoreTool) bool {
		for _, c := range list {
			if c.Name() == "api_contract" {
				return true
			}
		}
		return false
	}
	if !has(ts.WorkerTools()) {
		t.Error("worker 工具集里没有 api_contract")
	}
	if !has(ts.PlannerTools()) {
		t.Error("planner 工具集里没有 api_contract")
	}
	if !has(ts.MainAgentTools()) {
		t.Error("mainagent 工具集里没有 api_contract")
	}
}

// 端到端：真起一个提供 OpenAPI 契约的 HTTP 服务，
// 跑「工具 → 命令 → 真实 curl → 真实解析脚本」完整链路。
func TestApiContractEndToEndWithRealServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/openapi.json":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(openapi3Sample))
		case "/v3/api-docs":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(openapi3Sample))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	ts := &ToolSet{}
	tool := ts.apiContractTool()
	ctx := intercept.WithReviewContext(context.Background(), dir, intercept.ReviewBackground{})
	in, _ := json.Marshal(map[string]any{"base_url": srv.URL})
	res, err := tool.Call(ctx, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("工具报错: %v", res.Content)
	}
	out := toolResultJSON(t, res)
	cmd, _ := out["run_with_bash"].(string)
	if cmd == "" {
		t.Fatalf("应返回命令: %v", out)
	}
	t.Logf("cmd = %s", cmd)

	// 真跑这条命令（用 bash，因为生产环境是 Linux 的 bash -c）。
	sh, err := exec.LookPath("bash")
	if err != nil {
		sh, err = exec.LookPath("sh")
		if err != nil {
			t.Skip("找不到 shell")
		}
	}
	run := exec.Command(sh, "-c", cmd)
	run.Dir = dir
	raw, err := run.CombinedOutput()
	if err != nil {
		t.Fatalf("命令执行失败: %v\n%s", err, raw)
	}
	t.Logf("output = %s", string(raw))

	// 解析脚本的 JSON 输出（命令前半段是探测，会先打印 CONTRACT 行）。
	text := string(raw)
	idx := strings.Index(text, "{")
	if idx < 0 {
		t.Fatalf("输出里没有 JSON: %s", text)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(text[idx:]), &parsed); err != nil {
		t.Fatalf("输出不是 JSON: %v\n%s", err, text[idx:])
	}
	if parsed["found"] != true {
		t.Fatalf("应识别出契约: %v", parsed)
	}
	if n, _ := parsed["api_count"].(float64); n != 5 {
		t.Errorf("接口数应为 5，实际 %v", parsed["api_count"])
	}
	authEps, _ := parsed["auth_endpoints"].([]any)
	if len(authEps) != 4 {
		t.Errorf("需鉴权接口应为 4 条，实际 %d", len(authEps))
	}
	// 契约文件应落在工作目录。
	if _, err := os.Stat(filepath.Join(dir, "api_contract.json")); err != nil {
		t.Errorf("契约文件应落在工作目录: %v", err)
	}
}

// --- user_agent（issue #41）---

// buildContractCmd 的 UA golden：空值必须与改动前逐字节一致；非空时条件追加 -A。
//
// 期望串是**手工按改动前的格式串拼出来的**（不是调 buildContractCmd 生成），
// 这样格式串漂移（比如 -A 挪进 -o 之后、--max-time 位置变了）会直接红。
func TestApiContractUserAgentCommand(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, contractScriptName)
	base := "https://t.example.com"
	paths := []string{"/openapi.json", "/swagger.json"}
	contract := quoteShell(filepath.Join(dir, "api_contract.json"))

	wantEmpty := fmt.Sprintf(
		"for u in %s; do code=$(curl -sS -o %s -w '%%{http_code}' --max-time 20 \"$u\"); "+
			"if [ \"$code\" = \"200\" ]; then echo \"CONTRACT $u\"; break; fi; done",
		quoteShell(base+"/openapi.json")+" "+quoteShell(base+"/swagger.json"), contract,
	) + " ; " + fmt.Sprintf("python3 %s %s", quoteShell(script), contract)

	gotEmpty := buildContractCmd(base, paths, script, dir, "")
	if gotEmpty != wantEmpty {
		t.Fatalf("空 UA 的命令应与改动前逐字节一致:\n got: %s\nwant: %s", gotEmpty, wantEmpty)
	}

	got := buildContractCmd(base, paths, script, dir, "MyUA/1.0")
	// -A 必须紧跟 -sS（curl 选项语义最窄、且没有 @file 读取），在 -o 之前。
	if !strings.Contains(got, "curl -sS -A 'MyUA/1.0' -o") {
		t.Fatalf("非空 UA 应在 -sS 后追加 -A: %s", got)
	}
	// 只给探测 curl 加 -A；解析脚本那半段不该出现 UA。
	if strings.Count(got, "-A 'MyUA/1.0'") != 1 {
		t.Fatalf("-A 应恰好出现一次: %s", got)
	}
	// %%{http_code} 仍渲染为 %{http_code}，--max-time 位置不变。
	if !strings.Contains(got, `-w '%{http_code}' --max-time 20`) {
		t.Fatalf("%%{http_code} 渲染或 --max-time 位置漂移: %s", got)
	}

	// UA 里的 % 系列必须是 Sprintf 参数原样保留——不能被格式串二次解释。
	//（-w '%{http_code}' 那份是命令本身的格式占位，与 UA 里的字面量并存是合法形态。）
	tricky := "%s %d %{http_code} %%"
	gotTricky := buildContractCmd(base, []string{"/openapi.json"}, script, dir, tricky)
	if !strings.Contains(gotTricky, "'"+tricky+"'") {
		t.Fatalf("UA 中的 %% 序列应原样保留: %s", gotTricky)
	}
}

// 工具面：非空 UA 进命令；CRLF（JSON 转义形式）被拒且不落脚本。
func TestApiContractUserAgentTool(t *testing.T) {
	dir := t.TempDir()
	ts := &ToolSet{}
	tool := ts.apiContractTool()
	ctx := intercept.WithReviewContext(context.Background(), dir, intercept.ReviewBackground{})

	call := func(extra map[string]any) (map[string]any, bool) {
		in := map[string]any{"base_url": "https://t.example.com/"}
		for k, v := range extra {
			in[k] = v
		}
		raw, _ := json.Marshal(in)
		res, err := tool.Call(ctx, raw, nil)
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		if res.IsError {
			return map[string]any{"err": toolResultText(t, res)}, true
		}
		return toolResultJSON(t, res), false
	}

	out, isErr := call(nil)
	if isErr {
		t.Fatalf("默认调用不应报错: %v", out)
	}
	if strings.Contains(out["run_with_bash"].(string), " -A ") {
		t.Fatalf("默认命令不应含 -A: %s", out["run_with_bash"])
	}

	out, isErr = call(map[string]any{"user_agent": "MyUA/1.0"})
	if isErr {
		t.Fatalf("合法 UA 不应报错: %v", out)
	}
	if !strings.Contains(out["run_with_bash"].(string), "-A 'MyUA/1.0'") {
		t.Fatalf("命令应含 -A 'MyUA/1.0': %s", out["run_with_bash"])
	}

	// 非法值：报错、不落脚本——normalize 在 WriteContractScript 之前，
	// 换全新目录才能断言「脚本不存在」。
	badDir := t.TempDir()
	badTS := &ToolSet{}
	badTool := badTS.apiContractTool()
	badCtx := intercept.WithReviewContext(context.Background(), badDir, intercept.ReviewBackground{})
	badIn, _ := json.Marshal(map[string]any{
		"base_url":   "https://t.example.com/",
		"user_agent": "a\r\nX-Injected: 1",
	})
	badRes, err := badTool.Call(badCtx, badIn, nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !badRes.IsError {
		t.Fatalf("CRLF UA 应被拒绝: %s", toolResultText(t, badRes))
	}
	bad := toolResultText(t, badRes)
	if !strings.Contains(bad, "user_agent") || !strings.Contains(bad, "0-based") {
		t.Fatalf("错误应点名 user_agent 且带偏移: %s", bad)
	}
	if _, err := os.Stat(filepath.Join(badDir, contractScriptName)); !os.IsNotExist(err) {
		t.Fatalf("非法 UA 不应写出脚本")
	}
}

// E2E：真跑命令，目标侧收到的 UA——默认是 curl 自报的默认 UA，传入后是自定义值。
func TestApiContractUserAgentEndToEnd(t *testing.T) {
	uaCh := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/openapi.json" {
			uaCh <- r.Header.Get("User-Agent")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(openapi3Sample))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	sh, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("未找到 bash")
	}

	run := func(ua string) map[string]any {
		t.Helper()
		dir := t.TempDir()
		ts := &ToolSet{}
		tool := ts.apiContractTool()
		ctx := intercept.WithReviewContext(context.Background(), dir, intercept.ReviewBackground{})
		in := map[string]any{"base_url": srv.URL}
		if ua != "" {
			in["user_agent"] = ua
		}
		raw, _ := json.Marshal(in)
		res, err := tool.Call(ctx, raw, nil)
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		if res.IsError {
			t.Fatalf("工具报错: %s", toolResultText(t, res))
		}
		out := toolResultJSON(t, res)
		cmd, _ := out["run_with_bash"].(string)
		run_ := exec.Command(sh, "-c", strings.ReplaceAll(cmd, `\`, `/`))
		run_.Dir = dir
		if b, err := run_.CombinedOutput(); err != nil {
			t.Fatalf("命令执行失败: %v\n%s", err, b)
		}
		select {
		case got := <-uaCh:
			return map[string]any{"ua": got, "dir": dir}
		case <-time.After(10 * time.Second):
			t.Fatal("目标侧没有收到请求")
			return nil
		}
	}

	def := run("")
	if !strings.HasPrefix(def["ua"].(string), "curl/") {
		t.Errorf("不传 UA 时目标侧应收到 curl 默认 UA（curl/…），实际 %q", def["ua"])
	}
	custom := run("ContractUA/1.0 (issue41)")
	if custom["ua"] != "ContractUA/1.0 (issue41)" {
		t.Errorf("传入 UA 后目标侧应收到自定义值，实际 %q", custom["ua"])
	}
}
