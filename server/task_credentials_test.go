package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chu0119/pojun/db"
)

func TestTaskCredentialDTOAndPromptHideSecrets(t *testing.T) {
	cfg := &db.TaskAuthConfig{
		Enabled: true, LoginURL: "https://t/login", Method: "POST", ContentType: "form",
		AuthMode: "form", UsernameField: "user", PasswordField: "pass",
		Username: "alice", Password: "s3cret", TOTPSecret: "GEZDGNBV",
	}
	c := &db.TaskCredential{ID: 3, TaskID: 1, Role: "user_a", Config: cfg, Status: "ok"}
	raw, _ := json.Marshal(taskCredentialDTOOf(c))
	if strings.Contains(string(raw), "s3cret") || strings.Contains(string(raw), "GEZDGNBV") {
		t.Fatalf("DTO leaked secret: %s", raw)
	}
	block := taskCredentialsPromptBlock([]*db.TaskCredential{c})
	for _, secret := range []string{"s3cret", "GEZDGNBV", "alice"} {
		if strings.Contains(block, secret) {
			t.Fatalf("prompt block leaked %q: %s", secret, block)
		}
	}
	for _, want := range []string{"credential_id=3", "role=user_a", "status=ok", "https://t/login"} {
		if !strings.Contains(block, want) {
			t.Fatalf("prompt block missing %q: %s", want, block)
		}
	}
}

func TestTaskCredentialBreakerStateMachine(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil || dsn == "" {
		t.Skip("no test database configured")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Close() })
	task, err := pg.CreateTask("credential breaker", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(task.ID) })
	cfg := &db.TaskAuthConfig{
		Enabled: true, LoginURL: "https://t/login", Method: "POST", ContentType: "form",
		AuthMode: "form", Username: "alice", Password: "bad", UsernameField: "username", PasswordField: "password",
	}
	_ = cfg.Normalize()
	cred, err := pg.CreateTaskCredential(context.Background(), task.ID, "user_a", cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{m: &Manager{pg: pg}}
	if _, err := s.applyTaskCredentialOutcome(context.Background(), cred, false, true, "HTTP 401"); err != nil {
		t.Fatal(err)
	}
	got, _ := pg.GetTaskCredential(context.Background(), task.ID, cred.ID)
	if got.Status != "failed" || got.FailureCount != 1 || got.LockedUntil != nil {
		t.Fatalf("first failure state=%+v", got)
	}
	if _, err := s.applyTaskCredentialOutcome(context.Background(), got, false, true, "HTTP 401"); err != nil {
		t.Fatal(err)
	}
	got, _ = pg.GetTaskCredential(context.Background(), task.ID, cred.ID)
	if got.Status != "locked" || got.FailureCount != 2 || got.LockedUntil == nil || !got.LockedUntil.After(time.Now()) {
		t.Fatalf("second failure state=%+v", got)
	}
	if _, err := s.applyTaskCredentialOutcome(context.Background(), got, true, false, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = pg.GetTaskCredential(context.Background(), task.ID, cred.ID)
	if got.Status != "ok" || got.FailureCount != 0 || got.LockedUntil != nil {
		t.Fatalf("success reset state=%+v", got)
	}
}

func TestTaskCredentialsHandlerRedactsSecret(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil || dsn == "" {
		t.Skip("no test database configured")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Close() })
	task, err := pg.CreateTask("credential handler", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.DeleteTask(task.ID) })
	cfg := &db.TaskAuthConfig{
		Enabled: true, LoginURL: "https://t/login", Method: "POST", ContentType: "form",
		AuthMode: "form", Username: "alice", Password: "s3cret", UsernameField: "username", PasswordField: "password",
	}
	_ = cfg.Normalize()
	if _, err := pg.CreateTaskCredential(context.Background(), task.ID, "user_a", cfg); err != nil {
		t.Fatal(err)
	}
	s := &Server{m: &Manager{pg: pg}}
	req := httptest.NewRequest("GET", "/api/tasks/"+strconv.FormatInt(task.ID, 10)+"/credentials", nil)
	req.SetPathValue("id", strconv.FormatInt(task.ID, 10))
	rr := httptest.NewRecorder()
	s.pgTaskCredentials(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "s3cret") || strings.Contains(body, `"password":"`) {
		t.Fatalf("handler leaked secret: %s", body)
	}
	if !strings.Contains(body, `"role":"user_a"`) || !strings.Contains(body, `"has_password":true`) {
		t.Fatalf("unexpected DTO: %s", body)
	}
}
