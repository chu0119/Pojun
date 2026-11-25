package agent

import (
	"context"

	"github.com/chu0119/pojun/db"
)

// TaskCredentialResolver resolves a task's named credential pool without the
// model ever receiving the secret. The server implements it; agent tests can
// leave it nil (then only asset/explicit credentials work).
type TaskCredentialResolver interface {
	ResolveTaskCredential(ctx context.Context, taskID, credentialID int64) (*db.TaskCredential, error)
	// RecordTaskCredentialLogin applies the same 401/403 breaker semantics as the
	// asset-pool path. It returns a user-facing note on state change.
	RecordTaskCredentialLogin(ctx context.Context, taskID, credentialID int64, httpStatus int) (string, error)
}
