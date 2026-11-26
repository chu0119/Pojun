package db

// LLM 审计留存治理。
//
// llm_records 是排障用的辅助审计，不是证据：它会保存请求/响应正文，长期只增
// 不减会把库撑大，也会让敏感内容超出授权窗口。这里提供两个有界入口，由服务端
// 留存巡检按设置调用：
//   - PruneLLMRecordsBefore：删除 ts 早于 cutoff 的记录；
//   - TrimLLMRecordsToMaxRows：总行数超上限时按最旧优先删除。
//
// 两者都按批删除，每批独立事务，不会把大表操作拖成分钟级长事务。

import (
	"context"
	"fmt"
	"time"
)

// PruneLLMRecordsBefore deletes at most batch rows older than cutoff and returns
// the number removed. Callers loop until fewer than batch rows are returned.
func (d *DB) PruneLLMRecordsBefore(ctx context.Context, cutoff time.Time, batch int) (int64, error) {
	if batch <= 0 {
		batch = 1000
	}
	if cutoff.IsZero() {
		return 0, nil
	}
	res, err := d.ExecContext(ctx, `
DELETE FROM llm_records WHERE id IN (
  SELECT id FROM llm_records WHERE ts < $1 ORDER BY id LIMIT $2
)`, cutoff, batch)
	if err != nil {
		return 0, fmt.Errorf("删除过期 LLM 审计: %w", err)
	}
	return res.RowsAffected()
}

// TrimLLMRecordsToMaxRows deletes at most batch rows, oldest first, while the
// table holds more than maxRows. It returns the number removed.
func (d *DB) TrimLLMRecordsToMaxRows(ctx context.Context, maxRows int64, batch int) (int64, error) {
	if maxRows <= 0 {
		return 0, nil
	}
	if batch <= 0 {
		batch = 1000
	}
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		var n int64
		if err := d.QueryRowContext(ctx, `SELECT count(*) FROM llm_records`).Scan(&n); err != nil {
			return total, err
		}
		excess := n - maxRows
		if excess <= 0 {
			return total, nil
		}
		if int64(batch) > excess {
			batch = int(excess)
		}
		res, err := d.ExecContext(ctx, `
DELETE FROM llm_records WHERE id IN (
  SELECT id FROM llm_records ORDER BY id LIMIT $1
)`, batch)
		if err != nil {
			return total, fmt.Errorf("按行数上限删除 LLM 审计: %w", err)
		}
		removed, _ := res.RowsAffected()
		total += removed
		if removed == 0 {
			return total, nil
		}
	}
}

// DeleteOrphanLLMRecords removes LLM rows whose numeric task id no longer exists.
// Empty/NULL task_id rows (chat calls) are kept. Used by the storage patrol after
// a crash left the asynchronous cleanup half-done.
func (d *DB) DeleteOrphanLLMRecords(ctx context.Context, batch int) (int64, error) {
	if batch <= 0 {
		batch = 1000
	}
	var total int64
	for {
		res, err := d.ExecContext(ctx, `
DELETE FROM llm_records WHERE id IN (
  SELECT r.id FROM llm_records r
  WHERE r.task_id ~ '^[0-9]+$'
    AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.id::text = r.task_id)
  LIMIT $1)`, batch)
		if err != nil {
			return total, fmt.Errorf("清理孤儿 LLM 审计: %w", err)
		}
		n, _ := res.RowsAffected()
		total += n
		if n < int64(batch) {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}
