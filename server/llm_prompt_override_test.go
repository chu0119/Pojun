package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chu0119/pojun/db"
)

func TestNormalizeAgentPromptRequest(t *testing.T) {
	// Empty text stores the explicit "disabled" state, dropping any mode.
	text, mode, msg := normalizeAgentPromptRequest("", "append")
	if msg != "" || text != "" || mode != "" {
		t.Fatalf("empty text must clear the mode: text=%q mode=%q msg=%q", text, mode, msg)
	}

	// Non-empty text without a mode defaults to prepend, like the default layer.
	text, mode, msg = normalizeAgentPromptRequest("hello", "")
	if msg != "" || text != "hello" || mode != "prepend" {
		t.Fatalf("default mode mismatch: text=%q mode=%q msg=%q", text, mode, msg)
	}

	// CRLF plus NBSP (U+00A0) and U+2028 are normalized away, matching JS trim.
	text, mode, msg = normalizeAgentPromptRequest("a\r\nb  ", "append")
	if msg != "" || text != "a\nb" || mode != "append" {
		t.Fatalf("normalization mismatch: text=%q mode=%q msg=%q", text, mode, msg)
	}

	if _, _, msg = normalizeAgentPromptRequest("a\x00b", ""); msg == "" {
		t.Fatal("NUL must be rejected")
	}
	if _, _, msg = normalizeAgentPromptRequest("x", "bogus"); msg == "" {
		t.Fatal("invalid mode must be rejected")
	}
	if _, _, msg = normalizeAgentPromptRequest(strings.Repeat("好", 65536), ""); msg == "" {
		t.Fatal("over-length prompt must be rejected")
	}
	if _, _, msg = normalizeAgentPromptRequest(strings.Repeat("好", 65535), ""); msg != "" {
		t.Fatalf("65535-rune prompt must pass: %s", msg)
	}
}

// newTempAgent inserts a throwaway agent row so agent-scoped override tests never
// touch the built-in agents another test may be using.
func newTempAgent(t *testing.T, pg *db.DB, prefix string) string {
	t.Helper()
	key := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	if _, err := pg.Exec(`INSERT INTO agents(key, name, role) VALUES ($1,$1,'worker')`, key); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() { _, _ = pg.Exec(`DELETE FROM agents WHERE key=$1`, key) })
	return key
}

func TestAgentProfilePromptStoreThreeStates(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { m.Close() })
	pg := m.pg

	id, err := pg.SaveProfile(&db.LLMProfile{
		Name: "agent-prompt-store-" + time.Now().Format("150405.000000"), Format: "openai", Model: "m", APIKey: "sk-test",
	})
	if err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteProfile(id) })
	agentKey := newTempAgent(t, pg, "t_app_store")

	if _, ok, err := pg.AgentProfilePrompt(agentKey, id); err != nil || ok {
		t.Fatalf("no row expected: ok=%v err=%v", ok, err)
	}
	if err := pg.SetAgentProfilePrompt(agentKey, id, "OVERRIDE", "append"); err != nil {
		t.Fatalf("SetAgentProfilePrompt: %v", err)
	}
	row, ok, err := pg.AgentProfilePrompt(agentKey, id)
	if err != nil || !ok || row.Text != "OVERRIDE" || row.Mode != "append" {
		t.Fatalf("roundtrip mismatch: %+v ok=%v err=%v", row, ok, err)
	}
	// Empty text is a stored row (explicit disable), not a delete.
	if err := pg.SetAgentProfilePrompt(agentKey, id, "", ""); err != nil {
		t.Fatalf("SetAgentProfilePrompt(disable): %v", err)
	}
	row, ok, err = pg.AgentProfilePrompt(agentKey, id)
	if err != nil || !ok || row.Text != "" || row.Mode != "" {
		t.Fatalf("disabled row mismatch: %+v ok=%v err=%v", row, ok, err)
	}
	if found, err := pg.DeleteAgentProfilePrompt(agentKey, id); err != nil || !found {
		t.Fatalf("DeleteAgentProfilePrompt: found=%v err=%v", found, err)
	}
	if _, ok, err := pg.AgentProfilePrompt(agentKey, id); err != nil || ok {
		t.Fatalf("row must be gone: ok=%v err=%v", ok, err)
	}
	if found, err := pg.DeleteAgentProfilePrompt(agentKey, id); err != nil || found {
		t.Fatalf("second delete must be idempotent: found=%v err=%v", found, err)
	}
	// Unknown agent/profile are sentinel errors the API maps to 404.
	if err := pg.SetAgentProfilePrompt("no_such_agent", id, "x", "prepend"); err == nil || !strings.Contains(err.Error(), "agent not found") {
		t.Fatalf("unknown agent error = %v", err)
	}
	if err := pg.SetAgentProfilePrompt(agentKey, 99999999, "x", "prepend"); err == nil {
		t.Fatal("unknown profile must error")
	}
}

func TestAgentPromptOverrideResolution(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { m.Close() })
	s := New(context.Background(), m, "../skills", t.TempDir(), t.TempDir())
	t.Cleanup(func() { s.Close() })
	pg := m.pg

	prev, _ := pg.ActiveProfile()
	id, err := pg.SaveProfile(&db.LLMProfile{
		Name: "agent-prompt-resolve-" + time.Now().Format("150405.000000"), Format: "openai", Model: "m", APIKey: "sk-test",
	})
	if err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	t.Cleanup(func() {
		if prev != nil {
			_ = pg.SetActiveProfile(prev.ID)
		}
		_, _ = pg.Exec(`UPDATE llm_profiles SET is_default=false WHERE id=$1`, id)
		_ = pg.DeleteProfile(id)
	})
	if err := pg.SetProfilePrompt(id, "PROFILE DEFAULT", "prepend", false); err != nil {
		t.Fatalf("SetProfilePrompt: %v", err)
	}
	agentKey := newTempAgent(t, pg, "t_app_resolve")
	if err := pg.SetAgentLLMProfile(agentKey, &id); err != nil {
		t.Fatalf("bind agent: %v", err)
	}

	// 1. No override row → the profile default applies (explicit binding).
	pp := s.profilePromptForTaskRole(&Task{}, agentKey)
	if pp.Text != "PROFILE DEFAULT" || pp.Source != "agent_binding" {
		t.Fatalf("default layer mismatch: %+v", pp)
	}

	// 2. Non-empty override wins over the default and says so.
	if err := pg.SetAgentProfilePrompt(agentKey, id, "AGENT OVERRIDE", "append"); err != nil {
		t.Fatalf("SetAgentProfilePrompt: %v", err)
	}
	pp = s.profilePromptForTaskRole(&Task{}, agentKey)
	if pp.Text != "AGENT OVERRIDE" || pp.Mode != "append" || pp.Source != "agent_override" {
		t.Fatalf("override mismatch: %+v", pp)
	}

	// 3. Empty override row → explicitly no injection, even for a bound profile.
	if err := pg.SetAgentProfilePrompt(agentKey, id, "", ""); err != nil {
		t.Fatalf("SetAgentProfilePrompt(disable): %v", err)
	}
	if pp = s.profilePromptForTaskRole(&Task{}, agentKey); pp.Active() {
		t.Fatalf("disabled override must not inject: %+v", pp)
	}

	// 4. Deleting the row restores inheritance.
	if _, err := pg.DeleteAgentProfilePrompt(agentKey, id); err != nil {
		t.Fatalf("DeleteAgentProfilePrompt: %v", err)
	}
	if pp = s.profilePromptForTaskRole(&Task{}, agentKey); pp.Text != "PROFILE DEFAULT" {
		t.Fatalf("delete must restore the default layer: %+v", pp)
	}

	// 5. Overrides are NOT gated by prompt_apply_inherited. Unbind the agent and
	// make the default layer an inherited global profile with inheritance off.
	if err := pg.SetAgentLLMProfile(agentKey, nil); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	if err := pg.SetActiveProfile(id); err != nil {
		t.Fatalf("SetActiveProfile: %v", err)
	}
	if pp = s.profilePromptForTaskRole(&Task{}, agentKey); pp.Active() {
		t.Fatalf("inheritance off must suppress the default layer: %+v", pp)
	}
	if err := pg.SetAgentProfilePrompt(agentKey, id, "INHERITED OVERRIDE", "prepend"); err != nil {
		t.Fatalf("SetAgentProfilePrompt on inherited path: %v", err)
	}
	pp = s.profilePromptForTaskRole(&Task{}, agentKey)
	if pp.Text != "INHERITED OVERRIDE" || pp.Source != "agent_override" {
		t.Fatalf("override must bypass the inheritance switch: %+v", pp)
	}
	// Same rule on the chat path (global profile branch).
	conv := &db.Conversation{AgentKey: agentKey}
	if pp = s.profilePromptForChat(conv); pp.Text != "INHERITED OVERRIDE" || pp.Source != "agent_override" {
		t.Fatalf("chat override mismatch: %+v", pp)
	}
}

func TestAgentPromptOverrideChainProjection(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	t.Cleanup(func() { m.Close() })
	s := New(context.Background(), m, "../skills", t.TempDir(), t.TempDir())
	t.Cleanup(func() { s.Close() })
	pg := m.pg

	id, err := pg.SaveProfile(&db.LLMProfile{
		Name: "agent-prompt-chain-" + time.Now().Format("150405.000000"), Format: "openai", Model: "m", APIKey: "sk-test",
	})
	if err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteProfile(id) })
	if err := pg.SetProfilePrompt(id, "DEFAULT TEXT", "append", true); err != nil {
		t.Fatalf("SetProfilePrompt: %v", err)
	}
	agentKey := newTempAgent(t, pg, "t_app_chain")
	if err := pg.SetAgentLLMProfile(agentKey, &id); err != nil {
		t.Fatalf("bind agent: %v", err)
	}
	a, err := pg.GetAgentByKey(agentKey)
	if err != nil || a == nil {
		t.Fatalf("GetAgentByKey: %v", err)
	}
	findProfileSeg := func(chain *agentPromptChain) promptChainSegment {
		t.Helper()
		for _, seg := range chain.Segments {
			if seg.Key == "profile_prompt" {
				return seg
			}
		}
		t.Fatalf("profile_prompt segment missing")
		return promptChainSegment{}
	}

	// Inherited default layer.
	chain, err := s.buildAgentPromptChain(pg, a, "")
	if err != nil {
		t.Fatalf("buildAgentPromptChain: %v", err)
	}
	if chain.LLM.PromptScope != "profile_default" || !chain.LLM.PromptActive || chain.LLM.PromptChars != len("DEFAULT TEXT") {
		t.Fatalf("default scope mismatch: %+v", chain.LLM)
	}
	if chain.LLM.DefaultPromptChars != len("DEFAULT TEXT") || chain.LLM.DefaultPromptMode != "append" {
		t.Fatalf("default layer fields mismatch: %+v", chain.LLM)
	}

	// Non-empty override: effective fields describe the override; the default
	// layer stays visible for the editor's context.
	if err := pg.SetAgentProfilePrompt(agentKey, id, "OVERRIDE TEXT", "replace"); err != nil {
		t.Fatalf("SetAgentProfilePrompt: %v", err)
	}
	chain, err = s.buildAgentPromptChain(pg, a, "")
	if err != nil {
		t.Fatalf("buildAgentPromptChain(override): %v", err)
	}
	if chain.LLM.PromptScope != "agent_override" || !chain.LLM.PromptActive || chain.LLM.PromptMode != "replace" {
		t.Fatalf("override scope mismatch: %+v", chain.LLM)
	}
	if chain.LLM.PromptChars != len("OVERRIDE TEXT") || chain.LLM.DefaultPromptChars != len("DEFAULT TEXT") {
		t.Fatalf("effective/default chars mismatch: %+v", chain.LLM)
	}
	if seg := findProfileSeg(chain); seg.SourceLabel != "本 Agent 覆盖" || !seg.Present || seg.Text != "OVERRIDE TEXT" {
		t.Fatalf("override segment mismatch: %+v", seg)
	}

	// Empty override row: the chain still shows the object (so the UI can offer
	// "restore"), marked inactive with the default-layer size available.
	if err := pg.SetAgentProfilePrompt(agentKey, id, "", ""); err != nil {
		t.Fatalf("SetAgentProfilePrompt(disable): %v", err)
	}
	chain, err = s.buildAgentPromptChain(pg, a, "")
	if err != nil {
		t.Fatalf("buildAgentPromptChain(disabled): %v", err)
	}
	if chain.LLM.PromptScope != "agent_disabled" || chain.LLM.PromptActive || chain.LLM.PromptChars != 0 {
		t.Fatalf("disabled scope mismatch: %+v", chain.LLM)
	}
	seg := findProfileSeg(chain)
	if seg.Present || seg.SourceLabel != "本 Agent 已关闭" || !strings.Contains(seg.Reason, "关闭") {
		t.Fatalf("disabled segment mismatch: %+v", seg)
	}
}

func TestAgentPromptOverrideAPI(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	td := t.TempDir()
	s := New(context.Background(), m, td, td, td)
	pg := m.pg

	id, err := pg.SaveProfile(&db.LLMProfile{
		Name: "agent-prompt-api-" + time.Now().Format("150405.000000"), Format: "openai", Model: "m", APIKey: "sk-test",
	})
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	t.Cleanup(func() { _ = pg.DeleteProfile(id) })
	agentKey := newTempAgent(t, pg, "t_app_api")

	call := func(method, body string, h http.HandlerFunc, profileID int64) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		path := "/api/agents/" + agentKey + "/llm-prompt-overrides"
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

	// Missing prompt is not "disabled": it is a malformed request.
	if rec, _ := call(http.MethodPut, `{"prompt_mode":"append"}`, s.pgSaveAgentPromptOverride, id); rec.Code != 400 {
		t.Fatalf("missing prompt must be 400, got %d (%s)", rec.Code, rec.Body.String())
	}
	if rec, _ := call(http.MethodPut, `{"prompt":"x","prompt_mode":"bogus"}`, s.pgSaveAgentPromptOverride, id); rec.Code != 400 {
		t.Fatalf("invalid mode must be 400, got %d", rec.Code)
	}
	if rec, _ := call(http.MethodPut, `{"prompt":"x"}`, s.pgSaveAgentPromptOverride, 99999999); rec.Code != 404 {
		t.Fatalf("missing profile must be 404, got %d", rec.Code)
	}
	// Save without a mode normalizes to prepend.
	if rec, _ := call(http.MethodPut, `{"prompt":"API OVERRIDE"}`, s.pgSaveAgentPromptOverride, id); rec.Code != 204 {
		t.Fatalf("save: %d (%s)", rec.Code, rec.Body.String())
	}
	row, ok, err := pg.AgentProfilePrompt(agentKey, id)
	if err != nil || !ok || row.Text != "API OVERRIDE" || row.Mode != "prepend" {
		t.Fatalf("stored override mismatch: %+v ok=%v err=%v", row, ok, err)
	}
	rec, out := call(http.MethodGet, "", s.pgAgentPromptOverrides, 0)
	if rec.Code != 200 {
		t.Fatalf("get: %d", rec.Code)
	}
	overrides, _ := out["overrides"].([]any)
	if len(overrides) != 1 {
		t.Fatalf("get overrides = %v", out)
	}
	// Empty prompt on PUT stores the explicit disabled row (204), not a delete.
	if rec, _ := call(http.MethodPut, `{"prompt":""}`, s.pgSaveAgentPromptOverride, id); rec.Code != 204 {
		t.Fatalf("disable: %d", rec.Code)
	}
	if row, ok, err = pg.AgentProfilePrompt(agentKey, id); err != nil || !ok || row.Text != "" {
		t.Fatalf("disabled row mismatch: %+v ok=%v err=%v", row, ok, err)
	}
	// DELETE restores inheritance and stays 204 when repeated.
	if rec, _ := call(http.MethodDelete, "", s.pgDeleteAgentPromptOverride, id); rec.Code != 204 {
		t.Fatalf("delete: %d", rec.Code)
	}
	if rec, _ := call(http.MethodDelete, "", s.pgDeleteAgentPromptOverride, id); rec.Code != 204 {
		t.Fatalf("second delete must stay 204, got %d", rec.Code)
	}
	if _, ok, _ := pg.AgentProfilePrompt(agentKey, id); ok {
		t.Fatal("row must be gone after delete")
	}
	rec, out = call(http.MethodGet, "", s.pgAgentPromptOverrides, 0)
	if rec.Code != 200 {
		t.Fatalf("get after delete: %d", rec.Code)
	}
	if overrides, ok := out["overrides"].([]any); !ok || len(overrides) != 0 {
		t.Fatalf("overrides must be an empty array, got %v", out["overrides"])
	}
}
