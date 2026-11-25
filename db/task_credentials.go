package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TaskCredential is one named credential in a task's multi-role pool (#24).
// Config contains secrets; callers returning it over HTTP must use a redacted
// DTO (HasPassword/HasToken/HasTOTP) — TaskAuthConfig itself also hides them in
// JSON via `json:"-"`, but the DB representation intentionally keeps them.
type TaskCredential struct {
	ID            int64           `json:"id"`
	TaskID        int64           `json:"task_id"`
	Role          string          `json:"role"`
	Config        *TaskAuthConfig `json:"config"`
	Status        string          `json:"status"` // pending|ok|failed|locked
	LastCheckedAt *time.Time      `json:"last_checked_at,omitempty"`
	LastError     string          `json:"last_error,omitempty"`
	FailureCount  int             `json:"failure_count"`
	LockedUntil   *time.Time      `json:"locked_until,omitempty"`
	SecretHash    string          `json:"-"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

var ErrTaskCredentialNotFound = errors.New("task credential not found")
var ErrTaskCredentialRoleExists = errors.New("task credential role already exists")

const taskCredentialCols = `id,task_id,role,config,status,last_checked_at,last_error,failure_count,locked_until,secret_hash,created_at,updated_at`

func scanTaskCredential(row interface {
	Scan(dest ...any) error
}) (*TaskCredential, error) {
	var c TaskCredential
	var raw []byte
	var lastChecked, lockedUntil sql.NullTime
	if err := row.Scan(&c.ID, &c.TaskID, &c.Role, &raw, &c.Status, &lastChecked, &c.LastError,
		&c.FailureCount, &lockedUntil, &c.SecretHash, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	c.Config = ParseTaskAuth(raw)
	if c.Config == nil {
		c.Config = &TaskAuthConfig{}
	}
	if lastChecked.Valid {
		c.LastCheckedAt = &lastChecked.Time
	}
	if lockedUntil.Valid {
		c.LockedUntil = &lockedUntil.Time
	}
	return &c, nil
}

// ListTaskCredentials returns the task's credential pool including secrets. It is
// for server-side login/check paths only; HTTP handlers must project a DTO.
func (d *DB) ListTaskCredentials(ctx context.Context, taskID int64) ([]*TaskCredential, error) {
	rows, err := d.QueryContext(ctx, `SELECT `+taskCredentialCols+` FROM task_credentials WHERE task_id=$1 ORDER BY role,id`, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*TaskCredential{}
	for rows.Next() {
		c, err := scanTaskCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetTaskCredential returns one credential scoped to the task.
func (d *DB) GetTaskCredential(ctx context.Context, taskID, id int64) (*TaskCredential, error) {
	c, err := scanTaskCredential(d.QueryRowContext(ctx, `SELECT `+taskCredentialCols+` FROM task_credentials WHERE task_id=$1 AND id=$2`, taskID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTaskCredentialNotFound
	}
	return c, err
}

// CreateTaskCredential inserts a named credential. Config must already be
// normalized by the caller.
func (d *DB) CreateTaskCredential(ctx context.Context, taskID int64, role string, cfg *TaskAuthConfig) (*TaskCredential, error) {
	raw, err := cfg.DBValue()
	if err != nil {
		return nil, err
	}
	var id int64
	err = d.QueryRowContext(ctx, `INSERT INTO task_credentials(task_id,role,config,status)
VALUES($1,$2,$3,'pending') RETURNING id`, taskID, strings.TrimSpace(role), raw).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "uq_task_credential_role") || strings.Contains(err.Error(), "duplicate key") {
			return nil, ErrTaskCredentialRoleExists
		}
		return nil, err
	}
	return d.GetTaskCredential(ctx, taskID, id)
}

// UpdateTaskCredential replaces a credential's role/config and resets its health
// state: a changed password/token must not inherit an old breaker.
func (d *DB) UpdateTaskCredential(ctx context.Context, taskID, id int64, role string, cfg *TaskAuthConfig) (*TaskCredential, error) {
	raw, err := cfg.DBValue()
	if err != nil {
		return nil, err
	}
	tag, err := d.ExecContext(ctx, `UPDATE task_credentials
SET role=$3, config=$4, status='pending', last_checked_at=NULL, last_error='', failure_count=0, locked_until=NULL, secret_hash='', updated_at=now()
WHERE task_id=$1 AND id=$2`, taskID, id, strings.TrimSpace(role), raw)
	if err != nil {
		if strings.Contains(err.Error(), "uq_task_credential_role") || strings.Contains(err.Error(), "duplicate key") {
			return nil, ErrTaskCredentialRoleExists
		}
		return nil, err
	}
	if n, _ := tag.RowsAffected(); n == 0 {
		return nil, ErrTaskCredentialNotFound
	}
	return d.GetTaskCredential(ctx, taskID, id)
}

func (d *DB) DeleteTaskCredential(ctx context.Context, taskID, id int64) error {
	tag, err := d.ExecContext(ctx, `DELETE FROM task_credentials WHERE task_id=$1 AND id=$2`, taskID, id)
	if err != nil {
		return err
	}
	if n, _ := tag.RowsAffected(); n == 0 {
		return ErrTaskCredentialNotFound
	}
	return nil
}

// UpdateTaskCredentialHealth records one login-check outcome. status is one of
// pending/ok/failed/locked; lastError must already be redacted (no secrets).
func (d *DB) UpdateTaskCredentialHealth(ctx context.Context, taskID, id int64, status, lastError, secretHash string, failureCount int, lockedUntil *time.Time) error {
	if status != "pending" && status != "ok" && status != "failed" && status != "locked" {
		return fmt.Errorf("invalid credential status %q", status)
	}
	_, err := d.ExecContext(ctx, `UPDATE task_credentials
SET status=$3, last_checked_at=now(), last_error=$4, failure_count=$5, locked_until=$6, secret_hash=$7, updated_at=now()
WHERE task_id=$1 AND id=$2`, taskID, id, status, lastError, failureCount, lockedUntil, secretHash)
	return err
}
