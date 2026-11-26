package server

import (
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Autumn-27/norma/skill"
	"github.com/chu0119/pojun/agent"
	"github.com/chu0119/pojun/db"
	"github.com/chu0119/pojun/traffic"
)

// The read-only prompt assembly view. It deliberately projects configuration +
// cached MCP metadata only: no provider is built, no MCP server is dialed, and
// runtime-only segments are listed by name/gate with no fabricated body.

// errInvalidPromptProfileID is returned for a malformed ?profile_id= value, so
// the handler can answer 400 instead of a server error.
var errInvalidPromptProfileID = errors.New("profile_id 格式错误")

type promptChainSegment struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	Kind        string `json:"kind"`
	Source      string `json:"source"`
	SourceLabel string `json:"source_label"`
	Editable    bool   `json:"editable"`
	Gate        string `json:"gate,omitempty"`
	Present     bool   `json:"present"`
	Available   bool   `json:"available"`
	Reason      string `json:"reason,omitempty"`
	Order       int    `json:"order"`
	Chars       int    `json:"chars"`
	Text        string `json:"text"`
}

type promptChainLLM struct {
	// ProfileID is the resolved profile (preview/binding/global), even when its
	// prompt is inactive; PromptActive reports the overlay separately.
	ProfileID            *int64 `json:"profile_id"`
	Name                 string `json:"name"`
	Model                string `json:"model"`
	Format               string `json:"format"`
	Source               string `json:"source"`
	SourceLabel          string `json:"source_label"`
	PromptMode           string `json:"prompt_mode"`
	PromptChars          int    `json:"prompt_chars"`
	PromptApplyInherited bool   `json:"prompt_apply_inherited"`
	PromptActive         bool   `json:"prompt_active"`
	// Effort is the reasoning effort this agent would send on this profile
	// (override → profile → unset), from the same resolver the runtime uses. The
	// chain view must not invent a second rule.
	Effort       string `json:"effort,omitempty"`
	EffortSource string `json:"effort_source,omitempty"`
}

type promptChainTool struct {
	Key         string `json:"key"`
	Description string `json:"description"`
	Deferred    bool   `json:"deferred"`
	Gate        string `json:"gate,omitempty"`
}

type promptChainExcluded struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

type promptChainTools struct {
	Builtin       []promptChainTool     `json:"builtin"`
	Custom        []promptChainTool     `json:"custom"`
	Traffic       []promptChainTool     `json:"traffic"`
	Shell         []promptChainTool     `json:"shell"`
	DeferredNames []string              `json:"deferred_names"`
	Excluded      []promptChainExcluded `json:"excluded"`
	Total         int                   `json:"total"`
}

type promptChainMCP struct {
	ID          int64        `json:"id"`
	Name        string       `json:"name"`
	Transport   string       `json:"transport"`
	Enabled     bool         `json:"enabled"`
	Mode        string       `json:"mode"`  // global | skill_gated | excluded
	Skill       string       `json:"skill"` // skill that unlocks it, when gated
	Health      string       `json:"health"`
	LastCheckAt string       `json:"last_check_at,omitempty"`
	LastError   string       `json:"last_error,omitempty"`
	ToolCount   int          `json:"tool_count"`
	CacheStale  bool         `json:"cache_stale"`
	Tools       []db.MCPTool `json:"tools"`
}

type promptChainSkill struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	InIndex     bool     `json:"in_index"`
	MCPGated    []string `json:"mcp_gated"`
}

type promptChainTotals struct {
	Segments  int `json:"segments"`
	Chars     int `json:"chars"`
	EstTokens int `json:"est_tokens"`
}

type agentPromptChain struct {
	AgentKey   string               `json:"agent_key"`
	AgentKind  string               `json:"agent_kind"`
	LLM        promptChainLLM       `json:"llm"`
	Segments   []promptChainSegment `json:"segments"`
	Tools      promptChainTools     `json:"tools"`
	MCP        []promptChainMCP     `json:"mcp"`
	Skills     []promptChainSkill   `json:"skills"`
	Totals     promptChainTotals    `json:"totals"`
	Warnings   []string             `json:"warnings"`
	StaticOnly bool                 `json:"static_only"`
	// 独立提示词上限。与 LLM 配置列表同源，页面据此渲染计数器与预校验。
	PromptMaxRunes int `json:"prompt_max_runes"`
}

func (s *Server) pgAgentPromptChain(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	chain, err := s.buildAgentPromptChain(pg, a, r.URL.Query().Get("profile_id"))
	if err != nil {
		if errors.Is(err, errInvalidPromptProfileID) {
			writeErr(w, 400, err.Error())
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, chain)
}

func (s *Server) buildAgentPromptChain(pg *db.DB, a *db.Agent, profileParam string) (*agentPromptChain, error) {
	warnings := []string{}
	var previewID *int64
	if strings.TrimSpace(profileParam) != "" {
		id, err := strconv.ParseInt(strings.TrimSpace(profileParam), 10, 64)
		if err != nil || id <= 0 {
			return nil, errInvalidPromptProfileID
		}
		previewID = &id
	}

	// A failed prompt read must not look like "no prompt configured": keep the
	// view usable but say the overlay state is untrustworthy.
	readPrompt := func(id int64) db.PromptInfo {
		pi, err := pg.ProfilePrompt(id)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("读取 LLM 配置 #%d 的独立提示词失败，相关状态不可信", id))
			return db.PromptInfo{}
		}
		return pi
	}

	// Resolve the profile the configuration page can honestly show: an explicit
	// preview override, the agent's binding, or the global active profile. Task
	// chain selection only exists at run time and is reported as such.
	var (
		profile   *db.LLMProfile
		stored    db.PromptInfo
		active    *int64
		profileID *int64
		source    = "none"
	)
	switch {
	case previewID != nil:
		p, err := pg.ProfileByID(*previewID)
		if err != nil || p == nil {
			source = "preview_override"
			warnings = append(warnings, "预览指定的 LLM 配置不存在")
			break
		}
		profile, source, profileID = p, "preview_override", previewID
		stored = readPrompt(p.ID)
		if stored.Text != "" && validProfilePromptMode(stored.Mode) {
			active = previewID
		}
	case a.LLMProfileID != nil:
		p, err := pg.ProfileByID(*a.LLMProfileID)
		if err != nil || p == nil {
			source = "agent_binding"
			warnings = append(warnings, "Agent 绑定的 LLM 配置不存在")
			break
		}
		profile, source, profileID = p, "agent_binding", a.LLMProfileID
		stored = readPrompt(p.ID)
		if stored.Text != "" && validProfilePromptMode(stored.Mode) {
			active = a.LLMProfileID
		}
	default:
		if p, err := pg.ActiveProfile(); err == nil && p != nil {
			profile, profileID = p, &p.ID
			stored = readPrompt(p.ID)
			// A global profile without an API key cannot serve a run, so
			// inheritedProfileID() will not load its prompt either. Report it as
			// inactive instead of claiming an overlay the runtime never applies.
			if p.APIKey != "" && stored.ApplyInherited && stored.Text != "" && validProfilePromptMode(stored.Mode) {
				active = &p.ID
				source = "global_profile"
			} else {
				source = "global_profile_inactive"
			}
		}
	}

	pp := agent.ProfilePrompt{}
	if active != nil {
		pp = profileOverlayFromInfo(*active, stored, source)
	}

	llm := promptChainLLM{
		ProfileID:            profileID,
		Source:               source,
		SourceLabel:          promptChainSourceLabel(source),
		PromptMode:           stored.Mode,
		PromptChars:          utf8.RuneCountInString(stored.Text),
		PromptApplyInherited: stored.ApplyInherited,
		PromptActive:         pp.Active(),
	}
	if profile != nil {
		llm.Name, llm.Model, llm.Format = profile.Name, profile.Model, profile.Format
		llm.Effort, llm.EffortSource = s.resolveEffort(a.Key, profile)
	}

	def := agent.DefaultPromptFor(a.Key)
	segments, renderWarning := agent.PreviewSystemSegments(a.Key, def, pp)
	if renderWarning != "" {
		warnings = append(warnings, renderWarning)
	}
	if !pp.Active() {
		synthetic := agent.PromptSegment{
			Key: "profile_prompt", Title: "独立提示词（LLM 配置）", Source: "profile_prompt",
			Available: true, Present: false, Reason: promptInactiveReason(profile, stored, source, previewID),
		}
		// Keep the configured text in the read-only projection so the detail can
		// show what the user wrote. It stays out of totals: Present is false.
		if stored.Text != "" {
			synthetic.Text = stored.Text
		}
		segments = append([]agent.PromptSegment{synthetic}, segments...)
	}

	// Body source label (DB template version vs built-in fallback).
	cur, _ := pg.CurrentPrompt(a.ID)
	bodyLabel := "内置默认正文"
	if cur != "" {
		bodyLabel = "DB 模板正文"
		if vers, err := pg.ListPromptVersions(a.ID); err == nil && len(vers) > 0 {
			bodyLabel = fmt.Sprintf("DB 模板正文 v%d", vers[0].Version)
		}
	}
	if renderWarning != "" {
		bodyLabel = "内置默认正文（渲染回退）"
	}

	workDir := "<任务工作目录>"
	runDir := "<本次意图工作目录>"
	if a.Role == "assistant" {
		workDir, runDir = "<会话工作目录>", "<会话工作目录>"
	}
	// The static view must not advance proxy-pool selection or echo credentials.
	proxyAddr := ""
	if a.Key == "worker" {
		proxyAddr = s.m.ProxyDisplayAddr()
	}
	// Build the tails through the SAME constructors the runtime builders use, so
	// this projection cannot drift from the live assembly on which fields an
	// agent's tails interpolate. Inputs are the preview's stand-ins (placeholder
	// dirs, display-only proxy); that approximation is the irreducible part.
	var tailOpts agent.TailOptions
	switch a.Key {
	case "planner":
		tailOpts = agent.PlannerTailOptions(workDir)
	case "worker":
		caCert := ""
		if s.m.TrafficEnabled() {
			caCert = "<ca.pem>"
		}
		tailOpts = agent.WorkerTailOptions(proxyAddr, caCert, runDir)
	case "mainagent":
		tailOpts = agent.MainAgentTailOptions(workDir)
	case "goals":
		// The static view has no task context here, so the scope tail stays gated.
		tailOpts = agent.GoalsTailOptions(false)
	default:
		tailOpts = agent.ChatTailOptions(workDir)
	}
	segments = append(segments, agent.StaticTailSegments(a.Key, tailOpts)...)
	if proxyAddr != "" {
		warnings = append(warnings, "静态视图中的出口代理地址可能已脱敏或为运行时占位；实际出口以运行为准")
	}
	segments = append(segments, promptChainRuntimeSegments(a.Key)...)
	if a.Key != "goals" {
		segments = append(segments, promptChainTailRuntimeSegments()...)
	}

	out := make([]promptChainSegment, 0, len(segments))
	totalChars := 0
	for i, seg := range segments {
		label := promptChainSegmentLabel(seg, pp, bodyLabel)
		chars := utf8.RuneCountInString(seg.Text)
		if seg.OriginalChars > 0 {
			chars = seg.OriginalChars
		} else if strings.HasPrefix(seg.Text, "\n\n") {
			chars = utf8.RuneCountInString(strings.TrimPrefix(seg.Text, "\n\n"))
		}
		if seg.Present && seg.Available {
			totalChars += chars
		}
		out = append(out, promptChainSegment{
			Key: seg.Key, Title: seg.Title, Kind: promptChainKind(seg),
			Source: seg.Source, SourceLabel: label, Editable: seg.Editable,
			Gate: seg.Gate, Present: seg.Present, Available: seg.Available,
			Reason: seg.Reason, Order: i + 1, Chars: chars, Text: seg.Text,
		})
	}

	tools, err := s.promptChainTools(pg, a)
	if err != nil {
		return nil, err
	}
	mcp, err := s.promptChainMCP(pg, a)
	if err != nil {
		return nil, err
	}
	skills := s.promptChainSkills(pg, a)

	if a.LLMProfileID == nil {
		warnings = append(warnings, "该 Agent 未绑定 LLM 配置：运行时按任务链/全局解析；只有开启「继承时也生效」的配置会注入独立提示词")
	}
	if profile != nil && profile.APIKey == "" {
		warnings = append(warnings, "该 LLM 配置未设置 API Key，运行时可能回落到其他配置")
	}
	if a.LLMProfileID != nil && s.m.LLMPoolBindFallback() {
		warnings = append(warnings, "故障转移兜底已开启：提示词跟随绑定配置，实际服务方可能切到其他配置（提示词不会跟着切换）")
	}
	if !s.m.TrafficEnabled() {
		if len(tools.Traffic) > 0 {
			warnings = append(warnings, "流量捕获未开启：traffic_* 工具不会出现在运行时")
		}
	}
	for _, m := range mcp {
		if m.Health == "never_checked" && m.Mode != "excluded" {
			warnings = append(warnings, fmt.Sprintf("MCP「%s」尚未检查：工具清单来自缓存，运行时可能为空", m.Name))
		}
	}

	return &agentPromptChain{
		AgentKey: a.Key, AgentKind: map[bool]string{true: "builtin", false: "custom"}[a.Builtin],
		LLM: llm, Segments: out, Tools: tools, MCP: mcp, Skills: skills,
		Totals: promptChainTotals{
			Segments: len(out), Chars: totalChars, EstTokens: (totalChars + 1) / 2,
		},
		Warnings: warnings, StaticOnly: true, PromptMaxRunes: maxProfilePromptRunes,
	}, nil
}

func promptChainSourceLabel(source string) string {
	switch source {
	case "agent_binding":
		return "Agent 显式绑定"
	case "global_profile":
		return "全局激活配置 · 继承生效"
	case "global_profile_inactive":
		return "全局激活配置 · 未生效"
	case "preview_override":
		return "预览指定配置"
	default:
		return "未绑定 · 运行时解析"
	}
}

func promptInactiveReason(profile *db.LLMProfile, stored db.PromptInfo, source string, previewID *int64) string {
	switch {
	case previewID != nil && profile == nil:
		return "预览指定的 LLM 配置不存在"
	case source == "agent_binding" && profile == nil:
		return "Agent 绑定的 LLM 配置不存在"
	case source == "global_profile_inactive":
		if profile != nil && profile.APIKey == "" {
			return "全局激活配置没有 API Key，运行时不会装载其独立提示词"
		}
		if stored.Text == "" {
			return "全局激活配置未设置独立提示词"
		}
		if !stored.ApplyInherited {
			return "全局激活配置设置了独立提示词，但未开启「继承时也生效」"
		}
		if !validProfilePromptMode(stored.Mode) {
			return "独立提示词模式无效，未注入"
		}
		return "全局激活配置的独立提示词未生效"
	case source == "none" && profile == nil:
		return "当前没有解析到 LLM 配置"
	case stored.Text == "":
		return "该 LLM 配置未设置独立提示词"
	case !validProfilePromptMode(stored.Mode):
		return "独立提示词模式无效，未注入"
	default:
		return "该 Agent 当前没有生效的独立提示词"
	}
}

func promptChainSegmentLabel(seg agent.PromptSegment, pp agent.ProfilePrompt, bodyLabel string) string {
	switch seg.Key {
	case "profile_prompt":
		switch pp.Source {
		case "agent_binding":
			return "LLM 配置 · 显式绑定"
		case "global_profile":
			return "LLM 配置 · 继承生效"
		case "preview_override":
			return "LLM 配置 · 预览指定"
		default:
			return "LLM 配置 · 未生效"
		}
	case "body":
		return bodyLabel
	case "skill_index":
		return "宿主注入 · Skill 绑定"
	}
	switch seg.Source {
	case "code":
		return "代码固定 · 只读"
	case "host":
		return "宿主注入"
	case "runtime":
		return "运行时注入"
	case "finding_guidance":
		return "按最终工具集派生"
	case "deferred":
		return "条件注入 · 第二 system 段"
	default:
		return seg.Source
	}
}

func promptChainKind(seg agent.PromptSegment) string {
	switch seg.Key {
	case "profile_prompt", "body", "skill_index":
		return seg.Key
	case "host_tools":
		return "host"
	case "finding_guidance", "deferred_tools":
		return seg.Key
	}
	if seg.Source == "code" {
		return "code_tail"
	}
	return seg.Key
}

func promptChainRuntimeSegments(agentKey string) []agent.PromptSegment {
	mk := func(key, title, gate string) agent.PromptSegment {
		return agent.PromptSegment{
			Key: key, Title: title, Source: "runtime", Gate: gate,
			Present: false, Available: false,
			Reason: "在任务运行时按当时事实注入；静态视图只展示注入点，不展开正文",
		}
	}
	switch agentKey {
	case "planner":
		return []agent.PromptSegment{
			mk("runtime_constraints", "任务操作约束", "任务配置了操作约束"),
			mk("runtime_program_profile", "Program Profile", "任务配置了 SRC/众测 profile"),
			mk("runtime_autopilot", "自主推进提示", "任务开启 autopilot"),
			mk("runtime_pacing", "派发节流提示", "任务有意图队列上限"),
		}
	case "worker":
		return []agent.PromptSegment{
			mk("runtime_constraints", "任务操作约束", "任务配置了操作约束"),
			mk("runtime_program_profile", "Program Profile", "任务配置了 SRC/众测 profile"),
			mk("runtime_task_auth", "任务登录凭据", "任务配置了登录凭据（正文永不在此展示）"),
			mk("runtime_artifact_guidance", "产物附件纪律", "每次 worker run 注入"),
			mk("runtime_sqlmap", "sqlmap 专项提示", "每次 worker run 注入"),
			mk("runtime_intent", "意图与资产快照", "每次 worker run 按领取的意图注入"),
			mk("runtime_facts", "既有事实", "图上已有相关 fact"),
			mk("runtime_lessons", "终态经验", "匹配到同资产/同维度的终态经验"),
			mk("runtime_autopilot", "自主推进提示", "任务开启 autopilot"),
			mk("runtime_launch", "启动指令", "每次 worker run 末尾追加"),
		}
	case "mainagent":
		return []agent.PromptSegment{
			mk("runtime_program_profile", "Program Profile", "任务配置了 SRC/众测 profile"),
		}
	case "goals":
		return nil
	default:
		return nil
	}
}

func promptChainTailRuntimeSegments() []agent.PromptSegment {
	return []agent.PromptSegment{
		{
			Key: "finding_guidance", Title: "证据纪律", Source: "finding_guidance",
			Gate: "由最终工具集决定", Present: false, Available: false,
			Reason: "有 report_finding 等工具时注入；属于运行时派生段",
		},
		{
			Key: "deferred_tools", Title: "延迟工具块", Source: "deferred",
			Gate: "存在全局可见 MCP 工具或延迟自定义工具", Present: false, Available: false,
			Reason: "有延迟工具时作为独立第二 system 段注入；静态视图不连接 MCP",
		},
	}
}

func (s *Server) promptChainTools(pg *db.DB, a *db.Agent) (promptChainTools, error) {
	rows, err := pg.ListTools()
	if err != nil {
		return promptChainTools{}, err
	}
	trafficKeys := map[string]bool{}
	for _, t := range traffic.SeedToolMetas() {
		trafficKeys[t.Name()] = true
	}
	out := promptChainTools{
		Builtin: []promptChainTool{}, Custom: []promptChainTool{}, Traffic: []promptChainTool{},
		Shell: []promptChainTool{}, DeferredNames: []string{}, Excluded: []promptChainExcluded{},
	}
	capture := s.m.TrafficEnabled()
	for _, row := range rows {
		if !row.Enabled {
			out.Excluded = append(out.Excluded, promptChainExcluded{Key: row.Key, Reason: "已停用"})
			continue
		}
		if !contains(row.Agents, a.Key) {
			out.Excluded = append(out.Excluded, promptChainExcluded{Key: row.Key, Reason: "该 Agent 未绑定"})
			continue
		}
		item := promptChainTool{Key: row.Key, Description: row.Description, Deferred: row.Deferred}
		switch {
		case trafficKeys[row.Key]:
			if !capture {
				item.Gate = "流量捕获未开启"
			}
			out.Traffic = append(out.Traffic, item)
		case row.Kind == "shell":
			item.Gate = "仅拼进 Bash 描述，不可直接调用"
			out.Shell = append(out.Shell, item)
		case row.Kind == "builtin" || row.Kind == "":
			out.Builtin = append(out.Builtin, item)
		default:
			out.Custom = append(out.Custom, item)
			if row.Deferred {
				out.DeferredNames = append(out.DeferredNames, row.Key)
			}
		}
	}
	out.Total = len(out.Builtin) + len(out.Custom) + len(out.Traffic) + len(out.Shell)
	return out, nil
}

func (s *Server) promptChainMCP(pg *db.DB, a *db.Agent) ([]promptChainMCP, error) {
	visibleIDs, _ := pg.AgentVisible(a.ID, "mcp")
	visible := map[int64]bool{}
	for _, id := range visibleIDs {
		visible[id] = true
	}
	viewSkills, _ := pg.AgentSkillNames(a.ID)
	byDir := map[string]skill.Skill{}
	if reg, err := skill.LoadDir(s.skillDir); err == nil && reg != nil {
		for _, sk := range reg.List() {
			if sk.Dir != "" {
				byDir[filepath.Base(sk.Dir)] = sk
			}
		}
	}
	gated := map[string]string{}
	for _, name := range viewSkills {
		sk, ok := byDir[name]
		if !ok {
			continue
		}
		for _, srv := range sk.MCPs {
			if _, exists := gated[srv]; !exists {
				gated[srv] = name
			}
		}
	}
	servers, err := pg.ListMCP()
	if err != nil {
		return nil, err
	}
	out := make([]promptChainMCP, 0, len(servers))
	for _, srv := range servers {
		mode := "excluded"
		if srv.Enabled {
			if _, ok := gated[srv.Name]; ok {
				mode = "skill_gated"
			} else if visible[srv.ID] {
				mode = "global"
			}
		}
		item := promptChainMCP{
			ID: srv.ID, Name: srv.Name, Transport: srv.Transport, Enabled: srv.Enabled,
			Mode: mode, Skill: gated[srv.Name], ToolCount: srv.ToolCount,
			Health: "never_checked", Tools: []db.MCPTool{},
		}
		if srv.LastCheckAt != nil {
			item.LastCheckAt = srv.LastCheckAt.UTC().Format(time.RFC3339)
			if srv.LastOK != nil && *srv.LastOK {
				item.Health = "ok"
			} else {
				item.Health = "failed"
			}
		}
		item.LastError = srv.LastError
		if mode != "excluded" {
			if tools, err := pg.MCPToolsDetailed(srv.ID); err == nil && tools != nil {
				item.Tools = tools
			}
			item.CacheStale = item.Health == "failed" && len(item.Tools) > 0
		}
		out = append(out, item)
	}
	return out, nil
}

func (s *Server) promptChainSkills(pg *db.DB, a *db.Agent) []promptChainSkill {
	names, _ := pg.AgentSkillNames(a.ID)
	out := make([]promptChainSkill, 0, len(names))
	if len(names) == 0 {
		return out
	}
	block := ""
	if agent.SkillIndexBlock != nil {
		block = agent.SkillIndexBlock(a.Key)
	}
	byDir := map[string]skill.Skill{}
	if reg, err := skill.LoadDir(s.skillDir); err == nil && reg != nil {
		for _, sk := range reg.List() {
			if sk.Dir != "" {
				byDir[filepath.Base(sk.Dir)] = sk
			}
		}
	}
	for _, name := range names {
		desc := ""
		mcps := []string{}
		if sk, ok := byDir[name]; ok {
			desc = strings.TrimSpace(sk.Description)
			if i := strings.IndexAny(desc, "。；;\n"); i > 0 {
				desc = desc[:i]
			}
			if r := []rune(desc); len(r) > 120 {
				desc = string(r[:120]) + "…"
			}
			if sk.MCPs != nil {
				mcps = sk.MCPs
			}
		}
		out = append(out, promptChainSkill{
			Name: name, Description: desc, MCPGated: mcps,
			InIndex: strings.Contains(block, "- "+name+"\n") || strings.Contains(block, "- "+name+"："),
		})
	}
	return out
}
