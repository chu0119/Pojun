package server

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/chu0119/pojun/agent"
	"github.com/chu0119/pojun/db"
)

// 任务级预算网关（v0.4.101 P0-3）。
//
// 现有 intent 级预算只约束单条意图；planner 轮、压缩、reporter、复测都在预算外。
// 这里在任务级再设一道闸：
//   - token / USD / 墙钟 三类上限（0=不限）；
//   - 达到 warn_pct（默认 80%）注入一次收尾提醒；planner 据此收敛打穿、不开新长链；
//   - 达到 100% 走 settleTaskWithReason("budget")，终态原因可与其他 timeout 区分。
//
// 不做自动换模型：任务链里的 profile 成本差异无法可靠推断，贸然切换可能把任务
// 降到不可用的小模型。保留为后续 P1（需要 profile 价格元数据）。
const (
	settingTaskBudgetTokens  = "task_budget_tokens"
	settingTaskBudgetUSD     = "task_budget_usd"
	settingTaskBudgetMinutes = "task_budget_minutes"
	settingTaskBudgetWarnPct = "task_budget_warn_pct"

	defaultTaskBudgetWarnPct = 80
)

type taskBudgetConfig struct {
	Tokens  int64
	USD     float64
	Minutes int
	WarnPct int
}

func (c taskBudgetConfig) enabled() bool {
	return c.Tokens > 0 || c.USD > 0 || c.Minutes > 0
}

func taskBudgetFromPG(pg *db.DB) taskBudgetConfig {
	c := taskBudgetConfig{WarnPct: defaultTaskBudgetWarnPct}
	if pg == nil {
		return c
	}
	if n, ok := settingInt(pg, settingTaskBudgetTokens); ok && n > 0 {
		c.Tokens = int64(clampInt(n, 100_000, 2_000_000_000))
	}
	if v, ok, err := pg.GetSetting(settingTaskBudgetUSD); err == nil && ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
			if f > 1_000_000 {
				f = 1_000_000
			}
			c.USD = f
		}
	}
	if n, ok := settingInt(pg, settingTaskBudgetMinutes); ok && n > 0 {
		c.Minutes = clampInt(n, 1, 10080)
	}
	if n, ok := settingInt(pg, settingTaskBudgetWarnPct); ok {
		c.WarnPct = clampInt(n, 50, 95)
	}
	return c
}

type taskBudgetStatus struct {
	Config      taskBudgetConfig
	TokensUsed  int64
	USDUsed     float64
	MinutesUsed float64
	Pct         float64
	Level       string // ok|warn|exceeded
	Detail      string
	Unpriced    bool
}

func (e *Engine) taskBudgetStatus(t *Task) *taskBudgetStatus {
	if e == nil || e.m == nil || e.m.pg == nil || t == nil {
		return nil
	}
	cfg := taskBudgetFromPG(e.m.pg)
	if !cfg.enabled() {
		return nil
	}
	st := &taskBudgetStatus{Config: cfg}
	if u, err := e.m.pg.UsageByTask(t.ID); err == nil {
		st.TokensUsed = int64(u.InputTokens + u.OutputTokens)
	}
	if rows, err := e.m.pg.UsageModelsByTask(t.ID); err == nil {
		for _, r := range rows {
			c, known := estimateUSDCost(r.Model, r.InputTokens, r.OutputTokens)
			st.USDUsed += c
			if !known {
				st.Unpriced = true
			}
		}
	}
	// 分钟预算 = 首次运行至今 - 累计暂停 - 当前这次暂停。直接读 DB 而不是
	// t.FirstRunAt：后者在 lifecycleMu 下写，裸读会和 StampTaskFirstRun 竞态；
	// 暂停时长不扣除会让“暂停一段时间再恢复”直接撞上限收尾。
	if lifecycle := t.lifecycleSnapshot(); lifecycle.FirstRunAt > 0 {
		var secs float64
		err := e.m.pg.QueryRow(`SELECT GREATEST(0,
  EXTRACT(EPOCH FROM (now() - first_run_at))
  - COALESCE(paused_total_seconds,0)
  - CASE WHEN paused_at IS NOT NULL THEN EXTRACT(EPOCH FROM (now() - paused_at)) ELSE 0 END)
FROM tasks WHERE id=$1 AND first_run_at IS NOT NULL`, t.ID).Scan(&secs)
		if err != nil {
			secs = time.Since(time.Unix(lifecycle.FirstRunAt, 0)).Seconds()
		}
		st.MinutesUsed = secs / 60
	}
	maxPct := 0.0
	if cfg.Tokens > 0 {
		p := float64(st.TokensUsed) / float64(cfg.Tokens) * 100
		if p > maxPct {
			maxPct = p
		}
	}
	if cfg.USD > 0 {
		p := st.USDUsed / cfg.USD * 100
		if p > maxPct {
			maxPct = p
		}
	}
	if cfg.Minutes > 0 {
		p := st.MinutesUsed / float64(cfg.Minutes) * 100
		if p > maxPct {
			maxPct = p
		}
	}
	st.Pct = maxPct
	switch {
	case maxPct >= 100:
		st.Level = "exceeded"
	case maxPct >= float64(cfg.WarnPct):
		st.Level = "warn"
	default:
		st.Level = "ok"
	}
	parts := make([]string, 0, 3)
	if cfg.Tokens > 0 {
		parts = append(parts, fmt.Sprintf("tokens %d/%d (%.0f%%)", st.TokensUsed, cfg.Tokens, float64(st.TokensUsed)/float64(cfg.Tokens)*100))
	}
	if cfg.USD > 0 {
		parts = append(parts, fmt.Sprintf("USD %.2f/%.2f (%.0f%%)", st.USDUsed, cfg.USD, st.USDUsed/cfg.USD*100))
	}
	if cfg.Minutes > 0 {
		parts = append(parts, fmt.Sprintf("%.0f/%d min", st.MinutesUsed, cfg.Minutes))
	}
	st.Detail = strings.Join(parts, "；")
	if st.Unpriced {
		st.Detail += "；部分模型无定价，USD 为下限"
	}
	return st
}

// enforceTaskBudget is called at planner-round and worker-claim boundaries.
// Returns true when the task was settled due to budget exhaustion.
func (e *Engine) enforceTaskBudget(t *Task) bool {
	if e == nil || t == nil {
		return false
	}
	st := e.taskBudgetStatus(t)
	if st == nil || st.Level == "ok" {
		return false
	}
	switch st.Level {
	case "warn":
		key := fmt.Sprintf("task_budget_warned_%s", t.ID)
		if v, ok, _ := e.m.pg.GetSetting(key); ok {
			if ts, err := strconv.ParseInt(v, 10, 64); err == nil && time.Since(time.Unix(ts, 0)) < 30*time.Minute {
				return false
			}
		}
		_ = e.m.pg.SetSetting(key, strconv.FormatInt(time.Now().Unix(), 10))
		msg := fmt.Sprintf("【任务预算预警】已达任务预算的 %.0f%%（%s）。立即收敛：停止开新长链/新方向，"+
			"优先把已有线索打穿并写回 fact/finding；预算耗尽时任务会被强制收尾。", st.Pct, st.Detail)
		t.trigMu.Lock()
		t.pendingTriggers = append(t.pendingTriggers, agent.TriggerEvent{Kind: "budget_warn", Detail: msg})
		t.trigMu.Unlock()
		log.Printf("[budget] task %s 任务预算 %.0f%%（%s），已注入收敛提醒", t.ID, st.Pct, st.Detail)
		t.Notify()
		return false
	case "exceeded":
		log.Printf("[budget] task %s 任务预算耗尽（%s），强制收尾", t.ID, st.Detail)
		e.emitActivity(t, db.Activity{Worker: "system", Kind: "text",
			Summary: fmt.Sprintf("任务预算耗尽：%s；停止新增探索并收尾", st.Detail)})
		// 预算收尾不再跑终局 planner（那会再烧一轮 token）；直接按已有目标/证据落终态。
		e.settleTaskWithReason(context.Background(), t, "budget", "任务预算耗尽："+st.Detail)
		return true
	}
	return false
}
