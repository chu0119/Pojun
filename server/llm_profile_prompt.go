package server

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/chu0119/pojun/agent"
	"github.com/chu0119/pojun/db"
)

// 独立提示词上限。上限只在这里定义一处；保存校验、报错文案与两个接口的
// prompt_max_runes 字段都从它派生，前端不再写死数字。
const maxProfilePromptRunes = 65535

func validProfilePromptMode(m string) bool {
	switch m {
	case "prepend", "append", "replace":
		return true
	}
	return false
}

// trimPromptText normalizes line endings and strips trailing whitespace with
// the same Unicode class as JS String.trim(), so "blank means clear" agrees
// between the editor and the stored value (NBSP/U+2028 included).
func trimPromptText(v string) string {
	v = strings.ReplaceAll(v, "\r\n", "\n")
	v = strings.ReplaceAll(v, "\r", "\n")
	return strings.TrimRightFunc(v, unicode.IsSpace)
}

// normalizeProfilePromptRequest applies the save-path three-state semantics:
// nil text/mode/apply keeps the stored value, an explicit empty text clears the
// prompt, and a non-empty text without a mode defaults to prepend. Returns a
// user-facing error string (empty = ok). Plain text only: CRLF is normalized and
// NUL is rejected, but {{.Var}} is left verbatim (no template execution).
func normalizeProfilePromptRequest(old db.PromptInfo, text, mode *string, apply *bool) (db.PromptInfo, string) {
	next := old
	if text != nil {
		v := trimPromptText(*text)
		if strings.ContainsRune(v, 0) {
			return db.PromptInfo{}, "独立提示词包含非法字符"
		}
		next.Text = v
	}
	if mode != nil {
		next.Mode = strings.TrimSpace(*mode)
	}
	if apply != nil {
		next.ApplyInherited = *apply
	}
	if utf8.RuneCountInString(next.Text) > maxProfilePromptRunes {
		return db.PromptInfo{}, fmt.Sprintf("独立提示词过长（上限 %d 字符）", maxProfilePromptRunes)
	}
	if next.Mode != "" && !validProfilePromptMode(next.Mode) {
		return db.PromptInfo{}, "prompt_mode 只能是 prepend / append / replace"
	}
	if next.Text == "" {
		next.Mode = ""
		return next, ""
	}
	if next.Mode == "" {
		next.Mode = "prepend"
	}
	return next, ""
}

// normalizeAgentPromptRequest validates one (agent × profile) override save.
// An empty body stores an explicit "do not inject" row (a DELETE restores
// inheritance instead); a non-empty body without a mode defaults to prepend,
// matching the profile-default save path. Returns the normalized text/mode and
// a user-facing error string.
func normalizeAgentPromptRequest(text, mode string) (string, string, string) {
	v := trimPromptText(text)
	if strings.ContainsRune(v, 0) {
		return "", "", "独立提示词包含非法字符"
	}
	if utf8.RuneCountInString(v) > maxProfilePromptRunes {
		return "", "", fmt.Sprintf("独立提示词过长（上限 %d 字符）", maxProfilePromptRunes)
	}
	m := strings.TrimSpace(mode)
	if m != "" && !validProfilePromptMode(m) {
		return "", "", "prompt_mode 只能是 prepend / append / replace"
	}
	if v == "" {
		return "", "", ""
	}
	if m == "" {
		m = "prepend"
	}
	return v, m, ""
}

// profileOverlayFromInfo builds an active overlay from a stored prompt. An empty
// body yields the zero value; a non-empty body without a mode is treated as
// prepend (the same normalization the save path applies), so a direct DB edit
// cannot create a prompt that is silently neither injected nor reported.
func profileOverlayFromInfo(id int64, pi db.PromptInfo, source string) agent.ProfilePrompt {
	if strings.TrimSpace(pi.Text) == "" {
		return agent.ProfilePrompt{}
	}
	mode := pi.Mode
	if mode == "" {
		mode = "prepend"
	}
	if !validProfilePromptMode(mode) {
		return agent.ProfilePrompt{}
	}
	return agent.ProfilePrompt{Text: pi.Text, Mode: mode, ProfileID: id, Source: source}
}

// effectiveProfilePrompt is THE (profile × agent) independent-prompt rule,
// shared by every runtime injection point:
//
//  1. An (agent, profile) override row wins outright:
//     - empty body → this agent explicitly disabled the profile's prompt;
//     - non-empty body → injected with its mode (empty mode = prepend).
//     The override is never gated by prompt_apply_inherited: that flag belongs
//     to the default layer only.
//  2. Otherwise the profile's default prompt applies: always for an explicit
//     binding / conversation pin (requireInherit=false), and for task-chain or
//     global selection only when the profile set prompt_apply_inherited.
//
// A read error yields no overlay: fail closed rather than inject a prompt the
// user may have disabled.
func (s *Server) effectiveProfilePrompt(profileID int64, agentKey, source string, requireInherit bool) agent.ProfilePrompt {
	if s == nil || s.m == nil || s.m.pg == nil || profileID <= 0 {
		return agent.ProfilePrompt{}
	}
	pg := s.m.pg
	if agentKey != "" {
		ov, ok, err := pg.AgentProfilePrompt(agentKey, profileID)
		if err != nil {
			return agent.ProfilePrompt{}
		}
		if ok {
			return profileOverlayFromInfo(profileID, db.PromptInfo{Text: ov.Text, Mode: ov.Mode}, "agent_override")
		}
	}
	pi, err := pg.ProfilePrompt(profileID)
	if err != nil {
		return agent.ProfilePrompt{}
	}
	if requireInherit && !pi.ApplyInherited {
		return agent.ProfilePrompt{}
	}
	return profileOverlayFromInfo(profileID, pi, source)
}

// inheritedProfileID returns the profile a task role would use when it has no
// explicit agent binding: the active task-chain entry, else the global profile.
// It mirrors taskLLMRuntime.current's precedence without building a provider;
// ok=false covers exhausted chains and profiles without an API key, which the
// runtime cannot serve. The source string is task_chain or global_profile.
func (s *Server) inheritedProfileID(t *Task) (int64, string, bool) {
	if s == nil || s.m == nil || s.m.pg == nil || t == nil {
		return 0, "", false
	}
	state := t.llmStateSnapshot()
	if len(state.ProfileIDs) > 0 {
		if state.ActiveID == nil {
			return 0, "task_chain", false
		}
		p, err := s.m.pg.ProfileByID(*state.ActiveID)
		if err != nil || p == nil || p.APIKey == "" {
			return 0, "task_chain", false
		}
		return p.ID, "task_chain", true
	}
	p, err := s.m.pg.ActiveProfile()
	if err != nil || p == nil || p.APIKey == "" {
		return 0, "global_profile", false
	}
	return p.ID, "global_profile", true
}

// profilePromptForTaskRole resolves the overlay for one task role at run start.
//
// Precedence:
//  1. agents.llm_profile_id — an explicit binding resolves that profile; its
//     (agent × profile) override wins, else the profile default applies (even if
//     the runtime later falls back to the chain — identity, not service).
//  2. Task-chain / global profile — resolves the same way, but the profile
//     default layer requires prompt_apply_inherited; an explicit override row
//     still applies.
//
// The system prompt is built once per run, so a mid-run quota failover does not
// rewrite it.
//
// goals/planner/worker/mainagent all use this; there is no per-role exclusion:
// every agent row can bind a profile, and the same rule applies uniformly.
func (s *Server) profilePromptForTaskRole(t *Task, agentKey string) agent.ProfilePrompt {
	if s == nil || s.m == nil || s.m.pg == nil {
		return agent.ProfilePrompt{}
	}
	pg := s.m.pg
	if a, err := pg.GetAgentByKey(agentKey); err == nil && a != nil && a.LLMProfileID != nil {
		return s.effectiveProfilePrompt(*a.LLMProfileID, agentKey, "agent_binding", false)
	}
	id, source, ok := s.inheritedProfileID(t)
	if !ok {
		return agent.ProfilePrompt{}
	}
	return s.effectiveProfilePrompt(id, agentKey, source, true)
}

// profilePromptForChat mirrors resolveChatAgent's precedence: agent binding →
// this conversation's pin → global profile. Binding and pin are explicit
// choices, so the profile default layer applies regardless of inheritance; the
// global profile requires prompt_apply_inherited. An (agent × profile) override
// wins in all three cases.
func (s *Server) profilePromptForChat(c *db.Conversation) agent.ProfilePrompt {
	if s == nil || s.m == nil || s.m.pg == nil || c == nil {
		return agent.ProfilePrompt{}
	}
	pg := s.m.pg
	if a, err := pg.GetAgentByKey(c.AgentKey); err == nil && a != nil && a.LLMProfileID != nil {
		return s.effectiveProfilePrompt(*a.LLMProfileID, c.AgentKey, "agent_binding", false)
	}
	if c.LLMProfileID != nil {
		return s.effectiveProfilePrompt(*c.LLMProfileID, c.AgentKey, "conversation_pin", false)
	}
	p, err := pg.ActiveProfile()
	if err != nil || p == nil || p.APIKey == "" {
		return agent.ProfilePrompt{}
	}
	return s.effectiveProfilePrompt(p.ID, c.AgentKey, "global_profile", true)
}
