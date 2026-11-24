package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chu0119/pojun/db"
)

func TestNormalizeProfilePromptRequest(t *testing.T) {
	// Empty text + no explicit mode defaults to prepend.
	pi, err := normalizeProfilePromptRequest(db.PromptInfo{}, strPtr("你是测试角色。"), nil, nil)
	if err != "" {
		t.Fatalf("unexpected error: %s", err)
	}
	if pi.Text != "你是测试角色。" || pi.Mode != "prepend" {
		t.Fatalf("default mode mismatch: %+v", pi)
	}

	// Missing fields preserve the stored value.
	old := db.PromptInfo{Text: "old", Mode: "append", ApplyInherited: true}
	pi, err = normalizeProfilePromptRequest(old, nil, nil, nil)
	if err != "" || pi != old {
		t.Fatalf("omitted fields must preserve old: %+v err=%s", pi, err)
	}

	// Explicit empty text clears the prompt and its mode.
	pi, err = normalizeProfilePromptRequest(old, strPtr(""), nil, nil)
	if err != "" || pi.Text != "" || pi.Mode != "" {
		t.Fatalf("explicit clear mismatch: %+v err=%s", pi, err)
	}

	// CRLF normalization + trailing whitespace trim.
	pi, err = normalizeProfilePromptRequest(db.PromptInfo{}, strPtr("a\r\nb\r\n  "), strPtr("append"), nil)
	if err != "" || pi.Text != "a\nb" || pi.Mode != "append" {
		t.Fatalf("normalization mismatch: %+v err=%s", pi, err)
	}

	// NUL / invalid mode / over-length are rejected with user-facing messages.
	if _, err := normalizeProfilePromptRequest(db.PromptInfo{}, strPtr("a\x00b"), nil, nil); err == "" {
		t.Fatal("NUL must be rejected")
	}
	if _, err := normalizeProfilePromptRequest(db.PromptInfo{}, strPtr("x"), strPtr("bogus"), nil); err == "" {
		t.Fatal("invalid mode must be rejected")
	}
	if _, err := normalizeProfilePromptRequest(db.PromptInfo{}, strPtr(strings.Repeat("a", 8001)), nil, nil); err == "" {
		t.Fatal("over-length prompt must be rejected")
	}
}

func strPtr(s string) *string { return &s }

func TestPromptInactiveReasonNoProfile(t *testing.T) {
	if got := promptInactiveReason(nil, db.PromptInfo{}, "none", nil); got != "当前没有解析到 LLM 配置" {
		t.Fatalf("no-profile reason = %q", got)
	}
}

func TestProfilePromptChatResolution(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { m.Close() })
	s := New(context.Background(), m, "../skills", t.TempDir(), t.TempDir())
	t.Cleanup(func() { s.Close() })

	name := "prompt-chat-" + time.Now().Format("150405.000000")
	id, err := s.m.pg.SaveProfile(&db.LLMProfile{Name: name, Format: "openai", Model: "m", APIKey: "sk-test"})
	if err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	t.Cleanup(func() { _ = s.m.pg.DeleteProfile(id) })
	if err := s.m.pg.SetProfilePrompt(id, "PIN PROMPT", "append", false); err != nil {
		t.Fatalf("SetProfilePrompt: %v", err)
	}

	conv := &db.Conversation{AgentKey: "pentest", LLMProfileID: &id}
	pp := s.profilePromptForChat(conv)
	if pp.Text != "PIN PROMPT" || pp.Mode != "append" || pp.Source != "conversation_pin" {
		t.Fatalf("conversation pin overlay mismatch: %+v", pp)
	}

	// An agent binding outranks the conversation pin.
	if err := s.m.pg.SetAgentLLMProfile("pentest", &id); err != nil {
		t.Fatalf("bind agent: %v", err)
	}
	t.Cleanup(func() { _ = s.m.pg.SetAgentLLMProfile("pentest", nil) })
	pp = s.profilePromptForChat(&db.Conversation{AgentKey: "pentest"})
	if pp.Text != "PIN PROMPT" || pp.Source != "agent_binding" {
		t.Fatalf("agent binding overlay mismatch: %+v", pp)
	}
}

func TestAgentPromptChainHandler(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { m.Close() })
	s := New(context.Background(), m, "../skills", t.TempDir(), t.TempDir())
	t.Cleanup(func() { s.Close() })

	name := "prompt-chain-" + time.Now().Format("150405.000000")
	id, err := s.m.pg.SaveProfile(&db.LLMProfile{Name: name, Format: "openai", Model: "m", APIKey: "sk-test"})
	if err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	t.Cleanup(func() { _ = s.m.pg.DeleteProfile(id) })
	if err := s.m.pg.SetProfilePrompt(id, "PROFILE CHAIN PROMPT", "prepend", true); err != nil {
		t.Fatalf("SetProfilePrompt: %v", err)
	}
	if err := s.m.pg.SetAgentLLMProfile("worker", &id); err != nil {
		t.Fatalf("bind worker: %v", err)
	}
	t.Cleanup(func() { _ = s.m.pg.SetAgentLLMProfile("worker", nil) })

	req := httptest.NewRequest(http.MethodGet, "/api/agents/worker/prompt/chain", nil)
	req.SetPathValue("key", "worker")
	rr := httptest.NewRecorder()
	s.pgAgentPromptChain(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var chain agentPromptChain
	if err := json.Unmarshal(rr.Body.Bytes(), &chain); err != nil {
		t.Fatalf("decode chain: %v", err)
	}
	if chain.AgentKey != "worker" || !chain.StaticOnly {
		t.Fatalf("chain meta mismatch: %+v", chain)
	}
	if chain.Warnings == nil || chain.Segments == nil || chain.MCP == nil || chain.Skills == nil {
		t.Fatalf("chain arrays must not be null: warnings=%v segments=%v mcp=%v skills=%v", chain.Warnings, chain.Segments, chain.MCP, chain.Skills)
	}
	if chain.Tools.Builtin == nil || chain.Tools.Custom == nil || chain.Tools.Traffic == nil || chain.Tools.Shell == nil || chain.Tools.DeferredNames == nil || chain.Tools.Excluded == nil {
		t.Fatalf("tool arrays must not be null: %+v", chain.Tools)
	}
	badReq := httptest.NewRequest(http.MethodGet, "/api/agents/worker/prompt/chain?profile_id=abc", nil)
	badReq.SetPathValue("key", "worker")
	badRR := httptest.NewRecorder()
	s.pgAgentPromptChain(badRR, badReq)
	if badRR.Code != http.StatusBadRequest {
		t.Fatalf("invalid profile_id status=%d body=%s", badRR.Code, badRR.Body.String())
	}
	if !chain.LLM.PromptActive || chain.LLM.Source != "agent_binding" || chain.LLM.PromptChars == 0 {
		t.Fatalf("llm overlay mismatch: %+v", chain.LLM)
	}
	if chain.LLM.ProfileID == nil || *chain.LLM.ProfileID != id {
		t.Fatalf("llm profile id = %v, want %d", chain.LLM.ProfileID, id)
	}
	find := func(key string) (promptChainSegment, bool) {
		for _, seg := range chain.Segments {
			if seg.Key == key {
				return seg, true
			}
		}
		return promptChainSegment{}, false
	}
	profile, ok := find("profile_prompt")
	if !ok || !profile.Present || !strings.Contains(profile.Text, "PROFILE CHAIN PROMPT") {
		t.Fatalf("profile segment missing: %+v", profile)
	}
	if profile.Order != 1 {
		t.Fatalf("prepend profile must be first, got order %d", profile.Order)
	}
	body, ok := find("body")
	if !ok || !body.Present || !body.Editable {
		t.Fatalf("body segment missing: %+v", body)
	}
	if skill, ok := find("skill_index"); !ok || skill.Available != true {
		t.Fatalf("skill segment missing: %+v", skill)
	}
	if seg, ok := find("scanner_first"); !ok || seg.Kind != "code_tail" {
		t.Fatalf("code tail missing: %+v", seg)
	}
	if seg, ok := find("runtime_intent"); !ok || seg.Available || seg.Present {
		t.Fatalf("runtime segment must be a non-present injection point: %+v", seg)
	}
	if chain.Totals.Chars == 0 || chain.Totals.EstTokens == 0 {
		t.Fatalf("totals empty: %+v", chain.Totals)
	}

	// replace: the body leaves the request, but the chain still reports how much
	// text was dropped instead of showing 0.
	if err := s.m.pg.SetProfilePrompt(id, "PROFILE CHAIN PROMPT", "replace", true); err != nil {
		t.Fatalf("SetProfilePrompt(replace): %v", err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/agents/worker/prompt/chain", nil)
	req.SetPathValue("key", "worker")
	rr = httptest.NewRecorder()
	s.pgAgentPromptChain(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("replace status=%d body=%s", rr.Code, rr.Body.String())
	}
	chain = agentPromptChain{}
	if err := json.Unmarshal(rr.Body.Bytes(), &chain); err != nil {
		t.Fatalf("decode replace chain: %v", err)
	}
	profile, ok = find("profile_prompt")
	if !ok || !profile.Present || !strings.Contains(profile.Text, "PROFILE CHAIN PROMPT") {
		t.Fatalf("replace profile segment mismatch: %+v", profile)
	}
	body, ok = find("body")
	if !ok || body.Present || body.Text != "" || body.Chars == 0 || !strings.Contains(body.Reason, "原正文") {
		t.Fatalf("replace body must keep its original size: %+v", body)
	}
}

func TestAgentPromptChainInactiveProfileExposesText(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { m.Close() })
	s := New(context.Background(), m, "../skills", t.TempDir(), t.TempDir())
	t.Cleanup(func() { s.Close() })

	prev, _ := s.m.pg.ActiveProfile()
	name := "prompt-chain-inactive-" + time.Now().Format("150405.000000")
	id, err := s.m.pg.SaveProfile(&db.LLMProfile{Name: name, Format: "openai", Model: "m", APIKey: "sk-test"})
	if err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	t.Cleanup(func() {
		if prev != nil {
			_ = s.m.pg.SetActiveProfile(prev.ID)
		} else {
			_, _ = s.m.pg.Exec(`UPDATE llm_profiles SET is_default=false`)
		}
		_, _ = s.m.pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1`, id)
		_ = s.m.pg.DeleteProfile(id)
	})
	if err := s.m.pg.SetProfilePrompt(id, "INACTIVE PROFILE TEXT", "append", false); err != nil {
		t.Fatalf("SetProfilePrompt: %v", err)
	}
	if err := s.m.pg.SetActiveProfile(id); err != nil {
		t.Fatalf("SetActiveProfile: %v", err)
	}
	if err := s.m.pg.SetAgentLLMProfile("worker", nil); err != nil {
		t.Fatalf("clear worker binding: %v", err)
	}
	a, err := s.m.pg.GetAgentByKey("worker")
	if err != nil || a == nil {
		t.Fatalf("GetAgentByKey(worker): %v", err)
	}
	chain, err := s.buildAgentPromptChain(s.m.pg, a, "")
	if err != nil {
		t.Fatalf("buildAgentPromptChain: %v", err)
	}
	if chain.LLM.Source != "global_profile_inactive" || chain.LLM.PromptActive {
		t.Fatalf("global inactive overlay mismatch: %+v", chain.LLM)
	}
	if chain.LLM.SourceLabel != "全局激活配置 · 未生效" {
		t.Fatalf("inactive source label must stay neutral: %q", chain.LLM.SourceLabel)
	}
	if chain.LLM.ProfileID == nil || *chain.LLM.ProfileID != id {
		t.Fatalf("llm profile id = %v, want %d", chain.LLM.ProfileID, id)
	}
	var profile, body *promptChainSegment
	for i := range chain.Segments {
		switch chain.Segments[i].Key {
		case "profile_prompt":
			profile = &chain.Segments[i]
		case "body":
			body = &chain.Segments[i]
		}
	}
	if profile == nil || profile.Present || !profile.Available || profile.Order != 1 {
		t.Fatalf("inactive profile segment state mismatch: %+v", profile)
	}
	if !strings.Contains(profile.Text, "INACTIVE PROFILE TEXT") || profile.Chars == 0 {
		t.Fatalf("inactive profile segment must expose the configured text: %+v", profile)
	}
	if !strings.Contains(profile.Reason, "继承") {
		t.Fatalf("inactive reason must explain the inheritance gate: %q", profile.Reason)
	}
	if body == nil || !body.Present || body.Text == "" {
		t.Fatalf("body must remain assembled: %+v", body)
	}
	counted := 0
	for _, seg := range chain.Segments {
		if seg.Present && seg.Available {
			counted += seg.Chars
		}
	}
	if counted != chain.Totals.Chars {
		t.Fatalf("totals %d != counted segments %d", chain.Totals.Chars, counted)
	}
}

func TestProxyDisplayAddrMasksCredentials(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"not a proxy", "<出口代理已配置（地址不可展示）>"},
	}
	for _, c := range cases {
		if got := proxyDisplayAddr(c.in); got != c.want {
			t.Fatalf("proxyDisplayAddr(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// The masked value is a serialized URL, so "*" may be percent-encoded.
	// Assert the semantics (userinfo replaced, host preserved) instead of the
	// exact escaping, and require the real credentials to be absent.
	got := proxyDisplayAddr("http://user:pass@127.0.0.1:8080")
	if strings.Contains(got, "user") || strings.Contains(got, "pass") {
		t.Fatalf("credentials leaked into %q", got)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("masked value is not a URL: %q (%v)", got, err)
	}
	if u.User == nil || u.User.Username() != "***" {
		t.Fatalf("userinfo = %v, want masked username", u.User)
	}
	if _, hasPassword := u.User.Password(); hasPassword {
		t.Fatalf("masked URL must not keep a password: %q", got)
	}
	if u.Host != "127.0.0.1:8080" {
		t.Fatalf("host = %q, want 127.0.0.1:8080", u.Host)
	}
}

func TestProfilePromptForTaskRole(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { m.Close() })
	s := New(context.Background(), m, "../skills", t.TempDir(), t.TempDir())
	t.Cleanup(func() { s.Close() })

	prev, _ := s.m.pg.ActiveProfile()
	name := "prompt-task-role-" + time.Now().Format("150405.000000")
	keyed, err := s.m.pg.SaveProfile(&db.LLMProfile{Name: name, Format: "openai", Model: "m", APIKey: "sk-test"})
	if err != nil {
		t.Fatalf("SaveProfile(keyed): %v", err)
	}
	noKey, err := s.m.pg.SaveProfile(&db.LLMProfile{Name: name + "-nokey", Format: "openai", Model: "m"})
	if err != nil {
		t.Fatalf("SaveProfile(noKey): %v", err)
	}
	t.Cleanup(func() {
		if prev != nil {
			_ = s.m.pg.SetActiveProfile(prev.ID)
		} else {
			_, _ = s.m.pg.Exec(`UPDATE llm_profiles SET is_default=false`)
		}
		_, _ = s.m.pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1`, keyed)
		_, _ = s.m.pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1`, noKey)
		_ = s.m.pg.DeleteProfile(keyed)
		_ = s.m.pg.DeleteProfile(noKey)
	})
	if err := s.m.pg.SetProfilePrompt(keyed, "TASK ROLE PROMPT", "prepend", true); err != nil {
		t.Fatalf("SetProfilePrompt(keyed): %v", err)
	}
	if err := s.m.pg.SetProfilePrompt(noKey, "NO KEY PROMPT", "prepend", true); err != nil {
		t.Fatalf("SetProfilePrompt(noKey): %v", err)
	}
	if err := s.m.pg.SetAgentLLMProfile("worker", nil); err != nil {
		t.Fatalf("clear worker binding: %v", err)
	}
	t.Cleanup(func() { _ = s.m.pg.SetAgentLLMProfile("worker", nil) })
	if err := s.m.pg.SetActiveProfile(keyed); err != nil {
		t.Fatalf("SetActiveProfile(keyed): %v", err)
	}

	t0 := &Task{}
	pp := s.profilePromptForTaskRole(t0, "worker")
	if pp.Text != "TASK ROLE PROMPT" || pp.Source != "global_profile" {
		t.Fatalf("global inherited overlay mismatch: %+v", pp)
	}
	tChain := &Task{LLMProfileIDs: []int64{keyed}, ActiveLLMProfileID: &keyed}
	pp = s.profilePromptForTaskRole(tChain, "worker")
	if pp.Text != "TASK ROLE PROMPT" || pp.Source != "task_chain" {
		t.Fatalf("task-chain overlay mismatch: %+v", pp)
	}

	if err := s.m.pg.SetProfilePrompt(keyed, "TASK ROLE PROMPT", "prepend", false); err != nil {
		t.Fatalf("disable inheritance: %v", err)
	}
	if pp = s.profilePromptForTaskRole(t0, "worker"); pp.Active() {
		t.Fatalf("global inheritance off must not inject: %+v", pp)
	}
	if pp = s.profilePromptForTaskRole(tChain, "worker"); pp.Active() {
		t.Fatalf("task-chain inheritance off must not inject: %+v", pp)
	}

	if err := s.m.pg.SetAgentLLMProfile("worker", &keyed); err != nil {
		t.Fatalf("bind worker: %v", err)
	}
	pp = s.profilePromptForTaskRole(tChain, "worker")
	if pp.Text != "TASK ROLE PROMPT" || pp.Source != "agent_binding" {
		t.Fatalf("explicit binding must outrank the task chain: %+v", pp)
	}
	if err := s.m.pg.SetAgentLLMProfile("worker", nil); err != nil {
		t.Fatalf("clear worker binding: %v", err)
	}

	if err := s.m.pg.SetActiveProfile(noKey); err != nil {
		t.Fatalf("SetActiveProfile(noKey): %v", err)
	}
	if pp = s.profilePromptForTaskRole(t0, "worker"); pp.Active() {
		t.Fatalf("global profile without API key must not inject: %+v", pp)
	}
	tChainNoKey := &Task{LLMProfileIDs: []int64{noKey}, ActiveLLMProfileID: &noKey}
	if pp = s.profilePromptForTaskRole(tChainNoKey, "worker"); pp.Active() {
		t.Fatalf("task-chain profile without API key must not inject: %+v", pp)
	}
}

func TestSaveProfilePromptThreeState(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { m.Close() })
	s := New(context.Background(), m, "../skills", t.TempDir(), t.TempDir())
	t.Cleanup(func() { s.Close() })

	name := "prompt-save-" + time.Now().Format("150405.000000")
	save := func(body map[string]any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/llm/profiles", bytes.NewReader(raw))
		rr := httptest.NewRecorder()
		s.pgSaveProfile(rr, req)
		return rr
	}

	rr := save(map[string]any{
		"name": name, "format": "openai", "model": "m", "api_key": "sk-test",
		"prompt": "LONGTEXT", "prompt_mode": "append",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", rr.Code, rr.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil || created.ID == 0 {
		t.Fatalf("decode id: %v", err)
	}
	id := created.ID
	t.Cleanup(func() { _ = s.m.pg.DeleteProfile(id) })

	// Omitted prompt fields on update must keep the stored text.
	rr = save(map[string]any{"id": id, "name": name, "format": "openai", "model": "m2"})
	if rr.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", rr.Code, rr.Body.String())
	}
	pi, _ := s.m.pg.ProfilePrompt(id)
	if pi.Text != "LONGTEXT" || pi.Mode != "append" {
		t.Fatalf("omitted fields cleared the prompt: %+v", pi)
	}

	// Explicit empty text clears both text and mode.
	rr = save(map[string]any{"id": id, "name": name, "format": "openai", "model": "m2", "prompt": ""})
	if rr.Code != http.StatusOK {
		t.Fatalf("clear status=%d body=%s", rr.Code, rr.Body.String())
	}
	pi, _ = s.m.pg.ProfilePrompt(id)
	if pi.Text != "" || pi.Mode != "" {
		t.Fatalf("explicit clear failed: %+v", pi)
	}

	// Over-length is a 400, not a DB 500.
	rr = save(map[string]any{
		"id": id, "name": name, "format": "openai", "model": "m2",
		"prompt": strings.Repeat("a", 8001), "prompt_mode": "prepend",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("over-length should be 400, got %d: %s", rr.Code, rr.Body.String())
	}
}
