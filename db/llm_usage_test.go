package db

import (
	"fmt"
	"testing"
	"time"
)

// TestLatestTaskUsageProfile pins the per-role "newest row" view used to show
// which profile/model each lane of a task is currently bound to.
//   - newest ts wins for a worker, older rows are shadowed;
//   - a tie on ts is broken by id DESC (same-instant writes are common: one
//     transaction, or a clock tick shared by two calls);
//   - only requested workers are returned;
//   - empty workers short-circuits to an empty map;
//   - an unknown task yields an empty map and a nil error.
func TestLatestTaskUsageProfile(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	if err := d.EnsureLLMUsageTable(); err != nil {
		t.Fatalf("ensure llm_usage: %v", err)
	}

	taskID := fmt.Sprintf("t_lup_%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_usage WHERE task_id=$1`, taskID) })

	// Truncate to microseconds: TIMESTAMPTZ keeps µs, and Equal would otherwise
	// trip on the dropped nanoseconds.
	base := time.Now().UTC().Truncate(time.Microsecond)
	insert := func(worker, profile, model, status string, ts time.Time) {
		t.Helper()
		if _, err := d.Exec(`
INSERT INTO llm_usage(task_id, worker, model, profile_name, status, ts)
VALUES ($1,$2,$3,$4,$5,$6)`, taskID, worker, model, profile, status, ts); err != nil {
			t.Fatalf("insert %s @ %s: %v", worker, ts.Format(time.RFC3339Nano), err)
		}
	}

	// Same worker, two timestamps: the newer one must shadow the older.
	insert("worker", "old-profile", "old-model", "error", base)
	insert("worker", "new-profile", "new-model", "ok", base.Add(2*time.Minute))
	// Second worker, so the map must carry two keys.
	insert("planner", "plan-profile", "plan-model", "ok", base.Add(time.Minute))
	// Third worker, identical ts twice: id DESC must pick the later insert.
	tie := base.Add(3 * time.Minute)
	insert("goals", "goals-first", "goals-model", "error", tie)
	insert("goals", "goals-second", "goals-model", "ok", tie)

	got, err := d.LatestTaskUsageProfile(taskID, []string{"worker", "planner", "goals"})
	if err != nil {
		t.Fatalf("latest profile: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("map keys=%d want 3: %+v", len(got), got)
	}
	if w := got["worker"]; w.ProfileName != "new-profile" || w.Model != "new-model" || w.Status != "ok" ||
		!w.At.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("worker must be the newest row: %+v", w)
	}
	if p := got["planner"]; p.ProfileName != "plan-profile" || p.Model != "plan-model" || p.Status != "ok" {
		t.Fatalf("planner row: %+v", p)
	}
	if g := got["goals"]; g.ProfileName != "goals-second" || g.Status != "ok" || !g.At.Equal(tie) {
		t.Fatalf("ts tie must be broken by id DESC: %+v", g)
	}

	// Only the requested roles come back.
	subset, err := d.LatestTaskUsageProfile(taskID, []string{"planner"})
	if err != nil {
		t.Fatalf("subset: %v", err)
	}
	if len(subset) != 1 {
		t.Fatalf("subset keys=%d want 1: %+v", len(subset), subset)
	}
	if _, ok := subset["worker"]; ok {
		t.Fatalf("unrequested worker leaked into result: %+v", subset)
	}

	// Empty worker list: empty map, no query, no error.
	for _, workers := range [][]string{nil, {}} {
		empty, err := d.LatestTaskUsageProfile(taskID, workers)
		if err != nil {
			t.Fatalf("empty workers (len=%d): %v", len(workers), err)
		}
		if len(empty) != 0 {
			t.Fatalf("empty workers (len=%d) must yield an empty map: %+v", len(workers), empty)
		}
	}

	// Unknown task: empty map, nil error.
	none, err := d.LatestTaskUsageProfile(taskID+"_missing", []string{"worker", "planner"})
	if err != nil {
		t.Fatalf("unknown task: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("unknown task must yield an empty map: %+v", none)
	}
}
