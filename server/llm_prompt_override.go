package server

import (
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/chu0119/pojun/db"
)

// Per-(agent, LLM profile) independent-prompt overrides.
//
// The profile's default prompt stays on llm_profiles.prompt / prompt_mode /
// prompt_apply_inherited and is edited from the LLM configuration page. These
// endpoints manage only the agent-scoped layer:
//
//   - no row     → inherit the profile default
//   - empty body → explicit "do not inject" for this agent
//   - non-empty  → override the profile default for this agent
//
// An override is never gated by prompt_apply_inherited: that switch belongs to
// the default layer. Deleting the row restores inheritance; PUT with an empty
// body is the "disabled" state, not a delete.

type agentPromptOverrideDTO struct {
	ProfileID  int64  `json:"profile_id"`
	Prompt     string `json:"prompt"`
	PromptMode string `json:"prompt_mode"`
	UpdatedAt  string `json:"updated_at,omitempty"`
}

type agentPromptOverridesDTO struct {
	AgentKey  string                   `json:"agent_key"`
	Overrides []agentPromptOverrideDTO `json:"overrides"`
}

// pgAgentPromptOverrides lists this agent's override rows (only rows that
// exist; "inherit default" is the absence of a row).
func (s *Server) pgAgentPromptOverrides(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	rows, err := pg.AgentProfilePrompts(a.Key)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	ids := make([]int64, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := agentPromptOverridesDTO{AgentKey: a.Key, Overrides: []agentPromptOverrideDTO{}}
	for _, id := range ids {
		row := rows[id]
		out.Overrides = append(out.Overrides, agentPromptOverrideDTO{
			ProfileID: row.ProfileID, Prompt: row.Text, PromptMode: row.Mode,
			UpdatedAt: row.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, 200, out)
}

// pgSaveAgentPromptOverride sets one override at
// PUT /api/agents/{key}/llm-prompt-overrides/{profile_id}.
// Body: {"prompt": "...", "prompt_mode": "prepend|append|replace|"}. An empty
// prompt with an empty mode stores the explicit "do not inject" state; use
// DELETE to restore inheritance. Success is 204.
func (s *Server) pgSaveAgentPromptOverride(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	id, err := parsePositivePathID(r, "profile_id")
	if err != nil {
		writeErr(w, 400, "profile_id 必须为正整数")
		return
	}
	var req struct {
		Prompt     *string `json:"prompt"`
		PromptMode string  `json:"prompt_mode"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.Prompt == nil {
		writeErr(w, 400, "缺少 prompt（空字符串表示本 Agent 不注入；恢复继承请用 DELETE）")
		return
	}
	text, mode, msg := normalizeAgentPromptRequest(*req.Prompt, req.PromptMode)
	if msg != "" {
		writeErr(w, 400, msg)
		return
	}
	if p, err := pg.ProfileByID(id); err != nil || p == nil {
		writeErr(w, 404, "LLM 配置不存在")
		return
	}
	if err := pg.SetAgentProfilePrompt(a.Key, id, text, mode); err != nil {
		if errors.Is(err, db.ErrAgentNotFound) {
			writeErr(w, 404, "agent not found")
			return
		}
		if errors.Is(err, db.ErrLLMProfileNotFound) {
			writeErr(w, 404, "LLM 配置不存在")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	s.invalidateProfileAgents()
	w.WriteHeader(http.StatusNoContent)
}

// pgDeleteAgentPromptOverride removes one override (restoring the profile
// default). Idempotent: 204 even when no row existed.
func (s *Server) pgDeleteAgentPromptOverride(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	id, err := parsePositivePathID(r, "profile_id")
	if err != nil {
		writeErr(w, 400, "profile_id 必须为正整数")
		return
	}
	// 不做「配置是否存在」的前置校验：删除配置会级联清掉覆盖行，重试 DELETE 时
	// 配置已不存在，但终态正是调用方要的——按幂等处理（204），而不是报 404。
	if _, err := pg.DeleteAgentProfilePrompt(a.Key, id); err != nil {
		if errors.Is(err, db.ErrAgentNotFound) {
			writeErr(w, 404, "agent not found")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	s.invalidateProfileAgents()
	w.WriteHeader(http.StatusNoContent)
}
