package db

import (
	"fmt"
	"testing"
	"time"
)

func TestValidReasoningEffort(t *testing.T) {
	for _, v := range []string{"low", "medium", "high", "xhigh", "max"} {
		if !ValidReasoningEffort(v) {
			t.Fatalf("%q must be valid", v)
		}
	}
	for _, v := range []string{"", "LOW", "none", "ultra"} {
		if ValidReasoningEffort(v) {
			t.Fatalf("%q must be rejected", v)
		}
	}
}

// TestEffortOverrideRoundTrip exercises the (agent, profile) override store:
// upsert-overwrite, read, delete, idempotent delete, and cascade when either
// side disappears. The FK cascade matters because a deleted LLM profile or agent
// must never leave an orphaned override that silently changes a future request.
func TestEffortOverrideRoundTrip(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	suffix := time.Now().UnixNano()
	agentKey := fmt.Sprintf("t_eff_%d", suffix)
	if _, err := d.Exec(`INSERT INTO agents(key, name, role) VALUES ($1,$1,'worker')`, agentKey); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM agents WHERE key=$1`, agentKey) })

	profileID, err := d.SaveProfile(&LLMProfile{
		Name: fmt.Sprintf("t-effort-%d", suffix), Format: "openai", Model: "m", Streaming: true,
	})
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteProfile(profileID) })

	if err := d.SetEffortOverride(agentKey, profileID, "low"); err != nil {
		t.Fatalf("set low: %v", err)
	}
	got, ok, err := d.EffortOverride(agentKey, profileID)
	if err != nil || !ok || got.ReasoningEffort != "low" {
		t.Fatalf("read override: %+v ok=%v err=%v", got, ok, err)
	}

	if err := d.SetEffortOverride(agentKey, profileID, "xhigh"); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	all, err := d.EffortOverrides(agentKey)
	if err != nil {
		t.Fatalf("list overrides: %v", err)
	}
	if len(all) != 1 || all[profileID].ReasoningEffort != "xhigh" {
		t.Fatalf("override must be replaced, not duplicated: %+v", all)
	}

	// Deleting a profile cascades its overrides; the row must not survive.
	if err := d.DeleteProfile(profileID); err != nil {
		t.Fatalf("delete profile: %v", err)
	}
	if _, ok, err := d.EffortOverride(agentKey, profileID); err != nil || ok {
		t.Fatalf("override must cascade with the profile: ok=%v err=%v", ok, err)
	}

	// Deleting a missing override is idempotent (found=false, no error).
	if found, err := d.DeleteEffortOverride(agentKey, profileID); err != nil || found {
		t.Fatalf("delete missing override: found=%v err=%v", found, err)
	}

	// Deleting the agent cascades too.
	other, err := d.SaveProfile(&LLMProfile{Name: fmt.Sprintf("t-effort2-%d", suffix), Format: "openai", Model: "m", Streaming: true})
	if err != nil {
		t.Fatalf("create second profile: %v", err)
	}
	t.Cleanup(func() { _ = d.DeleteProfile(other) })
	if err := d.SetEffortOverride(agentKey, other, "max"); err != nil {
		t.Fatalf("set max: %v", err)
	}
	if _, err := d.Exec(`DELETE FROM agents WHERE key=$1`, agentKey); err != nil {
		t.Fatalf("delete agent: %v", err)
	}
	if _, ok, err := d.EffortOverride(agentKey, other); err != nil || ok {
		t.Fatalf("override must cascade with the agent: ok=%v err=%v", ok, err)
	}
}
