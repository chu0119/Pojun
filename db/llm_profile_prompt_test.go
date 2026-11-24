package db

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The profile prompt columns were added after the initial CREATE TABLE, so old
// databases only get them from schema.sql's idempotent ALTER block. This test
// rolls the table back to the pre-column shape, applies the real schema twice,
// and checks defaults + CHECK constraints. PGDDL is transactional, so the whole
// test is rolled back.
func TestSchemaMigrateAddsLLMProfilePrompt(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	tx, err := d.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck // 测试永不提交

	for _, stmt := range []string{
		`ALTER TABLE llm_profiles DROP CONSTRAINT IF EXISTS llm_profiles_prompt_pair_check`,
		`ALTER TABLE llm_profiles DROP CONSTRAINT IF EXISTS llm_profiles_prompt_len_check`,
		`ALTER TABLE llm_profiles DROP CONSTRAINT IF EXISTS llm_profiles_prompt_mode_check`,
		`ALTER TABLE llm_profiles DROP COLUMN IF EXISTS prompt_mode`,
		`ALTER TABLE llm_profiles DROP COLUMN IF EXISTS prompt_apply_inherited`,
		`ALTER TABLE llm_profiles DROP COLUMN IF EXISTS prompt`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			t.Fatalf("simulate pre-upgrade table: %v", err)
		}
	}

	// Run the real schema (production applies the whole file on every startup).
	if _, err := tx.Exec(schemaSQL); err != nil {
		t.Fatalf("schema: %v", err)
	}

	var n int
	if err := tx.QueryRow(`
SELECT count(*) FROM information_schema.columns
 WHERE table_name='llm_profiles' AND column_name IN ('prompt','prompt_mode','prompt_apply_inherited')`).Scan(&n); err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if n != 3 {
		t.Fatalf("prompt columns missing: got %d", n)
	}

	// Idempotent: production executes the whole schema on every start.
	if _, err := tx.Exec(schemaSQL); err != nil {
		t.Fatalf("schema second run must be idempotent: %v", err)
	}

	base := fmt.Sprintf("prompt-migrate-%d", time.Now().UnixNano())
	// Every expected failure aborts the surrounding transaction. Wrap each insert
	// in a savepoint so the next case (and the final valid insert) still runs.
	const sp = "sp_prompt_insert"
	insert := func(name, prompt, mode string) error {
		if _, err := tx.Exec("SAVEPOINT " + sp); err != nil {
			t.Fatalf("savepoint: %v", err)
		}
		_, err := tx.Exec(`INSERT INTO llm_profiles(name,format,model,prompt,prompt_mode) VALUES($1,'openai','m',$2,$3)`, name, prompt, mode)
		if _, rbErr := tx.Exec("ROLLBACK TO SAVEPOINT " + sp); rbErr != nil {
			t.Fatalf("rollback to savepoint: %v", rbErr)
		}
		return err
	}
	// (prompt = '') must equal (prompt_mode = '').
	if err := insert(base+"-pair", "x", ""); err == nil {
		t.Fatal("prompt without mode must be rejected")
	}
	if err := insert(base+"-mode", "x", "bogus"); err == nil {
		t.Fatal("invalid prompt_mode must be rejected")
	}
	if err := insert(base+"-len", strings.Repeat("a", 8001), "append"); err == nil {
		t.Fatal("8001-char prompt must be rejected")
	}
	if err := insert(base+"-ok", "x", "append"); err != nil {
		t.Fatalf("valid prompt rejected: %v", err)
	}
}

func TestProfilePromptRoundTrip(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	name := fmt.Sprintf("prompt-roundtrip-%d", time.Now().UnixNano())
	p := &LLMProfile{Name: name, Format: "openai", Model: "m"}
	id, err := d.SaveProfile(p)
	if err != nil {
		t.Fatalf("SaveProfile: %v", err)
	}
	// Registered after `defer d.Close()`, so LIFO runs this before the pool closes.
	// A t.Cleanup would run after the function returns (pool already closed) and
	// silently leak the row into the shared test database.
	defer func() { _ = d.DeleteProfile(id) }()

	pi, err := d.ProfilePrompt(id)
	if err != nil {
		t.Fatalf("ProfilePrompt empty: %v", err)
	}
	if pi.Text != "" || pi.Mode != "" || pi.ApplyInherited {
		t.Fatalf("new profile should have no prompt, got %+v", pi)
	}

	if err := d.SetProfilePrompt(id, "你是测试角色。", "prepend", true); err != nil {
		t.Fatalf("SetProfilePrompt: %v", err)
	}
	pi, err = d.ProfilePrompt(id)
	if err != nil {
		t.Fatalf("ProfilePrompt read: %v", err)
	}
	if pi.Text != "你是测试角色。" || pi.Mode != "prepend" || !pi.ApplyInherited {
		t.Fatalf("prompt roundtrip mismatch: %+v", pi)
	}

	all, err := d.ProfilePrompts()
	if err != nil {
		t.Fatalf("ProfilePrompts: %v", err)
	}
	if got := all[id]; got.Text != pi.Text || got.Mode != pi.Mode || got.ApplyInherited != pi.ApplyInherited {
		t.Fatalf("ProfilePrompts mismatch: %+v", got)
	}

	// Editing other profile fields through SaveProfile must not touch the prompt.
	edited := &LLMProfile{ID: id, Name: name, Format: "openai", Model: "m2"}
	if _, err := d.SaveProfile(edited); err != nil {
		t.Fatalf("SaveProfile update: %v", err)
	}
	pi, err = d.ProfilePrompt(id)
	if err != nil || pi.Text != "你是测试角色。" {
		t.Fatalf("SaveProfile must preserve prompt, got %+v err=%v", pi, err)
	}
}
