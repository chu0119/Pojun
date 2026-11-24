package agent

import (
	"bytes"
	"context"
	"fmt"
	"text/template"
	"time"
	"unicode/utf8"
)

// PromptOverride, if set, returns the stored system-prompt template for an agent
// key and whether one exists. The server wires it to the PG agent_prompts table.
// When nil or no override exists, agents use their built-in default prompt — so
// behavior is identical until a user edits a prompt in the UI.
var PromptOverride func(agentKey string) (string, bool)

// Prompt-variable structs — fields mirror each agent's catalog (docs §5a) so a
// user template referencing a catalog variable renders; referencing anything else
// fails template execution and falls back to the built-in default.
type PlannerVars struct{ Goal, Scope, AssetSummary, DataDir, Now string }
type WorkerVars struct{ ProxyAddr, WorkerName, DataDir, Now string }
type MainVars struct{ Goal, AssetSummary, FindingsSummary, DataDir, Now string }
type GoalsVars struct{ EngagementDescription, DataDir, Now string }

// nowStr is the server-local wall-clock string exposed as the universal {{.Now}}
// prompt variable. renderSystem runs on every agent turn/round, so this is fresh
// each run — a prompt can subtract it from a fixed start stamp to reason about
// elapsed time (e.g. a timed benchmark's "last N hours" window).
func nowStr() string { return time.Now().Format("2006-01-02 15:04:05 MST") }

// renderSystem returns the rendered system-prompt BODY (段 [A]) for agentKey.
// Precedence: the DB-stored template (if any) over the built-in default template
// (def). BOTH are Go templates now — the built-in default is seeded into the DB
// verbatim, so the two paths render identically until a user edits the prompt.
// Rendering always runs (def used to be pre-substituted plain text; it is now a
// {{.Var}} template like the DB one). On any render error we fall back to the
// default template, then to the raw default string — an agent never starts with a
// half-rendered prompt. Callers append the code-owned tail (trafficTool / 中间产物
// 输出规约) AFTER this, so those can't be edited away via the DB body.
// SkillIndexBlock（v0.4.60，宿主注入）：把该 agent 已绑定的 skill 索引块追加进
// system prompt。为什么必须进 prompt 而不只是工具描述：Skill 是元工具，模型不
// 被显式提醒就几乎不会主动调（实测 56k 次调用里 Skill 触发 8 次，任务 18 零次
// ——与当年宿主工具"装了没用起来"同一病根，解法也同款：清单进 system prompt）。
// 返回空串 = 该 agent 没绑技能，不加块。
var SkillIndexBlock func(agentKey string) string

// ProfilePrompt is the LLM-profile-scoped prompt segment applied to an agent's
// system prompt when that profile is in effect for the agent. Text is plain text
// (never rendered as a Go template); Mode is "prepend", "append" or "replace".
type ProfilePrompt struct {
	Text      string
	Mode      string
	ProfileID int64
	// Source reports why this overlay is active: agent_binding | conversation_pin |
	// task_chain | global_profile | preview_override. Empty = no overlay.
	Source string
}

// Active reports whether this overlay should be injected.
func (p ProfilePrompt) Active() bool {
	if p.Text == "" {
		return false
	}
	switch p.Mode {
	case "prepend", "append", "replace":
		return true
	}
	return false
}

type profilePromptCtxKey struct{}

// WithProfilePrompt attaches a per-run overlay to ctx. Chat and goals read it
// back; planner/worker/mainagent use per-instance resolvers because they are
// long-lived task objects, while a chat turn belongs to one conversation.
func WithProfilePrompt(ctx context.Context, p ProfilePrompt) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, profilePromptCtxKey{}, p)
}

// ProfilePromptFrom returns the overlay attached to ctx, or the zero value.
func ProfilePromptFrom(ctx context.Context) ProfilePrompt {
	if ctx == nil {
		return ProfilePrompt{}
	}
	if p, ok := ctx.Value(profilePromptCtxKey{}).(ProfilePrompt); ok {
		return p
	}
	return ProfilePrompt{}
}

func renderSystem(agentKey, def string, vars any) string {
	return renderSystemWith(agentKey, def, vars, ProfilePrompt{})
}

// renderSystemWith renders the editable body, the host-injected skill index and
// the optional profile-scoped prompt. Ordering:
//
//	prepend: [P] + body + skills
//	append:  body + skills + [P]
//	replace: [P] + skills        (the original body is dropped, skills/tails kept)
//
// Without an active overlay the output is byte-for-byte identical to before.
func renderSystemWith(agentKey, def string, vars any, pp ProfilePrompt) string {
	segs, _ := renderSystemSegments(agentKey, def, vars, pp)
	return JoinPromptSegments(segs)
}

// renderSystemSegments is renderSystemWith's segment form. It also reports a
// non-empty warning when the selected template failed to render and the built-in
// default was used instead. The runtime text is unchanged by the warning.
func renderSystemSegments(agentKey, def string, vars any, pp ProfilePrompt) ([]PromptSegment, string) {
	tmpl := def
	bodySource := "builtin_default"
	if PromptOverride != nil {
		if t, ok := PromptOverride(agentKey); ok && t != "" {
			tmpl = t
			bodySource = "db_template"
		}
	}
	rendered := def
	warning := ""
	if out, err := renderTmpl(tmpl, vars); err == nil {
		rendered = out
	} else if bodySource == "db_template" {
		if alt, altErr := renderTmpl(def, vars); altErr == nil {
			rendered = alt
			warning = "DB 提示词模板渲染失败，已回退内置默认正文"
		} else {
			warning = "DB 提示词模板与内置默认均渲染失败，已回退内置默认原文"
		}
		bodySource = "builtin_default"
	} else {
		warning = "内置默认提示词模板渲染失败，已回退原文"
	}

	skill := ""
	if SkillIndexBlock != nil {
		skill = SkillIndexBlock(agentKey)
	}

	var out []PromptSegment
	appendProfile := func(text string) {
		out = append(out, PromptSegment{
			Key: "profile_prompt", Title: "独立提示词（LLM 配置）", Source: "profile_prompt",
			Present: true, Available: true, Text: text,
		})
	}
	bodySeg := PromptSegment{
		Key: "body", Title: "提示词正文", Source: bodySource, Editable: true,
		Present: true, Available: true, Text: rendered,
	}
	skillSeg := PromptSegment{
		Key: "skill_index", Title: "技能速查（宿主注入）", Source: "host",
		Gate: "该 Agent 绑定了 Skill", Present: skill != "", Available: true,
	}
	if skill != "" {
		skillSeg.Text = "\n\n" + skill
	} else {
		skillSeg.Reason = "该 Agent 未绑定 Skill"
	}

	switch {
	case pp.Active() && pp.Mode == "replace":
		appendProfile(pp.Text)
		bodySeg.Present = false
		bodySeg.Text = ""
		bodySeg.Reason = fmt.Sprintf("已被独立提示词替换（原正文 %d 字符；技能索引与代码硬尾巴仍保留）", utf8.RuneCountInString(rendered))
		out = append(out, bodySeg, skillSeg)
	case pp.Active() && pp.Mode == "prepend":
		appendProfile(pp.Text + "\n\n")
		out = append(out, bodySeg, skillSeg)
	default:
		out = append(out, bodySeg, skillSeg)
		if pp.Active() && pp.Mode == "append" {
			appendProfile("\n\n" + pp.Text)
		}
	}
	return out, warning
}

func renderTmpl(tmpl string, vars any) (string, error) {
	t, err := template.New("p").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, vars); err != nil {
		return "", err
	}
	return b.String(), nil
}
