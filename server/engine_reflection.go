package server

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"unicode/utf8"

	"github.com/chu0119/pojun/agent"
	"github.com/chu0119/pojun/db"
)

// recordIntentReflection writes one deterministic terminal lesson per intent
// (v0.4.100). No LLM call: state, write counts, tool/error counts and the
// intent's own hypothesis/criteria are enough to stop the planner/worker from
// repeating a dead direction. Failures are logged and ignored — a lesson write
// must never change the terminal state of an intent.
func (e *Engine) recordIntentReflection(t *Task, intent *db.Node, state, stopReason string, wrote agent.WriteCounts) {
	if t == nil || t.Store == nil || intent == nil {
		return
	}
	var p struct {
		Dimension       string  `json:"dimension"`
		Summary         string  `json:"summary"`
		Title           string  `json:"title"`
		Hypothesis      string  `json:"hypothesis"`
		SuccessCriteria string  `json:"success_criteria"`
		AssetIDs        []int64 `json:"asset_ids"`
	}
	_ = json.Unmarshal(intent.Payload, &p)
	summary := p.Summary
	if summary == "" {
		summary = p.Title
	}
	dim := agent.IntentDimension(p.Dimension, summary)
	if dim == "" {
		dim = "other"
	}
	if len(p.AssetIDs) == 0 && len(intent.Anchors) > 0 {
		p.AssetIDs = intent.Anchors
	}
	calls, errs, err := t.Store.ActivityStatsForIntent(intent.ID)
	if err != nil {
		calls, errs = 0, 0
	}
	success := state == "done" && wrote.Total() > 0
	// PostgreSQL text/jsonb rejects invalid UTF-8. Lesson truncation already keeps
	// rune boundaries, but other fields can inherit bytes from tool output or a
	// stop reason; sanitize every text column as a final guard.
	summary = sanitizeReflectionText(summary)
	criteria := sanitizeReflectionText(p.SuccessCriteria)
	reason := sanitizeReflectionText(stopReason)
	r := db.IntentReflection{
		IntentID:        intent.ID,
		Dimension:       dim,
		TerminalState:   state,
		TerminalReason:  reason,
		WroteBack:       wrote.Total(),
		ToolCalls:       calls,
		ToolErrors:      errs,
		Success:         success,
		Hypothesis:      sanitizeReflectionText(p.Hypothesis),
		SuccessCriteria: criteria,
		Summary:         summary,
		Lesson:          buildReflectionLesson(state, reason, summary, criteria, wrote, calls, errs),
		AssetIDs:        p.AssetIDs,
		Payload:         intent.Payload,
	}
	if err := t.Store.RecordIntentReflection(r); err != nil {
		log.Printf("[reflection] task %s 意图 #%d 终态经验落库失败: %v", t.ID, intent.ID, err)
	}
}

// buildReflectionLesson is pure and unit-tested.
func buildReflectionLesson(state, reason, summary, criteria string, wrote agent.WriteCounts, calls, errs int) string {
	var b strings.Builder
	switch state {
	case "done":
		if wrote.Total() > 0 {
			fmt.Fprintf(&b, "完成且有写回（事实%d/资产%d/漏洞%d，工具%d次/错误%d次）。",
				wrote.Facts, wrote.Assets, wrote.Findings, calls, errs)
		} else {
			fmt.Fprintf(&b, "完成但零写回（工具%d次/错误%d次）：需确认是否真的验证过，下轮补事实或换证据形态。", calls, errs)
		}
	case "exhausted":
		fmt.Fprintf(&b, "耗尽无产出（原因 %s；工具%d次/错误%d次）：该方向可能已到边界或打法无效，下轮换入口/换手法，不要原样重试。",
			shortReflectionReason(reason), calls, errs)
	case "blocked":
		fmt.Fprintf(&b, "阻塞（原因 %s；工具%d次/错误%d次）：先排除工具/环境/凭据问题再决定是否重试。",
			shortReflectionReason(reason), calls, errs)
	case "stopped":
		fmt.Fprintf(&b, "被终止（原因 %s；工具%d次/错误%d次）：按需复盘是否值得换角度继续。",
			shortReflectionReason(reason), calls, errs)
	default:
		fmt.Fprintf(&b, "终态 %s（工具%d次/错误%d次）。", state, calls, errs)
	}
	if s := strings.TrimSpace(summary); s != "" {
		fmt.Fprintf(&b, "原方向：%s。", firstLineText(s, 120))
	}
	if c := strings.TrimSpace(criteria); c != "" {
		fmt.Fprintf(&b, "验收标准：%s。", firstLineText(c, 100))
	}
	return b.String()
}

func shortReflectionReason(s string) string {
	s = sanitizeReflectionText(strings.TrimSpace(s))
	if s == "" {
		return "-"
	}
	if i := strings.IndexAny(s, ":： "); i > 0 {
		s = s[:i]
	}
	return truncateUTF8Bytes(s, 80)
}

func firstLineText(s string, max int) string {
	s = sanitizeReflectionText(strings.TrimSpace(strings.SplitN(s, "\n", 2)[0]))
	return truncateUTF8Bytes(s, max)
}

// sanitizeReflectionText makes a string safe for PostgreSQL text columns.
// JSON decoding usually replaces invalid bytes already, but stop reasons and
// tool-derived summaries can still carry them; U+FFFD is the least surprising
// replacement and keeps the surrounding content readable.
func sanitizeReflectionText(s string) string {
	return strings.ToValidUTF8(s, "\uFFFD")
}

// truncateUTF8Bytes returns at most maxBytes bytes without splitting a rune.
// A plain s[:max] on Chinese/emoji text can end in the middle of a multi-byte
// sequence, which PostgreSQL rejects with SQLSTATE 22021. Sanitize before
// calling this so invalid bytes cannot survive the backtracking loop.
func truncateUTF8Bytes(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	n := maxBytes
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
