package db

// 任务删除后的异步清理队列。
//
// 为什么需要它：任务删除曾经在 PostgreSQL 事务里顺带做 SQLite 流量删除。
// 一个大任务有十几万交换、十几 GB 的正文，这一条 SQLite 事务会持有 PG 事务
// 数分钟（idle in transaction），提交时又触发整库 checkpoint，既阻塞资产写入，
// 又让“删任务”变成十几分钟。现在 PG 事务只负责把清理工作写进这张表并秒级提交，
// 真正的流量/LLM/文件清理由后台 worker 分批做完；进程重启后从队列续跑。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

const cleanupJobsSchema = `
CREATE TABLE IF NOT EXISTS cleanup_jobs (
  id          BIGSERIAL PRIMARY KEY,
  task_id     BIGINT,
  kind        TEXT NOT NULL,
  payload     JSONB NOT NULL DEFAULT '{}'::jsonb,
  state       TEXT NOT NULL DEFAULT 'pending',
  attempts    INT NOT NULL DEFAULT 0,
  total       BIGINT NOT NULL DEFAULT 0,
  done        BIGINT NOT NULL DEFAULT 0,
  last_error  TEXT NOT NULL DEFAULT '',
  run_after   TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_cleanup_jobs_state ON cleanup_jobs(state, run_after, id);
CREATE INDEX IF NOT EXISTS idx_cleanup_jobs_task ON cleanup_jobs(task_id);
-- One active job per (task, kind): a concurrent second delete must not enqueue
-- the same expensive work twice. Done/failed rows are kept for audit.
CREATE UNIQUE INDEX IF NOT EXISTS idx_cleanup_jobs_active
  ON cleanup_jobs(task_id, kind)
  WHERE state IN ('pending','running') AND task_id IS NOT NULL;
`

// CleanupJob is one queued post-delete cleanup unit.
type CleanupJob struct {
	ID        int64     `json:"id"`
	TaskID    *int64    `json:"task_id,omitempty"`
	Kind      string    `json:"kind"`
	Payload   string    `json:"payload"`
	State     string    `json:"state"`
	Attempts  int       `json:"attempts"`
	Total     int64     `json:"total"`
	Done      int64     `json:"done"`
	LastError string    `json:"last_error,omitempty"`
	RunAfter  time.Time `json:"run_after"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// EnsureCleanupJobsTable creates the queue table. Idempotent; safe on old DBs.
// It follows the same schema-migration advisory lock as the other lazy tables,
// so two processes starting together cannot race the DDL.
func (d *DB) EnsureCleanupJobsTable() error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := coordinateWithSchemaMigration(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(cleanupJobsSchema); err != nil {
		return err
	}
	return tx.Commit()
}

// EnqueueCleanupJobTx inserts one cleanup job inside the caller's transaction, so
// the queue entry and the task row disappear/commit atomically. Duplicate
// pending/running jobs for the same task and kind are ignored: a second delete
// request must not enqueue the same expensive work twice.
func EnqueueCleanupJobTx(tx *sql.Tx, taskID int64, kind string, payload any) error {
	if tx == nil {
		return fmt.Errorf("enqueue cleanup job: nil transaction")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode cleanup payload: %w", err)
	}
	var task any
	if taskID > 0 {
		task = taskID
	}
	_, err = tx.Exec(`
INSERT INTO cleanup_jobs(task_id, kind, payload)
SELECT $1, $2, $3::jsonb
WHERE NOT EXISTS (
  SELECT 1 FROM cleanup_jobs
  WHERE task_id IS NOT DISTINCT FROM $1 AND kind=$2 AND state IN ('pending','running')
)
ON CONFLICT DO NOTHING`, task, kind, string(raw))
	if err != nil {
		return fmt.Errorf("enqueue cleanup job %s: %w", kind, err)
	}
	return nil
}

// ClaimCleanupJob picks the oldest runnable job and marks it running. minAge
// lets the caller give freshly deleted tasks a grace period before their hosts
// are touched, so a concurrent task that starts on the same host is seen by the
// ownership re-check rather than racing it.
func (d *DB) ClaimCleanupJob(ctx context.Context, minAge time.Duration) (*CleanupJob, error) {
	if minAge < 0 {
		minAge = 0
	}
	ageInterval := fmt.Sprintf("%d seconds", int(minAge.Seconds()))
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var job CleanupJob
	var taskID sql.NullInt64
	err = tx.QueryRowContext(ctx, `
WITH next AS (
  SELECT id FROM cleanup_jobs
  WHERE state='pending' AND run_after <= now() AND created_at <= now() - $1::interval
  ORDER BY id
  LIMIT 1
  FOR UPDATE SKIP LOCKED
)
UPDATE cleanup_jobs j
SET state='running', attempts=attempts+1, updated_at=now()
FROM next
WHERE j.id=next.id
RETURNING j.id, j.task_id, j.kind, j.payload::text, j.state, j.attempts,
          j.total, j.done, j.last_error, j.run_after, j.created_at, j.updated_at`, ageInterval).
		Scan(&job.ID, &taskID, &job.Kind, &job.Payload, &job.State, &job.Attempts,
			&job.Total, &job.Done, &job.LastError, &job.RunAfter, &job.CreatedAt, &job.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if taskID.Valid {
		id := taskID.Int64
		job.TaskID = &id
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &job, nil
}

// UpdateCleanupJobProgress records batch progress. Both 0 values are ignored so
// a worker that only knows its deleted count does not erase a known total.
func (d *DB) UpdateCleanupJobProgress(ctx context.Context, id, done, total int64) error {
	_, err := d.ExecContext(ctx, `
UPDATE cleanup_jobs SET
  done = CASE WHEN $2 >= 0 THEN $2 ELSE done END,
  total = CASE WHEN $3 > 0 THEN $3 ELSE total END,
  updated_at = now()
WHERE id=$1`, id, done, total)
	return err
}

// FinishCleanupJob records a terminal success, or schedules a bounded retry.
// After three failed attempts the job stays failed for the operator to retry.
func (d *DB) FinishCleanupJob(ctx context.Context, id int64, failure string) error {
	if failure == "" {
		_, err := d.ExecContext(ctx, `
UPDATE cleanup_jobs SET state='done', last_error='', done=CASE WHEN total>0 THEN total ELSE done END,
  updated_at=now() WHERE id=$1`, id)
		return err
	}
	_, err := d.ExecContext(ctx, `
UPDATE cleanup_jobs SET
  state = CASE WHEN attempts >= 3 THEN 'failed' ELSE 'pending' END,
  last_error = $2,
  run_after = CASE WHEN attempts >= 3 THEN run_after ELSE now() + interval '30 seconds' END,
  updated_at = now()
WHERE id=$1`, id, failure)
	return err
}

// RequeueStaleCleanupJobs recovers jobs whose worker died with the process.
// Called once at worker startup, when this process is the only owner of the
// queue, so every running row is by definition interrupted. Three attempts is
// the same bound FinishCleanupJob uses, so a permanently broken payload cannot
// spin forever.
func (d *DB) RequeueStaleCleanupJobs(ctx context.Context) (int64, error) {
	res, err := d.ExecContext(ctx, `
UPDATE cleanup_jobs SET
  state = CASE WHEN attempts >= 3 THEN 'failed' ELSE 'pending' END,
  last_error = CASE WHEN state='running' THEN 'worker interrupted' ELSE last_error END,
  run_after = now(),
  updated_at = now()
WHERE state='running'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// RetryCleanupJob reopens a failed job from the management API.
func (d *DB) RetryCleanupJob(ctx context.Context, id int64) (bool, error) {
	res, err := d.ExecContext(ctx, `
UPDATE cleanup_jobs SET state='pending', attempts=0, last_error='', run_after=now(), updated_at=now()
WHERE id=$1 AND state='failed'`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListCleanupJobs returns recent jobs, newest first. activeOnly keeps the
// dashboard banner small (pending/running only).
func (d *DB) ListCleanupJobs(ctx context.Context, activeOnly bool) ([]CleanupJob, error) {
	q := `SELECT id, task_id, kind, payload::text, state, attempts, total, done, last_error,
	             run_after, created_at, updated_at
	      FROM cleanup_jobs`
	if activeOnly {
		q += ` WHERE state IN ('pending','running')`
	}
	q += ` ORDER BY id DESC LIMIT 100`
	rows, err := d.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]CleanupJob, 0, 8)
	for rows.Next() {
		var job CleanupJob
		var taskID sql.NullInt64
		if err := rows.Scan(&job.ID, &taskID, &job.Kind, &job.Payload, &job.State, &job.Attempts,
			&job.Total, &job.Done, &job.LastError, &job.RunAfter, &job.CreatedAt, &job.UpdatedAt); err != nil {
			return nil, err
		}
		if taskID.Valid {
			id := taskID.Int64
			job.TaskID = &id
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// CountLLMRecordsForTask is used only for progress display.
func (d *DB) CountLLMRecordsForTask(ctx context.Context, taskID int64) (int64, error) {
	var n int64
	err := d.QueryRowContext(ctx, `SELECT count(*) FROM llm_records WHERE task_id=$1`,
		fmt.Sprintf("%d", taskID)).Scan(&n)
	return n, err
}

// DeleteLLMRecordsBatch deletes at most limit rows for one task and reports how
// many were removed. The worker loops so progress is visible between batches.
func (d *DB) DeleteLLMRecordsBatch(ctx context.Context, taskID int64, limit int) (int64, error) {
	if limit <= 0 {
		limit = 1000
	}
	res, err := d.ExecContext(ctx, `
DELETE FROM llm_records WHERE id IN (
  SELECT id FROM llm_records WHERE task_id=$1 LIMIT $2
)`, fmt.Sprintf("%d", taskID), limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
