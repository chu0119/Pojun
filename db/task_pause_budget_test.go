package db

import (
	"testing"
	"time"
)

// v0.9.90 backport: pausing must accumulate paused_total_seconds; the minute
// budget subtracts it so a long pause does not consume the task's runtime budget.
func TestSetPausedAccumulatesPausedSeconds(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	task, err := d.CreateTask("pause budget", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	if err := d.SetPaused(task.ID, true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if err := d.SetPaused(task.ID, false); err != nil {
		t.Fatal(err)
	}
	var secs int64
	if err := d.QueryRow(`SELECT paused_total_seconds FROM tasks WHERE id=$1`, task.ID).Scan(&secs); err != nil {
		t.Fatal(err)
	}
	if secs < 1 || secs > 10 {
		t.Fatalf("paused_total_seconds=%d, want 1..10", secs)
	}
}
