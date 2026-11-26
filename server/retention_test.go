package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chu0119/pojun/traffic"
)

func TestTrafficModeRoundTrip(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	prev := m.TrafficMode()
	t.Cleanup(func() { _ = m.SetTrafficMode(prev) })

	if err := m.SetTrafficMode("evidence"); err != nil {
		t.Fatal(err)
	}
	if got := m.TrafficMode(); got != "evidence" {
		t.Fatalf("mode=%q, want evidence", got)
	}
	if !m.TrafficEnabled() {
		t.Fatal("evidence mode must capture traffic")
	}
	if err := m.SetTrafficMode("off"); err != nil {
		t.Fatal(err)
	}
	if m.TrafficEnabled() {
		t.Fatal("off mode must not capture traffic")
	}
	if err := m.SetTrafficMode("legacy"); err == nil {
		t.Fatal("invalid mode accepted")
	}
	if got := m.TrafficMode(); got != "off" {
		t.Fatalf("mode changed after invalid input: %q", got)
	}
}

func TestEvidenceRetentionKeepsPinnedTraffic(t *testing.T) {
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

	s := &Server{m: m}
	prevMode, prevHours, prevMax := m.TrafficMode(), s.trafficEvidenceHours(), s.trafficEvidenceMaxGB()
	t.Cleanup(func() {
		_ = m.SetTrafficMode(prevMode)
		_ = m.pg.SetSetting(settingTrafficEvidenceHours, fmt.Sprintf("%d", prevHours))
		_ = m.pg.SetSetting(settingTrafficEvidenceMaxGB, fmt.Sprintf("%d", prevMax))
	})
	if err := m.SetTrafficMode("evidence"); err != nil {
		t.Fatal(err)
	}
	if err := m.pg.SetSetting(settingTrafficEvidenceHours, "1"); err != nil {
		t.Fatal(err)
	}
	if err := m.pg.SetSetting(settingTrafficEvidenceMaxGB, "0"); err != nil {
		t.Fatal(err)
	}
	insert := func(id string, ts int64) {
		t.Helper()
		if _, err := tr.DB().Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES(?,?,'retention.example','GET','/','http://retention.example/',200,'text/plain',1,1,'')`, id, ts); err != nil {
			t.Fatal(err)
		}
	}
	insert("pinned", 1)
	insert("unpinned", 1)
	if err := tr.PinEvidence([]string{"pinned"}); err != nil {
		t.Fatal(err)
	}
	res, err := s.runEvidenceRetentionLocked(context.Background(), tr, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Mode != "evidence" || res.Deleted != 1 {
		t.Fatalf("result=%+v, want evidence deleted=1", res)
	}
	var n int
	_ = tr.DB().QueryRow(`SELECT count(*) FROM exchanges WHERE id='pinned'`).Scan(&n)
	if n != 1 {
		t.Fatalf("pinned row deleted n=%d", n)
	}
	_ = tr.DB().QueryRow(`SELECT count(*) FROM exchanges WHERE id='unpinned'`).Scan(&n)
	if n != 0 {
		t.Fatalf("unpinned row remained n=%d", n)
	}
}

func TestLLMRetentionAgeAndRowCapSettings(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if err := m.pg.EnsureLLMRecordsTable(); err != nil {
		t.Fatal(err)
	}
	srv := &Server{m: m}
	prevDays, prevRows := srv.llmKeepDays(), srv.llmMaxRows()
	t.Cleanup(func() {
		_ = m.pg.SetSetting(settingStorageLLMKeepDays, fmt.Sprintf("%d", prevDays))
		_ = m.pg.SetSetting(settingStorageLLMMaxRows, fmt.Sprintf("%d", prevRows))
	})
	if err := m.pg.SetSetting(settingStorageLLMKeepDays, "1"); err != nil {
		t.Fatal(err)
	}
	if err := m.pg.SetSetting(settingStorageLLMMaxRows, "0"); err != nil {
		t.Fatal(err)
	}
	session := fmt.Sprintf("server-llm-retention-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = m.pg.Exec(`DELETE FROM llm_records WHERE session_id LIKE $1`, session+"%") })
	insert := func(ts time.Time, suffix string) {
		t.Helper()
		if _, err := m.pg.Exec(`INSERT INTO llm_records(ts,model,session_id,task_id,status) VALUES($1,'test',$2,'','ok')`, ts, session+"-"+suffix); err != nil {
			t.Fatal(err)
		}
	}
	insert(time.Now().Add(-48*time.Hour), "old")
	insert(time.Now(), "new")
	res, err := srv.runLLMRetentionLocked(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 {
		t.Fatalf("deleted=%d, want 1", res.Deleted)
	}
	var oldCount, newCount int
	_ = m.pg.QueryRow(`SELECT count(*) FROM llm_records WHERE session_id=$1`, session+"-old").Scan(&oldCount)
	_ = m.pg.QueryRow(`SELECT count(*) FROM llm_records WHERE session_id=$1`, session+"-new").Scan(&newCount)
	if oldCount != 0 || newCount != 1 {
		t.Fatalf("old=%d new=%d, want 0/1", oldCount, newCount)
	}
}

func TestStoragePatrolRemovesOnlyOrphans(t *testing.T) {
	dataDir := t.TempDir()
	m, err := NewManager(dataDir, "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) - skipping", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	task, err := m.CreateTask("storage patrol", "storage patrol", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.DeleteTask(task.ID, DeleteTaskOptions{}) })
	conv, err := m.pg.CreateConversation("planner", "storage patrol", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.pg.DeleteConversation(conv.ID) })

	writePath := func(rel string, old bool) string {
		t.Helper()
		path := filepath.Join(dataDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if old {
			past := time.Now().Add(-48 * time.Hour)
			if err := os.Chtimes(path, past, past); err != nil {
				t.Fatal(err)
			}
		}
		return path
	}
	orphanTask := task.ID + "999999"
	liveTaskPath := writePath(filepath.Join("tasks", task.ID, "keep.txt"), false)
	orphanTaskPath := writePath(filepath.Join("tasks", orphanTask, "gone.txt"), true)
	liveConvPath := writePath(filepath.Join("sessions", fmt.Sprintf("conv-%d", conv.ID), "keep.txt"), false)
	orphanConvPath := writePath(filepath.Join("sessions", "conv-999999999", "gone.txt"), true)
	orphanTranscript := writePath(filepath.Join("transcripts", "conv-999999999.jsonl"), true)
	orphanExp := writePath(filepath.Join("transcripts", "exp999999999-main.jsonl"), true)
	past := time.Now().Add(-48 * time.Hour)
	for _, dir := range []string{
		filepath.Join(dataDir, "tasks", orphanTask),
		filepath.Join(dataDir, "sessions", "conv-999999999"),
	} {
		if err := os.Chtimes(dir, past, past); err != nil {
			t.Fatal(err)
		}
	}

	s := &Server{m: m}
	if _, err := s.pruneOrphanStorage(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{liveTaskPath, liveConvPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("live path removed %s: %v", path, err)
		}
	}
	for _, path := range []string{orphanTaskPath, orphanConvPath, orphanTranscript, orphanExp} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("orphan path remains %s: %v", path, err)
		}
	}
}
