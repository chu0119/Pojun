package vulnintel

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseEPSS(t *testing.T) {
	raw := `#model_version:v2025.03.14,score_date:2026-11-23T00:00:00Z
cve,epss,percentile
CVE-2021-44228,0.99999,1.00000
CVE-2024-0001,0.12,0.45
not-a-cve,0.1,0.2
CVE-2024-BAD,bad,0.1
`
	scores, err := ParseEPSS(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 2 {
		t.Fatalf("got %d scores, want 2: %+v", len(scores), scores)
	}
	if scores[0].CVE != "CVE-2021-44228" || scores[0].Score != 0.99999 || scores[0].Percentile != 1 {
		t.Fatalf("first=%+v", scores[0])
	}
	if scores[0].Date.IsZero() {
		t.Fatal("score_date not parsed")
	}
	if got := scores[0].Date.UTC().Format("2006-01-02"); got != "2026-11-23" {
		t.Fatalf("score_date=%s", got)
	}
}

func TestParseEPSSWithoutHeader(t *testing.T) {
	raw := "CVE-2024-0002,0.5,0.6\n"
	scores, err := ParseEPSS(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 1 || scores[0].CVE != "CVE-2024-0002" {
		t.Fatalf("scores=%+v", scores)
	}
}

func TestParseEPSSFromGzip(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte("#model_version:v1,score_date:2026-11-23T00:00:00Z\ncve,epss,percentile\nCVE-2024-0003,0.2,0.3\n")); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	plain := []byte("CVE-2024-0004,0.4,0.5\n")
	r, err := maybeGunzip(bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseEPSS(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].CVE != "CVE-2024-0004" {
		t.Fatalf("plain body not parsed: %+v", got)
	}
	r2, err := maybeGunzip(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	got2, err := ParseEPSS(r2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got2) != 1 || got2[0].CVE != "CVE-2024-0003" {
		t.Fatalf("gzip body not parsed: %+v", got2)
	}
}

// NaN/Inf 能通过 strconv.ParseFloat，但 encoding/json 无法编码，批量 upsert 会
// 整批失败并丢掉后面的所有行。解析阶段就要跳过这些行。
func TestParseEPSSSkipsNonFinite(t *testing.T) {
	raw := `#model_version:v2025.03.14,score_date:2026-11-23T00:00:00Z
cve,epss,percentile
CVE-2024-0001,NaN,0.5
CVE-2024-0002,0.4,Inf
CVE-2024-0003,-Inf,0.5
CVE-2024-0004,0.25,0.75
`
	scores, err := ParseEPSS(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 1 || scores[0].CVE != "CVE-2024-0004" {
		t.Fatalf("非有限值未被跳过：%+v", scores)
	}
	if _, err := json.Marshal([]EPSSScore{{CVE: "x", Score: math.NaN()}}); err == nil {
		t.Fatal("前提不成立：NaN 应该无法 JSON 编码")
	}
}

func TestUpsertEPSSUpdatesExistingCVE(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	id := "CVE-9000-EPSS"
	t.Cleanup(func() { _, _ = st.db.Exec(`DELETE FROM vuln_cves WHERE cve_id=$1`, id) })
	if err := st.UpsertCVE(ctx, cve(id, "acme", "widget", "1.0", 7.5)); err != nil {
		t.Fatal(err)
	}
	n, err := st.UpsertEPSS(ctx, []EPSSScore{{CVE: id, Score: 0.42, Percentile: 0.88, Date: time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("updated=%d, want 1", n)
	}
	d, err := st.GetVuln(ctx, id)
	if err != nil || d == nil {
		t.Fatalf("get vuln: %v %v", d, err)
	}
	if d.EPSS == nil || *d.EPSS < 0.419 || *d.EPSS > 0.421 ||
		d.EPSSPercentile == nil || *d.EPSSPercentile < 0.879 || *d.EPSSPercentile > 0.881 {
		t.Fatalf("epss=%v pct=%v", d.EPSS, d.EPSSPercentile)
	}
	if d.EPSSDate != "2026-11-23" {
		t.Fatalf("epss_date=%q", d.EPSSDate)
	}

	hits, err := st.SearchVulns(ctx, "", "widget", "1.0", 10)
	if err != nil {
		t.Fatal(err)
	}
	var hit *VulnHit
	for i := range hits {
		if hits[i].CVEID == id {
			hit = &hits[i]
		}
	}
	if hit == nil {
		t.Fatal("search did not return EPSS test CVE")
	}
	if hit.EPSS == nil || hit.EPSSDate != "2026-11-23" {
		t.Fatalf("hit=%+v", hit)
	}
}

func TestUpsertEPSSSkipsUnknownCVE(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	n, err := st.UpsertEPSS(ctx, []EPSSScore{{CVE: "CVE-9000-EPSS-MISSING", Score: 0.5, Percentile: 0.5}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("不存在的 CVE 不应被插入，updated=%d", n)
	}
}

// 批量路径（json_to_recordset）必须与逐条更新语义一致：空日期保留原值。
func TestUpsertEPSSBatchPreservesDate(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	ids := []string{"CVE-9000-EPSS-A", "CVE-9000-EPSS-B"}
	for _, id := range ids {
		if err := st.UpsertCVE(ctx, cve(id, "acme", "widget", "1.0", 7.5)); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = st.db.Exec(`DELETE FROM vuln_cves WHERE cve_id=$1`, id)
		}
	})
	first := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	if _, err := st.UpsertEPSS(ctx, []EPSSScore{
		{CVE: ids[0], Score: 0.1, Percentile: 0.2, Date: first},
		{CVE: ids[1], Score: 0.3, Percentile: 0.4, Date: first},
	}); err != nil {
		t.Fatal(err)
	}
	n, err := st.UpsertEPSS(ctx, []EPSSScore{{CVE: ids[0], Score: 0.7, Percentile: 0.8}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("updated=%d, want 1", n)
	}
	d, err := st.GetVuln(ctx, ids[0])
	if err != nil || d == nil {
		t.Fatalf("get vuln: %v %v", d, err)
	}
	if d.EPSS == nil || *d.EPSS < 0.699 || *d.EPSS > 0.701 {
		t.Fatalf("epss=%v", d.EPSS)
	}
	var gotDate time.Time
	if err := st.db.QueryRow(`SELECT epss_date FROM vuln_cves WHERE cve_id=$1`, ids[0]).Scan(&gotDate); err != nil {
		t.Fatal(err)
	}
	if !gotDate.Equal(first) {
		t.Fatalf("epss_date=%s, want %s（空日期不应清空原值）", gotDate, first)
	}
}

// 跨批（>5000）时每条 CVE 都要命中，且只更新存在的行。
func TestUpsertEPSSBulkAcrossBatches(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	const total = 5001
	if _, err := st.db.Exec(`INSERT INTO vuln_cves(cve_id) SELECT 'CVE-9000-EPSSBULK-'||i FROM generate_series(1,$1) i`, total); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = st.db.Exec(`DELETE FROM vuln_cves WHERE cve_id LIKE 'CVE-9000-EPSSBULK-%'`) })
	scores := make([]EPSSScore, 0, total+1)
	for i := 1; i <= total; i++ {
		scores = append(scores, EPSSScore{CVE: "CVE-9000-EPSSBULK-" + strconv.Itoa(i), Score: 0.5, Percentile: 0.5})
	}
	scores = append(scores, EPSSScore{CVE: "CVE-9000-EPSSBULK-MISSING", Score: 0.5, Percentile: 0.5})
	n, err := st.UpsertEPSS(ctx, scores)
	if err != nil {
		t.Fatal(err)
	}
	if n != total {
		t.Fatalf("updated=%d, want %d", n, total)
	}
	var got int
	if err := st.db.QueryRow(`SELECT count(*) FROM vuln_cves WHERE cve_id LIKE 'CVE-9000-EPSSBULK-%' AND epss=0.5`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != total {
		t.Fatalf("rows with epss=%d, want %d", got, total)
	}
}

// 排序：KEV > EPSS > 版本区间命中 > CVSS。版本区间只在 EPSS 同为 NULL 时生效。
func TestSearchVulnsEPSSRanking(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	ids := []string{
		"CVE-9000-RANK-KEV", "CVE-9000-RANK-EPSS-HI", "CVE-9000-RANK-EPSS-LO",
		"CVE-9000-RANK-VER", "CVE-9000-RANK-CVSS",
	}
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = st.db.Exec(`DELETE FROM vuln_cves WHERE cve_id=$1`, id)
			_, _ = st.db.Exec(`DELETE FROM vuln_cpe_products WHERE cve_id=$1`, id)
		}
	})
	for _, id := range ids[:4] {
		if err := st.UpsertCVE(ctx, cve(id, "acme", "rankwidget", "1.0", 5)); err != nil {
			t.Fatal(err)
		}
	}
	// 最后一条用不同版本：版本号不在 criteria 里，用于验证「版本命中」优先于 CVSS。
	if err := st.UpsertCVE(ctx, cve(ids[4], "acme", "rankwidget", "2.0", 9.8)); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkKEV(ctx, ids[0], time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	// KEV 行同时被最小 CPE 索引行覆盖，确保加入同一检索池。
	if _, err := st.db.Exec(`UPDATE vuln_cves SET cvss_score=1 WHERE cve_id=$1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertEPSS(ctx, []EPSSScore{
		{CVE: ids[1], Score: 0.95, Percentile: 0.99},
		{CVE: ids[2], Score: 0.05, Percentile: 0.1},
	}); err != nil {
		t.Fatal(err)
	}
	// ver/cvss 两条无 EPSS，但都命中版本字符串；cvss 一条 CVSS 更高、ver 一条
	// 版本区间命中。版本命中应排在纯 CVSS 之前。
	if _, err := st.db.Exec(`UPDATE vuln_cves SET cvss_score=9.8 WHERE cve_id=$1`, ids[4]); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE vuln_cves SET cvss_score=4 WHERE cve_id=$1`, ids[3]); err != nil {
		t.Fatal(err)
	}

	hits, err := st.SearchVulns(ctx, "acme", "rankwidget", "1.0", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 5 {
		t.Fatalf("expected 5 hits, got %d: %+v", len(hits), hits)
	}
	order := make([]string, 0, len(hits))
	for _, h := range hits {
		order = append(order, h.CVEID)
	}
	pos := func(id string) int {
		for i, v := range order {
			if v == id {
				return i
			}
		}
		return -1
	}
	if pos(ids[0]) != 0 {
		t.Fatalf("KEV 应排第一: %v", order)
	}
	if pos(ids[1]) > pos(ids[2]) {
		t.Fatalf("高 EPSS 应排在低 EPSS 之前: %v", order)
	}
	if pos(ids[2]) > pos(ids[3]) || pos(ids[2]) > pos(ids[4]) {
		t.Fatalf("有 EPSS 的应排在无 EPSS 之前: %v", order)
	}
	if pos(ids[3]) > pos(ids[4]) {
		t.Fatalf("版本命中应排在纯 CVSS 之前: %v", order)
	}
}

func TestEPSSStatusCoverage(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	id := "CVE-9000-EPSSCOV"
	t.Cleanup(func() { _, _ = st.db.Exec(`DELETE FROM vuln_cves WHERE cve_id=$1`, id) })
	if err := st.UpsertCVE(ctx, cve(id, "acme", "covwidget", "1.0", 5)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertEPSS(ctx, []EPSSScore{{CVE: id, Score: 0.3, Percentile: 0.4}}); err != nil {
		t.Fatal(err)
	}
	snap := st.StatusSnapshot(ctx)
	counts, _ := snap["counts"].(map[string]int64)
	if counts["epss"] < 1 {
		t.Fatalf("counts epss=%d", counts["epss"])
	}
	cov, ok := snap["coverage"].(map[string]any)
	if !ok {
		t.Fatalf("coverage missing: %+v", snap)
	}
	epss, ok := cov["epss"].(map[string]any)
	if !ok {
		t.Fatalf("coverage.epss missing: %+v", cov)
	}
	scored, _ := epss["scored"].(int64)
	total, _ := epss["total"].(int64)
	pct, _ := epss["pct"].(float64)
	if scored < 1 || total < scored || pct <= 0 || pct > 100 {
		t.Fatalf("bad coverage: %+v", epss)
	}
}

// syncEPSS 的端到端路径：本地 httptest 返回 gzip CSV，验证 URL 覆盖、解压、
// 落库与状态水位；失败路径必须写 last_error 而不是伪成功。
func TestSyncEPSSHTTP(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	id := "CVE-9000-EPSSHTTP"
	t.Cleanup(func() {
		_, _ = st.db.Exec(`DELETE FROM vuln_cves WHERE cve_id=$1`, id)
		_, _ = st.db.Exec(`DELETE FROM vuln_sync_state WHERE source='epss'`)
	})
	if err := st.UpsertCVE(ctx, cve(id, "acme", "httpwidget", "1.0", 6)); err != nil {
		t.Fatal(err)
	}
	body := "#model_version:v2025.03.14,score_date:2026-11-23T00:00:00Z\ncve,epss,percentile\n" + id + ",0.66,0.77\n"
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	if err := syncEPSS(ctx, st, Config{EPSSURL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	d, err := st.GetVuln(ctx, id)
	if err != nil || d == nil || d.EPSS == nil || *d.EPSS < 0.659 || *d.EPSS > 0.661 {
		t.Fatalf("detail=%+v err=%v", d, err)
	}
	if d.EPSSDate != "2026-11-23" {
		t.Fatalf("epss_date=%q", d.EPSSDate)
	}
	state, err := st.GetSyncState(ctx, "epss")
	if err != nil {
		t.Fatal(err)
	}
	if state.LastSyncAt == nil || state.TotalSynced != 1 || state.LastError != "" {
		t.Fatalf("state=%+v", state)
	}
}

func TestSyncEPSSHTTPFailureRecordsError(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	t.Cleanup(func() { _, _ = st.db.Exec(`DELETE FROM vuln_sync_state WHERE source='epss'`) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := syncEPSS(ctx, st, Config{EPSSURL: srv.URL})
	if err == nil {
		t.Fatal("expected error on HTTP 500")
	}
	state, gerr := st.GetSyncState(ctx, "epss")
	if gerr != nil {
		t.Fatal(gerr)
	}
	if !strings.Contains(state.LastError, "epss status 500") {
		t.Fatalf("last_error=%q", state.LastError)
	}
	if state.LastSyncAt != nil {
		t.Fatalf("失败不应推进 last_sync_at: %v", state.LastSyncAt)
	}
}

func TestSyncEPSSEmptyFeedRecordsError(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	t.Cleanup(func() { _, _ = st.db.Exec(`DELETE FROM vuln_sync_state WHERE source='epss'`) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("cve,epss,percentile\n"))
	}))
	defer srv.Close()

	if err := syncEPSS(ctx, st, Config{EPSSURL: srv.URL}); err != nil {
		t.Fatalf("空 feed 本身不算传输错误: %v", err)
	}
	state, err := st.GetSyncState(ctx, "epss")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(state.LastError, "没有有效记录") {
		t.Fatalf("空 feed 必须标记错误，last_error=%q", state.LastError)
	}
}

// 回归：json_to_recordset 的列类型必须与 float64 精度匹配（早期版本用 real，
// 0.123456789 会被截断到单精度）。这里只断言往返误差在可接受范围内。
func TestUpsertEPSSPrecision(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	id := "CVE-9000-EPSSPREC"
	t.Cleanup(func() { _, _ = st.db.Exec(`DELETE FROM vuln_cves WHERE cve_id=$1`, id) })
	if err := st.UpsertCVE(ctx, cve(id, "acme", "precwidget", "1.0", 5)); err != nil {
		t.Fatal(err)
	}
	const want = 0.123456789
	if _, err := st.UpsertEPSS(ctx, []EPSSScore{{CVE: id, Score: want, Percentile: 0.987654321}}); err != nil {
		t.Fatal(err)
	}
	d, err := st.GetVuln(ctx, id)
	if err != nil || d == nil || d.EPSS == nil {
		t.Fatalf("detail=%+v err=%v", d, err)
	}
	if fmt.Sprintf("%.9f", *d.EPSS) != fmt.Sprintf("%.9f", want) {
		t.Fatalf("epss=%v want %v", *d.EPSS, want)
	}
}
