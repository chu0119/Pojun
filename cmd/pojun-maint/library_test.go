package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chu0119/pojun/db"
)

func writeTestPack(t *testing.T, dir, id, title string, valid bool) {
	t.Helper()
	entry := map[string]any{
		"title":       title,
		"description": "测试条目",
		"detail":      "适用：单元测试。步骤：1) 执行。验证：返回 200。误报：无。",
		"commands":    []string{"curl -sS https://{{TARGET}}/"},
		"tags":        []string{"test"},
		"source":      "https://example.com/test",
	}
	if !valid {
		entry["commands"] = []string{}
		delete(entry, "source")
	}
	raw, _ := json.Marshal(entry)
	if err := os.WriteFile(filepath.Join(dir, "entries.jsonl"), append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{
		"id": id, "name": "测试包", "version": "1.0.0", "schema": 2,
		"license": "CC0-1.0", "review_status": "approved",
		"sources": []string{"https://example.com"}, "entry_count": 1,
	}
	mraw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), mraw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLintKBPack(t *testing.T) {
	good := t.TempDir()
	writeTestPack(t, good, "lint-good", "lint good entry", true)
	if err := lintKBPack(good); err != nil {
		t.Fatalf("合法包 lint 失败: %v", err)
	}
	bad := t.TempDir()
	writeTestPack(t, bad, "lint-bad", "lint bad entry", false)
	if err := lintKBPack(bad); err == nil {
		t.Fatal("缺 source/commands 的包应 lint 失败")
	}
}

func TestImportKBPackDefaultsDraftAndIdempotent(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no database config (%v) — skipping", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	pg.Close()

	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "config.json")
	cfg, _ := json.Marshal(map[string]any{"database": map[string]string{"dsn": dsn}})
	if err := os.WriteFile(cfgPath, cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	packID := fmt.Sprintf("maint-test-%d", time.Now().UnixNano())
	title := "Maint 导入测试 " + fmt.Sprint(time.Now().UnixNano())
	dir := t.TempDir()
	writeTestPack(t, dir, packID, title, true)
	flag := "kb_pack_" + strings.ToLower(packID) + "_1.0.0"
	nkey := strings.ToLower(strings.Join(strings.Fields(title), " "))
	cleanup := func() {
		d, err := db.Open(dsn)
		if err != nil {
			return
		}
		defer d.Close()
		_, _ = d.Exec(`DELETE FROM kb_entries WHERE title=$1`, title)
		_, _ = d.Exec(`DELETE FROM settings WHERE key=$1`, flag)
	}
	cleanup()
	t.Cleanup(cleanup)

	if err := importKBPack(dir, cfgPath, ""); err != nil {
		t.Fatalf("import: %v", err)
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	e, err := d.KB().GetByNKey(nkey)
	if err != nil {
		t.Fatalf("导入后未找到条目: %v", err)
	}
	if e.Status != db.KBStatusDraft {
		t.Fatalf("外部导入默认应为 draft，实际 %s", e.Status)
	}
	// 删除后重导同一 id+version：flag 已 done，不应复活。
	if _, err := d.KB().Delete(e.ID); err != nil {
		t.Fatal(err)
	}
	if err := importKBPack(dir, cfgPath, ""); err != nil {
		t.Fatalf("重导: %v", err)
	}
	if _, err := d.KB().GetByNKey(nkey); err != db.ErrKBEntryNotFound {
		t.Fatalf("同版本重导不应复活条目: %v", err)
	}
}
