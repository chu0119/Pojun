package db

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openKBTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func uniqueKBTitle(t *testing.T, suffix string) string {
	t.Helper()
	return fmt.Sprintf("KB 测试 %s %d", suffix, time.Now().UnixNano())
}

func cleanupKBEntry(t *testing.T, s *KBStore, id int64) {
	t.Helper()
	if id > 0 {
		t.Cleanup(func() { _, _ = s.Delete(id) })
	}
}

// 迁移必须可重复执行：schema.sql 里既有 CREATE TABLE IF NOT EXISTS，也有
// ALTER TABLE ADD COLUMN IF NOT EXISTS / DROP-CREATE TRIGGER，跑两遍不应报错，
// 且 v2 列/索引真实存在。
func TestKBSchemaMigrationIdempotent(t *testing.T) {
	d := openKBTestDB(t)
	for i := 0; i < 2; i++ {
		if _, err := d.Exec(schemaSQL); err != nil {
			t.Fatalf("apply schema #%d: %v", i+1, err)
		}
	}
	for _, col := range []string{"search_text", "quality_score", "products", "vuln_classes", "platforms", "cve_ids", "license", "verified_at"} {
		var exists bool
		if err := d.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name='kb_entries' AND column_name=$1)`, col).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("kb_entries 缺少列 %s", col)
		}
	}
	var idx int
	if err := d.QueryRow(`SELECT count(*) FROM pg_indexes
		WHERE tablename='kb_entries' AND indexname='idx_kb_entries_fts'`).Scan(&idx); err != nil {
		t.Fatal(err)
	}
	if idx != 1 {
		t.Fatalf("kb_entries FTS GIN 索引缺失")
	}
	// 检索日志的归因列必须是 NOT NULL（actor 有默认 unknown、task_id 默认 0）。
	var nullable string
	if err := d.QueryRow(`SELECT is_nullable FROM information_schema.columns
		WHERE table_name='kb_retrieval_logs' AND column_name='actor'`).Scan(&nullable); err != nil {
		t.Fatal(err)
	}
	if nullable != "NO" {
		t.Fatalf("kb_retrieval_logs.actor 必须 NOT NULL，实际 is_nullable=%s", nullable)
	}
}

func TestKBEntryCRUDAndStatus(t *testing.T) {
	d := openKBTestDB(t)
	s := d.KB()
	title := uniqueKBTitle(t, "CRUD")
	e, err := s.Create(KBEntryInput{
		Title: title, Description: "一句话说明", Detail: "适用：单元测试。步骤：1) 建。验证：查得到。误报：无。",
		Commands: []string{"curl -sS https://{{TARGET}}/"},
		Tags:     []string{"Test", "crud", "test"},
		Layer:    KBLayerGeneral, Status: KBStatusDraft, Confidence: KBConfidenceObserved,
		Source: "https://example.com/test", CreatedBy: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupKBEntry(t, s, e.ID)
	if e.Status != KBStatusDraft {
		t.Fatalf("默认状态应为 draft，实际 %s", e.Status)
	}
	if len(e.Tags) != 2 || e.Tags[0] != "crud" {
		t.Fatalf("tags 归一化/排序失败: %v", e.Tags)
	}
	if e.QualityScore <= 0 {
		t.Fatalf("质量分应 >0: %v", e.QualityScore)
	}

	// 同标题再建 → 冲突（nkey 唯一）。
	if _, err := s.Create(KBEntryInput{Title: title, Detail: "dup"}); err != ErrKBEntryNameConflict {
		t.Fatalf("同标题应返回 ErrKBEntryNameConflict，实际 %v", err)
	}

	got, err := s.Get(e.ID)
	if err != nil || got.Title != title {
		t.Fatalf("Get: %v %+v", err, got)
	}

	entries, total, err := s.List(KBFilter{Query: "单元测试", Status: KBStatusDraft}, 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range entries {
		if it.ID == e.ID {
			found = true
		}
	}
	if !found || total == 0 {
		t.Fatalf("List 未找到新建条目（total=%d）", total)
	}

	approved, err := s.SetStatus(e.ID, KBStatusApproved)
	if err != nil {
		t.Fatal(err)
	}
	if approved.Status != KBStatusApproved || approved.VerifiedAt == nil {
		t.Fatalf("批准后 status/verified_at 未更新: %+v", approved)
	}
	if err := s.MarkUsed(e.ID); err != nil {
		t.Fatal(err)
	}
	used, _ := s.Get(e.ID)
	if used.Uses != 1 || used.LastUsedAt == nil {
		t.Fatalf("MarkUsed 未累加: %+v", used)
	}

	counts, err := s.CountByStatus()
	if err != nil || counts[KBStatusApproved] == 0 {
		t.Fatalf("CountByStatus: %v %v", err, counts)
	}

	ok, err := s.Delete(e.ID)
	if err != nil || !ok {
		t.Fatalf("Delete: %v %v", err, ok)
	}
	if _, err := s.Get(e.ID); err != ErrKBEntryNotFound {
		t.Fatalf("删除后 Get 应 NotFound，实际 %v", err)
	}
}

// 中文长句 + 二元组：查询和标题用词不完全一致时仍应命中。
func TestKBSearchChineseLongQuery(t *testing.T) {
	d := openKBTestDB(t)
	s := d.KB()
	title := uniqueKBTitle(t, "Actuator 长句")
	e, err := s.Create(KBEntryInput{
		Title:       title,
		Description: "Spring Boot Actuator heapdump 提取数据库连接与密钥",
		Detail:      "适用：暴露 heapdump 的 Spring Boot 服务。步骤：下载 heapdump，用 strings 搜索 jdbc、password。验证：能解析出真实配置。误报：空文件或 403。",
		Commands:    []string{"curl -sS -o heapdump.bin 'https://{{TARGET}}/actuator/heapdump'"},
		Tags:        []string{"spring", "actuator", "heapdump"},
		Status:      KBStatusApproved,
		Source:      "https://example.com/actuator",
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupKBEntry(t, s, e.ID)

	hits, err := s.Search(KBSearchQuery{
		Query:  "如何在 Spring Boot Actuator 中通过 heapdump 提取数据库连接与密钥",
		Limit:  10,
		Actor:  "test-long-query",
		TaskID: 900001,
		NoLog:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	pos := -1
	for i, h := range hits {
		if h.ID == e.ID {
			pos = i
			break
		}
	}
	if pos < 0 {
		t.Fatalf("中文长句未召回目标条目，got %d hits", len(hits))
	}
	if pos >= 5 {
		t.Fatalf("中文长句目标条目排在第 %d 位（期望前 5），hits=%d", pos+1, len(hits))
	}

	// 草稿默认不参与检索。
	draftTitle := uniqueKBTitle(t, "草稿不可见")
	draft, err := s.Create(KBEntryInput{Title: draftTitle, Detail: "draft only", Status: KBStatusDraft})
	if err != nil {
		t.Fatal(err)
	}
	cleanupKBEntry(t, s, draft.ID)
	draftHits, err := s.Search(KBSearchQuery{Query: draftTitle, Limit: 10, Actor: "test", NoLog: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range draftHits {
		if h.ID == draft.ID {
			t.Fatalf("草稿不应出现在默认检索中")
		}
	}
}

func TestKBFacetsAndRRFChannels(t *testing.T) {
	d := openKBTestDB(t)
	s := d.KB()
	tag := fmt.Sprintf("rrfprod%d", time.Now().UnixNano())
	class := fmt.Sprintf("rrfclass%d", time.Now().UnixNano())
	title := uniqueKBTitle(t, "RRF 融合")
	e, err := s.Create(KBEntryInput{
		Title:       title,
		Description: "RRF 三路融合单元测试",
		Detail:      "适用：测试。步骤：查。验证：命中。误报：无。",
		Commands:    []string{"curl -sS https://{{TARGET}}/"},
		Status:      KBStatusApproved,
		Products:    []string{tag},
		VulnClasses: []string{class},
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupKBEntry(t, s, e.ID)

	// 只给 facet 条件（无 q）→ structured/usage 通道。
	facetHits, err := s.Search(KBSearchQuery{
		Products: []string{tag}, Limit: 10, Actor: "test-facet", NoLog: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(facetHits) == 0 || facetHits[0].ID != e.ID {
		t.Fatalf("facet 检索未命中: %+v", facetHits)
	}
	hasChannel := func(h *KBSearchHit, name string) bool {
		for _, c := range h.Channels {
			if c == name {
				return true
			}
		}
		return false
	}
	if !hasChannel(facetHits[0], "facet") {
		t.Fatalf("facet 检索应标记 facet 通道: %v", facetHits[0].Channels)
	}

	// q + facet → lexical + facet 两路都命中。
	both, err := s.Search(KBSearchQuery{
		Query: "RRF 融合", Products: []string{tag}, Limit: 10, Actor: "test-rrf", NoLog: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var target *KBSearchHit
	for _, h := range both {
		if h.ID == e.ID {
			target = h
		}
	}
	if target == nil {
		t.Fatalf("q+facet 未命中目标")
	}
	if !hasChannel(target, "lexical") || !hasChannel(target, "facet") {
		t.Fatalf("RRF 应合并 lexical/facet 通道，实际 %v", target.Channels)
	}

	facets, err := s.Facets(KBStatusApproved)
	if err != nil {
		t.Fatal(err)
	}
	foundProduct := false
	for _, f := range facets["products"] {
		if f.Value == tag && f.Count >= 1 {
			foundProduct = true
		}
	}
	if !foundProduct {
		t.Fatalf("Facets 未返回产品 %s: %+v", tag, facets["products"])
	}
}

// 检索日志必须带 Actor/TaskID 归因，且不落条目正文（只记 query/ids/延迟）。
func TestKBRetrievalLogAttribution(t *testing.T) {
	d := openKBTestDB(t)
	s := d.KB()
	if _, err := d.Exec(`DELETE FROM kb_retrieval_logs`); err != nil {
		t.Fatal(err)
	}
	title := uniqueKBTitle(t, "日志归因")
	secretBody := "这段正文不应出现在检索日志里 " + title
	e, err := s.Create(KBEntryInput{
		Title: title, Detail: secretBody, Status: KBStatusApproved,
		Commands: []string{"curl -sS https://{{TARGET}}/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	cleanupKBEntry(t, s, e.ID)

	const actor = "worker-test-attribution"
	const taskID = int64(987654321)
	if _, err := s.Search(KBSearchQuery{
		Query: title, Limit: 5, Actor: actor, TaskID: taskID,
	}); err != nil {
		t.Fatal(err)
	}
	var gotActor, gotQuery string
	var gotTask int64
	var returned int
	var hitTarget bool
	if err := d.QueryRow(`SELECT actor, task_id, query, returned,
		COALESCE($1::bigint = ANY(top_ids), false)
		FROM kb_retrieval_logs ORDER BY id DESC LIMIT 1`, e.ID).Scan(&gotActor, &gotTask, &gotQuery, &returned, &hitTarget); err != nil {
		t.Fatal(err)
	}
	if gotActor != actor || gotTask != taskID {
		t.Fatalf("归因丢失: actor=%q task=%d", gotActor, gotTask)
	}
	if returned < 1 || !hitTarget {
		t.Fatalf("日志未记录命中: returned=%d hitTarget=%v", returned, hitTarget)
	}
	if strings.Contains(gotQuery, secretBody) {
		t.Fatalf("日志 query 不应包含条目正文")
	}

	stats, err := s.RetrievalStats(7)
	if err != nil {
		t.Fatalf("RetrievalStats(7) 失败（days 文本绑定回归）: %v", err)
	}
	if stats.Days != 7 || stats.Total < 1 {
		t.Fatalf("stats 不符合预期: %+v", stats)
	}
	if stats.AttributedPct <= 0 || stats.Actors < 1 {
		t.Fatalf("归因统计应 >0: %+v", stats)
	}
	// 另一个天数路径也必须可执行（同一处文本绑定）。
	if _, err := s.RetrievalStats(30); err != nil {
		t.Fatalf("RetrievalStats(30): %v", err)
	}
}

func TestKBSearchEmptyKBReturnsExplicitEmpty(t *testing.T) {
	d := openKBTestDB(t)
	s := d.KB()
	hits, err := s.Search(KBSearchQuery{
		Tags:  []string{"zz-no-such-tag-" + fmt.Sprint(time.Now().UnixNano())},
		Limit: 5, Actor: "test-empty", NoLog: true,
	})
	if err != nil {
		t.Fatalf("空结果不应报错: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("期望空结果，实际 %d 条", len(hits))
	}
}

func TestShippedKBPacksValid(t *testing.T) {
	root := filepath.Join("..", "skills")
	dirs, err := DiscoverKBPacks(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) < 3 {
		t.Fatalf("随包 kb-pack 少于 3 个: %v", dirs)
	}
	want := map[string]bool{"kb-web-api": false, "kb-java-middleware": false, "kb-cloud-k8s": false}
	for _, dir := range dirs {
		p, err := LoadKBPack(dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if _, ok := want[p.Manifest.ID]; !ok {
			continue
		}
		want[p.Manifest.ID] = true
		if issues := ValidateKBPack(p); len(issues) > 0 {
			t.Fatalf("%s 校验失败: %v", p.Manifest.ID, issues)
		}
		if p.Manifest.EntryCount != len(p.Entries) || len(p.Entries) == 0 {
			t.Fatalf("%s 条目数不符: manifest=%d actual=%d", p.Manifest.ID, p.Manifest.EntryCount, len(p.Entries))
		}
		seen := map[string]bool{}
		for _, e := range p.Entries {
			if strings.TrimSpace(e.Source) == "" {
				t.Fatalf("%s《%s》缺 source", p.Manifest.ID, e.Title)
			}
			if !strings.HasPrefix(e.Source, "http://") && !strings.HasPrefix(e.Source, "https://") {
				t.Fatalf("%s《%s》source 不是链接: %s", p.Manifest.ID, e.Title, e.Source)
			}
			if len(e.Commands) == 0 {
				t.Fatalf("%s《%s》缺命令", p.Manifest.ID, e.Title)
			}
			key := kbNKey(e.Title)
			if seen[key] {
				t.Fatalf("%s 标题重复: %s", p.Manifest.ID, e.Title)
			}
			seen[key] = true
		}
	}
	for id, ok := range want {
		if !ok {
			t.Fatalf("缺少随包 kb-pack: %s", id)
		}
	}
}

// 播种按 id+version 幂等：同版本重播不复活被删条目；换版本才会再扫。
func TestKBPackSeedIdempotent(t *testing.T) {
	d := openKBTestDB(t)
	s := d.KB()
	packID := fmt.Sprintf("test-kb-pack-%d", time.Now().UnixNano())
	version := "1.0.0"
	dir := t.TempDir()
	title1 := uniqueKBTitle(t, "包条目一")
	title2 := uniqueKBTitle(t, "包条目二")
	entries := []map[string]any{
		{"title": title1, "description": "一", "detail": "适用：测试。验证：命中。误报：无。", "commands": []string{"echo 1"}, "source": "https://example.com/1"},
		{"title": title2, "description": "二", "detail": "适用：测试。验证：命中。误报：无。", "commands": []string{"echo 2"}, "source": "https://example.com/2"},
	}
	var b strings.Builder
	for _, e := range entries {
		raw, _ := json.Marshal(e)
		b.Write(raw)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "entries.jsonl"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{
		"id": packID, "name": "test pack", "version": version, "schema": 2,
		"license": "CC0-1.0", "review_status": "approved",
		"sources": []string{"https://example.com"}, "entry_count": len(entries),
	}
	rawManifest, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), rawManifest, 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadKBPack(dir)
	if err != nil {
		t.Fatal(err)
	}
	flag := kbPackFlag(packID, version)
	cleanup := func() {
		_, _ = d.Exec(`DELETE FROM kb_entries WHERE title IN ($1,$2)`, title1, title2)
		_, _ = d.Exec(`DELETE FROM settings WHERE key=$1`, flag)
	}
	cleanup()
	t.Cleanup(cleanup)

	if err := d.SeedKBPack(p); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM kb_entries WHERE title IN ($1,$2)`, title1, title2).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("首次播种应有 2 条，实际 %d", n)
	}
	// 用户删除一条后重播同一版本：flag=done，不得复活。
	var id int64
	if err := d.QueryRow(`SELECT id FROM kb_entries WHERE title=$1`, title1).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Delete(id); err != nil {
		t.Fatal(err)
	}
	if err := d.SeedKBPack(p); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM kb_entries WHERE title IN ($1,$2)`, title1, title2).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("同版本重播不应复活被删条目，实际 %d", n)
	}
}

// 启动播种入口：扫描 skills/*/kb-packs/* 并按 id+version 幂等落库。
func TestSeedKBPacksFromShippedSkills(t *testing.T) {
	d := openKBTestDB(t)
	root := filepath.Join("..", "skills")
	seeded, skipped, errs := d.SeedKBPacks(root)
	if len(errs) > 0 {
		t.Fatalf("播种失败: %v", errs)
	}
	if seeded+skipped < 3 {
		t.Fatalf("应发现至少 3 个包，seeded=%d skipped=%d", seeded, skipped)
	}
	// 第二次必须全部跳过（同 id+version flag），并且不复活被用户删除的条目。
	seeded2, skipped2, errs2 := d.SeedKBPacks(root)
	if len(errs2) > 0 {
		t.Fatalf("二次播种失败: %v", errs2)
	}
	if seeded2 != 0 || skipped2 < 3 {
		t.Fatalf("二次播种应全部跳过: seeded=%d skipped=%d", seeded2, skipped2)
	}
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM kb_entries
		WHERE origin='seed' AND status='approved'
		  AND title IN ('JWT 测试：alg/none、kid 注入与弱密钥','AWS IMDS 凭据链验证')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("随包条目未以 approved 落库: %d", n)
	}
}

// 外部包导入默认 draft：manifest 自填 approved 也不能绕过人工审核。
func TestKBPackExternalStatusOverride(t *testing.T) {
	dir := t.TempDir()
	body := `{"title":"外部条目","description":"x","detail":"适用：x。验证：x。误报：x。","commands":["echo x"],"source":"https://example.com"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "entries.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := `{"id":"ext","name":"ext","version":"1.0.0","schema":2,"license":"CC0-1.0","review_status":"approved","sources":["https://example.com"],"entry_count":1}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := LoadKBPack(dir)
	if err != nil {
		t.Fatal(err)
	}
	if p.Entries[0].Status != KBStatusApproved {
		t.Fatalf("随包 manifest approved 应映射为 approved")
	}
	if err := SetKBPackStatus(p, KBStatusDraft); err != nil {
		t.Fatal(err)
	}
	if p.Entries[0].Status != KBStatusDraft {
		t.Fatalf("外部导入默认应覆盖为 draft")
	}
}
