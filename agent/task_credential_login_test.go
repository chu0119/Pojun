package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chu0119/pojun/db"
)

type fakeTaskCredResolver struct {
	cred     *db.TaskCredential
	recorded []int
}

func (f *fakeTaskCredResolver) ResolveTaskCredential(context.Context, int64, int64) (*db.TaskCredential, error) {
	return f.cred, nil
}

func (f *fakeTaskCredResolver) RecordTaskCredentialLogin(_ context.Context, _, _ int64, status int) (string, error) {
	f.recorded = append(f.recorded, status)
	return "", nil
}

// #24：凭据 ID 模式由平台解析 URL/用户名/密码，模型只传 credential_id。
func TestSessionLoginUsesTaskCredentialByID(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_ = r.ParseForm()
		if got := r.PostForm.Get("username"); got != "alice" {
			t.Errorf("username=%q want alice", got)
		}
		if got := r.PostForm.Get("password"); got != "s3cret" {
			t.Errorf("password=%q want s3cret", got)
		}
		w.Header().Set("Set-Cookie", "sid=ok; Path=/")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	resolver := &fakeTaskCredResolver{cred: &db.TaskCredential{
		ID: 7, TaskID: 42, Role: "user_a", Status: "pending",
		Config: &db.TaskAuthConfig{
			Enabled: true, LoginURL: srv.URL + "/login", Method: "POST", ContentType: "form",
			AuthMode: "form", UsernameField: "username", PasswordField: "password",
			Username: "alice", Password: "s3cret", HasPassword: true,
		},
	}}
	ts := &ToolSet{taskID: 42, runDir: filepath.Join(t.TempDir(), "tasks", "42")}
	ts.SetTaskCredentialResolver(resolver)
	res, err := ts.sessionLogin().Call(context.Background(), json.RawMessage(`{"session":"user_a","credential_id":7}`), nil)
	if err != nil || res.IsError {
		t.Fatalf("login failed: err=%v out=%v", err, res.Flatten())
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("hits=%d want 1", hits)
	}
	if len(resolver.recorded) != 1 || resolver.recorded[0] != 200 {
		t.Fatalf("health record=%v want [200]", resolver.recorded)
	}
}

// 熔断期内 credential_id 必须在本地拒绝，不能发出请求。
func TestSessionLoginTaskCredentialLocked(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()
	until := time.Now().Add(10 * time.Minute)
	resolver := &fakeTaskCredResolver{cred: &db.TaskCredential{
		ID: 8, TaskID: 42, Role: "user_b", Status: "locked", LockedUntil: &until,
		Config: &db.TaskAuthConfig{
			Enabled: true, LoginURL: srv.URL + "/login", Method: "POST", ContentType: "form",
			AuthMode: "form", UsernameField: "username", PasswordField: "password",
			Username: "bob", Password: "x", HasPassword: true,
		},
	}}
	ts := &ToolSet{taskID: 42, runDir: filepath.Join(t.TempDir(), "tasks", "42")}
	ts.SetTaskCredentialResolver(resolver)
	res, err := ts.sessionLogin().Call(context.Background(), json.RawMessage(`{"session":"user_b","credential_id":8}`), nil)
	if err == nil && !res.IsError {
		t.Fatal("locked credential should be rejected")
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("locked credential sent %d requests", hits)
	}
	_ = fmt.Sprint(res.Flatten())
}
