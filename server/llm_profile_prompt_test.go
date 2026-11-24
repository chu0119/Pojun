package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	if !chain.LLM.PromptActive || chain.LLM.Source != "agent_binding" || chain.LLM.PromptChars == 0 {
		t.Fatalf("llm overlay mismatch: %+v", chain.LLM)
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
