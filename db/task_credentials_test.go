package db

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestTaskCredentialCRUDAndSecretRedaction(t *testing.T) {
	dsn, _, err := DSN()
	if err != nil || dsn == "" {
		t.Skip("no test database configured")
	}
	d, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	task, err := d.CreateTask("credential crud", "goal", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	cfg := &TaskAuthConfig{
		Enabled: true, LoginURL: "https://t.example/login", Method: "POST", ContentType: "form",
		AuthMode: "form", UsernameField: "user", PasswordField: "pass",
		Username: "alice", Password: "s3cret", TOTPSecret: "GEZDGNBV",
	}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	c, err := d.CreateTaskCredential(context.Background(), task.ID, "user_a", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if c.ID == 0 || c.Status != "pending" || c.Config.Password != "s3cret" || !c.Config.HasPassword {
		t.Fatalf("unexpected created credential: %+v", c)
	}
	if _, err := d.CreateTaskCredential(context.Background(), task.ID, "user_a", cfg); !errors.Is(err, ErrTaskCredentialRoleExists) {
		t.Fatalf("duplicate role err=%v", err)
	}

	// API/JSON view must not serialize Password/TOTP/Token.
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "s3cret") || strings.Contains(string(raw), "GEZDGNBV") {
		t.Fatalf("credential JSON leaked secret: %s", raw)
	}

	list, err := d.ListTaskCredentials(context.Background(), task.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%d err=%v", len(list), err)
	}
	until := time.Now().Add(30 * time.Minute)
	if err := d.UpdateTaskCredentialHealth(context.Background(), task.ID, c.ID, "locked", "HTTP 401", "hash", 2, &until); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetTaskCredential(context.Background(), task.ID, c.ID)
	if err != nil || got.Status != "locked" || got.FailureCount != 2 || got.LockedUntil == nil {
		t.Fatalf("health not persisted: %+v err=%v", got, err)
	}
	updated, err := d.UpdateTaskCredential(context.Background(), task.ID, c.ID, "admin", cfg)
	if err != nil || updated.Role != "admin" || updated.Status != "pending" || updated.FailureCount != 0 {
		t.Fatalf("update did not reset health: %+v err=%v", updated, err)
	}
	if err := d.DeleteTaskCredential(context.Background(), task.ID, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetTaskCredential(context.Background(), task.ID, c.ID); !errors.Is(err, ErrTaskCredentialNotFound) {
		t.Fatalf("deleted credential err=%v", err)
	}
}

func TestTaskAuthConfigUnmarshalSecrets(t *testing.T) {
	var cfg TaskAuthConfig
	if err := json.Unmarshal([]byte(`{"enabled":true,"login_url":"https://t/login","auth_mode":"bearer","token":"tok"}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "tok" || !cfg.HasToken {
		t.Fatalf("token not decoded: %+v", cfg)
	}
	// 响应侧仍然不包含秘密。
	raw, _ := json.Marshal(&cfg)
	if strings.Contains(string(raw), `"token":"tok"`) {
		t.Fatalf("response leaked token: %s", raw)
	}
}
