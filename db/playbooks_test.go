package db

import (
	"fmt"
	"testing"
	"time"
)

// v0.9.70 backport: playbooks are task-scoped. A row captured by task 101 must
// never be injected into task 102's planner, even when key/root/techs match.
func TestPlaybookTaskIsolation(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	if err := d.EnsurePlaybookSchema(); err != nil {
		t.Fatal(err)
	}
	suffix := time.Now().UnixNano()
	key := fmt.Sprintf("iso-%d", suffix)
	root := fmt.Sprintf("iso-%d.example", suffix)
	base := 100000 + suffix%1000000
	taskA, taskB, taskC := base, base+1, base+2
	for _, taskID := range []int64{taskA, taskB} {
		if _, err := d.Exec(`INSERT INTO playbooks
(name, key, vulnclass, severity, root_domain, techs, url_pattern, steps, task_id)
VALUES ($1,$2,'sql_injection','high',$3,$4,'api/item/N','[]'::jsonb,$5)`,
			fmt.Sprintf("task-%d", taskID), key, root, []string{"nginx"}, taskID); err != nil {
			t.Fatalf("insert task %d: %v", taskID, err)
		}
	}
	got := d.MatchPlaybooks(taskA, "", []string{"nginx"}, root, 10)
	if len(got) != 1 {
		t.Fatalf("task %d matched %d playbooks, want 1: %+v", taskA, len(got), got)
	}
	// The query must also filter by task_id rather than relying on tech overlap.
	if got[0].TaskID != taskA {
		t.Fatalf("task %d got playbook from task %d", taskA, got[0].TaskID)
	}
	if other := d.MatchPlaybooks(taskC, "", []string{"nginx"}, root, 10); len(other) != 0 {
		t.Fatalf("unrelated task matched %d playbooks", len(other))
	}
}
