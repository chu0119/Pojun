package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/chu0119/pojun/db"
)

var taskCredentialRoleRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,31}$`)

type taskCredentialDTO struct {
	ID              int64             `json:"id"`
	Role            string            `json:"role"`
	Enabled         bool              `json:"enabled"`
	LoginURL        string            `json:"login_url"`
	Method          string            `json:"method"`
	AuthMode        string            `json:"auth_mode"`
	ContentType     string            `json:"content_type"`
	UsernameField   string            `json:"username_field,omitempty"`
	PasswordField   string            `json:"password_field,omitempty"`
	Username        string            `json:"username,omitempty"`
	HasPassword     bool              `json:"has_password"`
	HasToken        bool              `json:"has_token"`
	HasTOTP         bool              `json:"has_totp"`
	ExtraFields     map[string]string `json:"extra_fields,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	SuccessStatus   int               `json:"success_status,omitempty"`
	SuccessContains string            `json:"success_contains,omitempty"`
	Status          string            `json:"status"`
	LastCheckedAt   string            `json:"last_checked_at,omitempty"`
	LastError       string            `json:"last_error,omitempty"`
	FailureCount    int               `json:"failure_count"`
	LockedUntil     string            `json:"locked_until,omitempty"`
}

func taskCredentialDTOOf(c *db.TaskCredential) taskCredentialDTO {
	if c == nil {
		return taskCredentialDTO{}
	}
	cfg := c.Config
	if cfg == nil {
		cfg = &db.TaskAuthConfig{}
	}
	dto := taskCredentialDTO{
		ID: c.ID, Role: c.Role, Enabled: cfg.Enabled, LoginURL: cfg.LoginURL,
		Method: cfg.Method, AuthMode: cfg.AuthMode, ContentType: cfg.ContentType,
		UsernameField: cfg.UsernameField, PasswordField: cfg.PasswordField,
		Username: cfg.Username, HasPassword: cfg.HasPassword, HasToken: cfg.HasToken,
		HasTOTP: cfg.HasTOTP, ExtraFields: cfg.ExtraFields, Headers: cfg.Headers,
		SuccessStatus: cfg.SuccessStatus, SuccessContains: cfg.SuccessContains,
		Status: c.Status, LastError: c.LastError, FailureCount: c.FailureCount,
	}
	if c.LastCheckedAt != nil {
		dto.LastCheckedAt = c.LastCheckedAt.UTC().Format(time.RFC3339)
	}
	if c.LockedUntil != nil {
		dto.LockedUntil = c.LockedUntil.UTC().Format(time.RFC3339)
	}
	return dto
}

func taskCredentialSecretHash(cfg *db.TaskAuthConfig) string {
	if cfg == nil {
		return ""
	}
	h := sha256.New()
	for _, s := range []string{cfg.AuthMode, cfg.LoginURL, cfg.Username, cfg.Password, cfg.Token, cfg.TOTPSecret} {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Server) taskIDFromPath(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid task id")
		return 0, false
	}
	if s.m.PG() == nil {
		writeErr(w, 500, "PG 不可用")
		return 0, false
	}
	t, err := s.m.PG().GetTask(id)
	if err != nil || t == nil {
		writeErr(w, 404, "task not found")
		return 0, false
	}
	return id, true
}

func (s *Server) pgTaskCredentials(w http.ResponseWriter, r *http.Request) {
	taskID, ok := s.taskIDFromPath(w, r)
	if !ok {
		return
	}
	list, err := s.m.PG().ListTaskCredentials(r.Context(), taskID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]taskCredentialDTO, 0, len(list))
	for _, c := range list {
		out = append(out, taskCredentialDTOOf(c))
	}
	writeJSON(w, 200, map[string]any{"credentials": out})
}

func decodeTaskCredential(w http.ResponseWriter, r *http.Request) (string, *db.TaskAuthConfig, bool) {
	var req struct {
		Role   string             `json:"role"`
		Config *db.TaskAuthConfig `json:"config"`
	}
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return "", nil, false
	}
	role := strings.TrimSpace(req.Role)
	if !taskCredentialRoleRE.MatchString(role) {
		writeErr(w, 400, "role 需以字母开头，仅含字母/数字/_/-，最多 32 字符")
		return "", nil, false
	}
	if req.Config == nil {
		writeErr(w, 400, "config 必填")
		return "", nil, false
	}
	return role, req.Config, true
}

func (s *Server) pgCreateTaskCredential(w http.ResponseWriter, r *http.Request) {
	taskID, ok := s.taskIDFromPath(w, r)
	if !ok {
		return
	}
	role, cfg, ok := decodeTaskCredential(w, r)
	if !ok {
		return
	}
	if err := cfg.Normalize(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	cred, err := s.m.PG().CreateTaskCredential(r.Context(), taskID, role, cfg)
	if err != nil {
		if errors.Is(err, db.ErrTaskCredentialRoleExists) {
			writeErr(w, 409, "该 role 已存在")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	s.invalidateTaskAgents()
	writeJSON(w, 200, taskCredentialDTOOf(cred))
}

func (s *Server) pgUpdateTaskCredential(w http.ResponseWriter, r *http.Request) {
	taskID, ok := s.taskIDFromPath(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("cid"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid credential id")
		return
	}
	role, cfg, ok := decodeTaskCredential(w, r)
	if !ok {
		return
	}
	// 编辑时密码/token/TOTP 留空 = 保持原值；需要清空请删除后重建（避免误清）。
	if old, oerr := s.m.PG().GetTaskCredential(r.Context(), taskID, id); oerr == nil && old != nil && old.Config != nil {
		if cfg.Password == "" {
			cfg.Password = old.Config.Password
		}
		if cfg.Token == "" {
			cfg.Token = old.Config.Token
		}
		if cfg.TOTPSecret == "" {
			cfg.TOTPSecret = old.Config.TOTPSecret
		}
	}
	if err := cfg.Normalize(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	cred, err := s.m.PG().UpdateTaskCredential(r.Context(), taskID, id, role, cfg)
	if err != nil {
		if errors.Is(err, db.ErrTaskCredentialNotFound) {
			writeErr(w, 404, "凭据不存在或不属于该任务")
			return
		}
		if errors.Is(err, db.ErrTaskCredentialRoleExists) {
			writeErr(w, 409, "该 role 已存在")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	s.invalidateTaskAgents()
	writeJSON(w, 200, taskCredentialDTOOf(cred))
}

func (s *Server) pgDeleteTaskCredential(w http.ResponseWriter, r *http.Request) {
	taskID, ok := s.taskIDFromPath(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("cid"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid credential id")
		return
	}
	if err := s.m.PG().DeleteTaskCredential(r.Context(), taskID, id); err != nil {
		if errors.Is(err, db.ErrTaskCredentialNotFound) {
			writeErr(w, 404, "凭据不存在或不属于该任务")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	s.invalidateTaskAgents()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) pgCheckTaskCredential(w http.ResponseWriter, r *http.Request) {
	taskID, ok := s.taskIDFromPath(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("cid"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "invalid credential id")
		return
	}
	cred, err := s.m.PG().GetTaskCredential(r.Context(), taskID, id)
	if err != nil || cred == nil || cred.Config == nil || !cred.Config.Enabled {
		writeErr(w, 400, "该凭据未启用或不存在")
		return
	}
	res := runAuthCheck(cred.Config, s.m.ProxyAddr(), s.m.ProxyCACert())
	fail := !res.OK && (res.StatusCode == 401 || res.StatusCode == 403 ||
		(cred.Config.SuccessContains != "" && !res.Matched) ||
		(cred.Config.SuccessStatus > 0 && res.StatusCode != cred.Config.SuccessStatus))
	note, uerr := s.applyTaskCredentialOutcome(r.Context(), cred, res.OK, fail, res.Detail)
	if uerr != nil {
		res.Detail += "；状态回写失败：" + uerr.Error()
	} else if note != "" {
		res.Detail += "；" + note
	}
	latest, _ := s.m.PG().GetTaskCredential(r.Context(), taskID, id)
	writeJSON(w, 200, map[string]any{"result": res, "credential": taskCredentialDTOOf(latest)})
}

// applyTaskCredentialOutcome is shared by manual auth-check and session_login.
// It updates status/failure/lock state and returns a user-facing note.
func (s *Server) applyTaskCredentialOutcome(ctx context.Context, c *db.TaskCredential, ok, failure bool, detail string) (string, error) {
	if c == nil || c.Config == nil {
		return "", nil
	}
	hash := taskCredentialSecretHash(c.Config)
	oldStatus, oldFailures := c.Status, c.FailureCount
	failures := c.FailureCount
	var lockedUntil *time.Time
	status := "failed"
	note := ""
	if ok {
		status, failures, note = "ok", 0, ""
		if c.FailureCount > 0 {
			note = "凭据已恢复，失败计数清零"
		}
	} else if failure {
		if c.SecretHash != "" && c.SecretHash != hash {
			failures = 0 // config changed since last failure: old breaker doesn't apply
		}
		failures++
		if failures >= 2 {
			until := time.Now().Add(30 * time.Minute)
			lockedUntil = &until
			status = "locked"
			note = fmt.Sprintf("连续 2 次鉴权失败，已熔断至 %s；后续 session_login 本地直接拒绝", until.Format(time.RFC3339))
		} else {
			status = "failed"
			note = "鉴权失败计数 1/2"
		}
	}
	if detail == "" {
		detail = note
	}
	if err := s.m.PG().UpdateTaskCredentialHealth(ctx, c.TaskID, c.ID, status, detail, hash, failures, lockedUntil); err != nil {
		return note, err
	}
	// Only a health/state change needs a fresh prompt; successful logins with an
	// unchanged status must not invalidate every task bundle on each request.
	if status != oldStatus || failures != oldFailures {
		s.invalidateTaskAgents()
	}
	return note, nil
}

// ResolveTaskCredential implements agent.TaskCredentialResolver.
func (s *Server) ResolveTaskCredential(ctx context.Context, taskID, credentialID int64) (*db.TaskCredential, error) {
	return s.m.PG().GetTaskCredential(ctx, taskID, credentialID)
}

// RecordTaskCredentialLogin implements agent.TaskCredentialResolver. Only clear
// auth rejections (401/403) count; 2xx resets. Transport errors never reach here.
func (s *Server) RecordTaskCredentialLogin(ctx context.Context, taskID, credentialID int64, httpStatus int) (string, error) {
	c, err := s.m.PG().GetTaskCredential(ctx, taskID, credentialID)
	if err != nil || c == nil {
		return "", err
	}
	if httpStatus >= 200 && httpStatus < 300 {
		return s.applyTaskCredentialOutcome(ctx, c, true, false, "")
	}
	if httpStatus == 401 || httpStatus == 403 {
		return s.applyTaskCredentialOutcome(ctx, c, false, true, fmt.Sprintf("HTTP %d", httpStatus))
	}
	return "", nil
}

// taskCredentialsPromptBlock exposes only IDs/roles/health/login URLs. The model
// must call session_login(credential_id=...) and never receives passwords/tokens.
func taskCredentialsPromptBlock(list []*db.TaskCredential) string {
	active := make([]*db.TaskCredential, 0, len(list))
	for _, c := range list {
		if c != nil && c.Config != nil && c.Config.Enabled {
			active = append(active, c)
		}
	}
	if len(active) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n━━ 任务级多角色凭据（只可引用，不能读取明文）━━\n")
	b.WriteString("这些凭据由平台保存，调用 session_login(credential_id=N, session=角色名) 使用；不要尝试读取/猜测密码、token 或 TOTP。\n")
	for _, c := range active {
		status := c.Status
		if c.LockedUntil != nil && c.LockedUntil.After(time.Now()) {
			status = "locked"
		}
		if status == "" {
			status = "pending"
		}
		line := fmt.Sprintf("- credential_id=%d role=%s status=%s login=%s", c.ID, c.Role, status, c.Config.LoginURL)
		if c.LockedUntil != nil && c.LockedUntil.After(time.Now()) {
			line += "（已熔断到 " + c.LockedUntil.Format(time.RFC3339) + "）"
		}
		if c.LastError != "" && status != "ok" {
			line += " last_error=" + c.LastError
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("越权对照时用不同 credential_id 建立不同 session；不要在报告里写出任何秘密。\n")
	return b.String()
}
