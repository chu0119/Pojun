package db

import (
	"context"
	"testing"
)

// ProtectedTrafficIDs must be queryable on a migrated schema; the retention
// worker calls it before every delete and fails closed on any error.
func TestProtectedTrafficIDsQuery(t *testing.T) {
	dsn, _, err := DSN()
	if err != nil || dsn == "" {
		t.Skip("no test database configured")
	}
	d, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	ids, err := d.ProtectedTrafficIDs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ids == nil {
		t.Fatal("expected non-nil map")
	}
}
