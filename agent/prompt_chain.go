package agent

import (
	"strings"

	"github.com/chu0119/pojun/hosttools"
)

// PromptSegment is one ordered piece of a system prompt, exposed for the
// read-only assembly view. Text is the exact chunk joined into the final prompt
// (including its leading separator); JoinPromptSegments concatenates chunks
// verbatim, so renderSystemWith and the preview stay byte-identical.
type PromptSegment struct {
	Key       string `json:"key"`
	Title     string `json:"title"`
	Source    string `json:"source"`
	Editable  bool   `json:"editable"`
	Gate      string `json:"gate,omitempty"`
	Present   bool   `json:"present"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	Text      string `json:"text"`
	// OriginalChars is the content length when Text was dropped from the request
	// (replace mode). Zero when Text is the content. The read-only chain projects
	// it so a removed body reports its original size instead of 0.
	OriginalChars int `json:"-"`
}

// JoinPromptSegments concatenates segment texts without inserting separators:
// code-owned tails already carry their own "\n\n" prefix, matching the historical
// string concatenation byte for byte.
func JoinPromptSegments(parts []PromptSegment) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// TailOptions supplies the few runtime values the static code-owned tails
// interpolate. Empty strings become visible placeholders, so a preview never
// claims a path that the production run does not use.
//
// The concrete values each runtime builder uses live in tail_options.go
// (PlannerTailOptions / WorkerTailOptions / MainAgentTailOptions /
// ChatTailOptions); the builders and the anti-drift tests call those
// constructors, so the struct literal is written once, not per site.
type TailOptions struct {
	WorkDir string
	RunDir  string
	// ProxyAddr may be a masked/placeholder value in read-only previews; the
	// runtime passes the real egress address.
	ProxyAddr        string
	CAEnabled        bool
	IncludeHostTools bool
	GoalScope        bool
}

// DefaultPromptFor returns the built-in editable body used when an agent has no
// saved DB prompt. Custom chat agents fall back to the assistant starter prompt.
func DefaultPromptFor(agentKey string) string {
	if t, ok := BuiltinPromptSeeds()[agentKey]; ok {
		return t
	}
	switch agentKey {
	case "reporter":
		return ReporterDefaultPrompt
	case "retester":
		return RetesterDefaultPrompt
	}
	return DefaultAssistantPrompt
}

// PreviewSystemSegments renders the editable body (DB template if present,
// otherwise the built-in default), the skill index and the profile overlay with
// sample values. The returned warning is non-empty when the selected template
// failed and a fallback was used. Runtime-only tails are not included here;
// callers append StaticTailSegments and their own runtime placeholders.
func PreviewSystemSegments(agentKey, def string, pp ProfilePrompt) ([]PromptSegment, string) {
	return renderSystemSegments(agentKey, def, PreviewVarsFor(agentKey), pp)
}

// RuntimeSystemSegments assembles the exact segment list a runtime turn joins for
// an agent: the editable body (with any ProfilePrompt overlay), the skill index,
// and the code-owned static tails. The runtime builders call this, and the
// read-only chain projection renders the same inputs through
// StaticTailSegments, so preview and runtime cannot drift. The returned warning
// is non-empty when the selected template failed and a fallback was used.
//
// Runtime-only injections (task constraints, Program Profile, task auth, intent
// context, facts/lessons, autopilot, launch instruction, finding guidance,
// deferred tools) are appended by callers and are not part of this function.
func RuntimeSystemSegments(agentKey, def string, vars any, pp ProfilePrompt, o TailOptions) ([]PromptSegment, string) {
	segs, warning := renderSystemSegments(agentKey, def, vars, pp)
	return append(segs, StaticTailSegments(agentKey, o)...), warning
}

// RuntimeSystem is RuntimeSystemSegments' joined form — the system prompt before
// any runtime-only injection.
func RuntimeSystem(agentKey, def string, vars any, pp ProfilePrompt, o TailOptions) (string, string) {
	segs, warning := RuntimeSystemSegments(agentKey, def, vars, pp, o)
	return JoinPromptSegments(segs), warning
}

// PreviewVarsFor returns the sample template variables for an agent. Exported so
// tests can rebuild a runtime assembly with exactly the values the preview shows.
func PreviewVarsFor(agentKey string) any {
	switch agentKey {
	case "goals":
		// EngagementDescription is registered in the catalog but the runtime
		// passes it empty (the description rides in the user message instead).
		return GoalsVars{EngagementDescription: "", DataDir: "<数据目录>", Now: nowStr()}
	case "planner":
		return PlannerVars{Goal: "<任务目标>", Scope: "", AssetSummary: "", DataDir: "<数据目录>", Now: nowStr()}
	case "worker":
		return WorkerVars{ProxyAddr: "<出口代理>", WorkerName: "", DataDir: "<数据目录>", Now: nowStr()}
	case "mainagent":
		return MainVars{Goal: "<任务目标>", AssetSummary: "", FindingsSummary: "", DataDir: "<数据目录>", Now: nowStr()}
	default:
		return chatVars{DataDir: "<数据目录>", Now: nowStr()}
	}
}

// StaticTailSegments returns the code-owned, non-editable tail segments for an
// agent, in the exact order the runtime builders append them. It shares the same
// constants/functions as the runtime, so the preview cannot silently drift.
// Runtime-only tails (constraints, Program Profile, task auth, intent, facts,
// lessons, autopilot, launch instruction) are represented separately by callers.
func StaticTailSegments(agentKey string, o TailOptions) []PromptSegment {
	workDir := o.WorkDir
	if workDir == "" {
		workDir = "<任务工作目录>"
	}
	runDir := o.RunDir
	if runDir == "" {
		runDir = "<本次意图工作目录>"
	}
	code := func(key, title string, text string) PromptSegment {
		return PromptSegment{Key: key, Title: title, Source: "code", Present: true, Available: true, Text: text}
	}
	conditional := func(key, title, gate, reason string) PromptSegment {
		return PromptSegment{Key: key, Title: title, Source: "code", Gate: gate, Present: false, Available: true, Reason: reason}
	}

	switch agentKey {
	case "planner":
		return []PromptSegment{
			code("artifact_spec", "中间产物输出规约", artifactSpec(workDir)),
			code("coverage_matrix", "漏洞覆盖矩阵", coverageMatrixPlannerGuidance),
			code("scope_verify", "授权边界与复核优先", scopeVerifyPlannerGuidance),
			code("exec_face_planner", "执行面编排", execFacePlannerGuidance),
			code("engagement_framing", "交战框定", engagementFraming),
			code("impact_orchestration", "影响编排", impactOrchestrationGuidance),
			code("blackboard", "规划黑板", blackboardPlannerGuidance),
			code("untrusted_output", "不可信工具输出边界", untrustedOutputGuidance),
		}

	case "worker":
		traffic := conditional("traffic_tools", "流量工具说明", "需开启流量捕获", "流量捕获未开启，运行时不会出现 traffic_* 工具")
		if o.CAEnabled {
			traffic.Present, traffic.Reason, traffic.Text = true, "", workerTrafficBlock(true)
		}
		egress := conditional("egress_proxy", "出口代理说明", "需配置出口代理", "未配置出口代理")
		if strings.TrimSpace(o.ProxyAddr) != "" {
			egress.Present, egress.Reason, egress.Text = true, "", workerEgressBlock(o.ProxyAddr)
		}
		out := []PromptSegment{
			traffic,
			egress,
			code("artifact_spec", "中间产物输出规约", workerArtifactSpec(runDir)),
			code("scanner_first", "扫描器优先", scannerFirstGuidance),
			code("skill_routing", "技能路由", skillRoutingGuidance),
			code("exec_face_worker", "执行面", execFaceWorkerGuidance),
			code("engagement_framing", "交战框定", engagementFraming),
			code("impact_verification", "影响验证", impactVerificationGuidance),
			code("untrusted_output", "不可信工具输出边界", untrustedOutputGuidance),
		}
		out = append(out, hostToolsSegment(o))
		return out

	case "mainagent":
		out := []PromptSegment{
			code("artifact_spec", "中间产物输出规约", artifactSpec(workDir)),
			code("engagement_framing", "交战框定", engagementFraming),
			code("impact_orchestration", "影响编排", impactOrchestrationGuidance),
			code("src_task_book", "SRC 任务书", srcTaskBookGuidance),
			code("untrusted_output", "不可信工具输出边界", untrustedOutputGuidance),
		}
		out = append(out, hostToolsSegment(o))
		return out

	case "goals":
		out := []PromptSegment{
			code("goals_verification", "验收标准", goalsVerificationTail),
		}
		scope := conditional("goals_scope", "范围提取", "存在资产库与任务上下文", "无资产库/任务上下文时不注入，也不挂 add_task_scope")
		if o.GoalScope {
			scope.Present, scope.Reason, scope.Text = true, "", goalsScopeTail
		}
		return append(out, scope)

	default: // conversational agents: pentest / reporter / retester / auto / custom
		out := []PromptSegment{
			code("chat_workdir", "文件输出规约", chatWorkDirSpec(workDir)),
		}
		switch agentKey {
		case "pentest":
			out = append(out,
				code("engagement_framing", "交战框定", engagementFraming),
				code("impact_verification", "影响验证", impactVerificationGuidance),
				code("impact_orchestration", "影响编排", impactOrchestrationGuidance),
				code("src_report_annex", "SRC 报告附录", srcReportAnnex),
			)
		case "reporter":
			out = append(out,
				code("engagement_framing", "交战框定", engagementFraming),
				code("src_report_annex", "SRC 报告附录", srcReportAnnex),
			)
		case "retester":
			out = append(out, code("engagement_framing", "交战框定", engagementFraming))
		}
		return out
	}
}

func hostToolsSegment(o TailOptions) PromptSegment {
	seg := PromptSegment{
		Key: "host_tools", Title: "本机可用工具", Source: "host",
		Gate: "宿主工具清单非空", Available: true,
	}
	if !o.IncludeHostTools {
		seg.Reason = "未请求宿主工具清单"
		return seg
	}
	if t := hosttools.SummaryText(); t != "" {
		seg.Present = true
		seg.Text = "\n\n【本机可用工具】\n" + t
		return seg
	}
	seg.Reason = "宿主未安装额外工具"
	return seg
}
