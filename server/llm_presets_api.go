package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/chu0119/pojun/agent"
	"github.com/chu0119/pojun/llmpresets"
)

// LLM 配置的厂商知识层端点（v0.4.91 配置重构）：全部只读、纯增量，
// 不改动既有 9 个 LLM 端点的行为。

// GET /api/llm/presets — 内置厂商预设清单（新建向导第一步的卡片数据）。
func (s *Server) llmPresets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"presets": llmpresets.All()})
}

// GET /api/llm/model-meta?model=xxx&format=openai — 按模型名推断元数据（context
// window / 输出上限 / 是否支持推理 / 该用哪个输出上限字段）。
// 来源 meta（元数据表）> preset（厂商预设条目）> none（前端让用户手填）。
// 配置保存时只把推断值填进**空值**，用户手改过的字段不会被覆盖。
func (s *Server) llmModelMeta(w http.ResponseWriter, r *http.Request) {
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	format := strings.TrimSpace(r.URL.Query().Get("format"))
	d := llmpresets.LookupModelDefaults(model, format)
	resp := map[string]any{
		"model":         model,
		"context_k":     d.ContextK,
		"reasoning":     d.Reasoning,
		"source":        d.Source,
		"vision":        d.Vision,
		"vision_source": d.VisionSource,
	}
	if d.MaxOutputK > 0 {
		resp["max_output_k"] = d.MaxOutputK
	}
	if d.MaxTokens > 0 {
		resp["max_tokens"] = d.MaxTokens
	}
	if d.MaxTokensField != "" {
		resp["max_tokens_field"] = d.MaxTokensField
	}
	if d.ThinkingType != "" {
		resp["thinking_type"] = d.ThinkingType
	}
	if d.ReasoningEffort != "" {
		resp["reasoning_effort"] = d.ReasoningEffort
	}
	writeJSON(w, 200, resp)
}

// llmURLHint 是 Base URL 输入框的即时提示：纯本地计算，不发网络请求。
// 返回归一结果、是否需要修改、能否确定（confident），以及协议建议（需用户确认）。
func (s *Server) llmURLHint(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Format  string `json:"format"`
		BaseURL string `json:"base_url"`
		Model   string `json:"model"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	norm := llmpresets.NormalizeBaseURL(req.Format, req.BaseURL)
	suggested, reason := llmpresets.ProtocolHint(req.Format, req.BaseURL, req.Model)
	resp := map[string]any{
		"normalized":       norm.Normalized,
		"changed":          norm.Changed,
		"confident":        norm.Confident,
		"note":             norm.Note,
		"suggested_format": suggested,
		"protocol_reason":  reason,
	}
	if suggested != "" && suggested != req.Format {
		alt := llmpresets.NormalizeBaseURL(suggested, req.BaseURL)
		resp["suggested_base_url"] = alt.Normalized
	}
	writeJSON(w, 200, resp)
}

// llmProbeReq 是 /api/llm/probe 的请求体（与 /llm/models、/llm/test 同形）。
type llmProbeReq struct {
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	BaseURL          string `json:"base_url"`
	Proxy            string `json:"proxy"`
	APIKey           string `json:"api_key"`
	ThinkingType     string `json:"thinking_type"`
	ReasoningEffort  string `json:"reasoning_effort"`
	ProfileID        *int64 `json:"profile_id"`
	Streaming        *bool  `json:"streaming"`
	SessionHeaderKey string `json:"session_header_key"`
	UserAgent        string `json:"user_agent"`
}

type probeCombo struct {
	Provider string
	BaseURL  string
}

// llmProbeChat 是「真实推理」这一步的取数函数；单测替换它即可在本地验证
// 预算/顺序/建议写回，不打外网。
var llmProbeChat = agent.TestConnection

// llmProbe 自动探测可用组合：当前协议×地址候选 + 备选协议，先拉模型列表，
// 再对最可能的两组跑真实推理。全程只读、有总预算，不写库。
//
// 预算：总 25s；模型列表每组 8s；真实推理最多 2 组、每组 12s（受总预算约束）。
// 组合最多 4 组，避免「探测」本身变成一个慢请求。
func (s *Server) llmProbe(w http.ResponseWriter, r *http.Request) {
	var req llmProbeReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	apiKey := strings.TrimSpace(req.APIKey)
	if req.ProfileID != nil {
		if p, err := s.m.pg.ProfileByID(*req.ProfileID); err == nil && p != nil {
			if apiKey == "" {
				apiKey = p.APIKey
			}
			if strings.TrimSpace(req.SessionHeaderKey) == "" {
				req.SessionHeaderKey = p.SessionHeaderKey
			}
			if strings.TrimSpace(req.UserAgent) == "" {
				req.UserAgent = p.UserAgent
			}
		}
	}
	if apiKey == "" {
		s.cfgMu.Lock()
		apiKey = s.llmCfg.APIKey
		s.cfgMu.Unlock()
	}
	primary := probeFormat(req.Provider)
	base := defaultModelBase(primary, req.BaseURL)
	norm := llmpresets.NormalizeBaseURL(primary, base)

	var combos []probeCombo
	seen := map[string]bool{}
	add := func(provider, b string) {
		b = strings.TrimRight(strings.TrimSpace(b), "/")
		key := provider + "|" + b
		if b == "" || seen[key] || len(combos) >= 4 {
			return
		}
		seen[key] = true
		combos = append(combos, probeCombo{Provider: provider, BaseURL: b})
	}
	for i, b := range llmpresets.BaseCandidates(primary, base) {
		if i >= 2 {
			break
		}
		add(primary, b)
	}
	if suggested, _ := llmpresets.ProtocolHint(primary, base, req.Model); suggested != "" && suggested != primary {
		for _, b := range llmpresets.BaseCandidates(suggested, base) {
			add(suggested, b)
			break
		}
	}

	parent, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	type attempt struct {
		Provider    string `json:"provider"`
		BaseURL     string `json:"base_url"`
		Stage       string `json:"stage"` // models | chat
		OK          bool   `json:"ok"`
		Reachable   bool   `json:"reachable,omitempty"`
		StatusCode  int    `json:"status_code,omitempty"`
		ErrorClass  string `json:"error_class,omitempty"`
		Error       string `json:"error,omitempty"`
		ModelsCount int    `json:"models_count,omitempty"`
	}
	var attempts []attempt
	var modelsOK []probeCombo
	for _, c := range combos {
		if parent.Err() != nil {
			break
		}
		ctx, cancel := context.WithTimeout(parent, 8*time.Second)
		models, code, errMsg, reachable := fetchModelsAt(ctx, c.Provider, c.BaseURL, apiKey, req.Proxy, req.UserAgent)
		cancel()
		a := attempt{Provider: c.Provider, BaseURL: c.BaseURL, Stage: "models", OK: len(models) > 0,
			Reachable: reachable, StatusCode: code, ModelsCount: len(models)}
		if len(models) == 0 {
			class, human := classifyModelListFailure(code, errMsg)
			a.ErrorClass, a.Error = class, human
			if human == "" {
				a.Error = sanitizeProbeError(errMsg, apiKey)
			}
		} else {
			modelsOK = append(modelsOK, c)
		}
		attempts = append(attempts, a)
	}

	best := struct {
		Provider  string `json:"provider"`
		BaseURL   string `json:"base_url"`
		LatencyMS int64  `json:"latency_ms"`
		Reply     string `json:"reply,omitempty"`
	}{}
	// 模型列表可达的组合优先；其余作为兜底（有些网关不开放 /models，但聊天可用）。
	testOrder := combos
	if len(modelsOK) > 0 {
		testOrder = append(append([]probeCombo{}, modelsOK...), combos...)
	}
	tested := 0
	model := strings.TrimSpace(req.Model)
	if model != "" && apiKey != "" {
		for _, c := range testOrder {
			if tested >= 2 || parent.Err() != nil {
				break
			}
			dup := false
			for _, a := range attempts {
				if a.Stage == "chat" && a.Provider == c.Provider && a.BaseURL == c.BaseURL {
					dup = true
				}
			}
			if dup {
				continue
			}
			tested++
			cfg := agent.ConfigFrom(c.Provider, model, c.BaseURL, apiKey, req.Proxy)
			cfg.ThinkingType = req.ThinkingType
			cfg.ReasoningEffort = req.ReasoningEffort
			if req.Streaming != nil {
				cfg.Stream = *req.Streaming
			}
			cfg.SessionHeaderKey = req.SessionHeaderKey
			cfg.UserAgent = req.UserAgent
			ctx, cancel := context.WithTimeout(parent, 12*time.Second)
			lat, reply, err := llmProbeChat(ctx, cfg)
			cancel()
			a := attempt{Provider: c.Provider, BaseURL: c.BaseURL, Stage: "chat", OK: err == nil}
			if err != nil {
				a.ErrorClass = "chat_failed"
				a.Error = sanitizeProbeError(err.Error(), apiKey)
			} else {
				best.Provider, best.BaseURL = c.Provider, c.BaseURL
				best.LatencyMS, best.Reply = lat.Milliseconds(), truncateReply(reply)
			}
			attempts = append(attempts, a)
			if err == nil {
				break
			}
		}
	}
	// 只拉模型、没填模型名：以模型列表可达作为成功（stage=models）。
	if model == "" && len(modelsOK) > 0 {
		best.Provider, best.BaseURL = modelsOK[0].Provider, modelsOK[0].BaseURL
	}
	ok := best.BaseURL != ""
	resp := map[string]any{
		"ok": ok, "best": best, "attempts": attempts,
		"normalized": norm.Normalized,
	}
	if ok && (best.Provider != primary || best.BaseURL != norm.Normalized) {
		resp["suggestion"] = map[string]any{
			"provider": best.Provider, "base_url": best.BaseURL, "reason": "探测到可用组合",
		}
	}
	writeJSON(w, 200, resp)
}

// sanitizeProbeError 保证探测错误里绝不带出 API Key（响应体若回显了凭据）。
func sanitizeProbeError(s, apiKey string) string {
	s = strings.TrimSpace(s)
	if key := strings.TrimSpace(apiKey); key != "" {
		s = strings.ReplaceAll(s, key, "***")
	}
	const max = 300
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

func probeFormat(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "openai", "openai-responses":
		return strings.ToLower(strings.TrimSpace(format))
	}
	return "anthropic"
}

// defaultModelBase 给空地址补厂商默认（与 fetchProviderModels 的历史行为一致）。
func defaultModelBase(provider, base string) string {
	base = strings.TrimSpace(base)
	if base != "" {
		return base
	}
	if provider == "openai" || provider == "openai-responses" {
		return "https://api.openai.com/v1"
	}
	return "https://api.anthropic.com"
}

// fetchModelsAt 在**指定的** Base URL 上拉一次模型列表（探测专用）。
//
// 与 fetchProviderModels 的区别：后者会自行拼候选并“第一个非空即止”，探测
// 需要每一组组合各自的真实结果（含失败原因），所以这里只探一组。reachable 表示
// 拿到了任意 HTTP 响应——网络不通与 404 是两种完全不同的修复动作。
func fetchModelsAt(ctx context.Context, provider, baseURL, apiKey, proxy, userAgent string) (models []string, lastCode int, lastErr string, reachable bool) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	bearerHdr := func() http.Header {
		h := http.Header{}
		h.Set("Authorization", "Bearer "+apiKey)
		return h
	}
	anthropicHdr := func() http.Header {
		h := http.Header{}
		h.Set("x-api-key", apiKey)
		h.Set("anthropic-version", "2023-06-01")
		return h
	}
	type candidate struct {
		url string
		hdr http.Header
	}
	var candidates []candidate
	switch provider {
	case "openai", "openai-responses":
		candidates = append(candidates, candidate{baseURL + "/models", bearerHdr()})
	default:
		candidates = append(candidates, candidate{baseURL + "/v1/models", anthropicHdr()})
		// Anthropic 兼容网关常把模型列表放在 OpenAI 路径（与 fetchProviderModels 同理）。
		if root := strings.TrimRight(strings.TrimSuffix(baseURL, "/anthropic"), "/"); root != baseURL {
			candidates = append(candidates,
				candidate{root + "/models", bearerHdr()},
				candidate{root + "/v1/models", bearerHdr()},
			)
		}
	}

	transport := &http.Transport{}
	if p := strings.TrimSpace(proxy); p != "" {
		if pu, err := url.Parse(p); err == nil {
			transport.Proxy = http.ProxyURL(pu)
		}
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}

	emptyOK := false
	for _, c := range candidates {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
		if err != nil {
			lastErr = "构建请求失败: " + err.Error()
			continue
		}
		httpReq.Header = c.hdr
		if userAgent != "" {
			httpReq.Header.Set("User-Agent", userAgent)
		}
		resp, err := client.Do(httpReq)
		if err != nil {
			lastErr = "请求失败: " + err.Error()
			continue
		}
		reachable = true
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		lastCode = resp.StatusCode
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Sprintf("API 返回 %d: %s", resp.StatusCode, string(body[:min(len(body), 512)]))
			continue
		}
		var parsed struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			lastErr = "解析响应失败: " + err.Error()
			continue
		}
		models = models[:0]
		for _, m := range parsed.Data {
			if m.ID != "" {
				models = append(models, m.ID)
			}
		}
		if len(models) > 0 {
			return models, lastCode, "", reachable
		}
		emptyOK = true
	}
	if emptyOK {
		return nil, lastCode, errAllEmpty, reachable
	}
	return nil, lastCode, lastErr, reachable
}

// POST /api/llm/check — 连通测试第一步（快速分诊）：按协议格式拉模型列表，
// 把失败分类成人话。第二步验收用既有 POST /api/llm/test（1-token 真实推理）。
// 请求体与 /api/llm/models 相同（key/UA 支持 profile_id 兜底）。
func (s *Server) llmCheck(w http.ResponseWriter, r *http.Request) {
	req, apiKey, userAgent, ok := s.decodeModelsReq(w, r)
	if !ok {
		return
	}
	if apiKey == "" {
		writeJSON(w, 200, map[string]any{
			"ok": false, "status_class": "missing_key",
			"error_human": "还没有填写 API Key：在第 2 步粘贴厂商给你的 Key 后重试。",
		})
		return
	}
	start := time.Now()
	models, lastCode, lastErr := fetchProviderModels(r.Context(), req.Provider, req.BaseURL, apiKey, req.Proxy, userAgent)
	latency := time.Since(start).Milliseconds()
	if len(models) > 0 {
		writeJSON(w, 200, map[string]any{
			"ok": true, "status_class": "ok", "models": models, "latency_ms": latency,
		})
		return
	}
	class, human := classifyModelListFailure(lastCode, lastErr)
	resp := map[string]any{
		"ok": false, "status_class": class, "error_human": human, "latency_ms": latency,
	}
	if lastErr != "" && lastErr != errAllEmpty {
		resp["error"] = lastErr // 原始错误保留给排障，UI 主展示 error_human
	}
	writeJSON(w, 200, resp)
}

// classifyModelListFailure 把模型列表拉取失败翻译成分类 + 人话 + 修复指引。
func classifyModelListFailure(lastCode int, lastErr string) (class, human string) {
	switch {
	case lastCode == 401 || lastCode == 403:
		return "key_invalid", "Key 无效或权限不足（HTTP " + strconv.Itoa(lastCode) + "）：确认 Key 是否复制完整、是否属于这个平台、账号是否还有额度。"
	case lastCode == 404 || lastCode == 405:
		return "path_or_format", "端点路径或协议不匹配（HTTP " + strconv.Itoa(lastCode) + "）：确认 Base URL 是否正确（一般以 /v1 结尾），以及厂商是否用 OpenAI 协议——Anthropic 协议的网关（如 DeepSeek 官方 Anthropic 端点、OpenCode Zen）要选对应预设。"
	case lastCode == 402:
		return "key_invalid", "账户余额不足（HTTP 402）：先去厂商控制台充值或换免费通道。"
	case lastCode == 429:
		return "server_error", "被限流（HTTP 429）：Key 有效但当前请求过快或额度受限，稍后重试。"
	case lastCode >= 500:
		return "server_error", "厂商服务端错误（HTTP " + strconv.Itoa(lastCode) + "）：服务暂不可用，稍后重试。"
	case lastCode == 200:
		return "empty_list", "端点连通但没返回任何模型（HTTP 200 空列表）：这个网关可能不开放模型列表接口——直接手填模型名即可。"
	case lastErr != "":
		return "network", "网络不通：" + netHint(lastErr) + "（若配置了代理，检查代理地址；若目标是外网厂商，检查本机出网。）"
	}
	return "unknown", "未知错误：请查看服务端日志。"
}

func netHint(lastErr string) string {
	if strings.Contains(lastErr, "timeout") || strings.Contains(lastErr, "deadline") {
		return "连接超时"
	}
	if strings.Contains(lastErr, "connection refused") || strings.Contains(lastErr, "connectex") {
		return "连接被拒绝（地址或端口不对）"
	}
	if strings.Contains(lastErr, "no such host") || strings.Contains(lastErr, "DNS") {
		return "域名无法解析"
	}
	if strings.Contains(lastErr, "certificate") || strings.Contains(lastErr, "x509") {
		return "TLS 证书校验失败"
	}
	return "底层网络错误"
}
