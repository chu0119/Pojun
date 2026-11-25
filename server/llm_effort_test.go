package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chu0119/pojun/db"
)

// TestSideProviderUsesEffectiveEffort ensures a side question taken from an
// agent that runs an override is accepted: the identity must be computed with the
// same effective effort the provider was built with, and a later override change
// must invalidate the old snapshot instead of silently using another effort.
func TestSideProviderUsesEffectiveEffort(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer m.Close()
	td := t.TempDir()
	s := New(context.Background(), m, td, td, td)
	pg := m.pg

	suffix := time.Now().UnixNano()
	id, err := pg.SaveProfile(&db.LLMProfile{
		Name: fmt.Sprintf("side-effort-%d", suffix), Format: "openai", Model: "m",
		APIKey: "probe-key", Streaming: true, ReasoningEffort: "low",
	})
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteProfile(id) })
	agentKey := fmt.Sprintf("t_side_%d", suffix)
	if _, err := pg.Exec(`INSERT INTO agents(key, name, role) VALUES ($1,$1,'worker')`, agentKey); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM agents WHERE key=$1`, agentKey) })
	if err := pg.SetEffortOverride(agentKey, id, "max"); err != nil {
		t.Fatalf("set override: %v", err)
	}

	prof, err := pg.ProfileByID(id)
	if err != nil || prof == nil {
		t.Fatalf("load profile: %v", err)
	}
	cfg, ok := s.profileConfig(prof)
	if !ok {
		t.Fatal("profile config unavailable")
	}
	cfg = applyEffortToConfig(cfg, "max")
	model := sideModel(cfg, id, prof.Name, agentKey)
	if _, err := s.sideProvider(model); err != nil {
		t.Fatalf("side provider with override must be accepted: %v", err)
	}
	// The override changes → the old snapshot describes different parameters.
	if err := pg.SetEffortOverride(agentKey, id, "high"); err != nil {
		t.Fatalf("change override: %v", err)
	}
	s.invalidateProfileAgents()
	if _, err := s.sideProvider(model); err == nil {
		t.Fatal("stale side snapshot must be rejected after the effort changes")
	}
}

// TestResolveEffortPrecedence pins the one rule the whole feature rests on:
// (agent, profile) override → the profile's own reasoning_effort → unset. It also
// checks that a failover target resolves against ITSELF, never carrying the
// previous profile's override.
func TestResolveEffortPrecedence(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer m.Close()
	td := t.TempDir()
	s := New(context.Background(), m, td, td, td)
	pg := m.pg

	suffix := time.Now().UnixNano()
	mk := func(name, effort string) *db.LLMProfile {
		t.Helper()
		p := &db.LLMProfile{
			Name: fmt.Sprintf("%s-%d", name, suffix), Format: "openai", Model: "m",
			APIKey: "probe-key", Streaming: true, ReasoningEffort: effort,
		}
		id, err := pg.SaveProfile(p)
		if err != nil {
			t.Fatalf("create profile %s: %v", name, err)
		}
		t.Cleanup(func() { _ = pg.DeleteProfile(id) })
		p.ID = id
		return p
	}
	agentKey := fmt.Sprintf("t_eff_%d", suffix)
	if _, err := pg.Exec(`INSERT INTO agents(key, name, role) VALUES ($1,$1,'worker')`, agentKey); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM agents WHERE key=$1`, agentKey) })

	a := mk("effort-a", "low")
	b := mk("effort-b", "high")

	if effort, source := s.resolveEffort(agentKey, a); effort != "low" || source != "profile" {
		t.Fatalf("profile effort: %q/%q", effort, source)
	}
	if err := pg.SetEffortOverride(agentKey, a.ID, "xhigh"); err != nil {
		t.Fatalf("set override: %v", err)
	}
	if effort, source := s.resolveEffort(agentKey, a); effort != "xhigh" || source != "override" {
		t.Fatalf("override must win: %q/%q", effort, source)
	}
	// Failover to b: b has no override and its own effort, so the answer comes from
	// b — not from a's xhigh override.
	if effort, source := s.resolveEffort(agentKey, b); effort != "high" || source != "profile" {
		t.Fatalf("failover target must resolve against itself: %q/%q", effort, source)
	}
	// A different agent has no override on a.
	if effort, source := s.resolveEffort("someone_else", a); effort != "low" || source != "profile" {
		t.Fatalf("other agent must not inherit an override: %q/%q", effort, source)
	}

	empty := mk("effort-empty", "")
	if err := pg.SetEffortOverride(agentKey, empty.ID, "max"); err != nil {
		t.Fatalf("set override on empty: %v", err)
	}
	if effort, source := s.resolveEffort(agentKey, empty); effort != "max" || source != "override" {
		t.Fatalf("override on empty profile: %q/%q", effort, source)
	}
	if _, err := pg.DeleteEffortOverride(agentKey, empty.ID); err != nil {
		t.Fatalf("clear override: %v", err)
	}
	if effort, source := s.resolveEffort(agentKey, empty); effort != "" || source != "unset" {
		t.Fatalf("unset after clear: %q/%q", effort, source)
	}
}

// TestResolveEffortRejectsInvalidStoredValue: a profile row edited outside the
// API to an unknown level must be treated as unset — never forwarded upstream.
func TestResolveEffortRejectsInvalidStoredValue(t *testing.T) {
	p := &db.LLMProfile{ID: 1, ReasoningEffort: "ultra"}
	if effort, source := ResolveEffort("worker", p, nil); effort != "" || source != EffortSourceUnset {
		t.Fatalf("invalid stored effort = %q/%q, want unset", effort, source)
	}
	if effort, source := ResolveEffort("worker", p, map[int64]string{1: "also-bad"}); effort != "" || source != EffortSourceUnset {
		t.Fatalf("invalid override must not resurrect profile value: %q/%q", effort, source)
	}
}

// TestProviderEffortIsPerAgent pins the request-level effect: the same profile
// yields providers whose config carries the agent's effective effort, and two
// agents at different efforts do not share one cache entry.
func TestProviderEffortIsPerAgent(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer m.Close()
	td := t.TempDir()
	s := New(context.Background(), m, td, td, td)
	pg := m.pg

	suffix := time.Now().UnixNano()
	id, err := pg.SaveProfile(&db.LLMProfile{
		Name: fmt.Sprintf("effort-provider-%d", suffix), Format: "openai", Model: "m",
		APIKey: "probe-key", Streaming: true, ReasoningEffort: "low",
	})
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteProfile(id) })

	agentA := fmt.Sprintf("t_eff_a_%d", suffix)
	agentB := fmt.Sprintf("t_eff_b_%d", suffix)
	for _, key := range []string{agentA, agentB} {
		if _, err := pg.Exec(`INSERT INTO agents(key, name, role) VALUES ($1,$1,'worker')`, key); err != nil {
			t.Fatalf("create agent %s: %v", key, err)
		}
	}
	t.Cleanup(func() {
		for _, key := range []string{agentA, agentB} {
			_, _ = pg.Exec(`DELETE FROM agents WHERE key=$1`, key)
		}
	})

	if err := pg.SetEffortOverride(agentB, id, "max"); err != nil {
		t.Fatalf("set override: %v", err)
	}
	_, cfgA, ok := s.providerForAgentProfile(agentA, id)
	if !ok || cfgA.ReasoningEffort != "low" {
		t.Fatalf("agent A cfg effort = %q ok=%v", cfgA.ReasoningEffort, ok)
	}
	_, cfgB, ok := s.providerForAgentProfile(agentB, id)
	if !ok || cfgB.ReasoningEffort != "max" {
		t.Fatalf("agent B cfg effort = %q ok=%v", cfgB.ReasoningEffort, ok)
	}
	// The profile-only path must ignore overrides entirely.
	_, cfgPlain, ok := s.providerForProfile(id)
	if !ok || cfgPlain.ReasoningEffort != "low" {
		t.Fatalf("profile-only cfg effort = %q ok=%v", cfgPlain.ReasoningEffort, ok)
	}
	// Two cache entries for the same profile at different efforts.
	s.provCacheMu.Lock()
	_, hasLow := s.provByProfile[provKey{profileID: id, effort: "low"}]
	_, hasMax := s.provByProfile[provKey{profileID: id, effort: "max"}]
	s.provCacheMu.Unlock()
	if !hasLow || !hasMax {
		t.Fatalf("provider cache must key on effort: low=%v max=%v", hasLow, hasMax)
	}
}

// TestAgentEffortOverrideAPI covers the three handlers' validation and clearing
// semantics without going through the authenticated mux (path values are set
// directly, mirroring what the mux supplies in production).
func TestAgentEffortOverrideAPI(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer m.Close()
	td := t.TempDir()
	s := New(context.Background(), m, td, td, td)
	pg := m.pg

	suffix := time.Now().UnixNano()
	id, err := pg.SaveProfile(&db.LLMProfile{
		Name: fmt.Sprintf("effort-api-%d", suffix), Format: "openai", Model: "m",
		APIKey: "probe-key", Streaming: true,
	})
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteProfile(id) })

	agentKey := fmt.Sprintf("t_eff_api_%d", suffix)
	if _, err := pg.Exec(`INSERT INTO agents(key, name, role) VALUES ($1,$1,'worker')`, agentKey); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM agents WHERE key=$1`, agentKey) })

	call := func(method, body string, h http.HandlerFunc, profileID int64) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		path := "/api/agents/" + agentKey + "/llm-effort-overrides"
		if profileID > 0 {
			path += fmt.Sprintf("/%d", profileID)
		}
		var req *http.Request
		if body == "" {
			req = httptest.NewRequest(method, path, nil)
		} else {
			req = httptest.NewRequest(method, path, bytes.NewBufferString(body))
		}
		req.SetPathValue("key", agentKey)
		if profileID > 0 {
			req.SetPathValue("profile_id", fmt.Sprint(profileID))
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec, out
	}

	// Invalid enum → 400.
	if rec, _ := call(http.MethodPut, `{"reasoning_effort":"ultra"}`, s.pgSaveAgentEffortOverride, id); rec.Code != 400 {
		t.Fatalf("invalid effort must be 400, got %d (%s)", rec.Code, rec.Body.String())
	}
	// Missing field → 400 (an omitted value is not "clear").
	if rec, _ := call(http.MethodPut, `{}`, s.pgSaveAgentEffortOverride, id); rec.Code != 400 {
		t.Fatalf("missing reasoning_effort must be 400, got %d", rec.Code)
	}
	// Missing profile → 404.
	if rec, _ := call(http.MethodPut, `{"reasoning_effort":"high"}`, s.pgSaveAgentEffortOverride, 99999999); rec.Code != 404 {
		t.Fatalf("missing profile must be 404, got %d", rec.Code)
	}
	// Valid save → 204, and GET reflects it.
	if rec, _ := call(http.MethodPut, `{"reasoning_effort":"high"}`, s.pgSaveAgentEffortOverride, id); rec.Code != 204 {
		t.Fatalf("save: %d (%s)", rec.Code, rec.Body.String())
	}
	rec, out := call(http.MethodGet, "", s.pgAgentEffortOverrides, 0)
	if rec.Code != 200 {
		t.Fatalf("get: %d", rec.Code)
	}
	overrides, _ := out["overrides"].([]any)
	if len(overrides) != 1 {
		t.Fatalf("get overrides = %v", out)
	}
	if got := overrides[0].(map[string]any); got["profile_id"].(float64) != float64(id) || got["reasoning_effort"] != "high" {
		t.Fatalf("get override payload = %v", got)
	}
	// The same GET carries every profile with its effective value, so the Agent
	// page needs one call, not N+1.
	profs, _ := out["profiles"].([]any)
	var mine map[string]any
	for _, raw := range profs {
		if p, ok := raw.(map[string]any); ok && p["profile_id"].(float64) == float64(id) {
			mine = p
		}
	}
	if mine == nil || mine["source"] != "override" || mine["effort"] != "high" || mine["has_key"] != true {
		t.Fatalf("profile row = %v", mine)
	}
	// DELETE → 204 (idempotent), then GET is an empty (non-nil) list.
	if rec, _ := call(http.MethodDelete, "", s.pgDeleteAgentEffortOverride, id); rec.Code != 204 {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec, _ := call(http.MethodDelete, "", s.pgDeleteAgentEffortOverride, id); rec.Code != 204 {
		t.Fatalf("second delete must stay 204, got %d", rec.Code)
	}
	rec, out = call(http.MethodGet, "", s.pgAgentEffortOverrides, 0)
	if rec.Code != 200 {
		t.Fatalf("get after delete: %d", rec.Code)
	}
	if overrides, ok := out["overrides"].([]any); !ok || len(overrides) != 0 {
		t.Fatalf("overrides must be an empty array, got %v", out["overrides"])
	}
	// Empty effort on PUT also clears (204).
	if rec, _ := call(http.MethodPut, `{"reasoning_effort":"low"}`, s.pgSaveAgentEffortOverride, id); rec.Code != 204 {
		t.Fatalf("save low: %d", rec.Code)
	}
	if rec, _ := call(http.MethodPut, `{"reasoning_effort":""}`, s.pgSaveAgentEffortOverride, id); rec.Code != 204 {
		t.Fatalf("clear via PUT: %d", rec.Code)
	}
	// Clearing an override for a profile that does not exist is a 404, matching
	// DELETE — not a silent 204 that hides a typo in the profile id.
	if rec, _ := call(http.MethodPut, `{"reasoning_effort":""}`, s.pgSaveAgentEffortOverride, 99999999); rec.Code != 404 {
		t.Fatalf("clear unknown profile via PUT must be 404, got %d", rec.Code)
	}
}

// TestAgentProfileWithoutOverrideMatchesProfile pins the compatibility half of
// the feature: an agent with NO override must resolve exactly to its profile's
// own configuration — same config, and the same cached provider instance, so
// adding the override layer cannot silently change an existing deployment's
// request parameters or split its rate limiter.
func TestAgentProfileWithoutOverrideMatchesProfile(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer m.Close()
	td := t.TempDir()
	s := New(context.Background(), m, td, td, td)
	pg := m.pg

	suffix := time.Now().UnixNano()
	id, err := pg.SaveProfile(&db.LLMProfile{
		Name: fmt.Sprintf("no-override-%d", suffix), Format: "openai", Model: "m",
		APIKey: "probe-key", Streaming: true, ReasoningEffort: "low",
	})
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteProfile(id) })
	agentKey := fmt.Sprintf("t_noov_%d", suffix)
	if _, err := pg.Exec(`INSERT INTO agents(key, name, role) VALUES ($1,$1,'worker')`, agentKey); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM agents WHERE key=$1`, agentKey) })

	provProfile, cfgProfile, ok := s.providerForProfile(id)
	if !ok {
		t.Fatal("profile provider unavailable")
	}
	provAgent, cfgAgent, ok := s.providerForAgentProfile(agentKey, id)
	if !ok {
		t.Fatal("agent provider unavailable")
	}
	if cfgAgent != cfgProfile {
		t.Fatalf("no override must reproduce the profile config verbatim:\n agent=%+v\nprofile=%+v", cfgAgent, cfgProfile)
	}
	if provAgent != provProfile {
		t.Fatal("no override must share the profile's cached provider (one rate limiter)")
	}

	// With an override, ONLY the effort changes; every other request parameter
	// stays the profile's, and the provider is a distinct (profile, effort) entry.
	if err := pg.SetEffortOverride(agentKey, id, "max"); err != nil {
		t.Fatalf("set override: %v", err)
	}
	s.invalidateProfileAgents()
	provMax, cfgMax, ok := s.providerForAgentProfile(agentKey, id)
	if !ok {
		t.Fatal("overridden provider unavailable")
	}
	if cfgMax.ReasoningEffort != "max" {
		t.Fatalf("override effort = %q, want max", cfgMax.ReasoningEffort)
	}
	if provMax == provProfile {
		t.Fatal("a different effort must not reuse the profile-effort provider")
	}
	cfgMax.ReasoningEffort = cfgProfile.ReasoningEffort
	if cfgMax != cfgProfile {
		t.Fatalf("override must not touch other parameters:\n  got=%+v\n want=%+v", cfgMax, cfgProfile)
	}
}

// TestFailoverTargetResolvesItsOwnEffort exercises the runtime selection layer:
// once the task chain advances to another profile, configForSelection must build
// that profile's config with THAT profile's (agent, profile) override — never the
// profile the run just left. This is the failover half of the effort rule; the
// pure precedence test alone cannot catch a caller that carries the old value.
func TestFailoverTargetResolvesItsOwnEffort(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer m.Close()
	td := t.TempDir()
	s := New(context.Background(), m, td, td, td)
	pg := m.pg

	suffix := time.Now().UnixNano()
	mk := func(name string) int64 {
		t.Helper()
		id, err := pg.SaveProfile(&db.LLMProfile{
			Name: fmt.Sprintf("%s-%d", name, suffix), Format: "openai", Model: "m",
			APIKey: "probe-key", Streaming: true, ReasoningEffort: "low",
		})
		if err != nil {
			t.Fatalf("create profile %s: %v", name, err)
		}
		t.Cleanup(func() { _ = pg.DeleteProfile(id) })
		return id
	}
	agentKey := fmt.Sprintf("t_failover_%d", suffix)
	if _, err := pg.Exec(`INSERT INTO agents(key, name, role) VALUES ($1,$1,'worker')`, agentKey); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM agents WHERE key=$1`, agentKey) })

	first, second := mk("failover-first"), mk("failover-second")
	// Different overrides per target: if the runtime carried first's value over,
	// the second selection would still report xhigh.
	if err := pg.SetEffortOverride(agentKey, first, "xhigh"); err != nil {
		t.Fatalf("override first: %v", err)
	}
	if err := pg.SetEffortOverride(agentKey, second, "max"); err != nil {
		t.Fatalf("override second: %v", err)
	}

	cfgFirst, ok := s.configForSelection(agentKey, taskLLMSelection{profileID: first})
	if !ok || cfgFirst.ReasoningEffort != "xhigh" {
		t.Fatalf("first selection effort = %q ok=%v, want xhigh", cfgFirst.ReasoningEffort, ok)
	}
	// Failover target: resolution must run against the target profile.
	cfgSecond, ok := s.configForSelection(agentKey, taskLLMSelection{profileID: second})
	if !ok || cfgSecond.ReasoningEffort != "max" {
		t.Fatalf("failover target effort = %q ok=%v, want max (not first's xhigh)", cfgSecond.ReasoningEffort, ok)
	}
	// A profile without an override for this agent keeps its own profile effort.
	third := mk("failover-third")
	cfgThird, ok := s.configForSelection(agentKey, taskLLMSelection{profileID: third})
	if !ok || cfgThird.ReasoningEffort != "low" {
		t.Fatalf("uncovered target effort = %q ok=%v, want profile low", cfgThird.ReasoningEffort, ok)
	}
}
