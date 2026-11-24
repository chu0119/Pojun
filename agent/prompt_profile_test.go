package agent

import (
	"context"
	"regexp"
	"strings"
	"testing"
)

func TestProfilePromptModes(t *testing.T) {
	oldOverride, oldSkill := PromptOverride, SkillIndexBlock
	t.Cleanup(func() { PromptOverride, SkillIndexBlock = oldOverride, oldSkill })
	PromptOverride = nil
	SkillIndexBlock = func(key string) string {
		if key == "worker" {
			return "SKILLS"
		}
		return ""
	}

	base := renderSystemWith("worker", "BODY", WorkerVars{}, ProfilePrompt{})
	if base != "BODY\n\nSKILLS" {
		t.Fatalf("baseline = %q", base)
	}

	cases := []struct {
		mode string
		want string
	}{
		{"prepend", "PROFILE\n\nBODY\n\nSKILLS"},
		{"append", "BODY\n\nSKILLS\n\nPROFILE"},
		{"replace", "PROFILE\n\nSKILLS"},
	}
	for _, c := range cases {
		got := renderSystemWith("worker", "BODY", WorkerVars{}, ProfilePrompt{Text: "PROFILE", Mode: c.mode})
		if got != c.want {
			t.Fatalf("%s = %q, want %q", c.mode, got, c.want)
		}
	}

	// Empty text / invalid mode must not change anything.
	for _, pp := range []ProfilePrompt{{Mode: "append"}, {Text: "PROFILE", Mode: "bogus"}} {
		if got := renderSystemWith("worker", "BODY", WorkerVars{}, pp); got != base {
			t.Fatalf("inactive overlay changed prompt: %q", got)
		}
	}
}

func TestReplaceSegmentKeepsOriginalBodyChars(t *testing.T) {
	oldOverride, oldSkill := PromptOverride, SkillIndexBlock
	t.Cleanup(func() { PromptOverride, SkillIndexBlock = oldOverride, oldSkill })
	PromptOverride = nil
	SkillIndexBlock = nil

	segs, _ := renderSystemSegments("worker", "BODY", WorkerVars{}, ProfilePrompt{Text: "P", Mode: "replace"})
	var body, profile *PromptSegment
	for i := range segs {
		switch segs[i].Key {
		case "body":
			body = &segs[i]
		case "profile_prompt":
			profile = &segs[i]
		}
	}
	if profile == nil || !profile.Present || profile.Text != "P" {
		t.Fatalf("replace profile segment mismatch: %+v", profile)
	}
	if body == nil || body.Present || body.Text != "" {
		t.Fatalf("replaced body must have no text: %+v", body)
	}
	if body.OriginalChars != 4 {
		t.Fatalf("original chars = %d, want 4", body.OriginalChars)
	}
	if !strings.Contains(body.Reason, "原正文 4 字符") {
		t.Fatalf("reason must state the original size: %q", body.Reason)
	}
}

func TestProfilePromptSitsBeforeCodeTails(t *testing.T) {
	oldOverride, oldSkill := PromptOverride, SkillIndexBlock
	t.Cleanup(func() { PromptOverride, SkillIndexBlock = oldOverride, oldSkill })
	PromptOverride = func(key string) (string, bool) {
		if key == "planner" {
			return "PLANNER-BODY-MARK {{.Goal}}", true
		}
		return "", false
	}
	SkillIndexBlock = nil

	appendPP := ProfilePrompt{Text: "PROFILE", Mode: "append"}
	got := plannerSystemWith("目标", "/data", "/work", appendPP)
	if !strings.Contains(got, "PLANNER-BODY-MARK 目标") {
		t.Fatalf("append must keep the body: %q", got)
	}
	if strings.Index(got, "PROFILE") == -1 || strings.Index(got, "PROFILE") > strings.Index(got, "中间产物输出规约") {
		t.Fatalf("append profile must precede code-owned tails: %q", got)
	}
	prepend := plannerSystemWith("目标", "/data", "/work", ProfilePrompt{Text: "PERSONA", Mode: "prepend"})
	if !strings.HasPrefix(prepend, "PERSONA\n\n") {
		t.Fatalf("prepend must be first: %q", prepend)
	}
	replaced := plannerSystemWith("目标", "/data", "/work", ProfilePrompt{Text: "REPLACED", Mode: "replace"})
	if !strings.HasPrefix(replaced, "REPLACED") || strings.Contains(replaced, "PLANNER-BODY-MARK") {
		t.Fatalf("replace must drop the body but keep tails: %q", replaced)
	}
	if !strings.Contains(replaced, "中间产物输出规约") {
		t.Fatalf("replace must keep code-owned tails: %q", replaced)
	}
}

func TestStaticTailSegmentsMatchRuntime(t *testing.T) {
	oldOverride, oldSkill := PromptOverride, SkillIndexBlock
	t.Cleanup(func() { PromptOverride, SkillIndexBlock = oldOverride, oldSkill })
	PromptOverride = nil
	SkillIndexBlock = nil

	check := func(name, runtime string, renderSegs []PromptSegment, static []PromptSegment) {
		t.Helper()
		prefix := JoinPromptSegments(renderSegs)
		if !strings.HasPrefix(runtime, prefix) {
			t.Fatalf("%s: runtime does not start with render segments", name)
		}
		if got, want := strings.TrimPrefix(runtime, prefix), JoinPromptSegments(static); got != want {
			t.Fatalf("%s: static tails drifted from runtime\n got=%q\nwant=%q", name, got, want)
		}
	}

	plannerRender, _ := renderSystemSegments("planner", plannerDefaultTmpl, PlannerVars{Goal: "g", DataDir: "/data", Now: "now"}, ProfilePrompt{})
	check("planner", plannerSystemWith("g", "/data", "/work", ProfilePrompt{}), plannerRender,
		StaticTailSegments("planner", TailOptions{WorkDir: "/work"}))

	workerRuntime := workerSystemWith("", "", "/data", "/run", ProfilePrompt{})
	workerRender, _ := renderSystemSegments("worker", workerDefaultTmpl, WorkerVars{DataDir: "/data", Now: "now"}, ProfilePrompt{})
	check("worker", workerRuntime, workerRender,
		StaticTailSegments("worker", TailOptions{WorkDir: "/data", RunDir: "/run", IncludeHostTools: true}))

	mainRuntime := mainAgentSystemWith("g", "/data", "/work", ProfilePrompt{})
	mainRender, _ := renderSystemSegments("mainagent", mainAgentDefaultTmpl, MainVars{Goal: "g", DataDir: "/data", Now: "now"}, ProfilePrompt{})
	check("mainagent", mainRuntime, mainRender,
		StaticTailSegments("mainagent", TailOptions{WorkDir: "/work", IncludeHostTools: true}))

	// Chat agents get their body from the DB seed in production; mirror that here
	// so chatSystemWith and the segment renderer pick the same template.
	PromptOverride = func(key string) (string, bool) { return DefaultPromptFor(key), true }
	for _, key := range []string{"pentest", "reporter", "retester", "auto", "custom_agent"} {
		def := DefaultPromptFor(key)
		chatRender, _ := renderSystemSegments(key, def, chatVars{DataDir: "/data", Now: "now"}, ProfilePrompt{})
		check("chat:"+key, chatSystemWith(key, "/data", "/work", ProfilePrompt{}), chatRender,
			StaticTailSegments(key, TailOptions{WorkDir: "/work"}))
	}
}

func TestProfilePromptContextRoundTrip(t *testing.T) {
	pp := ProfilePrompt{Text: "x", Mode: "append", ProfileID: 7, Source: "task_chain"}
	if got := ProfilePromptFrom(WithProfilePrompt(context.Background(), pp)); got != pp {
		t.Fatalf("context overlay mismatch: %+v", got)
	}
	if got := ProfilePromptFrom(nil); got.Active() {
		t.Fatalf("nil ctx must yield zero overlay: %+v", got)
	}
}

func TestChatFallbackUsesAgentDefault(t *testing.T) {
	oldOverride, oldSkill := PromptOverride, SkillIndexBlock
	t.Cleanup(func() { PromptOverride, SkillIndexBlock = oldOverride, oldSkill })
	PromptOverride = func(key string) (string, bool) {
		if key == "auto" {
			return "{{.Goal}}", true // undefined in chatVars → render fallback
		}
		return "", false
	}
	SkillIndexBlock = nil

	vars := chatVars{DataDir: "/data", Now: nowStr()}
	want := renderSystemWith("auto", DefaultPromptFor("auto"), vars, ProfilePrompt{}) + chatWorkDirSpec("/work")
	got := chatSystemWith("auto", "/data", "/work", ProfilePrompt{})
	normalize := func(s string) string {
		return regexp.MustCompile(`\d{4}-\d\d-\d\d \d\d:\d\d:\d\d \S+`).ReplaceAllString(s, "<NOW>")
	}
	if normalize(got) != normalize(want) {
		t.Fatalf("chat fallback drifted from DefaultPromptFor\n got=%q\nwant=%q", got, want)
	}
}
