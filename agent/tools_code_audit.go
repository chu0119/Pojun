package agent

// 白盒代码审计四件套（v0.9.86，A1-1）：repo_map / taint_spec / sast_scan / code_trace。
//
// ## 为什么需要（调研依据）
//
//   - Shannon（source-aware XBOW 变体 96.15%）与 IRIS（LLM 推断 taint 规格后
//     CodeQL 从 27/120 提到 55/120）说明：**白盒是"出货"最大增量**，但瓶颈不在
//     调用扫描器，而在"给扫描器正确的 source/sink 规格"。
//   - RepoAudit（仓库级数据流 agent，78-86% 精度）的做法是按需扩展、路径记忆、
//     独立复核；这里用 `code_trace` 的静态启发 + 证据门槛承接同一思路。
//   - 我们的 semgrep 一直只是个装了没用的二进制：扫出来的结果进不了图谱，
//     worker 也拿不到源码上下文（docs/漏洞挖掘能力对比.md 的 G3）。
//
// ## 四件套的分工
//
//	repo_map   —— 一次性摸清语言/框架/入口/路由/信任边界/密钥热点，落盘索引。
//	taint_spec —— 把模型推断的 source/sink/sanitizer **确定性**渲染成 Semgrep
//	              taint 规则（模型负责"想"，工具负责"写对 YAML"）。
//	sast_scan  —— 跑 semgrep，解析 JSON，按规则分组给候选，原文落盘。
//	code_trace —— 静态反查调用链：sink 出现在哪、谁能调到这里、路径上有没有
//	              source，作为候选排序（不是漏洞证明）。
//
// ## 设计约束（与 js_audit 同款）
//
//   - 路径必须位于本次任务工作目录内（`safeAuditDir`）：源码由 Bash clone/解压到
//     `tasks/<id>/code/`，工具不读任务之外的任意路径。
//   - 有界：文件数/单文件字节/总字节/输出条数都有上限，避免大仓 OOM。
//   - sast_scan 只跑 semgrep 子进程（无 shell、无网络请求），超时可控；
//     静态命中一律只是候选，工具说明里写清楚"不能直接当 confirmed 漏洞上报"。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/chu0119/pojun/hosttools"
)

const (
	codeAuditMaxFiles     = 5000
	codeAuditMaxFileBytes = 256 << 10 // 单文件最多读 256KB（路由/密钥只需要前部）
	codeAuditMaxScanBytes = 64 << 20  // 一轮最多读 64MB
	codeAuditMaxSastBytes = 32 << 20  // semgrep stdout 上限
	codeAuditMaxPaths     = 500       // code_trace 记忆文件上限
)

// codeLangByExt 只收录"能被静态审计"的源码/标记扩展名；未知扩展名直接跳过。
var codeLangByExt = map[string]string{
	".go": "go", ".py": "python", ".php": "php", ".java": "java",
	".js": "javascript", ".jsx": "javascript", ".mjs": "javascript", ".cjs": "javascript",
	".ts": "typescript", ".tsx": "typescript", ".rb": "ruby", ".cs": "csharp",
	".kt": "kotlin", ".kts": "kotlin", ".scala": "scala",
	".c": "c", ".h": "c", ".cc": "cpp", ".cpp": "cpp", ".hpp": "cpp",
	".rs": "rust", ".swift": "swift", ".lua": "lua",
	".vue": "vue", ".svelte": "svelte", ".sql": "sql",
	".jsp": "jsp", ".asp": "asp", ".aspx": "asp", ".ftl": "freemarker",
}

// codeSkipDirs 是依赖/产物/版本控制目录，扫描只看第一方代码。
var codeSkipDirs = map[string]bool{
	".git": true, ".svn": true, ".hg": true, "node_modules": true, "bower_components": true,
	"vendor": true, "dist": true, "build": true, "out": true, "target": true,
	"bin": true, "obj": true, "__pycache__": true, ".venv": true, "venv": true,
	"env": true, ".tox": true, ".idea": true, ".vscode": true, "coverage": true,
	".next": true, ".nuxt": true, ".gradle": true, ".mvn": true, "Pods": true,
}

type codeFile struct {
	Rel  string // 相对审计根
	Abs  string
	Lang string
	Size int64
}

// codeAuditDir 把 agent 给的路径解析到任务工作目录内。相对路径以任务根为基准
// （worker 的 runDir 是 tasks/<id>/iN，planner 是 tasks/<id>；两者都要能定位到
// tasks/<id>/code）。绝对路径只允许落在任务根内，避免把 /etc 之类读进模型。
func (t *ToolSet) codeAuditDir(p string) (string, error) {
	target, err := t.codeAuditResolve(p)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(target)
	if err != nil {
		return "", fmt.Errorf("目录不存在：%s", p)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s 不是目录", p)
	}
	return target, nil
}

// codeAuditResolve 只做"落在任务目录内 + 存在"校验，文件/目录都接受
// （sast_scan 的 config 既可能是规则文件也可能是规则目录）。
func (t *ToolSet) codeAuditResolve(p string) (string, error) {
	if strings.TrimSpace(t.runDir) == "" {
		return "", fmt.Errorf("本次 run 没有工作目录")
	}
	root := filepath.Dir(t.runDir)
	if filepath.Base(root) == "tasks" { // planner/mainagent 注入的就是任务目录
		root = t.runDir
	}
	if filepath.Base(root) == "shared" {
		root = filepath.Dir(root)
	}
	within := func(candidate string) bool {
		rel, err := filepath.Rel(root, candidate)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
	}
	// 词法上的 within 不够：任务目录里可能被放进一个指向宿主机的符号链接
	// （第三方仓库自带，或 code/leak -> /root/.aws/credentials）。对存在的目标
	// 再按真实路径复核一次，把越界链接挡在读取/扫描之前。
	realRoot, rerr := filepath.EvalSymlinks(root)
	if rerr != nil {
		realRoot = filepath.Clean(root)
	}
	symlinkSafe := func(candidate string) bool {
		real, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			return false
		}
		rel, err := filepath.Rel(realRoot, real)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
	}
	clean := filepath.Clean(strings.TrimSpace(p))
	if filepath.IsAbs(clean) {
		if !within(clean) {
			return "", fmt.Errorf("路径必须位于任务工作目录内（相对路径基准：%s；源码先 clone/解压到 code/）", root)
		}
		if _, err := os.Stat(clean); err != nil {
			return "", fmt.Errorf("路径不存在：%s", p)
		}
		if !symlinkSafe(clean) {
			return "", fmt.Errorf("路径经符号链接解析后逃逸出任务工作目录：%s", p)
		}
		return clean, nil
	}
	// 相对路径先按本次 run 目录解析，再按任务根解析：worker 的 Bash 相对路径
	// 落在 tasks/<id>/iN/，规划者/共享目录落在 tasks/<id>/；两者都要能用同一个
	// dir=code 命中，否则用户必须记住自己是在哪一层 clone 的。
	candidates := []string{filepath.Join(t.runDir, clean), filepath.Join(root, clean)}
	if t.runDir == root {
		candidates = candidates[:1]
	}
	for _, c := range candidates {
		if !within(c) {
			return "", fmt.Errorf("路径逃逸出任务工作目录：%s（相对路径基准：%s）", p, root)
		}
		if _, err := os.Stat(c); err == nil {
			if !symlinkSafe(c) {
				return "", fmt.Errorf("路径经符号链接解析后逃逸出任务工作目录：%s", p)
			}
			return c, nil
		}
	}
	return "", fmt.Errorf("路径不存在：%s（已尝试 %s；源码先 clone/解压到 code/）", p, strings.Join(candidates, " 与 "))
}

// walkCodeRepo 有界遍历源码目录；truncated=true 表示命中文件数上限。
func walkCodeRepo(root string, maxFiles int) (files []codeFile, totalBytes int64, truncated bool, err error) {
	if maxFiles <= 0 {
		maxFiles = codeAuditMaxFiles
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // 单个不可读目录不影响整体
		}
		if d.IsDir() {
			if path != root && codeSkipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() {
			return nil
		}
		lang, ok := codeLangByExt[strings.ToLower(filepath.Ext(d.Name()))]
		if !ok {
			// 清单/配置文件没有可审计扩展名，但框架指纹与密钥热点完全依赖它们
			// （package.json / pom.xml / .env / application.yml …），必须收进来。
			base := filepath.Base(d.Name())
			switch {
			case len(codeManifestFrameworks[base]) > 0, codeSecretNameRe.MatchString(base):
				lang = "config"
			default:
				return nil
			}
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		if len(files) >= maxFiles {
			truncated = true
			return fs.SkipAll
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		files = append(files, codeFile{Rel: filepath.ToSlash(rel), Abs: path, Lang: lang, Size: info.Size()})
		totalBytes += info.Size()
		if totalBytes >= codeAuditMaxScanBytes {
			truncated = true
			return fs.SkipAll
		}
		return nil
	})
	return files, totalBytes, truncated, err
}

func readCodeText(abs string, max int64) string {
	if max <= 0 {
		max = codeAuditMaxFileBytes
	}
	f, err := os.Open(abs)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, max))
	return string(b)
}

// ---- 框架 / 路由 / 入口识别 ----

type codeFrameworkRule struct {
	marker    string
	framework string
}

// codeManifestFrameworks 按清单文件内容里的依赖串识别框架（小写比较）。
var codeManifestFrameworks = map[string][]codeFrameworkRule{
	"package.json": {
		{"\"express\"", "Express"}, {"\"koa\"", "Koa"}, {"\"next\"", "Next.js"},
		{"\"nuxt\"", "Nuxt"}, {"\"@nestjs/core\"", "NestJS"}, {"\"fastify\"", "Fastify"},
		{"\"react\"", "React"}, {"\"vue\"", "Vue"}, {"\"axios\"", "Axios"},
		{"\"jsonwebtoken\"", "JWT"}, {"\"sequelize\"", "Sequelize"}, {"\"prisma\"", "Prisma"},
	},
	"requirements.txt": {
		{"django", "Django"}, {"flask", "Flask"}, {"fastapi", "FastAPI"},
		{"sqlalchemy", "SQLAlchemy"}, {"celery", "Celery"}, {"jinja2", "Jinja2"},
		{"pyyaml", "PyYAML"}, {"requests", "Requests"},
	},
	"pyproject.toml": {
		{"django", "Django"}, {"flask", "Flask"}, {"fastapi", "FastAPI"},
		{"sqlalchemy", "SQLAlchemy"},
	},
	"go.mod": {
		{"gin-gonic/gin", "Gin"}, {"labstack/echo", "Echo"}, {"beego", "Beego"},
		{"gorilla/mux", "gorilla/mux"}, {"gorm.io", "GORM"}, {"go-redis", "go-redis"},
	},
	"composer.json": {
		{"laravel/framework", "Laravel"}, {"symfony/", "Symfony"}, {"topthink/framework", "ThinkPHP"},
		{"guzzlehttp", "Guzzle"}, {"doctrine/", "Doctrine"},
	},
	"pom.xml": {
		{"spring-boot", "Spring Boot"}, {"springframework", "Spring"}, {"shiro", "Shiro"},
		{"fastjson", "Fastjson"}, {"log4j", "Log4j"}, {"struts", "Struts2"},
		{"mybatis", "MyBatis"}, {"dubbo", "Dubbo"},
	},
	"build.gradle": {
		{"spring-boot", "Spring Boot"}, {"springframework", "Spring"}, {"mybatis", "MyBatis"},
	},
	"Gemfile": {
		{"rails", "Rails"}, {"sinatra", "Sinatra"}, {"devise", "Devise"},
	},
}

type codeRouteRule struct {
	lang string
	re   *regexp.Regexp
}

var codeRouteRules = []codeRouteRule{
	{"java", regexp.MustCompile(`@(?:Get|Post|Put|Delete|Patch|Request)Mapping\s*\(\s*(?:value\s*=\s*)?["']([^"']+)["']`)},
	{"java", regexp.MustCompile(`@Path\s*\(\s*["']([^"']+)["']`)},
	{"kotlin", regexp.MustCompile(`@(?:Get|Post|Put|Delete|Patch|Request)Mapping\s*\(\s*(?:value\s*=\s*)?["']([^"']+)["']`)},
	{"python", regexp.MustCompile(`@\w+\.(?:route|get|post|put|delete|patch)\s*\(\s*["']([^"']+)["']`)},
	{"php", regexp.MustCompile(`(?:Route::(?:get|post|put|delete|patch|any)|->(?:get|post|put|delete|patch))\s*\(\s*["']([^"']+)["']`)},
	{"javascript", regexp.MustCompile("\\b(?:app|router|server|fastify)\\.(?:get|post|put|delete|patch|all|use)\\s*\\(\\s*[\"'`]([^\"'`]+)")},
	{"typescript", regexp.MustCompile("\\b(?:app|router|server|fastify)\\.(?:get|post|put|delete|patch|all|use)\\s*\\(\\s*[\"'`]([^\"'`]+)")},
	{"go", regexp.MustCompile(`\.(?:GET|POST|PUT|DELETE|PATCH|HandleFunc|Handle)\s*\(\s*["']([^"']+)["']`)},
	{"ruby", regexp.MustCompile(`\b(?:get|post|put|delete|patch)\s+["']([^"']+)["']`)},
	{"csharp", regexp.MustCompile(`\[(?:HttpGet|HttpPost|HttpPut|HttpDelete|Route)\s*\(\s*["']([^"']+)["']`)},
}

var codeEntryNames = map[string]bool{
	"main.go": true, "main.py": true, "app.py": true, "manage.py": true, "wsgi.py": true,
	"asgi.py": true, "index.js": true, "server.js": true, "app.js": true, "index.ts": true,
	"server.ts": true, "application.java": true, "program.cs": true, "index.php": true,
	"config.ru": true, "main.rs": true, "app.rb": true,
}

var codeTrustBoundaryRe = regexp.MustCompile(`(?i)(controller|handler|servlet|router|routes|api_?view|endpoint|resource)`)
var codeControllerMarkerRe = regexp.MustCompile(`@RestController|@Controller\b|HttpServlet|app\.route\(|router\.(?:get|post)|@Path\s*\(`)
var codeSecretNameRe = regexp.MustCompile(`(?i)^(\.env(\..*)?|config\.(php|json|yml|yaml|js|ts)|settings\.py|local_settings\.py|application(-[\w]+)?\.(yml|yaml|properties)|appsettings\.json|wp-config\.php|database\.yml|\.npmrc|\.pypirc|id_rsa|id_dsa)$`)
var codeSecretValueRe = regexp.MustCompile(`(?i)(password|passwd|secret|api[_-]?key|access[_-]?key|private[_-]?key|token)\s*[:=]\s*["'][^"']{6,}["']`)

func codeDetectFrameworks(root string, files []codeFile) (frameworks, manifests []string) {
	seen := map[string]bool{}
	for _, f := range files {
		base := filepath.Base(f.Abs)
		rules, ok := codeManifestFrameworks[base]
		if !ok {
			continue
		}
		manifests = append(manifests, f.Rel)
		body := strings.ToLower(readCodeText(f.Abs, 512<<10))
		for _, r := range rules {
			if strings.Contains(body, strings.ToLower(r.marker)) && !seen[r.framework] {
				seen[r.framework] = true
				frameworks = append(frameworks, r.framework)
			}
		}
	}
	sort.Strings(frameworks)
	sort.Strings(manifests)
	return frameworks, manifests
}

// codeRouteHit 是一条静态路由候选。
type codeRouteHit struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Path   string `json:"path"`
	Source string `json:"source"` // java-annotation / flask-route / express-router …
}

func codeScanRoutes(files []codeFile, limit int) []codeRouteHit {
	var out []codeRouteHit
	for _, f := range files {
		if f.Lang == "config" { // 配置/密钥文件不参与路由扫描（也不需要读正文）
			continue
		}
		body := readCodeText(f.Abs, codeAuditMaxFileBytes)
		if body == "" {
			continue
		}
		for i, line := range strings.Split(body, "\n") {
			for _, rule := range codeRouteRules {
				if rule.lang != f.Lang {
					continue
				}
				m := rule.re.FindStringSubmatch(line)
				if len(m) < 2 || !strings.HasPrefix(m[1], "/") {
					continue
				}
				out = append(out, codeRouteHit{File: f.Rel, Line: i + 1, Path: clipText(m[1], 120), Source: f.Lang + "-route"})
				if limit > 0 && len(out) >= limit {
					return out
				}
			}
		}
	}
	return out
}

func codeEntrypoints(files []codeFile, limit int) []string {
	var out []string
	for _, f := range files {
		base := strings.ToLower(filepath.Base(f.Abs))
		if codeEntryNames[base] || ((strings.Contains(f.Rel, "/cmd/") || strings.HasPrefix(f.Rel, "cmd/")) && strings.HasSuffix(f.Rel, "/main.go")) {
			out = append(out, f.Rel)
			if limit > 0 && len(out) >= limit {
				return out
			}
		}
	}
	return out
}

func codeTrustBoundaries(files []codeFile, limit int) []string {
	var out []string
	for _, f := range files {
		if !codeTrustBoundaryRe.MatchString(f.Rel) {
			continue
		}
		body := readCodeText(f.Abs, 32<<10)
		if body == "" || !codeControllerMarkerRe.MatchString(body) {
			continue
		}
		out = append(out, f.Rel)
		if limit > 0 && len(out) >= limit {
			return out
		}
	}
	return out
}

type codeSecretHotspot struct {
	File string `json:"file"`
	Hits int    `json:"hits"`
}

func codeSecretHotspots(files []codeFile, limit int) []codeSecretHotspot {
	var out []codeSecretHotspot
	for _, f := range files {
		base := filepath.Base(f.Abs)
		hits := 0
		if codeSecretNameRe.MatchString(base) {
			hits = 1
		}
		if hits == 0 {
			body := readCodeText(f.Abs, 64<<10)
			hits = len(codeSecretValueRe.FindAllString(body, 5))
		}
		if hits > 0 {
			out = append(out, codeSecretHotspot{File: f.Rel, Hits: hits})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Hits != out[j].Hits {
			return out[i].Hits > out[j].Hits
		}
		return out[i].File < out[j].File
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// ---- repo_map ----

func (t *ToolSet) repoMap() actool.CoreTool {
	return readTool("repo_map",
		"(白盒审计·第 1 步)源码仓库地图：语言分布、依赖清单、框架指纹、入口文件、路由候选、"+
			"信任边界（controller/handler）与密钥热点，结果落盘 code/repo_map.json 供 taint_spec/sast_scan 复用。\n"+
			"前置：源码要在任务工作目录内——先 Bash `git clone <repo> code` 或解压到 code/，再传 dir=code。\n"+
			"只读、有界（默认 5000 文件 / 64MB）。这是**地图**不是结论：路由/入口只用于排优先级。",
		obj(map[string]any{
			"dir":       str("源码目录（相对任务工作目录，如 code）"),
			"max_files": intp("最多扫描文件数（默认 5000，上限 20000）"),
		}, "dir"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Dir      string `json:"dir"`
				MaxFiles int    `json:"max_files"`
			}
			if uErr := json.Unmarshal(in, &a); uErr != nil {
				return actool.Errorf(fmt.Sprintf("参数不是合法 JSON：%v", uErr)), nil
			}
			root, err := t.codeAuditDir(a.Dir)
			if err != nil {
				return actool.Errorf(fmt.Sprintf("%v", err)), nil
			}
			maxFiles := a.MaxFiles
			if maxFiles <= 0 {
				maxFiles = codeAuditMaxFiles
			}
			if maxFiles > 20000 {
				maxFiles = 20000
			}
			files, totalBytes, truncated, err := walkCodeRepo(root, maxFiles)
			if err != nil {
				return actool.Errorf(fmt.Sprintf("遍历源码失败：%v", err)), nil
			}
			langCount := map[string]int{}
			langBytes := map[string]int64{}
			for _, f := range files {
				langCount[f.Lang]++
				langBytes[f.Lang] += f.Size
			}
			type langStat struct {
				Lang  string `json:"lang"`
				Files int    `json:"files"`
				Bytes int64  `json:"bytes"`
			}
			langs := make([]langStat, 0, len(langCount))
			for k, v := range langCount {
				langs = append(langs, langStat{Lang: k, Files: v, Bytes: langBytes[k]})
			}
			sort.Slice(langs, func(i, j int) bool {
				if langs[i].Files != langs[j].Files {
					return langs[i].Files > langs[j].Files
				}
				return langs[i].Lang < langs[j].Lang
			})
			frameworks, manifests := codeDetectFrameworks(root, files)
			report := map[string]any{
				"root":         a.Dir,
				"files":        len(files),
				"bytes":        totalBytes,
				"truncated":    truncated,
				"languages":    langs,
				"frameworks":   frameworks,
				"manifests":    manifests,
				"entrypoints":  codeEntrypoints(files, 50),
				"routes":       codeScanRoutes(files, 200),
				"boundaries":   codeTrustBoundaries(files, 100),
				"secret_spots": codeSecretHotspots(files, 50),
			}
			indexPath := ""
			if t.runDir != "" {
				if dir := ensureCodeDir(t.runDir); dir != "" {
					indexPath = filepath.Join(dir, "repo_map.json")
					if b, mErr := json.MarshalIndent(report, "", "  "); mErr == nil {
						_ = os.WriteFile(indexPath, b, 0o644)
					}
				}
			}
			report["index_file"] = indexPath
			report["next"] = "按框架/路由选高价值入口，用 taint_spec 生成 source→sink 规则，再 sast_scan(file://规则) 扫；" +
				"静态命中只是候选，必须 code_trace 或动态复现后才能在 report_finding 里写 confirmed。"
			return jsonResult(report)
		})
}

func ensureCodeDir(runDir string) string {
	if runDir == "" {
		return ""
	}
	dir := filepath.Join(runDir, "code")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	return dir
}

// ---- taint_spec ----

type codeTaintPreset struct {
	Language   string
	Sources    []string
	Sinks      []string
	Sanitizers []string
}

// codeTaintPresets 是各语言最常见的"输入→危险操作"起点。它们只是**种子**：
// 模型应按 repo_map 的框架/路由补充更精确的 source（如 Spring 的 @RequestParam）。
var codeTaintPresets = map[string]codeTaintPreset{
	"java-cmd": {
		Language: "java",
		Sources:  []string{`$REQ.getParameter(...)`, `$REQ.getHeader(...)`, `$REQ.getQueryString()`},
		Sinks:    []string{`Runtime.getRuntime().exec(...)`, `new ProcessBuilder(...)`},
	},
	"java-sqli": {
		Language: "java",
		Sources:  []string{`$REQ.getParameter(...)`, `$REQ.getHeader(...)`},
		Sinks:    []string{`$STMT.executeQuery(...)`, `$STMT.executeUpdate(...)`, `$STMT.execute(...)`},
	},
	"java-ssrf": {
		Language: "java",
		Sources:  []string{`$REQ.getParameter(...)`},
		Sinks:    []string{`new URL(...)`, `$CONN.connect()`},
	},
	"python-cmd": {
		Language: "python",
		Sources:  []string{`request.args.get(...)`, `request.form.get(...)`, `request.headers.get(...)`, `request.get_json()`},
		Sinks:    []string{`os.system(...)`, `subprocess.$F(...)`, `eval(...)`, `exec(...)`},
	},
	"python-sqli": {
		Language: "python",
		Sources:  []string{`request.args.get(...)`, `request.form.get(...)`},
		Sinks:    []string{`$CUR.execute($SQL)`, `$CUR.executemany(...)`},
	},
	"php-cmd": {
		Language: "php",
		Sources:  []string{`$_GET[$X]`, `$_POST[$X]`, `$_REQUEST[$X]`, `$_COOKIE[$X]`},
		Sinks:    []string{`system(...)`, `exec(...)`, `shell_exec(...)`, `passthru(...)`, `popen(...)`},
	},
	"php-sqli": {
		Language: "php",
		Sources:  []string{`$_GET[$X]`, `$_POST[$X]`},
		Sinks:    []string{`mysqli_query(...)`, `$PDO->query(...)`, `mysql_query(...)`},
	},
	"node-cmd": {
		Language: "javascript",
		Sources:  []string{`req.query.$X`, `req.body.$X`, `req.params.$X`},
		Sinks:    []string{`$CP.exec(...)`, `$CP.execSync(...)`, `$CP.spawn(...)`},
	},
	"go-cmd": {
		Language: "go",
		Sources:  []string{`$R.URL.Query().Get(...)`, `$R.FormValue(...)`},
		Sinks:    []string{`exec.Command(...)`, `exec.CommandContext(...)`},
	},
}

// renderTaintRule 把规格渲染成 Semgrep taint 模式规则。单引号 YAML 标量里
// 只需把 `'` 双写；换行会破坏 YAML 结构，直接拒绝（规则模式本来也不该跨行）。
func renderTaintRule(ruleID, language, message, severity string, sources, sinks, sanitizers []string) (string, error) {
	if !regexp.MustCompile(`^[A-Za-z0-9._-]{3,120}$`).MatchString(ruleID) {
		return "", fmt.Errorf("rule_id 只能包含字母/数字/._-，长度 3-120")
	}
	switch language {
	case "java", "python", "php", "javascript", "typescript", "go", "ruby", "csharp", "kotlin", "scala":
	default:
		return "", fmt.Errorf("不支持的语言 %q（可用：java/python/php/javascript/typescript/go/ruby/csharp/kotlin/scala）", language)
	}
	if len(sinks) == 0 {
		return "", fmt.Errorf("sinks 至少要有 1 条（危险操作模式）")
	}
	if len(sources) == 0 {
		return "", fmt.Errorf("sources 至少要有 1 条（用户输入模式）；不知道就先用 preset，再按框架补充")
	}
	switch strings.ToUpper(severity) {
	case "ERROR", "WARNING", "INFO":
	default:
		return "", fmt.Errorf("severity 只能是 ERROR/WARNING/INFO")
	}
	quote := func(s string) (string, error) {
		s = strings.TrimSpace(s)
		if s == "" {
			return "", fmt.Errorf("模式不能为空")
		}
		if strings.ContainsAny(s, "\r\n") {
			return "", fmt.Errorf("模式不能含换行：%q", clipText(s, 40))
		}
		return "'" + strings.ReplaceAll(s, "'", "''") + "'", nil
	}
	var b strings.Builder
	b.WriteString("rules:\n  - id: " + ruleID + "\n")
	b.WriteString("    mode: taint\n")
	b.WriteString("    languages: [" + language + "]\n")
	b.WriteString("    severity: " + strings.ToUpper(severity) + "\n")
	b.WriteString("    message: " + yamlQuote(message) + "\n")
	b.WriteString("    pattern-sources:\n")
	for _, s := range sources {
		q, err := quote(s)
		if err != nil {
			return "", fmt.Errorf("source: %w", err)
		}
		b.WriteString("      - pattern: " + q + "\n")
	}
	b.WriteString("    pattern-sinks:\n")
	for _, s := range sinks {
		q, err := quote(s)
		if err != nil {
			return "", fmt.Errorf("sink: %w", err)
		}
		b.WriteString("      - pattern: " + q + "\n")
	}
	if len(sanitizers) > 0 {
		b.WriteString("    pattern-sanitizers:\n")
		for _, s := range sanitizers {
			q, err := quote(s)
			if err != nil {
				return "", fmt.Errorf("sanitizer: %w", err)
			}
			b.WriteString("      - pattern: " + q + "\n")
		}
	}
	return b.String(), nil
}

// yamlQuote 用双引号 YAML 标量安全地承载 message（转义 \ " 与控制字符）。
func yamlQuote(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		s = "疑似污点数据流向危险操作"
	}
	repl := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`)
	return `"` + repl.Replace(s) + `"`
}

func (t *ToolSet) taintSpec() actool.CoreTool {
	return writeTool("taint_spec",
		"(白盒审计·第 2 步)把模型推断的 source→sink 规格**确定性**渲染成 Semgrep taint 规则，"+
			"落盘到 code/rules/<id>.yaml 并返回路径，交给 sast_scan(config=该路径)。\n"+
			"用法：先用 repo_map 看框架，再按框架写 source（如 Spring @RequestParam、Flask request.args），"+
			"不必写全量规则。preset 提供 java/python/php/node/go 的命令注入/SQLi/SSRF 起点，自定义字段会覆盖预设。\n"+
			"规则质量决定召回：写完先在 sast_scan 里跑一遍，命中为空就补 source/sanitizer 再试。",
		obj(map[string]any{
			"language":   str("语言：java/python/php/javascript/typescript/go/ruby/csharp/kotlin/scala"),
			"preset":     str("可选预设：java-cmd/java-sqli/java-ssrf/python-cmd/python-sqli/php-cmd/php-sqli/node-cmd/go-cmd"),
			"sources":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "用户输入模式（Semgrep pattern）"},
			"sinks":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "危险操作模式（Semgrep pattern）"},
			"sanitizers": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "可选中和函数（命中后不再算污点）"},
			"rule_id":    str("规则 ID（字母/数字/._-，默认按语言+时间生成）"),
			"message":    str("命中时给模型/报告看的说明"),
			"severity":   str("ERROR/WARNING/INFO（默认 ERROR）"),
		}, "language"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Language   string   `json:"language"`
				Preset     string   `json:"preset"`
				Sources    []string `json:"sources"`
				Sinks      []string `json:"sinks"`
				Sanitizers []string `json:"sanitizers"`
				RuleID     string   `json:"rule_id"`
				Message    string   `json:"message"`
				Severity   string   `json:"severity"`
			}
			if uErr := json.Unmarshal(in, &a); uErr != nil {
				return actool.Errorf(fmt.Sprintf("参数不是合法 JSON：%v", uErr)), nil
			}
			lang := strings.ToLower(strings.TrimSpace(a.Language))
			var preset codeTaintPreset
			if p := strings.ToLower(strings.TrimSpace(a.Preset)); p != "" {
				var ok bool
				preset, ok = codeTaintPresets[p]
				if !ok {
					return actool.Errorf(fmt.Sprintf("未知 preset %q", a.Preset)), nil
				}
				if lang == "" {
					lang = preset.Language
				} else if lang != preset.Language {
					return actool.Errorf(fmt.Sprintf("preset %q 的语言是 %s，与 language=%s 不一致；换 preset 或去掉 language", a.Preset, preset.Language, lang)), nil
				}
			}
			if lang == "" {
				return actool.Errorf("language 必填（或给 preset）"), nil
			}
			sources := a.Sources
			if len(sources) == 0 {
				sources = preset.Sources
			}
			sinks := a.Sinks
			if len(sinks) == 0 {
				sinks = preset.Sinks
			}
			sanitizers := a.Sanitizers
			if len(sanitizers) == 0 {
				sanitizers = preset.Sanitizers
			}
			ruleID := strings.TrimSpace(a.RuleID)
			if ruleID == "" {
				ruleID = fmt.Sprintf("pojun-%s-%d", lang, time.Now().Unix())
			}
			severity := strings.ToUpper(strings.TrimSpace(a.Severity))
			if severity == "" {
				severity = "ERROR"
			}
			yamlText, err := renderTaintRule(ruleID, lang, a.Message, severity, sources, sinks, sanitizers)
			if err != nil {
				return actool.Errorf(fmt.Sprintf("生成规则失败：%v", err)), nil
			}
			dir := ensureCodeDir(t.runDir)
			if dir == "" {
				return actool.Errorf("本次 run 没有可写工作目录"), nil
			}
			ruleDir := filepath.Join(dir, "rules")
			if err := os.MkdirAll(ruleDir, 0o755); err != nil {
				return actool.Errorf(fmt.Sprintf("创建规则目录失败：%v", err)), nil
			}
			path := filepath.Join(ruleDir, ruleID+".yaml")
			if err := os.WriteFile(path, []byte(yamlText), 0o644); err != nil {
				return actool.Errorf(fmt.Sprintf("写入规则失败：%v", err)), nil
			}
			return jsonResult(map[string]any{
				"rule_id":     ruleID,
				"language":    lang,
				"config_path": path,
				"rule_yaml":   yamlText,
				"next":        "sast_scan(dir=..., config=" + path + ")。命中为 0 时先怀疑 source 太窄，再补 sanitizer 后重试。",
			})
		})
}

// ---- sast_scan ----

type capBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	// io.Writer 契约：返回“已接受的字节数”。即使只保留了前缀，也必须报告原始
	// 长度，否则 os/exec 的 io.Copy 会判 ErrShortWrite 并关闭管道，semgrep 收到
	// EPIPE 提前退出（表现为“规则包/网络问题”，实际是输出超过上限）。
	n := len(p)
	remain := c.max - c.buf.Len()
	if remain <= 0 {
		c.truncated = true
		return n, nil
	}
	if len(p) > remain {
		c.truncated = true
		p = p[:remain]
	}
	c.buf.Write(p)
	return n, nil
}

func findSemgrep() (string, error) {
	if tool, ok := hosttools.Lookup("semgrep"); ok && tool.AvailableInPath() {
		if p, err := exec.LookPath(tool.Bin); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath("semgrep"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("未找到 semgrep：请先安装（pipx install semgrep / pojun-hostd 的宿主工具安装），" +
		"或改用 code_trace 做纯静态调用链分析")
}

type semgrepFinding struct {
	CheckID  string `json:"check_id"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Code     string `json:"code"`
}

// semgrepSeverityRank 把 Semgrep 的三档映射成可比较的序。
var semgrepSeverityRank = map[string]int{"INFO": 1, "WARNING": 2, "ERROR": 3}

// parseSemgrepOutput 解析 `semgrep --json` 输出：按 (规则,文件,行) 去重、
// 按严重度/路径排序、把绝对路径改成相对 root 的路径。纯函数，便于单测
// （不依赖本机是否装了 semgrep）。
func parseSemgrepOutput(raw []byte, root, minSeverity string) ([]semgrepFinding, map[string]int, []string, error) {
	var parsed struct {
		Results []struct {
			CheckID string `json:"check_id"`
			Path    string `json:"path"`
			Start   struct {
				Line int `json:"line"`
			} `json:"start"`
			Extra struct {
				Message  string `json:"message"`
				Severity string `json:"severity"`
				Lines    string `json:"lines"`
			} `json:"extra"`
		} `json:"results"`
		Errors []struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, nil, nil, err
	}
	minRank := semgrepSeverityRank[strings.ToUpper(strings.TrimSpace(minSeverity))]
	var findings []semgrepFinding
	byRule := map[string]int{}
	dedup := map[string]bool{}
	for _, r := range parsed.Results {
		sev := strings.ToUpper(r.Extra.Severity)
		if minRank > 0 && semgrepSeverityRank[sev] < minRank {
			continue
		}
		rel := r.Path
		if root != "" {
			if rr, relErr := filepath.Rel(root, r.Path); relErr == nil {
				rel = rr
			}
		}
		rel = filepath.ToSlash(rel)
		key := fmt.Sprintf("%s|%s|%d", r.CheckID, rel, r.Start.Line)
		if dedup[key] {
			continue
		}
		dedup[key] = true
		byRule[r.CheckID]++
		findings = append(findings, semgrepFinding{
			CheckID: r.CheckID, Path: rel, Line: r.Start.Line,
			Severity: sev, Message: clipText(strings.TrimSpace(r.Extra.Message), 300),
			Code: clipText(strings.TrimSpace(r.Extra.Lines), 300),
		})
	}
	sort.Slice(findings, func(i, j int) bool {
		if semgrepSeverityRank[findings[i].Severity] != semgrepSeverityRank[findings[j].Severity] {
			return semgrepSeverityRank[findings[i].Severity] > semgrepSeverityRank[findings[j].Severity]
		}
		if findings[i].Path != findings[j].Path {
			return findings[i].Path < findings[j].Path
		}
		return findings[i].Line < findings[j].Line
	})
	var ruleErrors []string
	for i, e := range parsed.Errors {
		if i >= 3 {
			break
		}
		ruleErrors = append(ruleErrors, clipText(e.Type+": "+e.Message, 200))
	}
	return findings, byRule, ruleErrors, nil
}

func (t *ToolSet) sastScan() actool.CoreTool {
	return writeTool("sast_scan",
		"(白盒审计·第 3 步)对任务目录内的源码跑 Semgrep，解析 JSON 并按规则分组返回候选。"+
			"config 可以是 taint_spec 产出的规则文件路径、任务内规则目录，或 Semgrep 规则包（p/default、p/java 等，首次需要能访问规则仓库）。"+
			"**静态命中只是候选**：必须 code_trace 或动态复现确认，才能在 report_finding 写 confirmed；"+
			"证据里要带上规则 ID、文件:行、原始匹配片段。全程无 shell、无目标请求，超时可配。",
		obj(map[string]any{
			"dir":          str("源码目录（相对任务工作目录）"),
			"config":       str("规则来源：任务内路径（taint_spec 的 config_path）或 p/<pack>；与 rules 二选一"),
			"rules":        str("规则 YAML 原文（可选；给了就写入 code/rules/inline-*.yaml 并优先于 config）"),
			"severity":     str("最低级别过滤：ERROR/WARNING/INFO（默认不过滤）"),
			"max_findings": intp("最多返回多少条（默认 100，上限 200）"),
			"timeout_sec":  intp("扫描超时秒数（默认 600，上限 1800）"),
		}, "dir"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Dir         string `json:"dir"`
				Config      string `json:"config"`
				Rules       string `json:"rules"`
				Severity    string `json:"severity"`
				MaxFindings int    `json:"max_findings"`
				TimeoutSec  int    `json:"timeout_sec"`
			}
			if uErr := json.Unmarshal(in, &a); uErr != nil {
				return actool.Errorf(fmt.Sprintf("参数不是合法 JSON：%v", uErr)), nil
			}
			root, err := t.codeAuditDir(a.Dir)
			if err != nil {
				return actool.Errorf(fmt.Sprintf("%v", err)), nil
			}
			cfg := strings.TrimSpace(a.Config)
			if strings.TrimSpace(a.Rules) != "" {
				dir := ensureCodeDir(t.runDir)
				if dir == "" {
					return actool.Errorf("本次 run 没有可写工作目录"), nil
				}
				ruleDir := filepath.Join(dir, "rules")
				if err := os.MkdirAll(ruleDir, 0o755); err != nil {
					return actool.Errorf(fmt.Sprintf("创建规则目录失败：%v", err)), nil
				}
				path := filepath.Join(ruleDir, fmt.Sprintf("inline-%d.yaml", time.Now().UnixNano()))
				if err := os.WriteFile(path, []byte(a.Rules), 0o644); err != nil {
					return actool.Errorf(fmt.Sprintf("写入规则失败：%v", err)), nil
				}
				cfg = path
			}
			if cfg != "" {
				if p, err := t.codeAuditResolve(cfg); err == nil {
					cfg = p
				} else if !regexp.MustCompile(`^p/[a-z0-9-]+$`).MatchString(cfg) {
					return actool.Errorf(fmt.Sprintf("config 必须是任务内规则路径或 p/<pack>：%v", err)), nil
				}
			}
			if cfg == "" {
				cfg = "p/default"
			}
			bin, err := findSemgrep()
			if err != nil {
				return actool.Errorf(fmt.Sprintf("%v", err)), nil
			}
			timeout := a.TimeoutSec
			if timeout <= 0 {
				timeout = 600
			}
			if timeout > 1800 {
				timeout = 1800
			}
			maxFindings := a.MaxFindings
			if maxFindings <= 0 {
				maxFindings = 100
			}
			if maxFindings > 200 {
				maxFindings = 200
			}
			// 跟随调用方 context（harness 取消/任务停止时要能立刻停），再加自身超时。
			runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
			defer cancel()
			args := []string{
				"--json", "--metrics=off", "--disable-version-check",
				"--timeout", "300", "--max-target-bytes", "2000000", "--jobs", "1",
				"--config", cfg, root,
			}
			cmd := exec.CommandContext(runCtx, bin, args...)
			cmd.SysProcAttr = jobSysProcAttr() // 自成进程组：超时能杀掉 semgrep-core 子进程
			stdout := &capBuffer{max: codeAuditMaxSastBytes}
			stderr := &capBuffer{max: 64 << 10}
			cmd.Stdout, cmd.Stderr = stdout, stderr
			runErr := cmd.Run()
			timedOut := runCtx.Err() == context.DeadlineExceeded
			if timedOut {
				// CommandContext 只杀主进程，semgrep 派生的 core 会残留继续吃 CPU；
				// 按进程组补一刀。
				if cmd.Process != nil {
					_ = jobKillGroup(cmd.Process.Pid)
				}
				return actool.Errorf(fmt.Sprintf("semgrep 超时（%ds）。缩小 dir（先扫一个模块）或减小规则范围后重试", timeout)), nil
			}
			if stdout.truncated {
				return actool.Errorf(fmt.Sprintf("semgrep 输出超过 %dMB 上限，JSON 不完整。缩小 dir（先扫一个模块）或用 rules/taint 限定范围后重试", codeAuditMaxSastBytes>>20)), nil
			}
			allFindings, byRule, ruleErrors, pErr := parseSemgrepOutput(stdout.buf.Bytes(), root, a.Severity)
			if pErr != nil {
				detail := strings.TrimSpace(stderr.buf.String())
				if detail == "" {
					detail = clipText(strings.TrimSpace(stdout.buf.String()), 500)
				}
				msg := fmt.Sprintf("semgrep 未返回可解析 JSON（exit=%v）：%s", runErr, detail)
				if cfg == "p/default" {
					msg += "；规则包需要访问 Semgrep 规则仓库，离线环境请先用 taint_spec 生成本地规则"
				}
				return actool.Errorf(msg), nil
			}
			total := len(allFindings)
			findings := allFindings
			if len(findings) > maxFindings {
				findings = findings[:maxFindings]
			}
			reportPath := ""
			if dir := ensureCodeDir(t.runDir); dir != "" {
				reportPath = filepath.Join(dir, fmt.Sprintf("sast-%d.json", time.Now().Unix()))
				raw := map[string]any{
					"dir": a.Dir, "config": cfg, "exit": fmt.Sprint(runErr),
					"total": total, "returned": len(findings), "by_rule": byRule,
					"findings": allFindings, "errors": ruleErrors,
				}
				if b, mErr := json.MarshalIndent(raw, "", "  "); mErr == nil {
					_ = os.WriteFile(reportPath, b, 0o644)
				}
			}
			return jsonResult(map[string]any{
				"config":       cfg,
				"total":        total,
				"returned":     len(findings),
				"by_rule":      byRule,
				"findings":     findings,
				"rule_errors":  ruleErrors,
				"stderr":       clipText(strings.TrimSpace(stderr.buf.String()), 400),
				"report_file":  reportPath,
				"semgrep_exit": fmt.Sprint(runErr),
				"note": "静态命中=候选。逐条用 code_trace 定位调用链、能在动态面复现的才 report_finding；" +
					"报告里带规则 ID + 文件:行 + 原始片段。规则噪音高时给 sinks 加限定（如特定库调用）或补 sanitizer。",
			})
		})
}

// ---- patch_variants ----

// patchLine 是 diff 里的一行（行号是该行在原文件中的位置；新增行没有原行号）。
type patchLine struct {
	File string
	Line int
	Text string
}

type patchParseResult struct {
	Files   []string
	Removed []patchLine
	Added   []patchLine
}

var patchHunkRE = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// parseUnifiedDiff 解析 unified diff（git diff / git show 输出）。只关心
// 文件路径与删除/新增行；二进制/重命名段跳过。解析失败返回 error 由工具转成提示。
func parseUnifiedDiff(diff string) (patchParseResult, error) {
	var out patchParseResult
	lines := strings.Split(strings.ReplaceAll(diff, "\r\n", "\n"), "\n")
	curFile := ""
	oldLine, newLine := 0, 0
	seen := map[string]bool{}
	for i := 0; i < len(lines); i++ {
		raw := lines[i]
		// 只有紧邻的 "--- a/x" + "+++ b/x" 才是文件头。删除的行本身可能以
		// "-- " 开头（SQL 注释），旧实现按前缀匹配会把 "--- comment" 当成文件头，
		// 既丢掉这条删除行，又把后续行挂到错误文件上。
		if strings.HasPrefix(raw, "--- ") && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "+++ ") {
			name := strings.TrimSpace(strings.TrimPrefix(lines[i+1], "+++ "))
			if j := strings.IndexByte(name, '\t'); j >= 0 {
				name = name[:j]
			}
			name = strings.TrimPrefix(name, "b/")
			curFile = ""
			if name != "/dev/null" {
				curFile = filepath.ToSlash(name)
				if curFile != "" && !seen[curFile] {
					seen[curFile] = true
					out.Files = append(out.Files, curFile)
				}
			}
			i++ // 跳过 +++ 行
			continue
		}
		if m := patchHunkRE.FindStringSubmatch(raw); m != nil {
			oldLine, _ = strconv.Atoi(m[1])
			newLine, _ = strconv.Atoi(m[3])
			continue
		}
		if curFile == "" {
			continue
		}
		switch {
		case strings.HasPrefix(raw, "-"):
			out.Removed = append(out.Removed, patchLine{File: curFile, Line: oldLine, Text: strings.TrimPrefix(raw, "-")})
			oldLine++
		case strings.HasPrefix(raw, "+"):
			out.Added = append(out.Added, patchLine{File: curFile, Line: newLine, Text: strings.TrimPrefix(raw, "+")})
			newLine++
		case strings.HasPrefix(raw, " ") || raw == "":
			oldLine++
			newLine++
		}
	}

	if len(out.Files) == 0 {
		return out, fmt.Errorf("没有解析到文件段：需要 unified diff（git diff / git show 输出，含 ---/+++/@@）")
	}
	return out, nil
}

var patchIdentRE = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\s*\(`)

// patchLineSignature 归一一行代码用于"复制粘贴"比对（去首尾空白、压缩空白）。
func patchLineSignature(line string) string {
	t := strings.TrimSpace(line)
	if len(t) < 10 || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "#") ||
		strings.HasPrefix(t, "*") || strings.HasPrefix(t, "/*") || strings.Trim(t, "{}();,") == "" {
		return ""
	}
	return strings.Join(strings.Fields(t), " ")
}

func (t *ToolSet) patchVariants() actool.CoreTool {
	return readTool("patch_variants",
		"(白盒审计·变体分析)给一段 unified diff（git diff/git show 输出，或任务内 diff 文件路径），"+
			"从被删除的脆弱代码出发找变体：同仓库中复制粘贴的相同代码行（copy_paste），以及同一函数在别处的调用点（same_call）。\n"+
			"用途：补丁只修了一处时，确认同类写法是否还在别处（Big Sleep 的 patch→变体思路）。\n"+
			"这是文本级启发式：same_call 只说明同一函数在别处也被调用，是否有污点要看调用方；候选必须 code_trace 或动态验证。",
		obj(map[string]any{
			"dir":            str("源码目录（相对任务工作目录，如 code）"),
			"diff":           str("unified diff 原文；若给的是任务内文件路径（如 code/fix.patch）则读该文件"),
			"max_candidates": intp("每类最多返回多少条（默认 30，上限 100）"),
		}, "dir", "diff"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Dir           string `json:"dir"`
				Diff          string `json:"diff"`
				MaxCandidates int    `json:"max_candidates"`
			}
			if uErr := json.Unmarshal(in, &a); uErr != nil {
				return actool.Errorf(fmt.Sprintf("参数不是合法 JSON：%v", uErr)), nil
			}
			root, err := t.codeAuditDir(a.Dir)
			if err != nil {
				return actool.Errorf(fmt.Sprintf("%v", err)), nil
			}
			diff := strings.TrimSpace(a.Diff)
			if diff == "" {
				return actool.Errorf("diff 不能为空"), nil
			}
			// 允许把 diff 存成任务内文件（长 diff 粘贴不方便）。
			if !strings.Contains(diff, "diff --git") && !strings.Contains(diff, "--- ") && !strings.Contains(diff, "@@") {
				if p, pErr := t.codeAuditResolve(diff); pErr == nil {
					if body := readCodeText(p, 4<<20); body != "" {
						diff = body
					}
				}
			}
			parsed, err := parseUnifiedDiff(diff)
			if err != nil {
				return actool.Errorf(fmt.Sprintf("%v", err)), nil
			}
			maxCands := a.MaxCandidates
			if maxCands <= 0 {
				maxCands = 30
			}
			if maxCands > 100 {
				maxCands = 100
			}
			files, _, _, err := walkCodeRepo(root, codeAuditMaxFiles)
			if err != nil {
				return actool.Errorf(fmt.Sprintf("遍历源码失败：%v", err)), nil
			}
			// copy_paste：删除行的归一签名 → 其他文件中出现同样签名。
			sigToPattern := map[string]string{}
			for _, pl := range parsed.Removed {
				if sig := patchLineSignature(pl.Text); sig != "" {
					sigToPattern[sig] = strings.TrimSpace(pl.Text)
				}
			}
			type variantHit struct {
				File     string `json:"file"`
				Line     int    `json:"line"`
				Snippet  string `json:"snippet"`
				Pattern  string `json:"pattern,omitempty"`
				Relation string `json:"relation"` // copy_paste|same_call
			}
			changed := map[string]bool{}
			for _, f := range parsed.Files {
				changed[f] = true
			}
			var copyPaste, sameCall []variantHit
			// same_call：删除行里调用的函数名。
			idents := map[string]bool{}
			for _, pl := range parsed.Removed {
				for _, m := range patchIdentRE.FindAllStringSubmatch(pl.Text, 5) {
					name := m[1]
					switch name {
					case "if", "for", "while", "switch", "catch", "return", "new", "else", "do", "try", "function", "def", "func":
						continue
					}
					if len(name) >= 4 {
						idents[name] = true
					}
				}
			}
			for _, f := range files {
				lines := strings.Split(readCodeText(f.Abs, codeAuditMaxFileBytes), "\n")
				for i, line := range lines {
					if len(copyPaste) < maxCands && !changed[f.Rel] {
						if sig := patchLineSignature(line); sig != "" {
							if pat, ok := sigToPattern[sig]; ok {
								copyPaste = append(copyPaste, variantHit{
									File: f.Rel, Line: i + 1, Snippet: clipText(strings.TrimSpace(line), 200),
									Pattern: pat, Relation: "copy_paste",
								})
							}
						}
					}
					if len(sameCall) < maxCands && !changed[f.Rel] {
						for _, m := range patchIdentRE.FindAllStringSubmatch(line, 5) {
							name := m[1]
							if !idents[name] {
								continue
							}
							sameCall = append(sameCall, variantHit{
								File: f.Rel, Line: i + 1, Snippet: clipText(strings.TrimSpace(line), 200),
								Pattern: name + "(", Relation: "same_call",
							})
							break
						}
					}
				}
			}
			// 同一行已被 copy_paste 命中时不再重复报 same_call（前者证据更强）。
			if len(copyPaste) > 0 && len(sameCall) > 0 {
				hit := map[string]bool{}
				for _, h := range copyPaste {
					hit[h.File+":"+strconv.Itoa(h.Line)] = true
				}
				kept := sameCall[:0]
				for _, h := range sameCall {
					if !hit[h.File+":"+strconv.Itoa(h.Line)] {
						kept = append(kept, h)
					}
				}
				sameCall = kept
			}
			reportPath := ""
			if dir := ensureCodeDir(t.runDir); dir != "" {
				reportPath = filepath.Join(dir, "patch_variants.json")
				if b, mErr := json.MarshalIndent(map[string]any{
					"changed_files": parsed.Files,
					"removed":       parsed.Removed,
					"added":         parsed.Added,
					"copy_paste":    copyPaste,
					"same_call":     sameCall,
				}, "", "  "); mErr == nil {
					_ = os.WriteFile(reportPath, b, 0o644)
				}
			}
			return jsonResult(map[string]any{
				"changed_files": parsed.Files,
				"removed_lines": len(parsed.Removed),
				"added_lines":   len(parsed.Added),
				"copy_paste":    copyPaste,
				"same_call":     sameCall,
				"report_file":   reportPath,
				"note": "copy_paste=其他文件里存在与修复前完全相同的代码行（优先确认是否同样可控）；" +
					"same_call=同一函数在别处也被调用（需要看调用方参数是否来自外部输入）。" +
					"两类都只是候选：用 code_trace 查数据流，能动态复现才 report_finding。",
			})
		})
}

// ---- code_trace ----

var codeFuncDefRules = map[string][]*regexp.Regexp{
	"go":         {regexp.MustCompile(`^func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)\s*\(`)},
	"python":     {regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z_]\w*)\s*\(`)},
	"php":        {regexp.MustCompile(`\bfunction\s+([A-Za-z_]\w*)\s*\(`)},
	"ruby":       {regexp.MustCompile(`^\s*def\s+([A-Za-z_]\w*[?!]?)`)},
	"javascript": {regexp.MustCompile(`(?:function\s+([A-Za-z_$][\w$]*)|(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s*)?(?:function|\())`)},
	"typescript": {regexp.MustCompile(`(?:function\s+([A-Za-z_$][\w$]*)|(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s*)?(?:function|\())`)},
	"java":       {regexp.MustCompile(`\b(?:public|private|protected|static|final|synchronized|native|abstract|override|suspend|\s)+[\w<>\[\],.?\s]+\s+([A-Za-z_]\w*)\s*\(`)},
	"kotlin":     {regexp.MustCompile(`\bfun\s+(?:<[^>]+>\s*)?(?:[\w.]+\.)?([A-Za-z_]\w*)\s*\(`)},
	"csharp":     {regexp.MustCompile(`\b(?:public|private|protected|internal|static|async|override|virtual|sealed|\s)+[\w<>\[\],.?\s]+\s+([A-Za-z_]\w*)\s*\(`)},
	"scala":      {regexp.MustCompile(`\bdef\s+([A-Za-z_]\w*)\s*[\[(]`)},
	"c":          {regexp.MustCompile(`^[\w\*\s]+\s+([A-Za-z_]\w*)\s*\([^;]*\)\s*\{?\s*$`)},
	"cpp":        {regexp.MustCompile(`^[\w\*\s:<>]+\s+([A-Za-z_]\w*)\s*\([^;]*\)\s*\{?\s*$`)},
	"rust":       {regexp.MustCompile(`\bfn\s+([A-Za-z_]\w*)\s*\(`)},
}

// enclosingCodeFunc 由命中行向上找最近的函数定义（最多 300 行），拿不到就返回空。
func enclosingCodeFunc(lines []string, idx int, lang string) (string, int) {
	rules := codeFuncDefRules[lang]
	if len(rules) == 0 {
		return "", 0
	}
	start := idx - 300
	if start < 0 {
		start = 0
	}
	for i := idx; i >= start; i-- {
		for _, re := range rules {
			m := re.FindStringSubmatch(lines[i])
			if len(m) < 2 {
				continue
			}
			name := m[1]
			if name == "" && len(m) > 2 {
				name = m[2]
			}
			if name == "" || isCodeKeyword(name) {
				continue
			}
			return name, i + 1
		}
	}
	return "", 0
}

func isCodeKeyword(s string) bool {
	switch s {
	case "if", "for", "while", "switch", "catch", "return", "new", "else", "do", "try", "function":
		return true
	}
	return false
}

// compileLooseRegex 先按正则编译；写错时退化为字面量匹配（模型常把 `Runtime.getRuntime().exec` 直接当正则）。
func compileLooseRegex(p string) *regexp.Regexp {
	if re, err := regexp.Compile(p); err == nil {
		return re
	}
	return regexp.MustCompile(regexp.QuoteMeta(p))
}

type codeTraceNode struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Func string `json:"func,omitempty"`
}

type codeTracePath struct {
	Sink       string          `json:"sink"`
	Source     string          `json:"source,omitempty"`
	Depth      int             `json:"depth"`
	Confidence string          `json:"confidence"`
	SourceSeen bool            `json:"source_seen"`
	Nodes      []codeTraceNode `json:"nodes"`
}

func (t *ToolSet) codeTrace() actool.CoreTool {
	return readTool("code_trace",
		"(白盒审计·第 4 步)静态调用链反查：给定 sink 模式（函数/表达式）与可选 source 模式，"+
			"找出 sink 出现在哪些函数里、谁能调到这些函数（最多 5 层），以及路径上是否出现过 source。"+
			"输出按置信度排序的候选路径，并累计到 code/code_trace.json。\n"+
			"这是**启发式**：只看文本调用关系，不解析类型/动态派发；结论只能用于排序和补证据，"+
			"不能单独当漏洞证明。source 只按文件匹配，不保证同一数据流。",
		obj(map[string]any{
			"dir":       str("源码目录（相对任务工作目录）"),
			"sink":      str("危险操作模式（正则；写错会按字面量匹配），如 Runtime.getRuntime\\(\\).exec 或 exec.Command"),
			"source":    str("可选：用户输入模式（正则），用于判断路径上是否存在 source"),
			"max_depth": intp("向上追几层调用（1-5，默认 3）"),
			"max_nodes": intp("最多访问多少个函数节点（默认 200，上限 500）"),
		}, "dir", "sink"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				Dir      string `json:"dir"`
				Sink     string `json:"sink"`
				Source   string `json:"source"`
				MaxDepth int    `json:"max_depth"`
				MaxNodes int    `json:"max_nodes"`
			}
			if uErr := json.Unmarshal(in, &a); uErr != nil {
				return actool.Errorf(fmt.Sprintf("参数不是合法 JSON：%v", uErr)), nil
			}
			root, err := t.codeAuditDir(a.Dir)
			if err != nil {
				return actool.Errorf(fmt.Sprintf("%v", err)), nil
			}
			if strings.TrimSpace(a.Sink) == "" {
				return actool.Errorf("sink 不能为空"), nil
			}
			depth := a.MaxDepth
			if depth <= 0 {
				depth = 3
			}
			if depth > 5 {
				depth = 5
			}
			maxNodes := a.MaxNodes
			if maxNodes <= 0 {
				maxNodes = 200
			}
			if maxNodes > 500 {
				maxNodes = 500
			}
			files, _, _, err := walkCodeRepo(root, codeAuditMaxFiles)
			if err != nil {
				return actool.Errorf(fmt.Sprintf("遍历源码失败：%v", err)), nil
			}
			// 两遍读、不驻留全文：5000 文件 × 256KB 全存内存会到 GB 级。
			// 第一遍拿 source 文件集；第二遍逐文件找 sink 命中；第三遍按合并后的
			// 调用正则找调用点。单文件读取有界，重读的代价远小于内存风险。
			sourceRe := (*regexp.Regexp)(nil)
			if strings.TrimSpace(a.Source) != "" {
				sourceRe = compileLooseRegex(a.Source)
			}
			sourceFiles := map[string]bool{}
			if sourceRe != nil {
				for _, f := range files {
					if sourceRe.MatchString(readCodeText(f.Abs, codeAuditMaxFileBytes)) {
						sourceFiles[f.Rel] = true
					}
				}
			}
			sinkRe := compileLooseRegex(a.Sink)
			// 1) 定位 sink 命中与所在函数。
			var sites []codeTraceNode
			for _, f := range files {
				lines := strings.Split(readCodeText(f.Abs, codeAuditMaxFileBytes), "\n")
				for i, line := range lines {
					if sinkRe.MatchString(line) {
						fn, _ := enclosingCodeFunc(lines, i, f.Lang)
						sites = append(sites, codeTraceNode{File: f.Rel, Line: i + 1, Func: fn})
					}
				}
			}
			// 2) 建"谁调用了谁"的反向索引。函数名先合并成一条 alternation 正则，
			// 每行只匹配一次（逐名字匹配是 files×lines×names 的三重循环）。
			names := map[string]bool{}
			for _, s := range sites {
				if s.Func != "" {
					names[s.Func] = true
				}
			}
			if len(names) == 0 {
				return jsonResult(map[string]any{
					"sink": a.Sink, "hits": len(sites), "paths": []any{},
					"note": "sink 命中的位置不在可识别函数内（脚本/顶层代码），无法向上追调用链；" +
						"可直接看命中行的上下文，或换一个包含调用点的函数名作为 sink。",
				})
			}
			nameList := make([]string, 0, len(names))
			for n := range names {
				nameList = append(nameList, regexp.QuoteMeta(n))
			}
			sort.Strings(nameList)
			callRe := regexp.MustCompile(`\b(` + strings.Join(nameList, "|") + `)\s*\(`)
			funcCallers := map[string][]codeTraceNode{} // 函数名 → 调用点
			for _, f := range files {
				lines := strings.Split(readCodeText(f.Abs, codeAuditMaxFileBytes), "\n")
				for i, line := range lines {
					m := callRe.FindStringSubmatch(line)
					if len(m) < 2 {
						continue
					}
					n := m[1]
					fn, _ := enclosingCodeFunc(lines, i, f.Lang)
					if fn == n { // 定义行本身，不算调用
						continue
					}
					funcCallers[n] = append(funcCallers[n], codeTraceNode{File: f.Rel, Line: i + 1, Func: fn})
				}
			}
			// 3) 从每个 sink 站点向上 BFS。
			type state struct {
				nodes []codeTraceNode
				front codeTraceNode
			}
			var paths []codeTracePath
			visited := 0
			for _, site := range sites {
				if site.Func == "" {
					continue
				}
				queue := []state{{nodes: []codeTraceNode{site}, front: site}}
				seen := map[string]bool{site.Func + "@" + site.File: true}
				for d := 0; d < depth && len(queue) > 0; d++ {
					var next []state
					for _, st := range queue {
						callers := funcCallers[st.front.Func]
						if len(callers) == 0 {
							// 没有调用者：路径到这里就是顶层入口，作为一条完整路径。
							paths = append(paths, makeTracePath(a.Sink, a.Source, st.nodes, sourceFiles))
							continue
						}
						for _, c := range callers {
							key := c.Func + "@" + c.File
							if seen[key] {
								continue
							}
							seen[key] = true
							if visited >= maxNodes {
								break
							}
							visited++
							child := state{nodes: append(append([]codeTraceNode{}, st.nodes...), c), front: c}
							next = append(next, child)
							paths = append(paths, makeTracePath(a.Sink, a.Source, child.nodes, sourceFiles))
						}
					}
					queue = next
				}
			}
			sort.Slice(paths, func(i, j int) bool {
				ri, rj := traceRank(paths[i].Confidence), traceRank(paths[j].Confidence)
				if ri != rj {
					return ri > rj
				}
				return paths[i].Depth > paths[j].Depth
			})
			if len(paths) > 40 {
				paths = paths[:40]
			}
			memFile := ""
			if dir := ensureCodeDir(t.runDir); dir != "" {
				memFile = filepath.Join(dir, "code_trace.json")
				appendTraceMemory(memFile, a.Sink, a.Source, paths)
			}
			dist := map[string]int{}
			for _, p := range paths {
				dist[p.Confidence]++
			}
			return jsonResult(map[string]any{
				"sink":       a.Sink,
				"source":     a.Source,
				"hits":       len(sites),
				"paths":      paths,
				"confidence": dist,
				"trace_file": memFile,
				"note": "路径来自文本调用关系（启发式）：confidence=high 表示 sink 与 source 同文件，" +
					"medium 表示路径上出现过 source，low 表示没找到 source。先在动态面复现（http_request/Bash）" +
					"或补上鉴权条件，再决定是否 report_finding。",
			})
		})
}

func traceRank(c string) int {
	switch c {
	case "high":
		return 3
	case "medium":
		return 2
	default:
		return 1
	}
}

func makeTracePath(sink, source string, nodes []codeTraceNode, sourceFiles map[string]bool) codeTracePath {
	seen := false
	for _, n := range nodes {
		if sourceFiles[n.File] {
			seen = true
			break
		}
	}
	conf := "low"
	if seen {
		conf = "medium"
		// sink 与 source 在同一文件 → 数据流最可能闭环，给 high。
		if len(nodes) > 0 && sourceFiles[nodes[0].File] {
			conf = "high"
		}
	}
	return codeTracePath{
		Sink: sink, Source: source, Depth: len(nodes) - 1,
		Confidence: conf, SourceSeen: seen, Nodes: nodes,
	}
}

// appendTraceMemory 把本轮路径并入 code/code_trace.json（去重、上限 500 条）。
// 记忆的作用：多次 code_trace 之间不重复探索同一条链，也便于报告引用。
func appendTraceMemory(path, sink, source string, paths []codeTracePath) {
	type memory struct {
		Sink  string          `json:"sink"`
		Paths []codeTracePath `json:"paths"`
	}
	existing := map[string]memory{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &existing)
	}
	entry := existing[sink]
	entry.Sink = sink
	seen := map[string]bool{}
	for _, p := range entry.Paths {
		seen[traceKey(p)] = true
	}
	for _, p := range paths {
		k := traceKey(p)
		if seen[k] {
			continue
		}
		seen[k] = true
		entry.Paths = append(entry.Paths, p)
		if len(entry.Paths) >= codeAuditMaxPaths {
			break
		}
	}
	existing[sink] = entry
	if b, err := json.MarshalIndent(existing, "", "  "); err == nil && len(b) < 4<<20 {
		_ = os.WriteFile(path, b, 0o644)
	}
}

func traceKey(p codeTracePath) string {
	var b strings.Builder
	for _, n := range p.Nodes {
		fmt.Fprintf(&b, "%s:%d>", n.File, n.Line)
	}
	return b.String()
}
