package traffic

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mproxy "github.com/lqqyt2423/go-mitmproxy/proxy"
)

func withStatus(code int) flowOpt {
	return func(f *mproxy.Flow) { f.Response.StatusCode = code }
}

func withMethod(method string) flowOpt {
	return func(f *mproxy.Flow) { f.Request.Method = method }
}

func idscanDBPath(dir string) string { return filepath.Join(dir, "_index", "index.sqlite") }

func idscanRun(t *testing.T, dir string, opt IdentityScanOptions) *IdentityReport {
	t.Helper()
	opt.DBPath = idscanDBPath(dir)
	opt.TrafficDir = dir
	if opt.Now == nil {
		opt.Now = func() time.Time { return time.Unix(1700000000, 0) }
	}
	rep, err := ScanIdentity(context.Background(), opt)
	if err != nil {
		t.Fatalf("ScanIdentity: %v", err)
	}
	return rep
}

func TestIdentityBuiltinLexiconValid(t *testing.T) {
	l := BuiltinLexicon()
	if err := l.validate(); err != nil {
		t.Fatalf("内置词表不合法：%v", err)
	}
	for _, term := range l.Terms {
		if len(term.folded) == 0 {
			t.Fatalf("词条 %s 未生成折叠 pattern", term.ID)
		}
	}
}

func TestIdentityOpenReadOnlyMissingFileNoCreate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "missing.sqlite")
	if _, err := OpenIndexReadOnly(p); err == nil {
		t.Fatal("打开不存在的索引应报错")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("只读打开不应创建文件：%v", err)
	}
}

func TestIdentityOpenReadOnlyRejectsWrites(t *testing.T) {
	tr, dir := openTraffic(t)
	tr.record(newFlow("a.example.com", "GET", "/", nil, []byte("ok")))
	tr.Close()

	db, err := OpenIndexReadOnly(idscanDBPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var q int
	if err := db.QueryRow(`PRAGMA query_only`).Scan(&q); err != nil {
		t.Fatal(err)
	}
	if q != 1 {
		t.Fatalf("query_only=%d，期望 1", q)
	}
	if _, err := db.Exec(`CREATE TABLE idscan_probe(x)`); err == nil {
		t.Fatal("只读连接不应允许 CREATE TABLE")
	} else if !strings.Contains(strings.ToLower(err.Error()), "readonly") {
		t.Fatalf("写拒绝错误不像 SQLITE_READONLY：%v", err)
	}
}

func TestIdentityScanLeavesIndexUntouched(t *testing.T) {
	tr, dir := openTraffic(t)
	tr.record(newFlow("a.example.com", "GET", "/x", nil, []byte("pojun")))
	if _, err := tr.DB().Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	tr.Close()

	snapshot := filepath.Join(t.TempDir(), "index.sqlite")
	raw, err := os.ReadFile(idscanDBPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(snapshot, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	beforeVersion := identityDataVersion(t, snapshot)

	rep, err := ScanIdentity(context.Background(), IdentityScanOptions{
		DBPath: snapshot, TrafficDir: filepath.Dir(snapshot), Tiers: []string{"A"},
		Now: func() time.Time { return time.Unix(1700000000, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Scan.Scanned != 1 {
		t.Fatalf("scanned=%d", rep.Scan.Scanned)
	}
	after, err := os.Stat(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() {
		t.Fatalf("index.sqlite 大小变化：%d → %d", before.Size(), after.Size())
	}
	if afterVersion := identityDataVersion(t, snapshot); afterVersion != beforeVersion {
		t.Fatalf("data_version 变化：%d → %d", beforeVersion, afterVersion)
	}
	entries, err := os.ReadDir(filepath.Dir(snapshot))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "index.sqlite" && e.Name() != "index.sqlite-wal" && e.Name() != "index.sqlite-shm" {
			t.Fatalf("扫描在快照目录新建了 %q", e.Name())
		}
		if e.IsDir() {
			t.Fatalf("扫描新建了目录 %q", e.Name())
		}
		// -shm/-wal are SQLite's shared-memory sidecars for a WAL database; a
		// read-only connection may create them (typically a 32 KiB -shm). The
		// guarantee under test is that index.sqlite itself is not modified.
	}
}

func identityDataVersion(t *testing.T, path string) int64 {
	t.Helper()
	db, err := OpenIndexReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int64
	if err := db.QueryRow(`PRAGMA data_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestIdentityCanaryHits(t *testing.T) {
	tr, dir := openTraffic(t)
	tr.record(newFlow("a.example.com", "GET", "/a", nil, []byte(`{"note":"pojun"}`)))
	tr.record(newFlow("b.example.com", "GET", "/b", nil, []byte(`traffic_search bind_finding_traffic`)))
	tr.record(newFlow("c.example.com", "GET", "/c", nil, []byte(`sqlmap/1.8`)))
	tr.Close()

	rep := idscanRun(t, dir, IdentityScanOptions{})
	if rep.FindingsTotal < 2 {
		t.Fatalf("findings=%d，期望 A 命中 + B 共现", rep.FindingsTotal)
	}
	if rep.Lists["A"].Hits == 0 || rep.Lists["B"].Hits < 2 {
		t.Fatalf("A/B 计数异常：%+v", rep.Lists)
	}
	if rep.Lists["C"].Hits != 0 {
		t.Fatalf("默认 tiers 不应统计 C：%d", rep.Lists["C"].Hits)
	}

	repC := idscanRun(t, dir, IdentityScanOptions{Tiers: []string{"A", "B", "C"}})
	if repC.Lists["C"].Hits == 0 {
		t.Fatal("显式启用 C 后应统计 sqlmap/")
	}
	if repC.Lists["C"].Findings != 0 {
		t.Fatal("C 不应产生 finding")
	}
}

func TestIdentityCleanTrafficZeroFindings(t *testing.T) {
	tr, dir := openTraffic(t)
	tr.record(newFlow("clean.example.com", "GET", "/1", nil, []byte("pojunk Pojunior")))
	tr.record(newFlow("clean.example.com", "GET", "/2", nil, []byte(`{"ua":"Mozilla/5.0 Chrome/131.0.0.0"}`)))
	tr.record(newFlow("clean.example.com", "GET", "/3", nil, []byte(`single traffic_search mention`)))
	tr.Close()

	rep := idscanRun(t, dir, IdentityScanOptions{})
	if rep.FindingsTotal != 0 {
		t.Fatalf("正常流量产生 %d 个 finding：%+v", rep.FindingsTotal, rep.Findings)
	}
	if rep.Lists["A"].Hits != 0 {
		t.Fatalf("pojunk/Pojunior 误命中 A：%d", rep.Lists["A"].Hits)
	}
	if rep.Lists["B"].Hits != 1 {
		t.Fatalf("单个 B 应只记 1 次命中，实际 %d", rep.Lists["B"].Hits)
	}
}

func TestIdentityBCooccurrencePromotes(t *testing.T) {
	tr, dir := openTraffic(t)
	tr.record(newFlow("b.example.com", "GET", "/one", nil, []byte(`traffic_search`)))
	tr.record(newFlow("b.example.com", "GET", "/two", nil, []byte(`traffic_search bind_finding_traffic`)))
	tr.Close()

	rep := idscanRun(t, dir, IdentityScanOptions{})
	if rep.FindingsTotal != 1 {
		t.Fatalf("findings=%d，期望仅共现那条升级", rep.FindingsTotal)
	}
	if rep.Findings[0].List != "B" || len(rep.Findings[0].Terms) != 2 {
		t.Fatalf("B finding 结构异常：%+v", rep.Findings[0])
	}
}

func TestIdentityCoverageClosure(t *testing.T) {
	tr, dir := openTraffic(t)
	tr.record(newFlow("cov.example.com", "GET", "/body", nil, []byte("pojun")))
	tr.record(newFlow("cov.example.com", "GET", "/empty", nil, nil))
	tr.record(newFlow("cov.example.com", "GET", "/204", nil, nil, withStatus(204)))
	tr.record(newFlow("cov.example.com", "GET", "/304", nil, nil, withStatus(304)))
	tr.record(newFlow("cov.example.com", "HEAD", "/head", nil, nil, withMethod("HEAD")))
	tr.record(newFlow("cov.example.com", "GET", "/failed", nil, nil, withStatus(0)))
	if _, err := tr.DB().Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
		VALUES('legacy-1',1700000000,'legacy.example.com','GET','/','http://legacy.example.com/',200,'text/html',0,0,'legacy.example.com')`); err != nil {
		t.Fatal(err)
	}
	tr.Close()

	rep := idscanRun(t, dir, IdentityScanOptions{})
	cov := rep.Coverage
	if cov.Total != 7 {
		t.Fatalf("total=%d", cov.Total)
	}
	if cov.Eligible != 2 {
		t.Fatalf("eligible=%d，期望 2", cov.Eligible)
	}
	if cov.WithBody != 1 {
		t.Fatalf("with_body=%d，期望 1", cov.WithBody)
	}
	if cov.LegacyRows != 1 {
		t.Fatalf("legacy_rows=%d", cov.LegacyRows)
	}
	if cov.Excluded.HEAD != 1 || cov.Excluded.NoContent != 1 || cov.Excluded.NotModified304 != 1 || cov.Excluded.FailedStatus0 != 1 {
		t.Fatalf("排除计数异常：%+v", cov.Excluded)
	}
	if cov.CaptureCoverage != 0.5 {
		t.Fatalf("capture_coverage=%v，期望 0.5", cov.CaptureCoverage)
	}
	for _, w := range rep.Warnings {
		if strings.Contains(w, "未闭合") {
			t.Fatalf("覆盖率未闭合：%s", w)
		}
	}
}

func TestIdentityBlobPrecedenceAndDeepMarker(t *testing.T) {
	tr, dir := openTraffic(t)
	big := append([]byte(strings.Repeat("x", 100*1024)), []byte(" pojun ")...)
	big = append(big, []byte(strings.Repeat("y", 200*1024))...)
	tr.record(newFlow("blob.example.com", "GET", "/big", nil, big))
	id := onlyExchangeID(t, tr)
	var inline []byte
	var blob string
	if err := tr.DB().QueryRow(`SELECT resp_body, COALESCE(resp_blob,'') FROM exchange_bodies WHERE id=?`, id).Scan(&inline, &blob); err != nil {
		t.Fatal(err)
	}
	if blob == "" {
		t.Fatal("大正文应 spill 到 blob")
	}
	if strings.Contains(string(inline), "pojun") {
		t.Fatal("inline 预览不应包含 100KiB 处的 marker")
	}
	tr.Close()

	rep := idscanRun(t, dir, IdentityScanOptions{})
	if rep.Lists["A"].Hits != 1 {
		t.Fatalf("A 命中 %d，期望通过 blob 命中 1 次", rep.Lists["A"].Hits)
	}
	if len(rep.Findings) != 1 || rep.Findings[0].Where != "resp_body" {
		t.Fatalf("finding 位置异常：%+v", rep.Findings)
	}
	if rep.Scan.BlobMissing != 0 {
		t.Fatalf("blob 不应缺失：%d", rep.Scan.BlobMissing)
	}
}

func TestIdentityMaxBodyBytesTruncation(t *testing.T) {
	tr, dir := openTraffic(t)
	big := append([]byte(strings.Repeat("x", 100*1024)), []byte(" pojun ")...)
	big = append(big, []byte(strings.Repeat("y", 200*1024))...)
	tr.record(newFlow("trunc.example.com", "GET", "/big", nil, big))
	tr.Close()

	rep := idscanRun(t, dir, IdentityScanOptions{MaxBodyBytes: 64 << 10})
	if rep.Lists["A"].Hits != 0 {
		t.Fatalf("截断区内的 marker 不应命中：%d", rep.Lists["A"].Hits)
	}
	if rep.Coverage.BodyTruncated != 1 {
		t.Fatalf("body_truncated=%d", rep.Coverage.BodyTruncated)
	}
	if rep.Coverage.ScanCoverage != 0 {
		t.Fatalf("scan_coverage=%v，期望 0", rep.Coverage.ScanCoverage)
	}
}

func TestIdentityBinaryBodySkipped(t *testing.T) {
	tr, dir := openTraffic(t)
	tr.record(newFlow("bin.example.com", "GET", "/b", nil, []byte("pojun\x00traffic_search"), withRespType("application/octet-stream")))
	tr.Close()

	rep := idscanRun(t, dir, IdentityScanOptions{Tiers: []string{"A", "B", "C"}})
	if rep.Scan.BinarySkipped != 1 {
		t.Fatalf("binary_skipped=%d", rep.Scan.BinarySkipped)
	}
	if rep.Lists["A"].Hits != 0 || rep.Lists["B"].Hits != 0 {
		t.Fatalf("二进制正文不应命中：%+v", rep.Lists)
	}
}

func TestIdentityCaseFoldAndDuplicateTerms(t *testing.T) {
	dup := []byte(`{"version":"t","mode":"replace","terms":[
		{"id":"a","list":"A","pattern":"pojun","boundary":true},
		{"id":"b","list":"A","pattern":"POJUN"}]}`)
	if _, err := parseLexicon(dup); err == nil {
		t.Fatal("大小写折叠后的重复 pattern 应被拒绝")
	}

	lex, err := parseLexicon([]byte(`{"version":"t","mode":"replace","terms":[
		{"id":"A-test","list":"A","pattern":"pojun","boundary":true}]}`))
	if err != nil {
		t.Fatal(err)
	}
	tr, dir := openTraffic(t)
	tr.record(newFlow("case.example.com", "GET", "/1", nil, []byte("POJUN_WORKER_CHAT")))
	tr.record(newFlow("case.example.com", "GET", "/2", nil, []byte("pojunk")))
	tr.record(newFlow("case.example.com", "GET", "/3", nil, []byte("Pojunior")))
	tr.Close()

	rep := idscanRun(t, dir, IdentityScanOptions{Lexicon: lex})
	if rep.Lists["A"].Hits != 1 {
		t.Fatalf("A 命中 %d，期望仅 POJUN_WORKER_CHAT 1 次", rep.Lists["A"].Hits)
	}
}

func TestIdentityRedaction(t *testing.T) {
	lex, err := parseLexicon([]byte(`{"version":"t","mode":"replace","terms":[
		{"id":"A-test","list":"A","pattern":"canarybrandxyz","boundary":true}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body := "Authorization: Bearer supersecretvalue123\n" +
		"note=canarybrandxyz\n" +
		"password=hunter2\n" +
		"mail=user@example.com\n" +
		"jwt=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U\n"
	tr, dir := openTraffic(t)
	tr.record(newFlow("redact.example.com", "POST", "/r", []byte(body), []byte(body)))
	tr.Close()

	rep := idscanRun(t, dir, IdentityScanOptions{Lexicon: lex})
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, secret := range []string{"supersecretvalue123", "hunter2", "user@example.com", "eyJhbGciOiJIUzI1NiJ9", "canarybrandxyz"} {
		if strings.Contains(text, secret) {
			t.Fatalf("报告泄露了 %q", secret)
		}
	}
	if !strings.Contains(text, "A-test") {
		t.Fatalf("报告应保留 term id 占位：%s", text)
	}
}

func TestIdentityRenderSampleMasksSecrets(t *testing.T) {
	text := "Authorization: Bearer SECRETVALUE123\n" +
		"Cookie: sid=abc\n" +
		"password=hunter2\n" +
		"mail=user@example.com\n" +
		"jwt=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U\n" +
		"url=http://user:pass@example.com/x\n" +
		`{"token":"jsonsecret123"}`
	masked := maskIdentitySample(text)
	for _, secret := range []string{"SECRETVALUE123", "sid=abc", "hunter2", "user@example.com", "eyJhbGciOiJIUzI1NiJ9", "user:pass@", "jsonsecret123"} {
		if strings.Contains(masked, secret) {
			t.Fatalf("脱敏后仍包含 %q：%s", secret, masked)
		}
	}
	short := "prefix MARKERCANARY1999 suffix"
	idx := strings.Index(short, "MARKERCANARY1999")
	sample := renderIdentitySample(short, idx, idx+len("MARKERCANARY1999"), "A-test")
	if !strings.Contains(sample, "<A-test>") || strings.Contains(sample, "MARKERCANARY1999") {
		t.Fatalf("命中段未被占位替换：%s", sample)
	}
}

func TestIdentityTaskMapAggregation(t *testing.T) {
	tr, dir := openTraffic(t)
	tr.record(newFlow("a.example.com", "GET", "/a", nil, []byte("pojun")))
	tr.record(newFlow("b.example.com", "GET", "/b", nil, []byte("pojun")))
	tr.record(newFlow("c.example.com", "GET", "/c", nil, []byte("pojun")))
	tr.Close()

	rep := idscanRun(t, dir, IdentityScanOptions{
		Tiers: []string{"A"},
		TaskMap: map[string][]int64{
			"a.example.com": {1, 2},
			"b.example.com": {2},
		},
	})
	if len(rep.Tasks) != 2 {
		t.Fatalf("tasks=%d", len(rep.Tasks))
	}
	if rep.Tasks[0].TaskID != 1 || rep.Tasks[0].Hits["A"] != 1 || !rep.Tasks[0].Shared {
		t.Fatalf("task 1 异常：%+v", rep.Tasks[0])
	}
	if rep.Tasks[1].TaskID != 2 || rep.Tasks[1].Hits["A"] != 2 || !rep.Tasks[1].Shared || len(rep.Tasks[1].Hosts) != 2 {
		t.Fatalf("task 2 异常：%+v", rep.Tasks[1])
	}
	if len(rep.UnmappedHosts) != 1 || rep.UnmappedHosts[0] != "c.example.com" {
		t.Fatalf("unmapped_hosts=%v", rep.UnmappedHosts)
	}
}

func TestIdentityKeysetPaginationAndLimit(t *testing.T) {
	tr, dir := openTraffic(t)
	for i := 0; i < 25; i++ {
		tr.record(newFlow("page.example.com", "GET", "/"+string(rune('a'+i)), nil, []byte("pojun")))
	}
	tr.Close()

	rep := idscanRun(t, dir, IdentityScanOptions{Tiers: []string{"A"}, Batch: 7})
	if rep.Scan.Scanned != 25 || rep.Scan.Batches != 4 {
		t.Fatalf("scanned=%d batches=%d", rep.Scan.Scanned, rep.Scan.Batches)
	}
	if rep.Scan.Truncated {
		t.Fatal("未设 limit 不应 truncated")
	}
	if rep.Lists["A"].Hits != 25 {
		t.Fatalf("A hits=%d，期望 25（每行恰好一次）", rep.Lists["A"].Hits)
	}

	repLimit := idscanRun(t, dir, IdentityScanOptions{Tiers: []string{"A"}, Batch: 7, Limit: 10})
	if repLimit.Scan.Scanned != 10 || !repLimit.Scan.Truncated {
		t.Fatalf("limit: scanned=%d truncated=%v", repLimit.Scan.Scanned, repLimit.Scan.Truncated)
	}
	if repLimit.Coverage.Total != 10 {
		t.Fatalf("limit coverage total=%d", repLimit.Coverage.Total)
	}
	if repLimit.Lists["A"].Hits != 10 {
		t.Fatalf("limit A hits=%d", repLimit.Lists["A"].Hits)
	}
}

func TestIdentityResolvePaths(t *testing.T) {
	db, td, err := identityResolvePaths(IdentityScanOptions{DataDir: filepath.Join("tmp", "data")})
	if err != nil {
		t.Fatal(err)
	}
	wantDB := filepath.Join("tmp", "data", "traffic", "_index", "index.sqlite")
	if !strings.HasSuffix(db, wantDB) {
		t.Fatalf("db=%s，期望以 %s 结尾", db, wantDB)
	}
	if !strings.HasSuffix(td, filepath.Join("tmp", "data", "traffic")) {
		t.Fatalf("trafficDir=%s", td)
	}

	db2, td2, err := identityResolvePaths(IdentityScanOptions{DBPath: filepath.Join("tmp", "snap", "index.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(db2, filepath.Join("tmp", "snap", "index.sqlite")) {
		t.Fatalf("db2=%s", db2)
	}
	if !strings.HasSuffix(td2, filepath.Join("tmp", "snap")) {
		t.Fatalf("trafficDir2=%s", td2)
	}
}

func TestIdentityAllowlistSuppresses(t *testing.T) {
	tr, dir := openTraffic(t)
	tr.record(newFlow("allow.example.com", "GET", "/", nil, []byte("pojun")))
	tr.Close()

	rep := idscanRun(t, dir, IdentityScanOptions{
		Tiers: []string{"A"},
		Allowlist: &IdentityAllowlist{Rules: []IdentityAllowRule{{
			TermID: "A-brand-pojun", Host: "allow.example.com", Reason: "operator-owned domain",
		}}},
	})
	if rep.Lists["A"].Hits != 0 || rep.FindingsTotal != 0 {
		t.Fatalf("白名单未抑制：%+v findings=%d", rep.Lists, rep.FindingsTotal)
	}
	if rep.Suppressed.ByAllowlist != 1 || len(rep.Suppressed.Rules) != 1 {
		t.Fatalf("抑制记录异常：%+v", rep.Suppressed)
	}
}

func TestIdentityContextCancel(t *testing.T) {
	tr, dir := openTraffic(t)
	tr.record(newFlow("cancel.example.com", "GET", "/", nil, []byte("pojun")))
	tr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ScanIdentity(ctx, IdentityScanOptions{
		DBPath: idscanDBPath(dir), TrafficDir: dir, Tiers: []string{"A"},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v，期望 context.Canceled", err)
	}
}

func FuzzIdentityLexiconParse(f *testing.F) {
	f.Add([]byte(`{"version":"x","terms":[]}`))
	f.Add([]byte(`{"version":"x","mode":"replace","terms":[{"id":"a","list":"A","pattern":"pojun","boundary":true}]}`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, _ = parseLexicon(raw)
	})
}

func BenchmarkIdentityScan(b *testing.B) {
	dir := b.TempDir()
	tr, err := Open(dir, "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	body := []byte(strings.Repeat("normal traffic without markers ", 40))
	for i := 0; i < 500; i++ {
		tr.record(newFlow("bench.example.com", "GET", "/", nil, body))
	}
	tr.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ScanIdentity(context.Background(), IdentityScanOptions{
			DBPath: idscanDBPath(dir), TrafficDir: dir, Tiers: []string{"A", "B"},
		}); err != nil {
			b.Fatal(err)
		}
	}
}
