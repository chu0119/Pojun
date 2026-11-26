package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Autumn-27/norma/llm"
	"github.com/chu0119/pojun/agent"
	"github.com/chu0119/pojun/db"
	"github.com/chu0119/pojun/llmpresets"
)

// DTO 是配置页看到的“实际结论”：三态压过一切、探测缓存按指纹生效、否则走
// 预设/名字启发。UI 与请求侧守卫必须读同一个解析，否则会出现“页面说支持、
// 请求把图剥了”的分裂。
func TestLLMProfileDTOVisionResolution(t *testing.T) {
	sig := llmpresets.VisionSignature("openai", "https://api.example.com/v1", "unknown-model")
	cases := []struct {
		name   string
		p      *db.LLMProfile
		want   bool
		source string
	}{
		{"auto default", &db.LLMProfile{Model: "unknown-model"}, false, llmpresets.VisionSourceDefault},
		{"auto name", &db.LLMProfile{Model: "claude-opus-4-8"}, true, llmpresets.VisionSourceName},
		{"user on", &db.LLMProfile{Model: "unknown-model", VisionMode: "on"}, true, llmpresets.VisionSourceProfile},
		{"user off", &db.LLMProfile{Model: "claude-opus-4-8", VisionMode: "off"}, false, llmpresets.VisionSourceProfile},
		{"probe on", &db.LLMProfile{
			Model: "unknown-model", Format: "openai", BaseURL: "https://api.example.com/v1",
			VisionProbe: "on", VisionProbeSig: sig,
		}, true, llmpresets.VisionSourceProbe},
		{"probe off (stale sig)", &db.LLMProfile{
			Model: "claude-opus-4-8", Format: "openai", BaseURL: "https://api.example.com/v1",
			VisionProbe: "off", VisionProbeSig: sig,
		}, true, llmpresets.VisionSourceName}, // 指纹不匹配（模型不同）→ 探测作废
		{"user on beats probe off", &db.LLMProfile{
			Model: "unknown-model", Format: "openai", BaseURL: "https://api.example.com/v1",
			VisionMode: "on", VisionProbe: "off", VisionProbeSig: sig,
		}, true, llmpresets.VisionSourceProfile},
	}
	for _, c := range cases {
		dto := llmProfileDTO(c.p)
		if dto.Vision != c.want || dto.VisionSource != c.source {
			t.Fatalf("%s: vision=%v/%s，期望 %v/%s", c.name, dto.Vision, dto.VisionSource, c.want, c.source)
		}
		if dto.VisionMode != c.p.VisionMode {
			t.Fatalf("%s: vision_mode 未回传", c.name)
		}
	}
}

// model-meta 给配置页即时显示「自动」档的实际结论。
func TestLLMModelMetaVisionFields(t *testing.T) {
	s := &Server{}
	get := func(model string) map[string]any {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/llm/model-meta?model="+model, nil)
		s.llmModelMeta(w, req)
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s: %v", model, err)
		}
		return out
	}
	if r := get("claude-opus-4-8"); r["vision"] != true || r["vision_source"] != llmpresets.VisionSourceName {
		t.Fatalf("claude 应识别为视觉模型: %v", r)
	}
	if r := get("deepseek-v4.1-flash"); r["vision"] != false || r["vision_source"] != llmpresets.VisionSourceDefault {
		t.Fatalf("未知纯文本模型应默认关闭: %v", r)
	}
	if r := get("qwen2.5-vl-72b"); r["vision"] != true {
		t.Fatalf("qwen vl 应识别为视觉模型: %v", r)
	}
}

// 自动探测只花在「自动档 + 没测过/指纹变了」的配置上；同配置永不重复。
func TestVisionProbeNeeded(t *testing.T) {
	sig := llmpresets.VisionSignature("openai", "https://api.example.com/v1", "m")
	base := &db.LLMProfile{Format: "openai", BaseURL: "https://api.example.com/v1", Model: "m"}
	if !visionProbeNeeded(base) {
		t.Fatal("新配置应需要探测")
	}
	if visionProbeNeeded(&db.LLMProfile{
		Format: "openai", BaseURL: "https://api.example.com/v1", Model: "m",
		VisionProbe: "on", VisionProbeSig: sig,
	}) {
		t.Fatal("同指纹已有结果不应重复探测")
	}
	if visionProbeNeeded(&db.LLMProfile{
		Format: "openai", BaseURL: "https://api.example.com/v1", Model: "m",
		VisionProbe: "off", VisionProbeSig: sig,
	}) {
		t.Fatal("off 结果同样是有效缓存，不应重复探测")
	}
	if !visionProbeNeeded(&db.LLMProfile{
		Format: "openai", BaseURL: "https://api.example.com/v1", Model: "m2",
		VisionProbe: "on", VisionProbeSig: sig,
	}) {
		t.Fatal("模型变了（指纹不匹配）应重新探测")
	}
	if visionProbeNeeded(&db.LLMProfile{Model: "m", VisionMode: "on"}) {
		t.Fatal("用户显式选支持时不应探测")
	}
	if visionProbeNeeded(&db.LLMProfile{Model: "m", VisionMode: "off"}) {
		t.Fatal("用户显式选不支持时不应探测")
	}
	if visionProbeNeeded(nil) {
		t.Fatal("nil 配置不应探测")
	}
}

// 并发保存同一配置：只有第一个拿到探测权。
func TestBeginVisionProbeDedup(t *testing.T) {
	var m sync.Map
	if !beginVisionProbe(&m, 7) {
		t.Fatal("首次应拿到探测权")
	}
	if beginVisionProbe(&m, 7) {
		t.Fatal("并发第二次必须被去重")
	}
	if !beginVisionProbe(&m, 8) {
		t.Fatal("不同配置互不影响")
	}
	m.Delete(int64(7)) // key type must match the int64 the helper stores
	if !beginVisionProbe(&m, 7) {
		t.Fatal("释放后应可再次探测")
	}
}

// 探测请求必须：挂一次性 session 头（网关缺它会 400）、带上真实图片、且只把
// msg.Text() 当正文（Complete 第二个返回值是 stopReason）。
func TestProbeVisionConfigSessionHeaderAndReply(t *testing.T) {
	const header = "x-opencode-session"
	var gotHeader, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get(header)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, _ := json.Marshal(body)
		gotBody = string(raw)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"红色"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`))
	}))
	defer srv.Close()

	cfg := agent.Config{
		Format: llm.FormatOpenAI, BaseURL: srv.URL, APIKey: "k", Model: "vision-test",
		SessionHeaderKey: header,
	}
	out := probeVisionConfig(t.Context(), cfg)
	if out.ErrMsg != "" {
		t.Fatalf("探测失败: %s", out.ErrMsg)
	}
	if !out.OK || !strings.Contains(out.Reply, "红") {
		t.Fatalf("应识别红色，reply=%q ok=%v", out.Reply, out.OK)
	}
	if gotHeader == "" || !strings.HasPrefix(gotHeader, "visionprobe-") {
		t.Fatalf("探测必须带一次性 session 头，得 %q", gotHeader)
	}
	if !strings.Contains(gotBody, "data:image/png;base64,") {
		t.Fatalf("请求体不含图片 data URI: %s", gotBody)
	}
}

// 空正文 + finish_reason=stop：不得把 stopReason 当回答，必须判失败。
func TestProbeVisionConfigDoesNotUseStopReasonAsReply(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	cfg := agent.Config{Format: llm.FormatOpenAI, BaseURL: srv.URL, APIKey: "k", Model: "m"}
	// 关掉空响应重试，测试不必等退避。
	cfg.Retry.EmptyAttempts = -1
	out := probeVisionConfig(t.Context(), cfg)
	if out.ErrMsg != "" {
		t.Fatalf("请求本身应成功: %s", out.ErrMsg)
	}
	if out.Reply != "" || out.OK {
		t.Fatalf("空正文不应判通过（stopReason 不是正文）: reply=%q ok=%v", out.Reply, out.OK)
	}
}

// 探测端点：无 key/不存在 → 404；成功 → 200 ok + 结果落库（含指纹）。
func TestLLMVisionProbeEndpointPersistsResult(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	m := &Manager{pg: d}
	s := &Server{m: m}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"red"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	id, err := d.SaveProfile(&db.LLMProfile{Name: "t-vision-probe", Format: "openai", Model: "vision-test", BaseURL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.DeleteProfile(id) }()

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/llm/vision-probe", strings.NewReader(`{"profile_id":`+i64s(id)+`}`))
	s.llmVisionProbe(w, req)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["ok"] != true {
		t.Fatalf("应探测成功: %v", out)
	}
	p, err := d.ProfileByID(id)
	if err != nil || p == nil {
		t.Fatalf("重读配置: %v", err)
	}
	wantSig := llmpresets.VisionSignature(p.Format, p.BaseURL, p.Model)
	if p.VisionProbe != "on" || p.VisionProbeSig != wantSig {
		t.Fatalf("探测结果未按指纹落库: probe=%q sig=%q want=%q", p.VisionProbe, p.VisionProbeSig, wantSig)
	}

	// 不存在的配置 → 404（错误脱敏，不回显内部细节）。
	w2 := httptest.NewRecorder()
	s.llmVisionProbe(w2, httptest.NewRequest(http.MethodPost, "/api/llm/vision-probe", strings.NewReader(`{"profile_id":99999999}`)))
	if w2.Code != 404 {
		t.Fatalf("未知配置应 404，得 %d", w2.Code)
	}
}
