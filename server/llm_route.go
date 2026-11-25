package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chu0119/pojun/db"
)

// Read-only LLM route projection. Two independent layers are shown side by side
// and never conflated:
//
//  1. Task chain (explicit user order, quota-only advancement, DB cursor is the
//     only authority).
//  2. Provider pool (is_default / priority DESC / id ASC, circuit breaker, used
//     only by the global path and — when enabled — bound-profile fallback).
//
// The view never builds a provider, never selects from the pool, and never reads
// or echoes a key/base_url/proxy. Static availability is "configuration is
// complete", not "the endpoint works".
//
// A task-scoped view additionally reports last_used: the newest llm_usage row for
// the lane that actually serves the agent. That ledger is the only source of
// "what was really used" — it covers one task's planner/worker/mainagent lanes
// only, and it stores profile_name, never effort. The same profile can run at a
// different effort without any profile_name change, so effort CANNOT be
// recovered from it; see lastUsedNote.

type llmRouteProfile struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	Format             string `json:"format"`
	Model              string `json:"model"`
	Priority           int    `json:"priority"`
	PoolExclude        bool   `json:"pool_exclude"`
	HasKey             bool   `json:"has_key"`
	Availability       string `json:"availability"`
	AvailabilityReason string `json:"availability_reason,omitempty"`
	Effort             string `json:"effort,omitempty"`
	EffortSource       string `json:"effort_source,omitempty"`
}

// llmRouteLastUsed is the newest llm_usage row for the lane that actually serves
// this agent on the requested task. It is observed history, not configuration:
// the fields are what the ledger recorded, not what the chain is set to.
type llmRouteLastUsed struct {
	Worker      string `json:"worker"`
	ProfileName string `json:"profile_name,omitempty"`
	Model       string `json:"model,omitempty"`
	Status      string `json:"status,omitempty"`
	At          string `json:"at,omitempty"`
}

type llmRouteLayer struct {
	Layer     string           `json:"layer"` // pin | agent_binding | task_chain | global_profile | environment
	AppliesTo string           `json:"applies_to,omitempty"`
	Hit       bool             `json:"hit"`
	Profile   *llmRouteProfile `json:"profile,omitempty"`
	Reason    string           `json:"reason,omitempty"`
}

type llmRouteChainEntry struct {
	ProfileID   int64            `json:"profile_id"`
	Position    int              `json:"position"`
	Status      string           `json:"status"`
	IsCursor    bool             `json:"is_cursor"`
	LastError   string           `json:"last_error,omitempty"`
	ExhaustedAt *time.Time       `json:"exhausted_at,omitempty"`
	Profile     *llmRouteProfile `json:"profile,omitempty"`
}

type llmRoutePool struct {
	Enabled      bool                  `json:"enabled"`
	BindFallback bool                  `json:"bind_fallback"`
	Order        string                `json:"order"`
	Wraps        string                `json:"wraps"`
	Members      []LLMPoolMemberStatus `json:"members"`
	Note         string                `json:"note"`
}

type llmRoutePolicy struct {
	Trigger           string `json:"trigger"`
	DoesNotTrigger    string `json:"does_not_trigger"`
	AdvanceRule       string `json:"advance_rule"`
	SameProviderRetry string `json:"same_provider_retry"`
	PoolSelection     string `json:"pool_selection"`
}

type llmRouteTransition struct {
	ID        int64            `json:"id"`
	TaskID    string           `json:"task_id"`
	CreatedAt string           `json:"created_at"`
	Kind      string           `json:"kind"`
	Mode      string           `json:"mode"`
	Reason    string           `json:"reason,omitempty"`
	Summary   string           `json:"summary,omitempty"`
	IsError   bool             `json:"is_error"`
	Previous  *llmAuditProfile `json:"previous,omitempty"`
	Next      *llmAuditProfile `json:"next,omitempty"`
}

type llmRouteHistory struct {
	Scope          string               `json:"scope"`
	State          string               `json:"state"` // ok | no_records | error
	Error          string               `json:"error,omitempty"`
	Items          []llmRouteTransition `json:"items"`
	HasMore        bool                 `json:"has_more"`
	EarliestCursor int64                `json:"earliest_cursor,omitempty"`
	SnapshotCursor int64                `json:"snapshot_cursor,omitempty"`
	RetentionNote  string               `json:"retention_note,omitempty"`
}

type llmRouteRole struct {
	AgentKey     string `json:"agent_key"`
	Source       string `json:"source"`
	Available    bool   `json:"available"`
	Availability string `json:"availability,omitempty"`
	Reason       string `json:"reason,omitempty"`
	ProfileID    *int64 `json:"profile_id,omitempty"`
	Name         string `json:"name,omitempty"`
	Format       string `json:"format,omitempty"`
	Model        string `json:"model,omitempty"`
	Effort       string `json:"effort,omitempty"`
	EffortSource string `json:"effort_source,omitempty"`
}

type llmRouteAgent struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Binding *int64 `json:"binding"`
}

type llmRouteTaskRef struct {
	ID             string `json:"id"`
	Name           string `json:"name,omitempty"`
	Status         string `json:"status,omitempty"`
	CreatedAt      int64  `json:"created_at,omitempty"`
	FailoverState  string `json:"failover_state,omitempty"`
	FailoverReason string `json:"failover_reason,omitempty"`
}

type agentLLMRoute struct {
	Scope          string                  `json:"scope"` // task | static
	Agent          llmRouteAgent           `json:"agent"`
	Task           *llmRouteTaskRef        `json:"task,omitempty"`
	Precedence     []llmRouteLayer         `json:"precedence"`
	Roles          map[string]llmRouteRole `json:"roles"`
	Chain          []llmRouteChainEntry    `json:"chain"`
	ActiveProfile  *int64                  `json:"active_profile_id,omitempty"`
	LastUsed       *llmRouteLastUsed       `json:"last_used,omitempty"`
	FailoverState  string                  `json:"failover_state,omitempty"`
	FailoverReason string                  `json:"failover_reason,omitempty"`
	Pool           llmRoutePool            `json:"pool"`
	Policy         llmRoutePolicy          `json:"failover_policy"`
	History        llmRouteHistory         `json:"history"`
	HistoryNote    string                  `json:"history_note,omitempty"`
	Tasks          []llmRouteTaskRef       `json:"tasks"`
	ObservedAt     string                  `json:"observed_at"`
	Warnings       []string                `json:"warnings"`
}

// lastUsedNote is the human-readable boundary of the last_used projection. It
// must not claim more than the ledger proves: three lanes, no effort dimension.
const lastUsedNote = "最近一次实际使用的配置来自 llm_usage 记账，仅覆盖该任务的 planner/worker/mainagent 三条泳道；同一配置不同 effort 不会体现在 profile_name 上，因此 effort 无法从该表恢复。"

func (s *Server) pgAgentLLMRoute(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeErr(w, 400, "limit 必须为正整数")
			return
		}
		if n > 200 {
			n = 200
		}
		limit = n
	}
	var before int64
	if raw := strings.TrimSpace(r.URL.Query().Get("before")); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 0 {
			writeErr(w, 400, "before 必须为非负整数")
			return
		}
		before = n
	}

	var task *Task
	if raw := strings.TrimSpace(r.URL.Query().Get("task_id")); raw != "" {
		id, err := parseTaskID(raw)
		if err != nil {
			writeErr(w, 400, "task_id 必须为正整数")
			return
		}
		t, found := s.m.Task(strconv.FormatInt(id, 10))
		if !found || t == nil {
			// Archived tasks are removed from the manager, so their activity and
			// cursor are gone; 404 is the honest answer rather than an empty view.
			writeErr(w, 404, "任务不存在或已归档")
			return
		}
		task = t
	}

	route, err := s.buildAgentLLMRoute(pg, a, task, limit, before)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, route)
}

func (s *Server) buildAgentLLMRoute(pg *db.DB, a *db.Agent, task *Task, limit int, before int64) (*agentLLMRoute, error) {
	warnings := []string{}
	profiles, err := pg.ListProfiles()
	if err != nil {
		return nil, err
	}
	presence, err := pg.ProfileKeyPresence()
	if err != nil {
		warnings = append(warnings, "读取 LLM 配置的密钥状态失败，可用性可能不准确")
		presence = map[int64]bool{}
	}
	byID := make(map[int64]*db.LLMProfile, len(profiles))
	var globalID *int64
	for _, p := range profiles {
		byID[p.ID] = p
		if p.IsDefault {
			id := p.ID
			globalID = &id
		}
	}
	overrides, err := pg.EffortOverrides(a.Key)
	if err != nil {
		warnings = append(warnings, "读取该 Agent 的 effort 覆盖失败，展示的是配置自身强度")
		overrides = map[int64]db.AgentProfileEffort{}
	}
	overrideVals := make(map[int64]string, len(overrides))
	for id, ov := range overrides {
		overrideVals[id] = ov.ReasoningEffort
	}

	profileView := func(agentKey string, p *db.LLMProfile) *llmRouteProfile {
		if p == nil {
			return nil
		}
		avail, reason := staticProfileAvailability(p, presence[p.ID])
		effort, source := ResolveEffort(agentKey, p, overrideVals)
		return &llmRouteProfile{
			ID: p.ID, Name: p.Name, Format: p.Format, Model: p.Model,
			Priority: p.Priority, PoolExclude: p.PoolExclude, HasKey: presence[p.ID],
			Availability: avail, AvailabilityReason: reason,
			Effort: effort, EffortSource: string(source),
		}
	}

	// Task chain + cursor (same statement → one snapshot).
	var chain []llmRouteChainEntry
	var activeID *int64
	var lastUsed *llmRouteLastUsed
	failoverState, failoverReason := "default", ""
	if task != nil {
		taskNum, _ := parseTaskID(task.ID)
		legacy, active, _, rows, err := pg.TaskLLMContext(taskNum)
		_ = legacy
		if err != nil {
			warnings = append(warnings, "读取任务 LLM 配置链失败")
		}
		activeID = active
		for _, row := range rows {
			chain = append(chain, llmRouteChainEntry{
				ProfileID: row.ProfileID, Position: row.Position, Status: row.Status,
				IsCursor:  active != nil && row.ProfileID == *active && row.Status == "ready",
				LastError: sanitizeRouteError(row.LastError), ExhaustedAt: row.ExhaustedAt,
				Profile: profileView(a.Key, byID[row.ProfileID]),
			})
		}
		if len(chain) == 0 {
			chain = []llmRouteChainEntry{}
		}
		// Derive state from the same DB snapshot the chain came from (not the
		// in-memory mirror), so the two halves of this response cannot disagree.
		if len(chain) > 0 {
			var latest *llmRouteChainEntry
			activeReady := false
			for i := range chain {
				if activeID != nil && chain[i].ProfileID == *activeID && chain[i].Status == "ready" {
					activeReady = true
				}
				if chain[i].ExhaustedAt != nil && (latest == nil || chain[i].ExhaustedAt.After(*latest.ExhaustedAt)) {
					latest = &chain[i]
				}
			}
			if !activeReady {
				failoverState = "chain_exhausted"
			} else {
				failoverState = "ready"
			}
			if latest != nil {
				failoverReason = sanitizeRouteError(latest.LastError)
			}
		}
		// Last actual use, read from the always-on llm_usage ledger (separate
		// statement from the chain snapshot above). Only the three task lanes are
		// queried: the ledger has no other lane for this task, and it records
		// profile_name/model — never effort.
		usage, usageErr := pg.LatestTaskUsageProfile(task.ID, []string{"planner", "worker", "mainagent"})
		if usageErr != nil {
			// Do not swallow: silently reporting "no calls" is a different fact
			// from "the read failed".
			warnings = append(warnings, "读取最近一次实际使用记录失败")
			lastUsed = nil
		} else {
			var pick *db.TaskUsageProfile
			if p, ok := usage[a.Key]; ok {
				// The agent's own lane, when it is one of the three.
				pick = &p
			} else if a.Key != "planner" && a.Key != "worker" && a.Key != "mainagent" {
				// Custom / non-lane agents have no row of their own; their calls
				// run on the worker lane.
				if p, ok := usage["worker"]; ok {
					pick = &p
				}
			} else if p, ok := usage["planner"]; ok {
				// Lane agent without its own row: planner is the lane whose
				// calls dominate non-intent traffic.
				pick = &p
			}
			if pick != nil {
				lastUsed = &llmRouteLastUsed{
					Worker:      pick.Worker,
					ProfileName: pick.ProfileName,
					Model:       pick.Model,
					Status:      pick.Status,
					At:          pick.At.UTC().Format(time.RFC3339),
				}
			}
		}
	} else {
		chain = []llmRouteChainEntry{}
	}

	// Precedence, top to bottom.
	// A layer is "hit" only when the runtime could actually use it: the profile
	// exists AND its static availability is ok. A binding to a keyless or malformed
	// profile falls through at runtime, so claiming a hit would be a lie.
	hitOf := func(p *db.LLMProfile) bool {
		if p == nil {
			return false
		}
		avail, _ := staticProfileAvailability(p, presence[p.ID])
		return avail == "ok"
	}
	precedence := []llmRouteLayer{}
	if pin := s.m.CompactorProfileID(); pin > 0 {
		pp := byID[pin]
		precedence = append(precedence, llmRouteLayer{
			Layer: "pin", AppliesTo: "compactor", Hit: hitOf(pp), Profile: profileView("", pp),
			Reason: "仅钉住上下文压缩器；不影响该 Agent 的主调用，effort 按配置自身值",
		})
	} else {
		precedence = append(precedence, llmRouteLayer{Layer: "pin", AppliesTo: "compactor", Hit: false, Reason: "未设置压缩器专用配置"})
	}
	if a.LLMProfileID != nil {
		pp := byID[*a.LLMProfileID]
		reason := ""
		if !hitOf(pp) {
			reason = "绑定的配置当前不可用，运行时会回退到任务链/全局"
		}
		precedence = append(precedence, llmRouteLayer{Layer: "agent_binding", Hit: hitOf(pp), Profile: profileView(a.Key, pp), Reason: reason})
	} else {
		precedence = append(precedence, llmRouteLayer{Layer: "agent_binding", Hit: false, Reason: "该 Agent 未绑定 LLM 配置"})
	}
	if task != nil && len(chain) > 0 {
		pp := byID[ptrValue(activeID)]
		hit := activeID != nil && hitOf(pp)
		reason := ""
		if !hit {
			if activeID == nil {
				reason = "任务配置链已耗尽（游标为空）"
			} else {
				reason = "游标指向的配置当前不可用"
			}
		}
		precedence = append(precedence, llmRouteLayer{Layer: "task_chain", Hit: hit, Profile: profileView(a.Key, pp), Reason: reason})
	} else {
		precedence = append(precedence, llmRouteLayer{Layer: "task_chain", Hit: false, Reason: "任务未设置配置链"})
	}
	if globalID != nil {
		pp := byID[*globalID]
		reason := ""
		if !hitOf(pp) {
			reason = "激活配置当前不可用"
		}
		precedence = append(precedence, llmRouteLayer{Layer: "global_profile", Hit: hitOf(pp), Profile: profileView(a.Key, pp), Reason: reason})
	} else {
		precedence = append(precedence, llmRouteLayer{Layer: "global_profile", Hit: false, Reason: "没有已激活的 LLM 配置"})
	}
	envHit := globalID == nil && s.llmOn
	envReason := "存在已激活配置时不会用到"
	switch {
	case envHit:
		envReason = "使用环境变量/内置配置"
	case globalID == nil:
		envReason = "没有已激活配置，且未配置环境变量 LLM"
	}
	precedence = append(precedence, llmRouteLayer{Layer: "environment", Hit: envHit, Reason: envReason})

	// Per-role resolution (static only; no provider is built).
	roles := map[string]llmRouteRole{}
	for _, role := range []string{"mainagent", "planner", "worker", "goals", "chat"} {
		cl, aid := len(chain), activeID
		if role == "chat" {
			// 对话不跑在任务上：会话 pin → Agent 绑定 → 全局，不读任务配置链。
			cl, aid = 0, nil
		}
		roles[role] = s.staticRoleRoute(pg, role, cl, aid, byID, presence, globalID)
	}

	route := &agentLLMRoute{
		Scope:      "static",
		Agent:      llmRouteAgent{Key: a.Key, Name: a.Name, Kind: map[bool]string{true: "builtin", false: "custom"}[a.Builtin], Binding: a.LLMProfileID},
		Task:       taskRef(task),
		Precedence: precedence, Roles: roles, Chain: chain, ActiveProfile: activeID,
		LastUsed:      lastUsed,
		FailoverState: failoverState, FailoverReason: failoverReason,
		Pool: s.routePool(),
		Policy: llmRoutePolicy{
			Trigger:           "仅“额度耗尽”（provider 明确返回额度不足）推进任务配置链",
			DoesNotTrigger:    "429/401/403、网络错误、5xx 都不会推进任务链（同一条链上按重试策略处理）",
			AdvanceRule:       "推进到 position 更大且 status=ready 的最小 position；没有候选时清空游标并标记链耗尽",
			SameProviderRetry: "同 provider 重试与 thinking 降级只写日志，不写审计事件",
			PoolSelection:     "provider 池独立选路：激活配置优先，其后 priority DESC / id ASC；熔断冷却中的成员跳过。任务链不经过该池",
		},
		Tasks:      s.routeTaskRefs(20),
		ObservedAt: time.Now().UTC().Format(time.RFC3339),
		Warnings:   warnings,
	}
	route.History = s.routeHistory(task, limit, before)
	if task != nil {
		route.Scope = "task"
		route.HistoryNote = lastUsedNote
		// Static snapshot and history are separate reads; say so when the two
		// disagree rather than pretending they are one transaction.
		if len(route.History.Items) > 0 {
			// Items are newest-first: the cursor must agree with the LATEST event.
			// An exhausted event (Next==nil) expects an empty cursor; a non-empty
			// cursor after it is just as inconsistent as a wrong profile id.
			last := route.History.Items[0]
			mismatch := false
			if last.Next == nil {
				mismatch = route.ActiveProfile != nil
			} else {
				mismatch = route.ActiveProfile == nil || last.Next.ID != *route.ActiveProfile
			}
			if mismatch {
				route.Warnings = append(route.Warnings, "游标与最后一条转跳记录不一致：可能有并发修改，或随后编辑过任务配置链")
			}
		}
	}
	if route.History.State == "error" {
		route.Warnings = append(route.Warnings, "读取选路审计失败；静态层仍有效，运行时历史不可用")
	}
	return route, nil
}

func (s *Server) staticRoleRoute(pg *db.DB, role string, chainLen int, activeID *int64, byID map[int64]*db.LLMProfile, presence map[int64]bool, globalID *int64) llmRouteRole {
	out := llmRouteRole{AgentKey: role, Source: "none"}
	var p *db.LLMProfile
	if a, err := pg.GetAgentByKey(role); err == nil && a != nil && a.LLMProfileID != nil {
		out.Source, p = "agent_binding", byID[*a.LLMProfileID]
	} else if chainLen > 0 {
		if activeID == nil {
			out.Source, out.Available, out.Reason = "task_chain", false, "任务配置链已耗尽"
			return out
		}
		out.Source, p = "task_chain", byID[*activeID]
	} else if globalID != nil {
		out.Source, p = "global_profile", byID[*globalID]
	} else {
		out.Source, out.Available = "environment", s.llmOn
		if !s.llmOn {
			out.Reason = "未配置可用的全局 LLM"
		}
		return out
	}
	if p == nil {
		out.Reason = "解析到的 LLM 配置不存在"
		return out
	}
	avail, reason := staticProfileAvailability(p, presence[p.ID])
	out.ProfileID = &p.ID
	out.Name, out.Format, out.Model = p.Name, p.Format, p.Model
	out.Availability, out.Reason = avail, reason
	out.Available = avail == "ok"
	out.Effort, out.EffortSource = s.resolveEffort(role, p)
	return out
}

func staticProfileAvailability(p *db.LLMProfile, hasKey bool) (string, string) {
	if p == nil {
		return "invalid_config", "配置不存在"
	}
	if !hasKey {
		return "missing_key", "未设置 API Key，运行时无法使用"
	}
	if strings.TrimSpace(p.Model) == "" {
		return "invalid_config", "未填写模型"
	}
	switch p.Format {
	case "openai", "anthropic", "openai-responses":
	default:
		return "invalid_config", "未知的接口格式"
	}
	if p.BaseURL != "" {
		if u, err := url.Parse(p.BaseURL); err != nil || u.Scheme == "" || u.Host == "" {
			return "invalid_config", "Base URL 无法解析"
		}
	}
	return "ok", "配置完整（未验证连通性）"
}

func (s *Server) routePool() llmRoutePool {
	members := s.poolMemberStatuses()
	for i := range members {
		members[i].LastError = sanitizeRouteError(members[i].LastError)
	}
	enabled := s.m != nil && s.m.LLMPoolEnabled()
	bindFallback := s.m != nil && s.m.LLMPoolBindFallback()
	wraps := "global"
	if enabled && bindFallback {
		wraps = "global+binding"
	}
	return llmRoutePool{
		Enabled: enabled, BindFallback: bindFallback,
		Order: "激活配置优先，其后 priority DESC / id ASC；熔断冷却中的成员跳过",
		Wraps: wraps, Members: members,
		Note: "pool 只服务全局激活路径（bind_fallback 开启时也服务绑定路径）；任务配置链不经过它，priority/pool_exclude 不影响任务链顺序",
	}
}

func (s *Server) routeTaskRefs(limit int) []llmRouteTaskRef {
	if s.m == nil {
		return []llmRouteTaskRef{}
	}
	all := s.m.List()
	out := make([]llmRouteTaskRef, 0, limit)
	// Manager.List() puts pinned tasks first; the selector promises the newest
	// tasks, so pinned-but-old entries must not crowd out recent ones.
	for _, t := range all {
		if t == nil {
			continue
		}
		out = append(out, *taskRef(t))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func taskRef(t *Task) *llmRouteTaskRef {
	if t == nil {
		return nil
	}
	return &llmRouteTaskRef{
		ID: t.ID, Name: t.Name, Status: t.Status, CreatedAt: t.CreatedAt,
		FailoverState: t.LLMFailoverState, FailoverReason: sanitizeRouteError(t.LLMFailoverReason),
	}
}

func (s *Server) routeHistory(task *Task, limit int, before int64) llmRouteHistory {
	const note = "只记录额度耗尽导致的自动转跳、手动切换与链耗尽；同 provider 重试、thinking 降级、池内转移不写审计。没有记录不等于从未转跳。"
	if task != nil {
		return s.historyForTask(task, limit, before, note)
	}
	return s.recentHistory(limit, before, note)
}

func (s *Server) historyForTask(t *Task, limit int, before int64, note string) llmRouteHistory {
	out := llmRouteHistory{Scope: "task", State: "no_records", Items: []llmRouteTransition{}, RetentionNote: note}
	if t.Store == nil {
		out.State, out.Error = "error", "任务没有可用的探索存储"
		return out
	}
	maxID, err := t.Store.ActivityMaxID()
	if err != nil {
		out.State, out.Error = "error", sanitizeRouteError(err.Error())
		return out
	}
	out.SnapshotCursor = maxID
	items, hasMore, err := t.Store.ActivityPage(db.ActivitySessionFilter{Kinds: []string{"llm_switch", "llm_failover"}}, before, limit)
	if err != nil {
		out.State, out.Error = "error", sanitizeRouteError(err.Error())
		return out
	}
	out.Items = transitionsFrom(items, t.ID)
	// ActivityPage returns ascending id order; the view (and the cross-task
	// branch below) presents newest first so "load earlier" prepends older rows.
	for i, j := 0, len(out.Items)-1; i < j; i, j = i+1, j-1 {
		out.Items[i], out.Items[j] = out.Items[j], out.Items[i]
	}
	out.HasMore = hasMore
	if len(out.Items) > 0 {
		out.EarliestCursor = out.Items[len(out.Items)-1].ID
		out.State = "ok"
	}
	return out
}

// recentHistory merges the same two event kinds across the most recent live
// tasks. It is explicitly a cross-task view of task-level transitions — there is
// no process-wide pool-transition audit to show.
//
// has_more is reported as a LOWER BOUND: each task contributes its own newest
// page, so when any task still holds an older page an older global event may
// exist. Saying true (when any task had more) is honest; the cursor to fetch it
// is the merged newest-first list's oldest id, which the client passes back.
func (s *Server) recentHistory(limit int, before int64, note string) llmRouteHistory {
	out := llmRouteHistory{Scope: "recent_tasks", State: "no_records", Items: []llmRouteTransition{}, RetentionNote: note}
	tasks := s.m.List()
	// Manager.List() puts pinned tasks first; trim by creation time so an old
	// pinned task cannot crowd out a newer one (same rule as routeTaskRefs).
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].CreatedAt > tasks[j].CreatedAt })
	if len(tasks) > 20 {
		tasks = tasks[:20]
	}
	all := []llmRouteTransition{}
	anyMore := false
	for _, t := range tasks {
		if t.Store == nil {
			continue
		}
		items, more, err := t.Store.ActivityPage(db.ActivitySessionFilter{Kinds: []string{"llm_switch", "llm_failover"}}, before, limit)
		if err != nil {
			out.State, out.Error = "error", sanitizeRouteError(err.Error())
			return out
		}
		anyMore = anyMore || more
		all = append(all, transitionsFrom(items, t.ID)...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID > all[j].ID })
	if len(all) > limit {
		all = all[:limit]
	}
	out.Items = all
	out.HasMore = anyMore
	// Cross-task merging is approximate: each task contributes its own newest
	// page. has_more=true there means "some task has older rows", not "the exact
	// next page is one click away".
	out.RetentionNote = note + " 跨任务合并为近似视图：每个任务只取自己的最近一页；“加载更早”会以当前最旧事件为游标重新取一轮，不保证严格连续。"
	if len(all) > 0 {
		out.EarliestCursor = all[len(all)-1].ID
		out.State = "ok"
	}
	return out
}

func transitionsFrom(items []db.Activity, taskID string) []llmRouteTransition {
	out := make([]llmRouteTransition, 0, len(items))
	for _, a := range items {
		var meta llmActivityMetadata
		_ = json.Unmarshal(a.Metadata, &meta)
		tr := meta.LLMTransition
		out = append(out, llmRouteTransition{
			ID: a.ID, TaskID: taskID, CreatedAt: a.CreatedAt.UTC().Format(time.RFC3339),
			Kind: a.Kind, Mode: tr.Mode, Reason: sanitizeRouteError(tr.Reason), Summary: sanitizeRouteError(a.Summary),
			IsError: a.IsError || a.Kind == "llm_failover", Previous: tr.Previous, Next: tr.Next,
		})
	}
	return out
}

func ptrValue(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// sanitizeRouteError truncates and masks the shapes that commonly carry secrets
// in provider error strings. Matching is done case-insensitively on the ORIGINAL
// text (a lowercased copy's indices do not map back once Unicode case folding
// changes byte length).
func sanitizeRouteError(in string) string {
	if in == "" {
		return ""
	}
	out := in
	for _, rule := range routeSecretPatterns {
		out = rule.re.ReplaceAllString(out, rule.repl)
	}
	if r := []rune(out); len(r) > 500 {
		out = string(r[:500]) + "…"
	}
	return out
}

var routeSecretPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// Ordered rules; every one must handle the quoted-key form too: provider
	// error bodies are usually JSON ({"api_key":"..."}), so a rule that only
	// accepts bare `key=value` leaves the secret in place.
	{regexp.MustCompile(`(?i)bearer\s+["']?[^\s"',;}]+`), "Bearer ***"},
	{regexp.MustCompile(`(?i)basic\s+[A-Za-z0-9+/=]{8,}`), "Basic ***"},
	{regexp.MustCompile(`(?i)sk-[A-Za-z0-9_-]+`), "sk-***"},
	{regexp.MustCompile(`(?i)(api[_-]?key|apikey|access[_-]?token|auth[_-]?token|token|secret|password|passwd)["']?\s*[=:]\s*["']?[^\s&"',;}]+`), "$1=***"},
	{regexp.MustCompile(`(?i)(api[_-]?key|apikey|access[_-]?token|auth[_-]?token|token|secret|password|passwd)%3d[^&\s"']*`), "$1%3D***"},
	{regexp.MustCompile(`://[^/@\s]+@`), "://***@"},
}
