package eval

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/chu0119/pojun/db"
	"github.com/chu0119/pojun/pricing"
)

// RunResult 是一次任务运行的可比较快照（JSON 落盘后可用于 A/B diff）。
type RunResult struct {
	ScenarioID   string    `json:"scenario_id"`
	ScenarioName string    `json:"scenario_name"`
	TaskID       int64     `json:"task_id"`
	GeneratedAt  time.Time `json:"generated_at"`
	Status       string    `json:"status"`

	Pass       bool     `json:"pass"`
	PassReason string   `json:"pass_reason"`
	BudgetNote []string `json:"budget_notes,omitempty"`

	GoalsTotal  int  `json:"goals_total"`
	GoalsMet    int  `json:"goals_met"`
	RequireGoal bool `json:"require_goal"`

	FindingsTotal int              `json:"findings_total"`
	Expected      []ExpectedResult `json:"expected"`
	ExpectedFound int              `json:"expected_found"` // 含 optional
	RequiredTotal int              `json:"required_total"`
	RequiredFound int              `json:"required_found"`
	Unexpected    []FindingBrief   `json:"unexpected,omitempty"`
	UnexpectedN   int              `json:"unexpected_count"`
	ExtraRate     float64          `json:"extra_rate"` // unexpected / findings_total，没有人工复核，只是 proxy

	EvidenceCompleteness float64 `json:"evidence_completeness"` // 命中项中证据达标的占比

	Stages     map[string]bool `json:"stages,omitempty"`
	StagesSeen int             `json:"stages_seen"`
	StageTotal int             `json:"stage_total"`

	TokensInput  int64        `json:"tokens_input"`
	TokensOutput int64        `json:"tokens_output"`
	CostUSD      float64      `json:"cost_usd"`
	CostKnown    bool         `json:"cost_known"`
	UsageModels  []ModelUsage `json:"usage_models,omitempty"`

	DurationMinutes    float64 `json:"duration_minutes"`
	DurationComparable bool    `json:"duration_comparable"` // 只有终态任务的耗时可与场景上限比较
	Artifacts          int     `json:"artifacts"`
}

type ExpectedResult struct {
	VulnClass        string `json:"vulnclass"`
	Asset            string `json:"asset,omitempty"`
	Severity         string `json:"severity,omitempty"`
	Optional         bool   `json:"optional"`
	Matched          bool   `json:"matched"`
	FindingID        int64  `json:"finding_id,omitempty"`
	EvidenceRunes    int    `json:"evidence_runes,omitempty"`
	EvidenceOK       bool   `json:"evidence_ok"`
	MinEvidenceRunes int    `json:"min_evidence_runes"`
}

type FindingBrief struct {
	ID            int64  `json:"id"`
	VulnClass     string `json:"vulnclass"`
	Name          string `json:"name"`
	Severity      string `json:"severity"`
	EvidenceRunes int    `json:"evidence_runes"`
	AssetText     string `json:"asset_text,omitempty"`
}

type ModelUsage struct {
	Model        string  `json:"model"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	Priced       bool    `json:"priced"`
}

type findingRow struct {
	id        int64
	vulnClass string
	name      string
	severity  string
	evidence  string
	report    string
	status    string
	assetText string
}

// ScoreTask 对已有 task 做只读评分。同一份 DB 数据多次调用结果稳定。
func ScoreTask(ctx context.Context, d *db.DB, sc *Scenario, taskID int64) (*RunResult, error) {
	if sc == nil {
		return nil, fmt.Errorf("scenario 为空")
	}
	if err := sc.Validate(); err != nil {
		return nil, err
	}
	res := &RunResult{
		ScenarioID: sc.ID, ScenarioName: sc.Name, TaskID: taskID,
		GeneratedAt: time.Now().UTC(), RequireGoal: sc.RequireGoal,
		Stages: map[string]bool{},
	}
	// llm_usage.task_id / task_artifacts.task_id 是 TEXT（历史口径），findings.task_id
	// 是 BIGINT；显式准备字符串形态，避免 $1::text 收到 int64 时 pgx 无法编码。
	taskKey := fmt.Sprintf("%d", taskID)
	var (
		createdAt, firstRunAt, completedAt sql.NullTime
		expID                              int64
	)
	err := d.QueryRowContext(ctx, `
SELECT status, exploration_id, created_at, first_run_at, completed_at
FROM tasks WHERE id=$1`, taskID).Scan(&res.Status, &expID, &createdAt, &firstRunAt, &completedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("task %d 不存在", taskID)
	}
	if err != nil {
		return nil, err
	}
	if err := d.QueryRowContext(ctx, `
SELECT count(*), count(*) FILTER (WHERE state='met')
FROM exploration_nodes WHERE exploration_id=$1 AND kind='goal'`, expID).
		Scan(&res.GoalsTotal, &res.GoalsMet); err != nil {
		return nil, err
	}

	rows, err := d.QueryContext(ctx, `
SELECT f.id, COALESCE(f.vulnclass,''), COALESCE(f.name,''), COALESCE(f.severity,''),
       COALESCE(f.evidence,''), COALESCE(f.report,''), COALESCE(f.status,''),
       COALESCE((SELECT string_agg(COALESCE(a.url, a.domain, a.ip), ' ')
                   FROM assets a
                  WHERE a.id IN (SELECT jsonb_array_elements_text(f.asset_ids)::bigint)), '')
FROM findings f WHERE f.task_id=$1 AND f.status <> 'duplicate' AND f.merged_into_id IS NULL ORDER BY f.id`, taskID)
	if err != nil {
		return nil, err
	}
	var findings []findingRow
	for rows.Next() {
		var f findingRow
		if err := rows.Scan(&f.id, &f.vulnClass, &f.name, &f.severity, &f.evidence, &f.report, &f.status, &f.assetText); err != nil {
			rows.Close()
			return nil, err
		}
		findings = append(findings, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	res.FindingsTotal = len(findings)

	// ---- expected 匹配 ----
	used := make(map[int]bool, len(findings))
	evidenceOK := 0
	for _, e := range sc.Expected {
		want := db.NormalizeVulnClass(e.VulnClass)
		er := ExpectedResult{
			VulnClass: e.VulnClass, Asset: e.Asset, Severity: e.Severity,
			Optional: e.Optional, MinEvidenceRunes: e.minEvidence(),
		}
		for i, f := range findings {
			if used[i] {
				continue
			}
			if db.NormalizeVulnClass(f.vulnClass) != want {
				continue
			}
			if e.Asset != "" && !strings.Contains(strings.ToLower(f.assetText), strings.ToLower(e.Asset)) {
				continue
			}
			if e.Severity != "" && !strings.EqualFold(f.severity, e.Severity) {
				continue
			}
			used[i] = true
			er.Matched = true
			er.FindingID = f.id
			er.EvidenceRunes = len([]rune(f.evidence))
			er.EvidenceOK = er.MinEvidenceRunes == 0 || er.EvidenceRunes >= er.MinEvidenceRunes
			break
		}
		res.Expected = append(res.Expected, er)
		if er.Matched {
			res.ExpectedFound++
			if er.EvidenceOK {
				evidenceOK++
			}
			if !e.Optional {
				res.RequiredFound++
			}
		}
	}
	res.RequiredTotal = sc.RequiredCount()
	if res.ExpectedFound > 0 {
		res.EvidenceCompleteness = float64(evidenceOK) / float64(res.ExpectedFound)
	}
	for i, f := range findings {
		if used[i] {
			continue
		}
		res.UnexpectedN++
		if len(res.Unexpected) < 50 {
			res.Unexpected = append(res.Unexpected, FindingBrief{
				ID: f.id, VulnClass: f.vulnClass, Name: f.name, Severity: f.severity,
				EvidenceRunes: len([]rune(f.evidence)), AssetText: f.assetText,
			})
		}
	}
	if res.FindingsTotal > 0 {
		res.ExtraRate = float64(res.UnexpectedN) / float64(res.FindingsTotal)
	}

	// ---- 阶段覆盖 ----
	if len(sc.Stages) > 0 {
		seen, err := seenDimensions(ctx, d, expID)
		if err != nil {
			return nil, err
		}
		for _, st := range sc.Stages {
			res.Stages[st] = seen[strings.ToLower(strings.TrimSpace(st))]
			if res.Stages[st] {
				res.StagesSeen++
			}
		}
		res.StageTotal = len(sc.Stages)
	}

	// ---- 用量/成本 ----
	allPriced := true
	usageRows, err := d.QueryContext(ctx, `
SELECT COALESCE(model,''),
       COALESCE(SUM(COALESCE(input_tokens,0)+COALESCE(cache_read,0)+COALESCE(cache_write,0)),0),
       COALESCE(SUM(COALESCE(output_tokens,0)),0)
FROM llm_usage WHERE task_id=$1::text GROUP BY model ORDER BY model`, taskKey)
	if err != nil {
		if !isMissingTable(err) {
			return nil, err
		}
		// 极老的库/精简库可能还没有 llm_usage；评测继续，成本按 0 记。
		usageRows = nil
	}
	if usageRows != nil {
		for usageRows.Next() {
			var m ModelUsage
			if err := usageRows.Scan(&m.Model, &m.InputTokens, &m.OutputTokens); err != nil {
				usageRows.Close()
				return nil, err
			}
			m.CostUSD, m.Priced = pricing.Estimate(m.Model, m.InputTokens, m.OutputTokens)
			if !m.Priced {
				allPriced = false
			}
			res.TokensInput += m.InputTokens
			res.TokensOutput += m.OutputTokens
			res.CostUSD += m.CostUSD
			res.UsageModels = append(res.UsageModels, m)
		}
		usageRows.Close()
		if err := usageRows.Err(); err != nil {
			return nil, err
		}
	}
	res.CostKnown = allPriced || len(res.UsageModels) == 0

	// ---- 耗时/附件 ----
	start := createdAt.Time
	if firstRunAt.Valid {
		start = firstRunAt.Time
	}
	end := time.Now()
	if completedAt.Valid {
		end = completedAt.Time
	}
	if !start.IsZero() {
		res.DurationMinutes = end.Sub(start).Minutes()
	}
	// 只有终态任务的耗时才和场景的 max_minutes 可比。paused/running 的任务可能
	// 已闲置几十天，把 now-first_run 当耗时会让预算判定误报（线上实测暂停任务
	// 显示 27009 分钟）。
	switch res.Status {
	case "done", "failed", "timeout":
		res.DurationComparable = true
	default:
		res.BudgetNote = append(res.BudgetNote, fmt.Sprintf("任务未终态（status=%s），耗时不参与 max_minutes 判定", res.Status))
	}
	_ = d.QueryRowContext(ctx, `SELECT count(*) FROM task_artifacts WHERE task_id=$1::text`, taskKey).Scan(&res.Artifacts)

	// ---- 判定 ----
	res.Pass, res.PassReason = judge(sc, res)
	if sc.MaxMinutes > 0 && res.DurationComparable && res.DurationMinutes > float64(sc.MaxMinutes) {
		res.Pass = false
		res.BudgetNote = append(res.BudgetNote, fmt.Sprintf("超时：%.1f 分钟 > 上限 %d 分钟", res.DurationMinutes, sc.MaxMinutes))
	}
	if sc.MaxUSD > 0 {
		if !res.CostKnown {
			res.BudgetNote = append(res.BudgetNote, "有模型不在内置定价表，USD 上限未强制执行（token 仍计入）")
		} else if res.CostUSD > sc.MaxUSD {
			res.Pass = false
			res.BudgetNote = append(res.BudgetNote, fmt.Sprintf("超预算：$%.4f > 上限 $%.4f", res.CostUSD, sc.MaxUSD))
		}
	}
	if len(res.BudgetNote) > 0 {
		res.PassReason += "；" + strings.Join(res.BudgetNote, "；")
	}
	return res, nil
}

func isMissingTable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "42P01"
	}
	return strings.Contains(err.Error(), "does not exist")
}

func seenDimensions(ctx context.Context, d *db.DB, expID int64) (map[string]bool, error) {
	rows, err := d.QueryContext(ctx, `SELECT DISTINCT dimension FROM intent_reflections WHERE exploration_id=$1 AND dimension<>''`, expID)
	if err != nil {
		if isMissingTable(err) {
			return map[string]bool{}, nil
		}
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var dim string
		if err := rows.Scan(&dim); err != nil {
			return nil, err
		}
		out[strings.ToLower(strings.TrimSpace(dim))] = true
	}
	return out, rows.Err()
}

func judge(sc *Scenario, res *RunResult) (bool, string) {
	var missing []string
	for _, e := range res.Expected {
		if e.Optional {
			continue
		}
		if !e.Matched {
			missing = append(missing, e.VulnClass)
			continue
		}
		if !e.EvidenceOK {
			missing = append(missing, fmt.Sprintf("%s(证据 %d<%d 字)", e.VulnClass, e.EvidenceRunes, e.MinEvidenceRunes))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return false, "未满足必得项：" + strings.Join(missing, ", ")
	}
	if sc.RequireGoal {
		if res.GoalsTotal == 0 {
			return false, "任务没有 goal，无法满足 require_goal"
		}
		if res.GoalsMet < res.GoalsTotal {
			return false, fmt.Sprintf("目标未全部达成：%d/%d", res.GoalsMet, res.GoalsTotal)
		}
	}
	return true, fmt.Sprintf("必得项 %d/%d 命中且证据达标", res.RequiredFound, res.RequiredTotal)
}
