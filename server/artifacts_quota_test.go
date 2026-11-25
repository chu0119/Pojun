package server

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/chu0119/pojun/db"
)

func newArtifactTestServer(t *testing.T) (*Server, *db.DB, string) {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil || dsn == "" {
		t.Skip("no test database configured")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Close() })
	if err := pg.EnsureTaskArtifactsTable(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Cleanup(func() {
		_, _ = pg.Exec(`DELETE FROM task_artifacts WHERE task_id LIKE 'artifact-test-%' OR task_id='987654321'`)
		_ = pg.SetSetting(db.SettingArtifactMaxCount, "")
		_ = pg.SetSetting(db.SettingArtifactMaxBytes, "")
	})
	return &Server{m: &Manager{pg: pg, dir: dir}}, pg, dir
}

// #59：DELETE 必须删主文件、缩略图与预览目录；旧实现用相对路径，容器里永远删不到。
func TestDeleteTaskArtifactRemovesFiles(t *testing.T) {
	s, pg, base := newArtifactTestServer(t)
	taskID := "artifact-test-delete"
	artifactDir := filepath.Join(base, "tasks", taskID, "artifacts")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(artifactDir, "shot.png")
	thumb := main + ".thumb.jpg"
	if err := os.WriteFile(main, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(thumb, []byte("thumb"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &db.TaskArtifact{
		TaskID: taskID, Name: "shot.png", Kind: "image", SizeBytes: 3, SHA256: "delete-test",
		Storage: filepath.ToSlash(filepath.Join("tasks", taskID, "artifacts", "shot.png")),
	}
	if err := pg.InsertTaskArtifact(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	previewDir := s.previewDir(taskID, a.ID)
	if err := os.MkdirAll(previewDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(previewDir, "page-1.jpg"), []byte("page"), 0o600); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("DELETE", "/api/tasks/"+taskID+"/artifacts/"+strconv.FormatInt(a.ID, 10), nil)
	req.SetPathValue("id", taskID)
	req.SetPathValue("aid", strconv.FormatInt(a.ID, 10))
	rr := httptest.NewRecorder()
	s.deleteTaskArtifact(rr, req)
	if rr.Code != 200 {
		t.Fatalf("delete status=%d body=%s", rr.Code, rr.Body.String())
	}
	for _, p := range []string{main, thumb, previewDir} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("删除后仍存在: %s (err=%v)", p, err)
		}
	}
	list, err := pg.ListTaskArtifacts(taskID)
	if err != nil || len(list) != 0 {
		t.Fatalf("DB 行未删除: len=%d err=%v", len(list), err)
	}
}

// #59：scan 必须走同一配额；触顶时停止并返回可识别错误，而不是继续补登记。
func TestScanTaskArtifactDirRespectsQuota(t *testing.T) {
	s, pg, base := newArtifactTestServer(t)
	taskID := "artifact-test-scan"
	root := filepath.Join(base, "tasks", taskID)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.png", "b.png"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("image-"+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := pg.SetSetting(db.SettingArtifactMaxCount, "1"); err != nil {
		t.Fatal(err)
	}
	added, _, _, err := s.scanTaskArtifactDir(taskID)
	if added != 1 {
		t.Fatalf("added=%d want 1", added)
	}
	if !errors.Is(err, db.ErrArtifactQuotaExceeded) {
		t.Fatalf("scan err=%v want ErrArtifactQuotaExceeded", err)
	}
	count, _, err := pg.CountTaskArtifacts(taskID)
	if err != nil || count != 1 {
		t.Fatalf("count=%d err=%v want 1", count, err)
	}
}

// #59：并发登记必须由任务级锁串行化，不能一起越过 count 配额。
func TestConcurrentRecordArtifactQuota(t *testing.T) {
	s, pg, _ := newArtifactTestServer(t)
	taskID := int64(987654321)
	taskKey := strconv.FormatInt(taskID, 10)
	runDir := filepath.Join(s.m.dir, "tasks", taskKey)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const limit = 3
	if err := pg.SetSetting(db.SettingArtifactMaxCount, strconv.Itoa(limit)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("f-%d.txt", i)
		if err := os.WriteFile(filepath.Join(runDir, name), []byte(fmt.Sprintf("body-%d", i)), 0o600); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			if _, err := s.RecordArtifact(taskID, 0, runDir, name, name, "auto", "", ""); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}(i, name)
	}
	wg.Wait()
	count, _, err := pg.CountTaskArtifacts(taskKey)
	if err != nil {
		t.Fatal(err)
	}
	if ok != limit || count != limit {
		t.Fatalf("并发登记越过配额: ok=%d count=%d limit=%d", ok, count, limit)
	}
}
