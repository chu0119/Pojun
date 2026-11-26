package eval

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/chu0119/pojun/db"
)

type fakeKBSearcher struct {
	byQuery map[string][]*db.KBSearchHit
}

func (f fakeKBSearcher) Search(q db.KBSearchQuery) ([]*db.KBSearchHit, error) {
	return f.byQuery[q.Query], nil
}

func kbHit(id int64, title string) *db.KBSearchHit {
	return &db.KBSearchHit{KBEntry: &db.KBEntry{ID: id, Title: title}}
}

func TestEvaluateQueriesMetrics(t *testing.T) {
	searcher := fakeKBSearcher{byQuery: map[string][]*db.KBSearchHit{
		"q1": {kbHit(2, "wrong"), kbHit(1, "right")},
		"q2": nil,
	}}
	queries := []GoldenQuery{
		{ID: "1", Query: "q1", ExpectedIDs: []int64{1}},
		{ID: "2", Query: "q2", ExpectedIDs: []int64{9}},
	}
	m := EvaluateQueries(context.Background(), searcher, queries)
	if m.Queries != 2 {
		t.Fatalf("queries=%d", m.Queries)
	}
	if math.Abs(m.RecallAt5-0.5) > 1e-9 {
		t.Fatalf("Recall@5=%v want 0.5", m.RecallAt5)
	}
	if math.Abs(m.MRRAt10-0.25) > 1e-9 {
		t.Fatalf("MRR@10=%v want 0.25", m.MRRAt10)
	}
	wantNDCG := (1 / math.Log2(3)) / 2
	if math.Abs(m.NDCGAt10-wantNDCG) > 1e-9 {
		t.Fatalf("nDCG@10=%v want %v", m.NDCGAt10, wantNDCG)
	}
	if math.Abs(m.NoResultRate-0.5) > 1e-9 {
		t.Fatalf("NoResultRate=%v want 0.5", m.NoResultRate)
	}
	if m.StartedAt.IsZero() || m.FinishedAt.IsZero() {
		t.Fatalf("时间戳未记录")
	}
}

func TestEvaluateQueriesByTitle(t *testing.T) {
	searcher := fakeKBSearcher{byQuery: map[string][]*db.KBSearchHit{
		"中文 查询": {kbHit(7, "目标条目：Nacos 未授权")},
	}}
	m := EvaluateQueries(context.Background(), searcher, []GoldenQuery{
		{Query: "中文 查询", ExpectedTitles: []string{"目标条目：Nacos 未授权"}},
	})
	if m.RecallAt5 != 1 || m.MRRAt10 != 1 || m.NoResultRate != 0 {
		t.Fatalf("标题匹配指标异常: %+v", m)
	}
}

func TestBuiltinGoldenQueries(t *testing.T) {
	qs := BuiltinGoldenQueries()
	if len(qs) < 10 {
		t.Fatalf("内置样本过少: %d", len(qs))
	}
	seen := map[string]bool{}
	for _, q := range qs {
		if q.Query == "" || len(q.ExpectedTitles) == 0 {
			t.Fatalf("内置样本字段不完整: %+v", q)
		}
		if seen[q.ID] {
			t.Fatalf("内置样本 ID 重复: %s", q.ID)
		}
		seen[q.ID] = true
	}
}

func TestLoadGoldenQueries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "golden.jsonl")
	body := `# comment
{"id":"a","query":"nacos","expected_titles":["Nacos 未授权/默认 token 密钥"]}
{"id":"b","query":"jwt","expected_ids":[3],"k":5}
{"id":"skip","query":""}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	qs, err := LoadGoldenQueries(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 2 {
		t.Fatalf("解析条数=%d want 2", len(qs))
	}
	if qs[1].K != 5 {
		t.Fatalf("K 未保留: %+v", qs[1])
	}
}

func TestPercentile(t *testing.T) {
	got := percentile([]float64{5, 1, 3, 2, 4}, 0.95)
	if got != 5 {
		t.Fatalf("P95=%v want 5", got)
	}
	if percentile(nil, 0.95) != 0 {
		t.Fatalf("空样本应返回 0")
	}
}
