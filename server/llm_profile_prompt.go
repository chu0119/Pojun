package server

import (
	"fmt"
	"strings"
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

// normalizeProfilePromptRequest applies the save-path three-state semantics:
// nil text/mode/apply keeps the stored value, an explicit empty text clears the
// prompt, and a non-empty text without a mode defaults to prepend. Returns a
// user-facing error string (empty = ok). Plain text only: CRLF is normalized and
// NUL is rejected, but {{.Var}} is left verbatim (no template execution).
func normalizeProfilePromptRequest(old db.PromptInfo, text, mode *string, apply *bool) (db.PromptInfo, string) {
	next := old
	if text != nil {
		v := strings.ReplaceAll(*text, "\r\n", "\n")
		v = strings.ReplaceAll(v, "\r", "\n")
		v = strings.TrimRight(v, " \t\n")
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

// profileOverlayFromInfo builds an active overlay from a stored prompt. Empty or
// malformed prompts yield the zero value, so direct DB edits cannot inject a
// mode the runtime does not understand.
func profileOverlayFromInfo(id int64, pi db.PromptInfo, source string) agent.ProfilePrompt {
	if strings.TrimSpace(pi.Text) == "" || !validProfilePromptMode(pi.Mode) {
		return agent.ProfilePrompt{}
	}
	return agent.ProfilePrompt{Text: pi.Text, Mode: pi.Mode, ProfileID: id, Source: source}
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
//  1. agents.llm_profile_id — an explicit binding always applies its prompt,
//     even if the runtime later falls back to the chain (identity, not service).
//  2. Task-chain / global profile — applies only when the profile set
//     prompt_apply_inherited. The system prompt is built once per run, so a
//     mid-run quota failover does not rewrite it.
//
// goals/planner/worker/mainagent all use this; there is no per-role exclusion:
// every agent row can bind a profile, and the same rule applies uniformly.
func (s *Server) profilePromptForTaskRole(t *Task, agentKey string) agent.ProfilePrompt {
	if s == nil || s.m == nil || s.m.pg == nil {
		return agent.ProfilePrompt{}
	}
	pg := s.m.pg
	if a, err := pg.GetAgentByKey(agentKey); err == nil && a != nil && a.LLMProfileID != nil {
		pi, err := pg.ProfilePrompt(*a.LLMProfileID)
		if err != nil {
			return agent.ProfilePrompt{}
		}
		return profileOverlayFromInfo(*a.LLMProfileID, pi, "agent_binding")
	}
	id, source, ok := s.inheritedProfileID(t)
	if !ok {
		return agent.ProfilePrompt{}
	}
	pi, err := pg.ProfilePrompt(id)
	if err != nil || !pi.ApplyInherited {
		return agent.ProfilePrompt{}
	}
	return profileOverlayFromInfo(id, pi, source)
}

// profilePromptForChat mirrors resolveChatAgent's precedence: agent binding →
// this conversation's pin → global profile. Binding and pin are explicit
// choices, so their prompts apply even if the provider later falls back; the
// global profile requires prompt_apply_inherited.
func (s *Server) profilePromptForChat(c *db.Conversation) agent.ProfilePrompt {
	if s == nil || s.m == nil || s.m.pg == nil || c == nil {
		return agent.ProfilePrompt{}
	}
	pg := s.m.pg
	if a, err := pg.GetAgentByKey(c.AgentKey); err == nil && a != nil && a.LLMProfileID != nil {
		pi, err := pg.ProfilePrompt(*a.LLMProfileID)
		if err != nil {
			return agent.ProfilePrompt{}
		}
		return profileOverlayFromInfo(*a.LLMProfileID, pi, "agent_binding")
	}
	if c.LLMProfileID != nil {
		pi, err := pg.ProfilePrompt(*c.LLMProfileID)
		if err != nil {
			return agent.ProfilePrompt{}
		}
		return profileOverlayFromInfo(*c.LLMProfileID, pi, "conversation_pin")
	}
	p, err := pg.ActiveProfile()
	if err != nil || p == nil || p.APIKey == "" {
		return agent.ProfilePrompt{}
	}
	pi, err := pg.ProfilePrompt(p.ID)
	if err != nil || !pi.ApplyInherited {
		return agent.ProfilePrompt{}
	}
	return profileOverlayFromInfo(p.ID, pi, "global_profile")
}
