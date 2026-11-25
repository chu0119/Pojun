package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chu0119/pojun/agent"
	"github.com/chu0119/pojun/db"
)

// Per-(agent, LLM profile) reasoning-effort overrides. The effort itself stays a
// property of the LLM profile; this layer lets one agent run a different effort
// on a profile without cloning it. Precedence: override → profile → unset.
// Only request parameters change: model choice, failover order and quotas are
// untouched, and a failover to another profile re-resolves the override for that
// target profile (never carries the previous profile's value over).

type agentEffortOverrideDTO struct {
	ProfileID       int64  `json:"profile_id"`
	ReasoningEffort string `json:"reasoning_effort"`
	UpdatedAt       string `json:"updated_at,omitempty"`
}

// agentEffortProfileDTO is one LLM profile as seen from this agent: its own
// effort, whether it can serve a run, and the effective value after the override.
type agentEffortProfileDTO struct {
	ProfileID     int64  `json:"profile_id"`
	Name          string `json:"name"`
	Format        string `json:"format"`
	Model         string `json:"model"`
	IsDefault     bool   `json:"is_default"`
	HasKey        bool   `json:"has_key"`
	ProfileEffort string `json:"profile_effort"`
	Effort        string `json:"effort"`
	Source        string `json:"source"` // override | profile | unset
	UpdatedAt     string `json:"updated_at,omitempty"`
}

type agentEffortOverridesDTO struct {
	AgentKey  string                   `json:"agent_key"`
	Overrides []agentEffortOverrideDTO `json:"overrides"`
	Profiles  []agentEffortProfileDTO  `json:"profiles"`
}

func (s *Server) pgAgentEffortOverrides(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	rows, err := pg.AgentEffortRows(a.Key)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := agentEffortOverridesDTO{
		AgentKey: a.Key, Overrides: []agentEffortOverrideDTO{}, Profiles: []agentEffortProfileDTO{},
	}
	for _, row := range rows {
		effort, source := row.ProfileEffort, "profile"
		if row.OverrideEffort != "" {
			effort, source = row.OverrideEffort, "override"
		}
		if effort == "" {
			source = "unset"
		}
		p := agentEffortProfileDTO{
			ProfileID: row.ProfileID, Name: row.Name, Format: row.Format, Model: row.Model,
			IsDefault: row.IsDefault, HasKey: row.HasKey,
			ProfileEffort: row.ProfileEffort, Effort: effort, Source: source,
		}
		if row.UpdatedAt != nil {
			p.UpdatedAt = row.UpdatedAt.UTC().Format(time.RFC3339)
		}
		out.Profiles = append(out.Profiles, p)
		if row.OverrideEffort != "" {
			out.Overrides = append(out.Overrides, agentEffortOverrideDTO{
				ProfileID: row.ProfileID, ReasoningEffort: row.OverrideEffort, UpdatedAt: p.UpdatedAt,
			})
		}
	}
	writeJSON(w, 200, out)
}

// pgSaveAgentEffortOverride sets one override at PUT .../llm-effort-overrides/{profile_id}.
// Body is {"reasoning_effort": "..."}; an empty string clears it (same outcome as
// DELETE). Success is 204, matching the PRD contract.
func (s *Server) pgSaveAgentEffortOverride(w http.ResponseWriter, r *http.Request) {
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
		ReasoningEffort *string `json:"reasoning_effort"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON")
		return
	}
	if req.ReasoningEffort == nil {
		writeErr(w, 400, "缺少 reasoning_effort（空字符串表示清除覆盖）")
		return
	}
	effort := strings.TrimSpace(*req.ReasoningEffort)
	if effort == "" {
		if p, err := pg.ProfileByID(id); err != nil || p == nil {
			writeErr(w, 404, "LLM 配置不存在")
			return
		}
		if _, err := pg.DeleteEffortOverride(a.Key, id); err != nil {
			if errors.Is(err, db.ErrAgentNotFound) {
				writeErr(w, 404, "agent not found")
				return
			}
			writeErr(w, 500, err.Error())
			return
		}
		s.invalidateProfileAgents()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !db.ValidReasoningEffort(effort) {
		writeErr(w, 400, "reasoning_effort 只能是 low / medium / high / xhigh / max（空值表示清除覆盖）")
		return
	}
	if p, err := pg.ProfileByID(id); err != nil || p == nil {
		writeErr(w, 404, "LLM 配置不存在")
		return
	}
	if err := pg.SetEffortOverride(a.Key, id, effort); err != nil {
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

func (s *Server) pgDeleteAgentEffortOverride(w http.ResponseWriter, r *http.Request) {
	pg, a, ok := s.agentByKey(w, r)
	if !ok {
		return
	}
	id, err := parsePositivePathID(r, "profile_id")
	if err != nil {
		writeErr(w, 400, "profile_id 必须为正整数")
		return
	}
	if p, err := pg.ProfileByID(id); err != nil || p == nil {
		writeErr(w, 404, "LLM 配置不存在")
		return
	}
	if _, err := pg.DeleteEffortOverride(a.Key, id); err != nil {
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

func parsePositivePathID(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue(name)), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid %s", name)
	}
	return id, nil
}

// EffortSource says where an effective effort came from. It is surfaced in the
// UI and audit so "why is this request at high?" has a literal answer.
type EffortSource string

const (
	EffortSourceOverride EffortSource = "override"
	EffortSourceProfile  EffortSource = "profile"
	EffortSourceUnset    EffortSource = "unset"
)

// ResolveEffort is THE effort rule, shared by runtime provider construction, the
// chain projection, the route view and audit snapshots: an (agent, profile)
// override wins, then the profile's own effort, then unset. An empty agent key
// (pin/judge/env paths) ignores overrides. A stored value outside the accepted
// enum is treated as absent — never sent.
func ResolveEffort(agentKey string, p *db.LLMProfile, overrides map[int64]string) (string, EffortSource) {
	if p == nil {
		return "", EffortSourceUnset
	}
	if agentKey != "" {
		if v, ok := overrides[p.ID]; ok && db.ValidReasoningEffort(v) {
			return v, EffortSourceOverride
		}
	}
	if db.ValidReasoningEffort(p.ReasoningEffort) {
		return p.ReasoningEffort, EffortSourceProfile
	}
	return "", EffortSourceUnset
}

// resolveEffort loads this agent's override for one profile and applies the
// shared rule. Runtime hot path: one small indexed read per model call (the same
// order as the lazily-read task cursor that current() already performs).
func (s *Server) resolveEffort(agentKey string, p *db.LLMProfile) (effort, source string) {
	if p == nil {
		return "", string(EffortSourceUnset)
	}
	overrides := map[int64]string{}
	if agentKey != "" && s != nil && s.m != nil && s.m.pg != nil {
		if ov, ok, err := s.m.pg.EffortOverride(agentKey, p.ID); err == nil && ok {
			overrides[p.ID] = ov.ReasoningEffort
		}
	}
	resolved, src := ResolveEffort(agentKey, p, overrides)
	return resolved, string(src)
}

// applyEffortToConfig returns cfg with the agent's effective effort applied and
// the source label. Kept tiny and pure so the resolution rule is testable without
// a provider or a database.
func applyEffortToConfig(cfg agent.Config, effort string) agent.Config {
	cfg.ReasoningEffort = effort
	return cfg
}
