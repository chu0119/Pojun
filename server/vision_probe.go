package server

// 原生视觉能力的服务端装配：解析、探测、缓存、自动补测。
//
// 「测试视觉」为什么需要：三态/预设/名字启发都可能误判——有的网关标称支持图片，
// 实际返回 400；有的模型名字看不出多模态，实际支持。发一张 64×64 纯红 PNG 问颜色，
// 成本可忽略，但能把「模型不支持图片」从任务中途的 400 提前到配置页。
//
// 自动探测：保存配置后后台探测一次，结果按配置指纹（format|base_url|model）落库。
// 只有「视觉=自动」且「没测过/指纹变了」才会发请求，同配置不再重复花 token；
// 同 profile 并发保存按 id 去重。网络/网关错误不落库（不是「不支持视觉」的证据）。
// POJUN_VISION_AUTOPROBE=0 可关（测试与离线环境）。

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
	"github.com/chu0119/pojun/agent"
	"github.com/chu0119/pojun/db"
	"github.com/chu0119/pojun/llmpresets"
)

const (
	visionProbeTimeout = 30 * time.Second
	// visionProbeAutoTimeout 给后台自动探测留更宽的余量：它不影响任何请求。
	visionProbeAutoTimeout = 60 * time.Second
	// visionProbeBackfillDelay 给启动后的存量配置补测留出时间：先让服务把
	// 更重要的初始化（LLM 装配、任务恢复）走完。
	visionProbeBackfillDelay = 20 * time.Second
	// visionProbeBackfillGap 串行补测的间隔，避免一条网关限流把整个启动拖慢。
	visionProbeBackfillGap = 2 * time.Second
)

// visionProbeOutcome is one probe result. ErrMsgNonEmpty means the request
// itself failed (network / gateway / model error): that is NOT evidence of
// missing image support, so callers must not cache it as "off".
type visionProbeOutcome struct {
	OK        bool
	Reply     string
	LatencyMS int64
	ErrMsg    string
}

// profileVision is the single place the running platform turns a stored profile
// into its image-input capability. Keep every caller (config load, DTO, agent
// wiring) on this function: two resolutions drifting apart is how the UI ends up
// claiming "supports images" while the request guard strips them.
func profileVision(p *db.LLMProfile) bool {
	if p == nil {
		return false
	}
	v, _ := llmpresets.ResolveVisionStored(p.Model, p.VisionMode, p.VisionProbe, p.VisionProbeSig,
		llmpresets.VisionSignature(p.Format, p.BaseURL, p.Model))
	return v
}

// profileVisionSource is profileVision's explainability twin: the UI shows where
// the conclusion came from (profile / probe / preset / name / default).
func profileVisionSource(p *db.LLMProfile) (bool, string) {
	if p == nil {
		return false, llmpresets.VisionSourceDefault
	}
	return llmpresets.ResolveVisionStored(p.Model, p.VisionMode, p.VisionProbe, p.VisionProbeSig,
		llmpresets.VisionSignature(p.Format, p.BaseURL, p.Model))
}

// llmVisionProbe handles POST /api/llm/vision-probe {profile_id}: send one small
// red PNG and ask for its colour. Read-only with respect to the profile (aside
// from caching the verdict), authenticated like every other /api route.
func (s *Server) llmVisionProbe(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ProfileID int64 `json:"profile_id"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if body.ProfileID <= 0 {
		writeErr(w, 400, "profile_id 必填")
		return
	}
	if s.m == nil || s.m.pg == nil {
		writeErr(w, 503, "数据库不可用")
		return
	}
	cfg, ok := s.loadProfileConfig(body.ProfileID)
	if !ok {
		writeErr(w, 404, "LLM 配置不存在或缺少 API Key")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), visionProbeTimeout)
	defer cancel()
	out := probeVisionConfig(ctx, cfg)
	if out.ErrMsg != "" {
		writeJSON(w, 200, map[string]any{
			"ok": false, "latency_ms": out.LatencyMS,
			"error": "模型未能处理图片请求：" + out.ErrMsg,
			"hint":  "如果错误提到 image/vision/multimodal，说明该模型或网关不支持图片输入；把视觉设为「不支持」或换模型。",
		})
		return
	}
	// 只有拿到模型的有效回答才算“探测成功/失败”，此时才落库；网络错误不落库，
	// 免得把一次网关抖动固化成本配置「不支持视觉」。
	s.recordVisionProbe(body.ProfileID, out.OK)
	resp := map[string]any{"ok": out.OK, "reply": clipProbeText(out.Reply, 200), "latency_ms": out.LatencyMS}
	if !out.OK {
		resp["error"] = "模型返回了内容，但没有识别出红色——可能不支持图片，或把图片当成了空内容。"
	}
	writeJSON(w, 200, resp)
}

// recordVisionProbe persists a definitive probe result against the profile's
// current configuration fingerprint.
func (s *Server) recordVisionProbe(id int64, ok bool) {
	if s.m == nil || s.m.pg == nil {
		return
	}
	p, err := s.m.pg.ProfileByID(id)
	if err != nil || p == nil {
		return
	}
	sig := llmpresets.VisionSignature(p.Format, p.BaseURL, p.Model)
	s.applyVisionProbeResult(id, probeValue(ok), sig)
}

// applyVisionProbeResult persists a probe verdict and rebuilds the provider/chat
// caches so the new capability takes effect on the next run instead of waiting
// for a restart. Resolution itself stays in profileVision.
func (s *Server) applyVisionProbeResult(id int64, probe, sig string) {
	if s.m == nil || s.m.pg == nil {
		return
	}
	if err := s.m.pg.SetVisionProbe(id, probe, sig); err != nil {
		log.Printf("[llm] 视觉探测结果落库失败 profile=%d: %v", id, err)
		return
	}
	s.invalidateProfileAgents()
	// 全局 chat agent 捕获的是构建时的 cfg.Vision，必须重新装配才生效；测试里
	// 可能只构造了最小 Server（无 engine），此时跳过重装仍不影响解析结果。
	if s.engine != nil {
		s.reapplyActiveProfile()
	}
}

// scheduleVisionProbe runs one probe in the background when the saved config is
// new or changed. Idempotent per profile: concurrent saves share one probe.
func (s *Server) scheduleVisionProbe(id int64) {
	if !visionAutoprobeEnabled() || s.m == nil || s.m.pg == nil || id <= 0 {
		return
	}
	if !beginVisionProbe(&s.visionProbing, id) {
		return
	}
	go func() {
		defer s.visionProbing.Delete(id)
		p, err := s.m.pg.ProfileByID(id)
		if err != nil || p == nil {
			return
		}
		if !visionProbeNeeded(p) {
			return
		}
		sig := llmpresets.VisionSignature(p.Format, p.BaseURL, p.Model)
		cfg, ok := s.loadProfileConfig(id)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), visionProbeAutoTimeout)
		defer cancel()
		out := probeVisionConfig(ctx, cfg)
		if out.ErrMsg != "" {
			return // 网关/网络问题不构成「不支持视觉」的证据
		}
		s.applyVisionProbeResult(id, probeValue(out.OK), sig)
	}()
}

// beginVisionProbe is the per-profile concurrency gate. Returns false when a
// probe for this profile is already in flight (the caller must not start one).
func beginVisionProbe(m *sync.Map, id int64) bool {
	_, busy := m.LoadOrStore(id, struct{}{})
	return !busy
}

// probeValue maps a probe verdict onto the stored tri-state value.
func probeValue(ok bool) string {
	if ok {
		return "on"
	}
	return "off"
}

// visionProbeNeeded decides whether one automatic probe is worth a request.
// It is pure so the "never re-probe an unchanged config" rule is testable
// without touching the network:
//   - the user explicitly chose 支持/不支持 → never (their choice already decides);
//   - the same configuration already has a recorded result (on OR off) → never;
//   - otherwise (new config, or changed fingerprint) → yes.
func visionProbeNeeded(p *db.LLMProfile) bool {
	if p == nil || p.VisionMode != "" {
		return false
	}
	if p.VisionProbe == "" {
		return true
	}
	return p.VisionProbeSig != llmpresets.VisionSignature(p.Format, p.BaseURL, p.Model)
}

// visionAutoprobeEnabled lets tests and offline environments turn the automatic
// probe off (it costs one tiny image request per changed configuration).
func visionAutoprobeEnabled() bool {
	return strings.TrimSpace(os.Getenv("POJUN_VISION_AUTOPROBE")) != "0"
}

// startVisionProbeBackfill schedules the one-off upgrade sweep: 20s after boot,
// probe every stored profile that was never probed (serially, 2s apart). It is a
// no-op when autoprobe is disabled.
func (s *Server) startVisionProbeBackfill() {
	if s == nil || s.m == nil || s.m.pg == nil || !visionAutoprobeEnabled() {
		return
	}
	go func() {
		t := time.NewTimer(visionProbeBackfillDelay)
		defer t.Stop()
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		profs, err := s.m.pg.ListProfiles()
		if err != nil {
			log.Printf("[llm] 视觉补测读取配置失败: %v", err)
			return
		}
		for _, p := range profs {
			if s.ctx.Err() != nil {
				return
			}
			if !visionProbeNeeded(p) {
				continue
			}
			if !beginVisionProbe(&s.visionProbing, p.ID) {
				continue
			}
			cfg, ok := s.loadProfileConfig(p.ID)
			if !ok {
				s.visionProbing.Delete(p.ID)
				continue // 没 key 的配置发不出请求，跳过
			}
			sig := llmpresets.VisionSignature(p.Format, p.BaseURL, p.Model)
			ctx, cancel := context.WithTimeout(s.ctx, visionProbeAutoTimeout)
			out := probeVisionConfig(ctx, cfg)
			cancel()
			if out.ErrMsg == "" {
				s.applyVisionProbeResult(p.ID, probeValue(out.OK), sig)
			}
			s.visionProbing.Delete(p.ID)
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(visionProbeBackfillGap):
			}
		}
	}()
}

// probeVisionConfig sends one solid-red PNG and asks for its colour. It forces
// Vision=true so the request-side guard cannot strip the image: the point is to
// test what the endpoint can do.
//
// 两个真实踩过的坑：
//  1. 配了 SessionHeaderKey 的网关（如 OpenCode）缺 session 头会直接 400，
//     所以这里挂一个一次性 session id，让探测走与真实对话相同的发头逻辑；
//  2. Complete 的第二个返回值是 stopReason，不是正文。只认 msg.Text()——
//     否则 finish_reason="stop" 会被当成模型的回答，把「没看图」误判成通过。
func probeVisionConfig(ctx context.Context, cfg agent.Config) visionProbeOutcome {
	cfg.Vision = true
	start := time.Now()
	prov, err := cfg.NewProvider()
	if err != nil {
		return visionProbeOutcome{ErrMsg: "构建 provider 失败：" + err.Error()}
	}
	pngBytes, err := solidPNG(64, 64, color.RGBA{R: 220, G: 30, B: 30, A: 255})
	if err != nil {
		return visionProbeOutcome{ErrMsg: "生成测试图片失败：" + err.Error()}
	}
	ctx = transcript.WithSessionID(ctx, "visionprobe-"+transcript.NewSessionID())
	req := llm.CompletionRequest{
		System: []string{"你是一个严格的图像识别测试器，只按用户要求回答。"},
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{
			llm.TextBlock("这张图片是什么颜色？只回答颜色名称，不要解释。"),
			llm.ImageBlock("image/png", pngBytes),
		}}},
		MaxTokens: 64,
		Thinking:  "disabled",
	}
	msg, _, _, err := prov.Complete(ctx, req)
	out := visionProbeOutcome{LatencyMS: time.Since(start).Milliseconds()}
	if err != nil {
		out.ErrMsg = err.Error()
		return out
	}
	reply := strings.TrimSpace(msg.Text())
	out.Reply = reply
	low := strings.ToLower(reply)
	out.OK = strings.Contains(reply, "红") || strings.Contains(low, "red") || strings.Contains(low, "crimson")
	return out
}

// solidPNG renders a w×h solid-colour PNG (used only by the probe).
func solidPNG(w, h int, c color.RGBA) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func clipProbeText(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
