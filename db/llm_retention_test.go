package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestLLMRetentionPrunesByAgeAndRowCap(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	defer d.Close()
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	session := fmt.Sprintf("retention-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_records WHERE session_id=$1`, session) })
	insert := func(ts time.Time, suffix string) {
		t.Helper()
		if _, err := d.ExecContext(ctx, `
INSERT INTO llm_records(ts, model, session_id, task_id, status) VALUES($1,'test-model',$2,'',$3)`,
			ts, session+"-"+suffix, "ok"); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	insert(old, "old1")
	insert(old, "old2")
	insert(time.Now(), "new")

	cutoff := time.Now().Add(-24 * time.Hour)
	var removed int64
	for {
		n, err := d.PruneLLMRecordsBefore(ctx, cutoff, 1)
		if err != nil {
			t.Fatal(err)
		}
		removed += n
		if n < 1 {
			break
		}
	}
	if removed != 2 {
		t.Fatalf("age prune removed=%d, want 2", removed)
	}
	var remaining int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM llm_records WHERE session_id LIKE $1`, session+"%").Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("remaining=%d after age prune, want 1", remaining)
	}

	// Row cap: compute the current size and keep exactly one less, so the test is
	// independent of whatever else the dedicated database holds.
	insert(time.Now(), "new2")
	insert(time.Now(), "new3")
	var total int64
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM llm_records`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	trimmed, err := d.TrimLLMRecordsToMaxRows(ctx, total-1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if trimmed != 1 {
		t.Fatalf("trimmed=%d, want 1", trimmed)
	}
	var after int64
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM llm_records`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != total-1 {
		t.Fatalf("after trim count=%d, want %d", after, total-1)
	}
}

func TestDeleteOrphanLLMRecordsKeepsLiveAndChatRows(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	defer d.Close()
	if err := d.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	session := fmt.Sprintf("orphan-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM llm_records WHERE session_id LIKE $1`, session+"%") })

	task, err := d.CreateTask("orphan-retention", "orphan-retention", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	insert := func(taskID string, suffix string) {
		t.Helper()
		if _, err := d.ExecContext(ctx, `
INSERT INTO llm_records(ts, model, session_id, task_id, status) VALUES(now(),'test-model',$1,$2,'ok')`,
			session+"-"+suffix, taskID); err != nil {
			t.Fatal(err)
		}
	}
	insert(fmt.Sprintf("%d", task.ID), "live")
	insert(fmt.Sprintf("%d", time.Now().UnixNano()), "orphan")
	insert("", "chat")
	removed, err := d.DeleteOrphanLLMRecords(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if removed < 1 {
		t.Fatalf("orphan deletion removed=%d, want >=1", removed)
	}
	var live, chat, orphan int
	_ = d.QueryRowContext(ctx, `SELECT count(*) FROM llm_records WHERE session_id=$1`, session+"-live").Scan(&live)
	_ = d.QueryRowContext(ctx, `SELECT count(*) FROM llm_records WHERE session_id=$1`, session+"-chat").Scan(&chat)
	_ = d.QueryRowContext(ctx, `SELECT count(*) FROM llm_records WHERE session_id=$1`, session+"-orphan").Scan(&orphan)
	if live != 1 || chat != 1 || orphan != 0 {
		t.Fatalf("live=%d chat=%d orphan=%d, want 1/1/0", live, chat, orphan)
	}
}
