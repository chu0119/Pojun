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

	"github.com/chu0119/pojun/agent"
	"github.com/chu0119/pojun/db"
)

// TestLLMURLHintHandler 验证只读提示端点：归一 + 协议建议 + 备选地址。
func TestLLMURLHintHandler(t *testing.T) {
	s := &Server{m: &Manager{}}
	post := func(body map[string]any) map[string]any {
		raw, _ := json.Marshal(body)
		rr := httptest.NewRecorder()
		s.llmURLHint(rr, httptest.NewRequest(http.MethodPost, "/api/llm/url-hint", bytes.NewReader(raw)))
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	out := post(map[string]any{
		"format": "openai", "base_url": "https://dashscope.aliyuncs.com/compatible-mode",
	})
	if out["normalized"] != "https://dashscope.aliyuncs.com/compatible-mode/v1" || out["changed"] != true || out["confident"] != true {
		t.Fatalf("normalize response=%+v", out)
	}
	// 协议建议：Anthropic 协议 + compatible-mode 地址 → 建议 OpenAI，并给出对应地址。
	out = post(map[string]any{
		"format": "anthropic", "base_url": "https://dashscope.aliyuncs.com/compatible-mode/v1", "model": "deepseek-v4",
	})
	if out["suggested_format"] != "openai" || out["suggested_base_url"] == "" {
		t.Fatalf("protocol hint response=%+v", out)
	}
	// 已正确的地址：changed=false，输入原样返回（默认行为零变化）。
	out = post(map[string]any{"format": "openai", "base_url": "https://api.openai.com/v1"})
	if out["changed"] != false || out["normalized"] != "https://api.openai.com/v1" {
		t.Fatalf("idempotent response=%+v", out)
	}
}

func TestFetchModelsAt(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
	}))
	defer srv.Close()

	models, code, errMsg, reachable := fetchModelsAt(context.Background(), "openai", srv.URL+"/v1", "sk-test", "", "")
	if len(models) != 2 || code != 200 || errMsg != "" || !reachable {
		t.Fatalf("models=%v code=%d err=%q reachable=%v", models, code, errMsg, reachable)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("auth=%q", gotAuth)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer bad.Close()
	models, code, errMsg, reachable = fetchModelsAt(context.Background(), "openai", bad.URL, "k", "", "")
	if len(models) != 0 || code != 500 || errMsg == "" || !reachable {
		t.Fatalf("bad models=%v code=%d err=%q reachable=%v", models, code, errMsg, reachable)
	}

	// 网络不可达：reachable=false，且错误是传输层而不是 HTTP 层。
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	_, _, errMsg, reachable = fetchModelsAt(context.Background(), "openai", deadURL, "k", "", "")
	if reachable || errMsg == "" {
		t.Fatalf("network failure should be unreachable: reachable=%v err=%q", reachable, errMsg)
	}
}

// TestLLMProbeLocal 用本地 httptest 覆盖模型列表阶段，真实推理阶段用注入点
// 替换，验证：候选顺序、成功组合、协议建议与错误脱敏；全程不出网。
func TestLLMProbeLocal(t *testing.T) {
	const key = "sk-probe-secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			if r.Header.Get("Authorization") != "Bearer "+key {
				// Anthropic 候选走 x-api-key，这里故意拒绝，逼出「备选协议」路径。
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	oldChat := llmProbeChat
	t.Cleanup(func() { llmProbeChat = oldChat })
	var chatCalls []string
	llmProbeChat = func(ctx context.Context, cfg agent.Config) (time.Duration, string, error) {
		chatCalls = append(chatCalls, cfg.Provider()+"|"+cfg.BaseURL)
		if cfg.Provider() == "openai" {
			return 12 * time.Millisecond, "OK", nil
		}
		return 0, "", context.DeadlineExceeded
	}

	s := &Server{m: &Manager{}}
	body, _ := json.Marshal(map[string]any{
		"provider": "anthropic", "model": "gpt-4o", "base_url": srv.URL,
		"api_key": key, "streaming": false,
	})
	rr := httptest.NewRecorder()
	s.llmProbe(rr, httptest.NewRequest(http.MethodPost, "/api/llm/probe", bytes.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		OK      bool `json:"ok"`
		Best    struct {
			Provider string `json:"provider"`
			BaseURL  string `json:"base_url"`
			Reply    string `json:"reply"`
		} `json:"best"`
		Attempts []struct {
			Provider string `json:"provider"`
			Stage    string `json:"stage"`
			OK       bool   `json:"ok"`
			Error    string `json:"error"`
		} `json:"attempts"`
		Suggestion *struct {
			Provider string `json:"provider"`
			BaseURL  string `json:"base_url"`
		} `json:"suggestion"`
		Normalized string `json:"normalized"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.Best.Provider != "openai" || !strings.HasSuffix(out.Best.BaseURL, "/v1") || out.Best.Reply != "OK" {
		t.Fatalf("probe result=%+v attempts=%+v", out, out.Attempts)
	}
	if out.Suggestion == nil || out.Suggestion.Provider != "openai" {
		t.Fatalf("suggestion missing (probe found a better protocol): %+v", out.Suggestion)
	}
	if len(chatCalls) == 0 || !strings.HasPrefix(chatCalls[len(chatCalls)-1], "openai|") {
		t.Fatalf("chat stage should end on openai combo: %v", chatCalls)
	}
	// 错误脱敏：key 不得出现在任何 attempt 的错误里。
	for _, a := range out.Attempts {
		if strings.Contains(a.Error, key) {
			t.Fatalf("API key leaked in probe error: %+v", a)
		}
	}
	if strings.Contains(rr.Body.String(), key) {
		t.Fatalf("API key should not appear in probe response body")
	}
}

func TestLLMProbeNoModelUsesModelsStage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	oldChat := llmProbeChat
	t.Cleanup(func() { llmProbeChat = oldChat })
	llmProbeChat = func(ctx context.Context, cfg agent.Config) (time.Duration, string, error) {
		t.Fatal("没填模型名时不应发起真实推理")
		return 0, "", nil
	}
	s := &Server{m: &Manager{}}
	body, _ := json.Marshal(map[string]any{"provider": "openai", "base_url": srv.URL, "api_key": "k"})
	rr := httptest.NewRecorder()
	s.llmProbe(rr, httptest.NewRequest(http.MethodPost, "/api/llm/probe", bytes.NewReader(body)))
	var out struct {
		OK   bool `json:"ok"`
		Best struct {
			BaseURL string `json:"base_url"`
		} `json:"best"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.Best.BaseURL == "" {
		t.Fatalf("models-only probe should succeed: %s", rr.Body.String())
	}
}

// TestLLMProbeFailurePaths 验证全失败时 ok=false，并给出逐条结果（前端据此提示）。
func TestLLMProbeFailurePaths(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer srv.Close()
	oldChat := llmProbeChat
	t.Cleanup(func() { llmProbeChat = oldChat })
	llmProbeChat = func(ctx context.Context, cfg agent.Config) (time.Duration, string, error) {
		return 0, "", context.DeadlineExceeded
	}
	s := &Server{m: &Manager{}}
	body, _ := json.Marshal(map[string]any{
		"provider": "openai", "model": "m", "base_url": srv.URL, "api_key": "k",
	})
	rr := httptest.NewRecorder()
	s.llmProbe(rr, httptest.NewRequest(http.MethodPost, "/api/llm/probe", bytes.NewReader(body)))
	var out struct {
		OK       bool             `json:"ok"`
		Attempts []map[string]any `json:"attempts"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.OK {
		t.Fatalf("all-failing probe must not report ok: %s", rr.Body.String())
	}
	if len(out.Attempts) == 0 {
		t.Fatalf("expected per-attempt details: %s", rr.Body.String())
	}
}

// TestPGSaveProfileNormalizesBaseURLAndDefaults 验证保存时的地址归一与新建
// 默认值补全；更新路径不覆盖已有值。
func TestPGSaveProfileNormalizesBaseURLAndDefaults(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	if act, _ := d.ActiveProfile(); act != nil {
		t.Skip("shared DB has an active profile — skipping minimal-server save test")
	}
	s := &Server{m: &Manager{pg: d}}
	save := func(body map[string]any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		rr := httptest.NewRecorder()
		s.pgSaveProfile(rr, httptest.NewRequest(http.MethodPost, "/api/llm/profiles", bytes.NewReader(raw)))
		return rr
	}
	name := "t-url-normalize-" + time.Now().Format("150405.000000000")
	rr := save(map[string]any{
		"name": name, "format": "openai", "model": "gpt-5", "api_key": "sk-test",
		"base_url": "https://api.openai.com/v1/chat/completions",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", rr.Code, rr.Body.String())
	}
	var created struct {
		ID         int64  `json:"id"`
		BaseURL    string `json:"base_url"`
		Normalized bool   `json:"normalized"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil || created.ID == 0 {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	t.Cleanup(func() { _ = d.DeleteProfile(created.ID) })
	if !created.Normalized || created.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("save did not report normalization: %+v", created)
	}
	p, err := d.ProfileByID(created.ID)
	if err != nil || p == nil {
		t.Fatalf("profile: %v %v", p, err)
	}
	if p.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("stored base_url=%q", p.BaseURL)
	}
	if p.ContextWindowK != 400 || p.MaxTokens != 8192 || p.MaxTokensField != "max_completion_tokens" {
		t.Fatalf("new profile defaults not filled: cw=%d max=%d field=%q", p.ContextWindowK, p.MaxTokens, p.MaxTokensField)
	}

	// 更新：显式值必须原样保留（不被默认值覆盖）。
	rr = save(map[string]any{
		"id": created.ID, "name": name, "format": "openai", "model": "gpt-5",
		"base_url": "https://api.openai.com/v1", "context_window_k": 128,
		"max_tokens": 1024, "max_tokens_field": "",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", rr.Code, rr.Body.String())
	}
	p, err = d.ProfileByID(created.ID)
	if err != nil || p == nil {
		t.Fatalf("profile after update: %v %v", p, err)
	}
	if p.ContextWindowK != 128 || p.MaxTokens != 1024 || p.MaxTokensField != "" {
		t.Fatalf("update overwrote explicit values: cw=%d max=%d field=%q", p.ContextWindowK, p.MaxTokens, p.MaxTokensField)
	}
}
