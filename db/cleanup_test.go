package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func cleanupTestDB(t *testing.T) (*DB, int64) {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	if err := d.EnsureCleanupJobsTable(); err != nil {
		d.Close()
		t.Fatal(err)
	}
	taskID := time.Now().UnixNano()
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM cleanup_jobs WHERE task_id=$1`, taskID)
		d.Close()
	})
	return d, taskID
}

func TestCleanupJobsLifecycleStateMachine(t *testing.T) {
	d, taskID := cleanupTestDB(t)
	ctx := context.Background()
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := EnqueueCleanupJobTx(tx, taskID, "traffic", map[string]any{"hosts": []string{"a.example"}}); err != nil {
		t.Fatal(err)
	}
	// A second delete request for the same task/kind must not duplicate work.
	if err := EnqueueCleanupJobTx(tx, taskID, "traffic", map[string]any{"hosts": []string{"b.example"}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	jobs, err := d.ListCleanupJobs(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	var trafficJobs int
	for _, job := range jobs {
		if job.TaskID != nil && *job.TaskID == taskID && job.Kind == "traffic" {
			trafficJobs++
		}
	}
	if trafficJobs != 1 {
		t.Fatalf("duplicate active jobs=%d, want 1", trafficJobs)
	}

	job, err := d.ClaimCleanupJob(ctx, 0)
	if err != nil || job == nil {
		t.Fatalf("claim: job=%+v err=%v", job, err)
	}
	if job.TaskID == nil || *job.TaskID != taskID || job.Kind != "traffic" || job.State != "running" || job.Attempts != 1 {
		t.Fatalf("unexpected claimed job: %+v", job)
	}
	if err := d.UpdateCleanupJobProgress(ctx, job.ID, 5, 50); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishCleanupJob(ctx, job.ID, "boom"); err != nil {
		t.Fatal(err)
	}
	retry, err := d.ClaimCleanupJob(ctx, 0)
	if err != nil || retry != nil {
		t.Fatalf("job retried before backoff elapsed: job=%+v err=%v", retry, err)
	}
	if _, err := d.ExecContext(ctx, `UPDATE cleanup_jobs SET run_after=now() WHERE id=$1`, job.ID); err != nil {
		t.Fatal(err)
	}
	retry, err = d.ClaimCleanupJob(ctx, 0)
	if err != nil || retry == nil {
		t.Fatalf("second claim: job=%+v err=%v", retry, err)
	}
	if retry.Attempts != 2 {
		t.Fatalf("attempts=%d, want 2", retry.Attempts)
	}
	_ = d.FinishCleanupJob(ctx, retry.ID, "boom again")
	_, _ = d.ExecContext(ctx, `UPDATE cleanup_jobs SET run_after=now() WHERE id=$1`, job.ID)
	third, err := d.ClaimCleanupJob(ctx, 0)
	if err != nil || third == nil || third.Attempts != 3 {
		t.Fatalf("third claim: job=%+v err=%v", third, err)
	}
	if err := d.FinishCleanupJob(ctx, third.ID, "permanent"); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := d.QueryRowContext(ctx, `SELECT state FROM cleanup_jobs WHERE id=$1`, job.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "failed" {
		t.Fatalf("state=%q after three failures, want failed", state)
	}
	ok, err := d.RetryCleanupJob(ctx, job.ID)
	if err != nil || !ok {
		t.Fatalf("retry failed job: ok=%v err=%v", ok, err)
	}
	reopened, err := d.ClaimCleanupJob(ctx, 0)
	if err != nil || reopened == nil || reopened.Attempts != 1 || reopened.State != "running" {
		t.Fatalf("reopened claim: job=%+v err=%v", reopened, err)
	}
	if err := d.FinishCleanupJob(ctx, reopened.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRowContext(ctx, `SELECT state FROM cleanup_jobs WHERE id=$1`, job.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "done" {
		t.Fatalf("state=%q after success, want done", state)
	}
}

func TestTrafficHostsStillSharedRechecksOwnership(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	defer d.Close()
	first, err := d.CreateTask("shared host A", "shared host A", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.CreateTask("shared host B", "shared host B", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	suffix := time.Now().UnixNano()
	sharedHost := fmt.Sprintf("shared-%d.example.test", suffix)
	onlyHost := fmt.Sprintf("only-%d.example.test", suffix)
	t.Cleanup(func() {
		_ = d.DeleteTask(first.ID)
		_ = d.DeleteTask(second.ID)
		_, _ = d.Exec(`DELETE FROM assets WHERE url=$1 OR url=$2`,
			"https://"+sharedHost+"/", "https://"+onlyHost+"/")
	})
	for _, item := range []struct {
		host   string
		taskID int64
	}{
		{sharedHost, first.ID}, {sharedHost, second.ID}, {onlyHost, first.ID},
	} {
		if _, err := d.Assets().UpsertHTTPService(UpsertHTTPServiceReq{
			URL: "https://" + item.host + "/", TaskID: item.taskID,
		}); err != nil {
			t.Fatal(err)
		}
	}
	shared, err := d.TrafficHostsStillShared(context.Background(), first.ID, first.ExplorationID, []string{sharedHost, onlyHost})
	if err != nil {
		t.Fatal(err)
	}
	if !shared[sharedHost] {
		t.Fatalf("host owned by another live task not reported shared: %+v", shared)
	}
	if shared[onlyHost] {
		t.Fatalf("host owned only by the deleted task reported shared: %+v", shared)
	}
	// After the other task goes away, the host is no longer protected.
	if err := d.DeleteTask(second.ID); err != nil {
		t.Fatal(err)
	}
	shared, err = d.TrafficHostsStillShared(context.Background(), first.ID, first.ExplorationID, []string{sharedHost})
	if err != nil {
		t.Fatal(err)
	}
	if shared[sharedHost] {
		t.Fatalf("host remained shared after the other task was deleted: %+v", shared)
	}
}

func TestCleanupJobsRequeueStaleAndMinimumAge(t *testing.T) {
	d, taskID := cleanupTestDB(t)
	ctx := context.Background()
	tx, err := d.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := EnqueueCleanupJobTx(tx, taskID, "llm", map[string]any{"task_id": taskID}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// A grace period keeps a freshly enqueued job unclaimable.
	if job, err := d.ClaimCleanupJob(ctx, time.Hour); err != nil || job != nil {
		t.Fatalf("claimed inside grace: job=%+v err=%v", job, err)
	}
	job, err := d.ClaimCleanupJob(ctx, 0)
	if err != nil || job == nil {
		t.Fatalf("claim without grace: job=%+v err=%v", job, err)
	}
	// Simulate a process exit: the row is still running, and startup must requeue.
	n, err := d.RequeueStaleCleanupJobs(ctx)
	if err != nil || n < 1 {
		t.Fatalf("requeue stale: n=%d err=%v", n, err)
	}
	var state string
	var attempts int
	if err := d.QueryRowContext(ctx, `SELECT state, attempts FROM cleanup_jobs WHERE id=$1`, job.ID).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || attempts != 1 {
		t.Fatalf("after requeue state=%q attempts=%d", state, attempts)
	}
	// A job already at the retry ceiling must not spin forever.
	_, _ = d.ExecContext(ctx, `UPDATE cleanup_jobs SET state='running', attempts=3 WHERE id=$1`, job.ID)
	if _, err := d.RequeueStaleCleanupJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRowContext(ctx, `SELECT state FROM cleanup_jobs WHERE id=$1`, job.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "failed" {
		t.Fatalf("state=%q at retry ceiling, want failed", state)
	}
}
