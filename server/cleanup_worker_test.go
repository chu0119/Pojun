package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	pgdb "github.com/chu0119/pojun/db"
	"github.com/chu0119/pojun/traffic"
)

func TestValidTaskStagingDir(t *testing.T) {
	dataDir := t.TempDir()
	inside := filepath.Join(dataDir, ".delete-staging", "task-7-abc")
	if !validTaskStagingDir(dataDir, inside) {
		t.Fatal("direct child of .delete-staging rejected")
	}
	for _, bad := range []string{
		dataDir,
		filepath.Join(dataDir, ".delete-staging"),
		filepath.Join(dataDir, ".delete-staging", "other-7"),
		filepath.Join(dataDir, ".delete-staging", "task-7", "nested"),
		filepath.Join(dataDir, "..", "task-7-abc"),
		filepath.Join(dataDir, "tasks", "7"),
	} {
		if validTaskStagingDir(dataDir, bad) {
			t.Fatalf("unsafe staging path accepted: %s", bad)
		}
	}
}

func TestRecoverTaskFileStagingRestoresOrFinishes(t *testing.T) {
	dataDir := t.TempDir()
	taskDir := filepath.Join(dataDir, "tasks", "77")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(taskDir, "notes.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(dataDir, "transcripts", "exp12-main.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	stage, err := stageTaskFiles(dataDir, "77", 12)
	if err != nil {
		t.Fatal(err)
	}
	// The task row still exists (PostgreSQL rolled back): restore everything.
	if n, err := recoverTaskFileStaging(dataDir, func(id string) bool { return id == "77" }); err != nil || n != 1 {
		t.Fatalf("restore recovery n=%d err=%v", n, err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("workspace marker not restored: %v", err)
	}
	if _, err := os.Stat(transcript); err != nil {
		t.Fatalf("transcript not restored: %v", err)
	}
	if _, err := os.Stat(stage.stageDir); !os.IsNotExist(err) {
		t.Fatalf("stage directory remains after restore: %v", err)
	}

	// The task row is gone (PostgreSQL committed): finish the delete.
	if _, err := stageTaskFiles(dataDir, "77", 12); err != nil {
		t.Fatal(err)
	}
	if n, err := recoverTaskFileStaging(dataDir, func(string) bool { return false }); err != nil || n != 1 {
		t.Fatalf("finish recovery n=%d err=%v", n, err)
	}
	if _, err := os.Stat(taskDir); !os.IsNotExist(err) {
		t.Fatalf("workspace remains after committed recovery: %v", err)
	}
	if _, err := os.Stat(transcript); !os.IsNotExist(err) {
		t.Fatalf("transcript remains after committed recovery: %v", err)
	}
}

func TestRecoverTaskFileStagingRejectsEscapingJournal(t *testing.T) {
	dataDir := t.TempDir()
	outside := filepath.Join(filepath.Dir(dataDir), "outside-secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })
	stageDir := filepath.Join(dataDir, ".delete-staging", "task-99-bad")
	if err := os.MkdirAll(stageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	journal := taskFileDeleteJournal{
		Version: 1, TaskID: "99", ExplorationID: 1,
		Moves: []taskFileJournalMove{{Source: outside, Staged: filepath.Join(stageDir, "0-file")}},
	}
	if err := writeTaskFileJournal(filepath.Join(stageDir, taskFileDeleteJournalName), journal); err != nil {
		t.Fatal(err)
	}
	if _, err := recoverTaskFileStaging(dataDir, func(string) bool { return true }); err == nil {
		t.Fatal("escaping journal was accepted")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside file was moved: %v", err)
	}
}

func runCleanupJobForTest(t *testing.T, m *Manager, kind string, payload any) *pgdb.CleanupJob {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := m.pg.Begin()
	if err != nil {
		t.Fatal(err)
	}
	taskID := time.Now().UnixNano()
	if err := pgdb.EnqueueCleanupJobTx(tx, taskID, kind, payload); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.pg.Exec(`DELETE FROM cleanup_jobs WHERE task_id=$1`, taskID) })
	job := &pgdb.CleanupJob{ID: 0, TaskID: &taskID, Kind: kind, Payload: string(raw), State: "running", Attempts: 1}
	if err := m.pg.QueryRow(`SELECT id FROM cleanup_jobs WHERE task_id=$1 AND kind=$2 ORDER BY id DESC LIMIT 1`, taskID, kind).Scan(&job.ID); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestCleanupFilesJobStaysInsideDataDir(t *testing.T) {
	dataDir := t.TempDir()
	m, err := NewManager(dataDir, "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	s := &Server{m: m}

	stageDir := filepath.Join(dataDir, ".delete-staging", "task-1-test")
	if err := os.MkdirAll(stageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(stageDir, "payload.bin")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := runCleanupJobForTest(t, m, "files", cleanupFilesPayload{StageDir: stageDir})
	s.executeCleanupJob(context.Background(), job)
	if _, err := os.Stat(stageDir); !os.IsNotExist(err) {
		t.Fatalf("stage directory remains: %v", err)
	}
	var state string
	if err := m.pg.QueryRow(`SELECT state FROM cleanup_jobs WHERE id=$1`, job.ID).Scan(&state); err != nil || state != "done" {
		t.Fatalf("job state=%q err=%v", state, err)
	}

	// A payload outside .delete-staging must fail, not delete.
	outside := filepath.Join(dataDir, "tasks", "1")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	job = runCleanupJobForTest(t, m, "files", cleanupFilesPayload{StageDir: outside})
	s.executeCleanupJob(context.Background(), job)
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("job escaped staging root: %v", err)
	}
	// First failure schedules a bounded retry (three attempts, then failed).
	if err := m.pg.QueryRow(`SELECT state FROM cleanup_jobs WHERE id=$1`, job.ID).Scan(&state); err != nil || state != "pending" {
		t.Fatalf("unsafe job state=%q err=%v, want pending retry", state, err)
	}
}

func TestCleanupLLMJobDeletesOnlyTaskRows(t *testing.T) {
	dataDir := t.TempDir()
	m, err := NewManager(dataDir, "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if err := m.pg.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	taskID := time.Now().UnixNano()
	t.Cleanup(func() { _, _ = m.pg.Exec(`DELETE FROM llm_records WHERE task_id=$1`, fmt.Sprintf("%d", taskID)) })
	for i := 0; i < 2; i++ {
		if _, err := m.pg.Exec(`INSERT INTO llm_records(model,session_id,task_id,status) VALUES('test',$1,$2,'ok')`,
			fmt.Sprintf("cleanup-llm-%d-%d", taskID, i), fmt.Sprintf("%d", taskID)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.pg.Exec(`INSERT INTO llm_records(model,session_id,task_id,status) VALUES('test',$1,'','ok')`, fmt.Sprintf("cleanup-chat-%d", taskID)); err != nil {
		t.Fatal(err)
	}
	s := &Server{m: m}
	job := runCleanupJobForTest(t, m, "llm", cleanupLLMPayload{TaskID: taskID})
	s.executeCleanupJob(context.Background(), job)
	var n int
	_ = m.pg.QueryRow(`SELECT count(*) FROM llm_records WHERE task_id=$1`, fmt.Sprintf("%d", taskID)).Scan(&n)
	if n != 0 {
		t.Fatalf("task LLM rows=%d, want 0", n)
	}
	_ = m.pg.QueryRow(`SELECT count(*) FROM llm_records WHERE session_id=$1`, fmt.Sprintf("cleanup-chat-%d", taskID)).Scan(&n)
	if n != 1 {
		t.Fatalf("chat row deleted n=%d", n)
	}
}

func TestCleanupTrafficJobHonorsCutoff(t *testing.T) {
	dataDir := t.TempDir()
	m, err := NewManager(dataDir, "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	tr, err := traffic.Open(filepath.Join(dataDir, "traffic"), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m.traffic = tr
	t.Cleanup(func() { _ = tr.Close() })

	host := fmt.Sprintf("cleanup-traffic-%d.example.test", time.Now().UnixNano())
	if _, err := tr.DB().Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES('old',50,?,'GET','/','http://'||?||'/',200,'text/plain',1,1,'')`, host, host); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.DB().Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES('new',500,?,'GET','/','http://'||?||'/',200,'text/plain',1,1,'')`, host, host); err != nil {
		t.Fatal(err)
	}
	s := &Server{m: m}
	job := runCleanupJobForTest(t, m, "traffic", cleanupTrafficPayload{
		TaskID: time.Now().UnixNano(), ExplorationID: time.Now().UnixNano(), Hosts: []string{host}, Cutoff: 100,
	})
	s.executeCleanupJob(context.Background(), job)
	var n int
	_ = tr.DB().QueryRow(`SELECT count(*) FROM exchanges WHERE id='old'`).Scan(&n)
	if n != 0 {
		t.Fatalf("old row remained n=%d", n)
	}
	_ = tr.DB().QueryRow(`SELECT count(*) FROM exchanges WHERE id='new'`).Scan(&n)
	if n != 1 {
		t.Fatalf("post-cutoff row deleted n=%d", n)
	}
}
