package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chu0119/pojun/db"
)

func TestStaticProfileAvailability(t *testing.T) {
	cases := []struct {
		name  string
		p     *db.LLMProfile
		key   bool
		want  string
		match string
	}{
		{"missing profile", nil, true, "invalid_config", "不存在"},
		{"no key", &db.LLMProfile{Format: "openai", Model: "m"}, false, "missing_key", "API Key"},
		{"no model", &db.LLMProfile{Format: "openai"}, true, "invalid_config", "模型"},
		{"bad format", &db.LLMProfile{Format: "bogus", Model: "m"}, true, "invalid_config", "格式"},
		{"bad url", &db.LLMProfile{Format: "openai", Model: "m", BaseURL: "://x"}, true, "invalid_config", "URL"},
		{"ok", &db.LLMProfile{Format: "openai", Model: "m", BaseURL: "https://api.example.com"}, true, "ok", "未验证连通性"},
	}
	for _, c := range cases {
		got, reason := staticProfileAvailability(c.p, c.key)
		if got != c.want || !strings.Contains(reason, c.match) {
			t.Fatalf("%s: got %q/%q want %q containing %q", c.name, got, reason, c.want, c.match)
		}
	}
}

func TestSanitizeRouteError(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		leaks []string
	}{
		{
			name:  "headers and params",
			in:    `upstream 429: Authorization: Bearer sk-live-abcdef123456 api_key=SECRET token:tok_123 https://user:pass@host/v1 failed`,
			leaks: []string{"sk-live-abcdef123456", "SECRET", "tok_123", "user:pass"},
		},
		{
			name:  "upper-case prefix and basic auth",
			in:    `denied SK-LIVE-ABCDEF123456 Authorization: Basic dXNlcjpwYXNz password=hunter2`,
			leaks: []string{"SK-LIVE-ABCDEF123456", "dXNlcjpwYXNz", "hunter2"},
		},
		{
			name:  "percent-encoded parameter",
			in:    `GET /v1?api_key%3Dabc123secret&model=x`,
			leaks: []string{"abc123secret"},
		},
		{
			name:  "json body with quoted keys",
			in:    `{"password":"hunter2","token":"tok_123","api_key":"abc123"}`,
			leaks: []string{"hunter2", "tok_123", "abc123"},
		},
		{
			name:  "quoted assignment and quoted bearer",
			in:    `password="hunter2" Authorization: Bearer "abcdef123"`,
			leaks: []string{"hunter2", "abcdef123"},
		},
		{
			name:  "encoded secret and password",
			in:    `?password%3Dhunter2&secret%3Dtopsecret`,
			leaks: []string{"hunter2", "topsecret"},
		},
		{
			name:  "benign text survives",
			in:    `upstream 402: insufficient balance for model claude-sonnet-5-5`,
			leaks: nil,
		},
	}
	for _, c := range cases {
		out := sanitizeRouteError(c.in)
		for _, leak := range c.leaks {
			if strings.Contains(strings.ToLower(out), strings.ToLower(leak)) {
				t.Fatalf("%s: leaked %q in %q", c.name, leak, out)
			}
		}
		if len(c.leaks) == 0 && !strings.Contains(out, "insufficient balance") {
			t.Fatalf("%s: benign text should survive, got %q", c.name, out)
		}
	}
	long := strings.Repeat("错", 800)
	if got := sanitizeRouteError(long); len([]rune(got)) > 501 {
		t.Fatalf("must truncate to ~500 runes, got %d", len([]rune(got)))
	}
}

// TestAgentLLMRouteStatic checks the projection's honest separation of the task
// chain and the provider pool, plus its no-secret guarantee. Assertions are
// relative (shared test DB carries other profiles), and always derived from the
// returned data rather than hard-coded totals.
func TestAgentLLMRouteStatic(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer m.Close()
	td := t.TempDir()
	s := New(context.Background(), m, td, td, td)
	pg := m.pg

	suffix := time.Now().UnixNano()
	mk := func(name string, priority int, excluded bool) int64 {
		t.Helper()
		id, err := pg.SaveProfile(&db.LLMProfile{
			Name: fmt.Sprintf("%s-%d", name, suffix), Format: "openai", Model: "m",
			APIKey: "probe-key-must-not-leak", Streaming: true,
			Priority: priority, PoolExclude: excluded, ReasoningEffort: "medium",
		})
		if err != nil {
			t.Fatalf("create profile %s: %v", name, err)
		}
		t.Cleanup(func() { _ = pg.DeleteProfile(id) })
		return id
	}
	head := mk("route-head", 8, false)
	second := mk("route-second", 3, false)
	excluded := mk("route-excluded", 9, true)
	noKey, err := pg.SaveProfile(&db.LLMProfile{
		Name: fmt.Sprintf("route-nokey-%d", suffix), Format: "openai", Model: "m", Streaming: true,
	})
	if err != nil {
		t.Fatalf("create no-key profile: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteProfile(noKey) })

	agentKey := fmt.Sprintf("t_route_%d", suffix)
	ag, err := pg.CreateAgent(agentKey, "route probe", "read-only projection test")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteAgent(agentKey) })

	pt, err := pg.CreateTaskWithOptions("route-probe", "probe",
		db.TaskCreateOptions{LLMProfileIDs: []int64{head, second}})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(pt.ID) })
	task := taskFromPG(pt, pg.Exploration(pt.ExplorationID), nil)

	route, err := s.buildAgentLLMRoute(pg, ag, task, 50, 0)
	if err != nil {
		t.Fatalf("build route: %v", err)
	}

	// Precedence order is fixed and always five layers.
	wantLayers := []string{"pin", "agent_binding", "task_chain", "global_profile", "environment"}
	if len(route.Precedence) != len(wantLayers) {
		t.Fatalf("precedence layers = %d, want %d", len(route.Precedence), len(wantLayers))
	}
	for i, layer := range wantLayers {
		if route.Precedence[i].Layer != layer {
			t.Fatalf("precedence[%d] = %q, want %q", i, route.Precedence[i].Layer, layer)
		}
	}
	if route.Precedence[0].AppliesTo != "compactor" {
		t.Fatalf("pin must be scoped to the compactor: %+v", route.Precedence[0])
	}

	// chat never runs on a task: its resolution must skip the task chain even
	// when the requested task has one.
	if _, ok := route.Roles["chat"]; !ok {
		t.Fatal("chat role must be projected")
	}
	if route.Roles["chat"].Source == "task_chain" {
		t.Fatalf("chat must not resolve through the task chain: %+v", route.Roles["chat"])
	}

	// Task chain: user order preserved, exactly one cursor (the first ready entry).
	if len(route.Chain) != 2 || route.Chain[0].ProfileID != head || route.Chain[1].ProfileID != second {
		t.Fatalf("chain order = %+v", route.Chain)
	}
	cursors := 0
	for _, c := range route.Chain {
		if c.IsCursor {
			cursors++
		}
	}
	if cursors != 1 || route.ActiveProfile == nil || *route.ActiveProfile != head {
		t.Fatalf("cursor = %v (count %d), want head %d", route.ActiveProfile, cursors, head)
	}

	// Pool: self-consistent ordering and eligibility, independent of the task chain.
	prevActive, prevPriority := true, int(^uint(0)>>1)
	seen := map[int64]bool{}
	for _, mem := range route.Pool.Members {
		var id int64
		fmt.Sscanf(mem.ProfileID, "%d", &id)
		seen[id] = true
		if mem.Active {
			if !prevActive {
				t.Fatalf("active member must sort first: %+v", mem)
			}
		} else {
			prevActive = false
			if mem.Priority > prevPriority {
				t.Fatalf("priority order broken: %+v", mem)
			}
			prevPriority = mem.Priority
		}
		if mem.InChain != (mem.HasKey && (mem.Active || !mem.Excluded)) {
			t.Fatalf("in_chain inconsistent for %+v", mem)
		}
	}
	if !seen[excluded] {
		t.Fatalf("excluded profile must still be listed for context")
	}
	for _, mem := range route.Pool.Members {
		if mem.ProfileID == fmt.Sprint(excluded) && mem.InChain {
			t.Fatalf("excluded profile must not be in the failover chain")
		}
		if mem.ProfileID == fmt.Sprint(noKey) && mem.HasKey {
			t.Fatalf("profile without a key must report has_key=false")
		}
	}
	if !strings.Contains(route.Pool.Note, "不影响任务链") {
		t.Fatalf("pool note must state the independence: %q", route.Pool.Note)
	}

	// No secret shapes anywhere in the serialized projection.
	raw, err := json.Marshal(route)
	if err != nil {
		t.Fatalf("marshal route: %v", err)
	}
	for _, banned := range []string{"probe-key-must-not-leak", "api_key", "base_url", "proxy"} {
		if strings.Contains(string(raw), banned) {
			t.Fatalf("route response leaks %q", banned)
		}
	}
}

func TestAgentLLMRouteHistoryAndErrors(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer m.Close()
	td := t.TempDir()
	s := New(context.Background(), m, td, td, td)
	pg := m.pg

	suffix := time.Now().UnixNano()
	agentKey := fmt.Sprintf("t_route_hist_%d", suffix)
	ag, err := pg.CreateAgent(agentKey, "route history probe", "")
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteAgent(agentKey) })
	pt, err := pg.CreateTaskWithOptions("route-history", "probe", db.TaskCreateOptions{})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(pt.ID) })
	store := pg.Exploration(pt.ExplorationID)
	task := taskFromPG(pt, store, nil)

	meta := func(mode, reason string) json.RawMessage {
		raw, _ := json.Marshal(llmActivityMetadata{LLMTransition: llmTransitionAudit{
			Mode: mode, Reason: reason,
			Previous: &llmAuditProfile{ID: 1, Name: "A", Effort: "high", EffortSource: "override"},
			Next:     &llmAuditProfile{ID: 2, Name: "B"},
		}})
		return raw
	}
	if _, err := store.AppendActivity(db.Activity{Worker: "system", Kind: "llm_switch", Summary: "quota", Metadata: meta("automatic", "quota exhausted for Bearer sk-abc123")}); err != nil {
		t.Fatalf("append switch: %v", err)
	}
	if _, err := store.AppendActivity(db.Activity{Worker: "system", Kind: "llm_failover", IsError: true, Summary: "exhausted", Metadata: meta("exhausted", "chain exhausted")}); err != nil {
		t.Fatalf("append failover: %v", err)
	}
	if _, err := store.AppendActivity(db.Activity{Worker: "worker", Kind: "thinking", Summary: "not a route event"}); err != nil {
		t.Fatalf("append thinking: %v", err)
	}

	route, err := s.buildAgentLLMRoute(pg, ag, task, 50, 0)
	if err != nil {
		t.Fatalf("build route: %v", err)
	}
	if route.History.State != "ok" || len(route.History.Items) != 2 {
		t.Fatalf("history = %+v", route.History)
	}
	// Both scopes present newest first ("load earlier" prepends older rows).
	if route.History.Items[0].Mode != "exhausted" || route.History.Items[1].Mode != "automatic" {
		t.Fatalf("history modes = %+v", route.History.Items)
	}
	if route.History.Items[1].Previous == nil || route.History.Items[1].Previous.Effort != "high" {
		t.Fatalf("effort snapshot must survive: %+v", route.History.Items[1].Previous)
	}
	if strings.Contains(route.History.Items[1].Reason, "sk-abc123") {
		t.Fatalf("reason must be masked: %q", route.History.Items[1].Reason)
	}
	if route.History.EarliestCursor != route.History.Items[len(route.History.Items)-1].ID {
		t.Fatalf("earliest cursor = %d, want oldest item %d", route.History.EarliestCursor, route.History.Items[len(route.History.Items)-1].ID)
	}

	// A task with no events yields an explicit no_records state and an empty array.
	pt2, err := pg.CreateTaskWithOptions("route-empty", "probe", db.TaskCreateOptions{})
	if err != nil {
		t.Fatalf("create empty task: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(pt2.ID) })
	route2, err := s.buildAgentLLMRoute(pg, ag, taskFromPG(pt2, pg.Exploration(pt2.ExplorationID), nil), 50, 0)
	if err != nil {
		t.Fatalf("build empty route: %v", err)
	}
	if route2.History.State != "no_records" || route2.History.Items == nil || len(route2.History.Items) != 0 {
		t.Fatalf("empty history = %+v", route2.History)
	}

	// Handler validation: bad task_id → 400; unknown task → 404; unknown agent → 404.
	call := func(query string, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/agents/"+key+"/llm-route?"+query, nil)
		req.SetPathValue("key", key)
		rec := httptest.NewRecorder()
		s.pgAgentLLMRoute(rec, req)
		return rec
	}
	if rec := call("task_id=abc", agentKey); rec.Code != 400 {
		t.Fatalf("bad task_id = %d", rec.Code)
	}
	if rec := call("task_id="+strconv.Itoa(999999999), agentKey); rec.Code != 404 {
		t.Fatalf("unknown task = %d", rec.Code)
	}
	if rec := call("", "no_such_agent"); rec.Code != 404 {
		t.Fatalf("unknown agent = %d", rec.Code)
	}
	if rec := call("limit=999", agentKey); rec.Code != 200 {
		t.Fatalf("clamped limit must be accepted, got %d", rec.Code)
	}
}

// TestAgentLLMRouteLastUsed pins the ledger-backed "last actually used" half:
// it reports the newest llm_usage row for the agent's lane, and absence of a row
// is reported as nil (not a zero-valued struct that would look like a real call).
func TestAgentLLMRouteLastUsed(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer m.Close()
	td := t.TempDir()
	s := New(context.Background(), m, td, td, td)
	pg := m.pg

	ag, err := pg.GetAgentByKey("worker")
	if err != nil || ag == nil {
		t.Fatalf("builtin worker agent missing: %v", err)
	}
	pt, err := pg.CreateTaskWithOptions("route-last-used", "probe", db.TaskCreateOptions{})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(pt.ID) })
	task := taskFromPG(pt, pg.Exploration(pt.ExplorationID), nil)

	// Older row must be shadowed by the newer one; status matters (a failed last
	// call is still the last call).
	if _, err := pg.Exec(`INSERT INTO llm_usage(task_id, worker, model, profile_name, status, ts)
VALUES ($1,'worker','m-old','OLD','ok', now() - interval '1 hour')`, strconv.FormatInt(pt.ID, 10)); err != nil {
		t.Fatalf("insert old usage: %v", err)
	}
	if _, err := pg.Exec(`INSERT INTO llm_usage(task_id, worker, model, profile_name, status, ts)
VALUES ($1,'worker','m-new','NEW','error', now())`, strconv.FormatInt(pt.ID, 10)); err != nil {
		t.Fatalf("insert new usage: %v", err)
	}

	route, err := s.buildAgentLLMRoute(pg, ag, task, 50, 0)
	if err != nil {
		t.Fatalf("build route: %v", err)
	}
	if route.LastUsed == nil {
		t.Fatal("last_used must be present when the lane has ledger rows")
	}
	if route.LastUsed.ProfileName != "NEW" || route.LastUsed.Model != "m-new" ||
		route.LastUsed.Worker != "worker" || route.LastUsed.Status != "error" {
		t.Fatalf("last_used = %+v, want the newest worker row", route.LastUsed)
	}
	if route.HistoryNote == "" {
		t.Fatal("last_used must carry its boundary note")
	}

	// A task with no recorded call yields nil, not an empty struct.
	pt2, err := pg.CreateTaskWithOptions("route-last-used-empty", "probe", db.TaskCreateOptions{})
	if err != nil {
		t.Fatalf("create empty task: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(pt2.ID) })
	route2, err := s.buildAgentLLMRoute(pg, ag, taskFromPG(pt2, pg.Exploration(pt2.ExplorationID), nil), 50, 0)
	if err != nil {
		t.Fatalf("build empty route: %v", err)
	}
	if route2.LastUsed != nil {
		t.Fatalf("empty ledger must yield nil last_used, got %+v", route2.LastUsed)
	}
}
