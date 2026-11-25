// This file is the anti-drift gate between the read-only prompt-chain
// projection and the real runtime assembly.
//
// The projection side and the runtime side share ONE TailOptions definition —
// the agent.TailOptions struct — so the set of fields an agent's tails
// interpolate is declared in a single place. On the runtime side this test now
// also builds its expectation through the same exported constructors the real
// builders use (PlannerTailOptions / WorkerTailOptions / MainAgentTailOptions /
// ChatTailOptions), instead of a hand-written literal that could silently drift
// away from the first.
//
// What this does NOT make exactly equivalent: each side still passes its own
// SIMULATED inputs — a placeholder work/run directory and a display-only proxy
// address. The preview cannot know the real per-run directory, so those
// stand-in values are a known and irreducible approximation of the live run.
// This test pins the segment text those inputs produce; it is not a claim that
// preview and runtime are fully equivalent.
package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/chu0119/pojun/agent"
	"github.com/chu0119/pojun/db"
)

// chainStaticText joins the projection's non-runtime segments in display order.
// The projection also lists runtime injection points (constraints, Program
// Profile, task auth, intent/facts/lessons, autopilot, launch instruction,
// finding guidance, deferred tools) whose body only exists during a run; they
// are deliberately body-less and are not part of the static runtime assembly.
func chainStaticText(ch *agentPromptChain) string {
	var b strings.Builder
	for _, seg := range ch.Segments {
		switch seg.Kind {
		case "profile_prompt":
			// The synthetic "inactive overlay" segment reports a configured prompt
			// that the runtime never injects; only an active overlay has text in the
			// assembled prompt.
			if !seg.Present {
				continue
			}
		case "body", "skill_index", "code_tail", "host":
		default:
			continue
		}
		b.WriteString(seg.Text)
	}
	return b.String()
}

// firstRuneDiff locates the first rune-level difference, with a readable window
// on both sides. Byte slicing would split multi-byte Chinese text and hide the
// actual divergence.
func firstRuneDiff(got, want string) string {
	g, w := []rune(got), []rune(want)
	n := len(g)
	if len(w) < n {
		n = len(w)
	}
	for i := 0; i < n; i++ {
		if g[i] != w[i] {
			lo := i - 30
			if lo < 0 {
				lo = 0
			}
			ghi, whi := i+60, i+60
			if ghi > len(g) {
				ghi = len(g)
			}
			if whi > len(w) {
				whi = len(w)
			}
			return fmt.Sprintf("首个差异在 rune %d\ngot:  …%s…\nwant: …%s…", i, string(g[lo:ghi]), string(w[lo:whi]))
		}
	}
	return fmt.Sprintf("前缀一致但长度不同：got %d runes, want %d runes", len(g), len(w))
}

// TestAgentPromptChainMatchesRuntimeAssembly is the anti-drift gate between the
// read-only chain projection and the real runtime assembly: for the same inputs,
// every segment the runtime joins (body/profile/skill/code tails/host tools) must
// be byte-identical, in the same order, to what the chain page displays.
//
// It drives the projection through the preview override (an explicit profile id)
// so the result does not depend on whatever binding/global profile the shared
// test database happens to carry, and covers all three overlay modes — prepend
// and append keep the body, replace drops it while keeping the code tails.
func TestAgentPromptChainMatchesRuntimeAssembly(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer m.Close()
	td := t.TempDir()
	s := New(context.Background(), m, td, td, td)
	pg := m.pg
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())

	for _, key := range []string{"planner", "worker", "mainagent", "pentest"} {
		a, err := pg.GetAgentByKey(key)
		if err != nil {
			t.Fatalf("get agent %s: %v", key, err)
		}
		if a == nil {
			t.Fatalf("agent %s missing from the seeded catalog", key)
		}
		for _, mode := range []string{"prepend", "append", "replace"} {
			text := "PROFILE-OVERLAY-" + mode
			id, err := pg.SaveProfile(&db.LLMProfile{
				Name: "chain-probe-" + key + "-" + mode + "-" + suffix, Format: "openai",
				Model: "probe-model", APIKey: "probe-key", Streaming: true,
			})
			if err != nil {
				t.Fatalf("create probe profile: %v", err)
			}
			t.Cleanup(func() { _ = pg.DeleteProfile(id) })
			if err := pg.SetProfilePrompt(id, text, mode, false); err != nil {
				t.Fatalf("set profile prompt: %v", err)
			}
			pp := agent.ProfilePrompt{Text: text, Mode: mode, ProfileID: id, Source: "preview_override"}

			chain, err := s.buildAgentPromptChain(pg, a, fmt.Sprint(id))
			if err != nil {
				t.Fatalf("%s/%s: build chain: %v", key, mode, err)
			}

			workDir, runDir := "<任务工作目录>", "<本次意图工作目录>"
			if a.Role == "assistant" {
				workDir, runDir = "<会话工作目录>", "<会话工作目录>"
			}
			proxyAddr := ""
			if a.Key == "worker" {
				proxyAddr = s.m.ProxyDisplayAddr()
			}
			// Build TailOptions through the same agent-package constructors the
			// runtime builders use, so this test cannot diverge from the real
			// assembly on which fields an agent's tails interpolate.
			var opts agent.TailOptions
			switch a.Key {
			case "planner":
				opts = agent.PlannerTailOptions(workDir)
			case "worker":
				// CAEnabled is derived from cert presence, mirroring the runtime;
				// the preview only knows whether traffic capture is on.
				caCert := ""
				if s.m.TrafficEnabled() {
					caCert = "<ca.pem>"
				}
				opts = agent.WorkerTailOptions(proxyAddr, caCert, runDir)
			case "mainagent":
				opts = agent.MainAgentTailOptions(workDir)
			default: // conversational roles (pentest here)
				opts = agent.ChatTailOptions(workDir)
			}
			want, _ := agent.RuntimeSystem(
				a.Key, agent.DefaultPromptFor(a.Key), agent.PreviewVarsFor(a.Key), pp, opts,
			)
			if got := chainStaticText(chain); got != want {
				t.Fatalf("%s/%s: chain projection drifted from the runtime assembly\n%s", key, mode, firstRuneDiff(got, want))
			}
		}
	}
}

// TestProfilePromptSourceBoundaries pins the four overlay sources and the one
// rule the docs call out explicitly: a mid-run quota failover does NOT rebuild
// the system prompt, so the overlay is chosen once, from the source that was in
// effect at run start.
//
//   - agent binding  → always applied (identity, regardless of prompt_apply_inherited)
//   - task chain     → applied only with prompt_apply_inherited AND an API key
//   - conversation pin → always applied (an explicit choice, like a binding)
//   - global profile → applied only with prompt_apply_inherited AND an API key
func TestProfilePromptSourceBoundaries(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer m.Close()
	td := t.TempDir()
	s := New(context.Background(), m, td, td, td)
	pg := m.pg

	mkProfile := func(name string) int64 {
		t.Helper()
		id, err := pg.SaveProfile(&db.LLMProfile{
			Name: name, Format: "openai", Model: "probe-model",
			APIKey: "probe-key", Streaming: true,
		})
		if err != nil {
			t.Fatalf("create profile %s: %v", name, err)
		}
		t.Cleanup(func() { _ = pg.DeleteProfile(id) })
		return id
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	pBind := mkProfile("boundary-bind-" + suffix)
	pChain := mkProfile("boundary-chain-" + suffix)
	pPin := mkProfile("boundary-pin-" + suffix)
	pGlobal := mkProfile("boundary-global-" + suffix)

	planner, err := pg.GetAgentByKey("planner")
	if err != nil || planner == nil {
		t.Fatalf("planner agent: %v", err)
	}
	oldPlannerBind := planner.LLMProfileID
	t.Cleanup(func() { _ = pg.SetAgentLLMProfile("planner", oldPlannerBind) })

	// A task whose chain starts at pChain (active entry). It is built through the
	// DB layer and wrapped without registering with the manager, so no background
	// scheduler starts a real LLM run against the probe profiles.
	pt, err := pg.CreateTaskWithOptions("prompt-boundary", "probe the prompt sources",
		db.TaskCreateOptions{LLMProfileIDs: []int64{pChain}})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	task := taskFromPG(pt, nil, nil)
	t.Cleanup(func() { _ = pg.DeleteTask(pt.ID) })

	setPrompt := func(id int64, text, mode string, apply bool) {
		t.Helper()
		if err := pg.SetProfilePrompt(id, text, mode, apply); err != nil {
			t.Fatalf("set prompt on %d: %v", id, err)
		}
	}

	// 1) An explicit agent binding wins over the task chain, and applies its
	//    prompt regardless of prompt_apply_inherited.
	setPrompt(pBind, "BIND", "prepend", false)
	setPrompt(pChain, "CHAIN", "append", true)
	if err := pg.SetAgentLLMProfile("planner", &pBind); err != nil {
		t.Fatalf("bind planner: %v", err)
	}
	if pp := s.profilePromptForTaskRole(task, "planner"); pp.Source != "agent_binding" || pp.Text != "BIND" {
		t.Fatalf("binding must win: %+v", pp)
	}

	// 2) Without a binding, the task chain applies only when the profile opted in.
	if err := pg.SetAgentLLMProfile("planner", nil); err != nil {
		t.Fatalf("unbind planner: %v", err)
	}
	setPrompt(pChain, "CHAIN", "append", false)
	if pp := s.profilePromptForTaskRole(task, "planner"); pp.Active() {
		t.Fatalf("task chain without prompt_apply_inherited must not inject: %+v", pp)
	}
	setPrompt(pChain, "CHAIN", "append", true)
	if pp := s.profilePromptForTaskRole(task, "planner"); pp.Source != "task_chain" || pp.Text != "CHAIN" {
		t.Fatalf("task chain with prompt_apply_inherited must inject: %+v", pp)
	}

	// 3) Global fallback: a task with no chain resolves the active profile, which
	//    again obeys prompt_apply_inherited (and needs an API key to serve a run).
	freePT, err := pg.CreateTaskWithOptions("prompt-boundary-global", "no chain",
		db.TaskCreateOptions{})
	if err != nil {
		t.Fatalf("create free task: %v", err)
	}
	freeTask := taskFromPG(freePT, nil, nil)
	t.Cleanup(func() { _ = pg.DeleteTask(freePT.ID) })

	oldActive, err := pg.ActiveProfile()
	if err != nil {
		t.Fatalf("read active profile: %v", err)
	}
	t.Cleanup(func() {
		if oldActive != nil {
			_ = pg.SetActiveProfile(oldActive.ID)
			return
		}
		_, _ = pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE is_default`)
	})
	if err := pg.SetActiveProfile(pGlobal); err != nil {
		t.Fatalf("activate global probe: %v", err)
	}
	setPrompt(pGlobal, "GLOBAL", "prepend", false)
	if pp := s.profilePromptForTaskRole(freeTask, "planner"); pp.Active() {
		t.Fatalf("global profile without prompt_apply_inherited must not inject: %+v", pp)
	}
	setPrompt(pGlobal, "GLOBAL", "prepend", true)
	if pp := s.profilePromptForTaskRole(freeTask, "planner"); pp.Source != "global_profile" || pp.Text != "GLOBAL" {
		t.Fatalf("global profile with prompt_apply_inherited must inject: %+v", pp)
	}

	// 4) A conversation pin is an explicit choice: it applies without the flag,
	//    but a bound agent still outranks it. "auto" is the built-in general
	//    assistant agent row (there is no agent keyed "assistant").
	assistant, err := pg.GetAgentByKey("auto")
	if err != nil || assistant == nil {
		t.Fatalf("auto agent: %v", err)
	}
	oldAssistantBind := assistant.LLMProfileID
	t.Cleanup(func() { _ = pg.SetAgentLLMProfile("auto", oldAssistantBind) })
	if err := pg.SetAgentLLMProfile("auto", nil); err != nil {
		t.Fatalf("unbind auto: %v", err)
	}
	conv, err := pg.CreateConversation("auto", "prompt-boundary", &pPin)
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteConversation(conv.ID) })
	setPrompt(pPin, "PIN", "prepend", false)
	if pp := s.profilePromptForChat(conv); pp.Source != "conversation_pin" || pp.Text != "PIN" {
		t.Fatalf("conversation pin must apply without prompt_apply_inherited: %+v", pp)
	}
	if err := pg.SetAgentLLMProfile("auto", &pBind); err != nil {
		t.Fatalf("bind auto: %v", err)
	}
	if pp := s.profilePromptForChat(conv); pp.Source != "agent_binding" || pp.Text != "BIND" {
		t.Fatalf("agent binding must outrank a conversation pin: %+v", pp)
	}
}
