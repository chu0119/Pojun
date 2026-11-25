package db

import (
	"fmt"
	"testing"
	"time"
)

func TestProfileKeyPresence(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	suffix := time.Now().UnixNano()
	withKey, err := d.SaveProfile(&LLMProfile{Name: fmt.Sprintf("presence-key-%d", suffix), Format: "openai", Model: "m", APIKey: "k", Streaming: true})
	if err != nil {
		t.Fatalf("create keyed profile: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteProfile(withKey) })
	noKey, err := d.SaveProfile(&LLMProfile{Name: fmt.Sprintf("presence-nokey-%d", suffix), Format: "openai", Model: "m", Streaming: true})
	if err != nil {
		t.Fatalf("create keyless profile: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteProfile(noKey) })

	presence, err := d.ProfileKeyPresence()
	if err != nil {
		t.Fatalf("presence: %v", err)
	}
	if !presence[withKey] {
		t.Fatalf("keyed profile must report true")
	}
	if presence[noKey] {
		t.Fatalf("keyless profile must report false")
	}
}

// TestActivityKindsFilter proves the new kind filter narrows rows while an empty
// Kinds slice keeps the historical query behavior (no kind predicate at all).
func TestActivityKindsFilter(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	task, err := d.CreateTaskWithOptions("kinds-filter", "probe", TaskCreateOptions{})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })
	store := d.Exploration(task.ExplorationID)

	if _, err := store.AppendActivity(Activity{Worker: "worker", Kind: "thinking", Summary: "t"}); err != nil {
		t.Fatalf("append thinking: %v", err)
	}
	if _, err := store.AppendActivity(Activity{Worker: "system", Kind: "llm_switch", Summary: "s"}); err != nil {
		t.Fatalf("append switch: %v", err)
	}
	if _, err := store.AppendActivity(Activity{Worker: "system", Kind: "llm_failover", Summary: "f"}); err != nil {
		t.Fatalf("append failover: %v", err)
	}

	filtered, hasMore, err := store.ActivityPage(ActivitySessionFilter{Kinds: []string{"llm_switch", "llm_failover"}}, 0, 10)
	if err != nil {
		t.Fatalf("filtered page: %v", err)
	}
	if hasMore || len(filtered) != 2 {
		t.Fatalf("filtered page = %d rows hasMore=%v", len(filtered), hasMore)
	}
	for _, a := range filtered {
		if a.Kind != "llm_switch" && a.Kind != "llm_failover" {
			t.Fatalf("unexpected kind %q", a.Kind)
		}
	}
	// Empty Kinds must be byte-for-byte the old behavior: all three rows.
	all, hasMore, err := store.ActivityPage(ActivitySessionFilter{}, 0, 10)
	if err != nil {
		t.Fatalf("unfiltered page: %v", err)
	}
	if hasMore || len(all) != 3 {
		t.Fatalf("unfiltered page = %d rows hasMore=%v", len(all), hasMore)
	}
}

// TestTaskLLMContextSnapshot checks the exported one-statement snapshot keeps
// position order and the cursor in sync.
func TestTaskLLMContextSnapshot(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	suffix := time.Now().UnixNano()
	var ids []int64
	for i := 0; i < 2; i++ {
		id, err := d.SaveProfile(&LLMProfile{Name: fmt.Sprintf("ctx-%d-%d", suffix, i), Format: "openai", Model: "m", APIKey: "k", Streaming: true})
		if err != nil {
			t.Fatalf("create profile: %v", err)
		}
		t.Cleanup(func() { _ = d.DeleteProfile(id) })
		ids = append(ids, id)
	}
	task, err := d.CreateTaskWithOptions("ctx-snapshot", "probe", TaskCreateOptions{LLMProfileIDs: ids})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(task.ID) })

	_, active, _, chain, err := d.TaskLLMContext(task.ID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(chain) != 2 || chain[0].ProfileID != ids[0] || chain[1].ProfileID != ids[1] {
		t.Fatalf("chain = %+v", chain)
	}
	if chain[0].Position > chain[1].Position {
		t.Fatalf("positions must ascend: %+v", chain)
	}
	if active == nil || *active != ids[0] {
		t.Fatalf("active = %v, want %d", active, ids[0])
	}
	if chain[0].Status != "ready" {
		t.Fatalf("new chain entries must be ready: %+v", chain[0])
	}
}
