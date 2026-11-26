package agent

// 出货前 Triage 闸门（v0.9.86，A1-2）。
//
// ## 为什么
//
// ARTEMIS 的做法是：报告前先过独立 triager（相关性/范围/重复/可复现），
// 82% 的报告因此是有效漏洞。我们的现状是 report_finding 只管"写进去"，
// 证据不足、跨范围、同根因重复的记录照样进列表与报告——人工复核时才被发现。
//
// ## 为什么是确定性的
//
// 与 audit_finding 同口径：**不调模型**。LLM 判官会把"看起来很专业"的文本判成
// 真漏洞，且不可复现；这里只检查可枚举的事实（证据、流量、复核、重复候选、
// 资产归属），结论可解释、可回归。
//
// ## 为什么只提示不阻断
//
// 与 v0.9.85「evidence_warnings 只警告不拒绝」保持一致：真实漏洞常因目标环境
// 拿不到完整证据（无回显 RCE、TCP 服务、需要授权的写操作）。阻断会丢漏洞，
// 提示 + 报告里标注"待验证"才是正确取舍。verdict 会进 report_finding 回执与
// triage_finding 结果，reporter 写报告时按 verdict 决定是否标注。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/chu0119/pojun/db"

	actool "github.com/Autumn-27/norma/tool"
)

// TriageCheck 是一项检查的结论：pass / warn / fail。
type TriageCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// TriageResult 是一条 finding 的出货前体检结论。
type TriageResult struct {
	FindingID  int64         `json:"finding_id"`
	Verdict    string        `json:"verdict"` // pass|review|needs_evidence|duplicate
	Checks     []TriageCheck `json:"checks"`
	Missing    []string      `json:"missing,omitempty"`
	Duplicates []int64       `json:"duplicate_candidates,omitempty"`
	Note       string        `json:"note,omitempty"`
}

const (
	TriagePass          = "pass"
	TriageReview        = "review"
	TriageNeedsEvidence = "needs_evidence"
	TriageDuplicate     = "duplicate"
)

// triageCore 只依赖调用方已取到的数据（finding 行、verification_strategy、
// 流量数、同任务合并候选）。报告回执与 triage_finding 工具共用这一份逻辑，
// 避免两处口径漂移。纯函数、可单测。
func triageCore(f *db.DBFinding, payload map[string]any, trafficCount int, mergeCandidates []int64) TriageResult {
	out := TriageResult{FindingID: f.ID, Duplicates: mergeCandidates}
	add := func(name, status, detail string) {
		out.Checks = append(out.Checks, TriageCheck{Name: name, Status: status, Detail: detail})
		if status == "fail" {
			out.Missing = append(out.Missing, name)
		}
	}
	ev := strings.TrimSpace(f.Evidence)
	switch {
	case ev == "":
		add("evidence_present", "fail", "证据为空：没有任何请求/响应或命令输出，不能交付")
	default:
		runes := len([]rune(ev))
		if runes < 120 {
			add("evidence_depth", "warn", fmt.Sprintf("证据只有 %d 字，复核者无法独立重放；建议补原始请求/响应（≥120 字）", runes))
		} else {
			add("evidence_depth", "pass", fmt.Sprintf("%d 字", runes))
		}
	}
	audit := auditFindingPayload(f, payload, trafficCount)
	switch audit.Status {
	case "pass":
		add("verification", "pass", "策略="+audit.Strategy)
	case "needs_human":
		add("verification", "warn", "策略=manual："+audit.Note)
	default:
		add("verification", "fail", "策略="+audit.Strategy+" 未通过："+strings.Join(audit.Missing, "、"))
	}
	if trafficCount > 0 {
		add("traffic_bound", "pass", fmt.Sprintf("%d 条", trafficCount))
	} else {
		add("traffic_bound", "warn", "未绑定流量；HTTP 漏洞请在漏洞页补绑原始交换，TCP/无包可忽略")
	}
	if len(mergeCandidates) > 0 {
		ids := make([]string, 0, len(mergeCandidates))
		for _, id := range mergeCandidates {
			ids = append(ids, fmt.Sprintf("#%d", id))
		}
		add("duplicate", "warn", "同任务存在疑似同根因记录："+strings.Join(ids, "、")+"；交付前核对，确认同源就合并")
	} else {
		add("duplicate", "pass", "无同根因候选")
	}
	// 判定顺序：已合并/重复状态 → 证据硬缺口 → 其余提示。
	switch {
	case f.Status == db.FindingDuplicate:
		out.Verdict = TriageDuplicate
		out.Note = "该记录已是重复/已合并状态：不要重复上报，交付时以目标记录为准。"
	case len(out.Missing) > 0:
		out.Verdict = TriageNeedsEvidence
		out.Note = "证据硬缺口：" + strings.Join(out.Missing, "、") + "。补齐后再交付；报告里不要写成已确认。"
	default:
		out.Verdict = TriagePass
		for _, c := range out.Checks {
			if c.Status == "warn" {
				out.Verdict = TriageReview
				break
			}
		}
		if out.Verdict == TriageReview {
			out.Note = "可以记录，但交付前建议按 warn 项补齐；报告里区分「已确认」与「待补证」。"
		} else {
			out.Note = "确定性检查通过（不代表人工终审）。"
		}
	}
	return out
}

// triageFindingTool：planner/main 可用（与 audit_finding 同口径，worker 不给）。
func (t *ToolSet) triageFindingTool() actool.CoreTool {
	return t.readExpTool(
		"triage_finding",
		"独立只读出货前体检（确定性，不调模型、不改库）：证据是否可重放、verification 策略是否通过、"+
			"流量是否绑定、是否存在同根因重复、资产是否属于本任务。返回 verdict=pass/review/needs_evidence/duplicate，"+
			"以及 missing 清单与重复候选。\n"+
			"用途：交付/写报告前对 critical/high 逐条跑一遍；verdict 不是拒绝，报告里要如实标注「已确认」还是「待补证」。",
		obj(map[string]any{
			"finding_id": idp("漏洞记录的独立 ID（findings.id，不是图节点 ID）"),
		}, "finding_id"),
		func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
			var a struct {
				FindingID int64 `json:"finding_id"`
			}
			if uErr := json.Unmarshal(in, &a); uErr != nil {
				return actool.Errorf(fmt.Sprintf("参数不是合法 JSON：%v", uErr)), nil
			}
			if a.FindingID <= 0 {
				return actool.Errorf("finding_id 必填（findings.id）"), nil
			}
			pg := t.pg()
			if pg == nil {
				return actool.Errorf("triage_finding 需要数据库上下文"), nil
			}
			f, err := pg.GetFinding(a.FindingID)
			if err != nil {
				return actool.Errorf(fmt.Sprintf("读取 finding 失败：%v", err)), nil
			}
			if f == nil {
				return actool.Errorf("finding 不存在"), nil
			}
			if f.TaskID == nil || (t.taskID > 0 && *f.TaskID != t.taskID) {
				return actool.Errorf("该漏洞不属于本任务（跨任务 triage 无意义）"), nil
			}
			payload := map[string]any{}
			if f.NodeID != nil && t.ts != nil {
				if n, nErr := t.ts.GetNode(*f.NodeID); nErr == nil && n != nil {
					_ = json.Unmarshal(n.Payload, &payload)
				}
			}
			trafficCount := 0
			if ft, tErr := pg.GetFindingTraffic(ctx, f.ID); tErr == nil && ft != nil {
				trafficCount = len(ft.Bindings)
			}
			cands, _ := pg.FindingMergeCandidates(ctx, f.ID)
			ids := make([]int64, 0, len(cands))
			for _, c := range cands {
				ids = append(ids, c.ID)
			}
			out := triageCore(f, payload, trafficCount, ids)
			// 资产归属：triage 工具独有的检查（报告回执里没有 asset store 上下文）。
			if len(f.AssetIDs) == 0 {
				out.Checks = append(out.Checks, TriageCheck{Name: "scope_asset", Status: "warn", Detail: "finding 未锚定任何资产"})
				if out.Verdict == TriagePass {
					out.Verdict = TriageReview
				}
			} else if t.as != nil {
				assets, aErr := t.as.GetByIDs(f.AssetIDs)
				if aErr == nil {
					inScope := 0
					for _, asset := range assets {
						for _, tid := range asset.TaskIDs {
							if tid == *f.TaskID {
								inScope++
								break
							}
						}
					}
					if inScope == 0 {
						out.Checks = append(out.Checks, TriageCheck{Name: "scope_asset", Status: "warn", Detail: "锚定资产未与任务范围关联（可能跨范围）"})
						if out.Verdict == TriagePass {
							out.Verdict = TriageReview
						}
					} else {
						out.Checks = append(out.Checks, TriageCheck{Name: "scope_asset", Status: "pass", Detail: fmt.Sprintf("%d 个资产属于本任务", inScope)})
					}
				}
			}
			return jsonResult(out)
		})
}

// pg 取工具集背后的 DB（测试里可能为 nil）。
func (t *ToolSet) pg() *db.DB {
	if t.as != nil {
		return t.as.DB()
	}
	if t.ts != nil {
		return t.ts.DB()
	}
	return nil
}
