package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/chu0119/pojun/db"
)

func kbTestServer(t *testing.T) (*Server, *db.DB) {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no database config (%v) — skipping", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { pg.Close() })
	return &Server{m: &Manager{pg: pg}}, pg
}

func TestKBRetrievalStatsDaysBinding(t *testing.T) {
	s, pg := kbTestServer(t)
	// 先保证至少有一条日志，且 days 走文本绑定路径（历史 500 回归）。
	entry, err := pg.KB().Create(db.KBEntryInput{
		Title:  "KBServer stats " + strconv.FormatInt(time.Now().UnixNano(), 10),
		Detail: "适用：API 测试。验证：返回 200。误报：无。",
		Status: db.KBStatusApproved,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.KB().Delete(entry.ID) })
	if _, err := pg.KB().Search(db.KBSearchQuery{
		Query: entry.Title, Limit: 5, Actor: "server-test", TaskID: 123456,
	}); err != nil {
		t.Fatal(err)
	}

	for _, days := range []string{"7", "1", "30", "999", "abc"} {
		req := httptest.NewRequest(http.MethodGet, "/api/knowledge/retrieval-stats?days="+days, nil)
		rec := httptest.NewRecorder()
		s.kbRetrievalStats(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("days=%s: status=%d body=%s", days, rec.Code, rec.Body.String())
		}
		var out struct {
			Days  int     `json:"days"`
			Total int     `json:"total"`
			P95   float64 `json:"p95_latency_ms"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("days=%s: %v", days, err)
		}
		if out.Days <= 0 || out.Total < 1 {
			t.Fatalf("days=%s 返回异常: %+v", days, out)
		}
	}
}

func TestKBListStatsAndFacetsAPI(t *testing.T) {
	s, pg := kbTestServer(t)
	product := "api-test-prod-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	entry, err := pg.KB().Create(db.KBEntryInput{
		Title:       "KBServer list " + strconv.FormatInt(time.Now().UnixNano(), 10),
		Description: "列表与 facets API 测试",
		Detail:      "适用：API 测试。验证：列表可见。误报：无。",
		Status:      db.KBStatusApproved,
		Products:    []string{product},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.KB().Delete(entry.ID) })

	// 列表：q/status/product 过滤。
	req := httptest.NewRequest(http.MethodGet,
		"/api/knowledge?q=API&status=approved&product="+product+"&page=1&page_size=10", nil)
	rec := httptest.NewRecorder()
	s.kbList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", rec.Code, rec.Body.String())
	}
	var list struct {
		Entries []map[string]any `json:"entries"`
		Total   int              `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if list.Total < 1 || len(list.Entries) < 1 {
		t.Fatalf("列表未返回条目: %+v", list)
	}

	// 统计。
	rec = httptest.NewRecorder()
	s.kbStats(rec, httptest.NewRequest(http.MethodGet, "/api/knowledge/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("stats status=%d", rec.Code)
	}

	// facets：product 必须出现在计数里。
	rec = httptest.NewRecorder()
	s.kbFacets(rec, httptest.NewRequest(http.MethodGet, "/api/knowledge/facets?status=approved", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("facets status=%d body=%s", rec.Code, rec.Body.String())
	}
	var facets struct {
		Facets map[string][]db.KBFacet `json:"facets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &facets); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range facets.Facets["products"] {
		if f.Value == product && f.Count >= 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("facets 未返回产品 %s: %+v", product, facets.Facets["products"])
	}

	// PATCH 状态（管理页采纳动作）。
	body, _ := json.Marshal(map[string]any{"status": db.KBStatusDraft})
	req = httptest.NewRequest(http.MethodPatch, "/api/knowledge/"+strconv.FormatInt(entry.ID, 10), bytes.NewReader(body))
	req.SetPathValue("id", strconv.FormatInt(entry.ID, 10))
	rec = httptest.NewRecorder()
	s.kbUpdate(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", rec.Code, rec.Body.String())
	}
	got, err := pg.KB().Get(entry.ID)
	if err != nil || got.Status != db.KBStatusDraft {
		t.Fatalf("状态未更新: %v %+v", err, got)
	}
}
