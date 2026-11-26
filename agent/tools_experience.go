package agent

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/chu0119/pojun/db"

	actool "github.com/Autumn-27/norma/tool"
)

// searchExperience is an on-demand, read-only experience search (v0.4.103):
// current-task terminal lessons (intent_reflections) + cross-task successful
// playbooks. Worker/planner call it before repeating a direction or switching
// tactics; it is deliberately not injected wholesale into every prompt.
func (t *ToolSet) searchExperience() actool.CoreTool {
	return readTool(
		"search_experience",
		"检索经验（只读）：按关键词/维度查本任务的终态经验（成功、零产出、阻塞原因）与本任务成功 playbook。"+
			"在重复同一方向、换打法、或判断某条路是否已经试过之前调用；返回 lesson/playbook 摘要与来源。",
		obj(map[string]any{
			"query":     str("关键词（可选）：匹配摘要/lesson/假设/验收标准"),
			"dimension": str("维度过滤（可选）：recon/authz/injection/upload/ssrf/rce/cred-reuse/data-export/payment/privesc/lateral"),
			"vulnclass": str("漏洞类别（可选，用于匹配跨任务 playbook）"),
			"techs":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "技术栈关键词（可选，用于匹配 playbook）"},
			"limit":     intp("返回条数上限（默认 10，最大 30）"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.ts == nil {
				return actool.Errorf("search_experience 需要任务上下文"), nil
			}
			var a struct {
				Query     string   `json:"query"`
				Dimension string   `json:"dimension"`
				VulnClass string   `json:"vulnclass"`
				Techs     []string `json:"techs"`
				Limit     int      `json:"limit"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			limit := a.Limit
			if limit <= 0 {
				limit = 10
			}
			if limit > 30 {
				limit = 30
			}
			q := strings.ToLower(strings.TrimSpace(a.Query))
			dim := IntentDimension(a.Dimension, "")
			lessons := []map[string]any{}
			if items, err := t.ts.RecentReflections(200); err == nil {
				for _, r := range items {
					if dim != "" && r.Dimension != dim {
						continue
					}
					if q != "" {
						hay := strings.ToLower(r.Summary + " " + r.Lesson + " " + r.Hypothesis + " " + r.SuccessCriteria)
						if !strings.Contains(hay, q) {
							continue
						}
					}
					lessons = append(lessons, map[string]any{
						"intent_id": r.IntentID, "dimension": r.Dimension, "state": r.TerminalState,
						"reason": r.TerminalReason, "wrote": r.WroteBack, "calls": r.ToolCalls,
						"errors": r.ToolErrors, "lesson": r.Lesson,
					})
					if len(lessons) >= limit {
						break
					}
				}
			}
			playbooks := []db.Playbook{}
			if MatchPlaybooksFn != nil && (strings.TrimSpace(a.VulnClass) != "" || len(a.Techs) > 0) {
				playbooks = MatchPlaybooksFn(t.taskID, strings.TrimSpace(a.VulnClass), a.Techs, "", limit)
			}
			out := map[string]any{"lessons": lessons, "playbooks": playbooks}
			if len(lessons) == 0 && len(playbooks) == 0 {
				out["note"] = "没有匹配经验；这可能是新方向，按当前证据继续，并在终态把 lesson 写回（引擎自动）。"
			}
			return jsonResult(out)
		})
}
