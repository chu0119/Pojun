package agent

import (
	"context"
	"encoding/json"
	"testing"

	actool "github.com/Autumn-27/norma/tool"
	"github.com/chu0119/pojun/db"
)

type fakeKBStore struct {
	lastQuery  db.KBSearchQuery
	searchHits []*db.KBSearchHit
	entry      *db.KBEntry
	marked     int64
	err        error
}

func (f *fakeKBStore) Search(q db.KBSearchQuery) ([]*db.KBSearchHit, error) {
	f.lastQuery = q
	return f.searchHits, f.err
}

func (f *fakeKBStore) Get(id int64) (*db.KBEntry, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.entry == nil {
		return nil, db.ErrKBEntryNotFound
	}
	return f.entry, nil
}

func (f *fakeKBStore) MarkUsed(id int64) error {
	if f.err != nil {
		return f.err
	}
	f.marked = id
	return nil
}

func withFakeKB(t *testing.T, f *fakeKBStore) {
	t.Helper()
	old := kbStore
	SetKBStore(f)
	t.Cleanup(func() { SetKBStore(old) })
}

func TestKBSearchWithoutStoreReturnsExplicitEmpty(t *testing.T) {
	old := kbStore
	SetKBStore(nil)
	t.Cleanup(func() { SetKBStore(old) })

	ts := NewToolSet(nil, "worker")
	res, err := ts.kbSearch().Call(context.Background(), json.RawMessage(`{"q":"nacos"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Count   int    `json:"count"`
		Enabled bool   `json:"enabled"`
		Note    string `json:"note"`
	}
	if err := json.Unmarshal([]byte(toolResultText(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if out.Count != 0 || out.Enabled || out.Note == "" {
		t.Fatalf("未启用时应返回明确空结果: %+v", out)
	}
}

func TestKBSearchAttributesActorAndTask(t *testing.T) {
	f := &fakeKBStore{}
	withFakeKB(t, f)

	ts := NewToolSet(nil, "worker")
	ts.SetTaskID(4242)
	if _, err := ts.kbSearch().Call(context.Background(),
		json.RawMessage(`{"q":"shiro"}`), nil); err != nil {
		t.Fatal(err)
	}
	if f.lastQuery.Actor != "worker" || f.lastQuery.TaskID != 4242 {
		t.Fatalf("归因未传入: actor=%q task=%d", f.lastQuery.Actor, f.lastQuery.TaskID)
	}

	// 无 ToolSet 任务上下文时回退 RunInfo（planner/chat 场景）。
	ts2 := NewToolSet(nil, "")
	ctx := WithRunInfo(context.Background(), RunInfo{TaskID: 77, Kind: KindPlanner})
	if _, err := ts2.kbSearch().Call(ctx, json.RawMessage(`{"q":"jwt"}`), nil); err != nil {
		t.Fatal(err)
	}
	if f.lastQuery.Actor != KindPlanner || f.lastQuery.TaskID != 77 {
		t.Fatalf("RunInfo 归因未传入: actor=%q task=%d", f.lastQuery.Actor, f.lastQuery.TaskID)
	}
}

func TestKBSearchEmptyResultNotes(t *testing.T) {
	f := &fakeKBStore{searchHits: nil}
	withFakeKB(t, f)
	ts := NewToolSet(nil, "worker")
	res, err := ts.kbSearch().Call(context.Background(), json.RawMessage(`{"q":"zzz"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Count int    `json:"count"`
		Note  string `json:"note"`
	}
	if err := json.Unmarshal([]byte(toolResultText(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if out.Count != 0 || out.Note == "" {
		t.Fatalf("空结果应带说明: %+v", out)
	}
}

func TestKBGetAndMarkUsed(t *testing.T) {
	f := &fakeKBStore{entry: &db.KBEntry{
		ID: 12, Title: "Nacos 未授权", Detail: "steps", Commands: []string{"curl"},
		Status: db.KBStatusApproved, Tags: []string{"nacos"},
	}}
	withFakeKB(t, f)
	ts := NewToolSet(nil, "worker")

	res, err := ts.kbGet().Call(context.Background(), json.RawMessage(`{"id":12}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal([]byte(toolResultText(t, res)), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != 12 || got.Title != "Nacos 未授权" {
		t.Fatalf("kb_get 返回异常: %+v", got)
	}

	if res, _ := ts.kbGet().Call(context.Background(), json.RawMessage(`{"id":0}`), nil); !res.IsError {
		t.Fatalf("id=0 应报错")
	}
	if _, err := ts.kbMarkUsed().Call(context.Background(), json.RawMessage(`{"id":12}`), nil); err != nil {
		t.Fatal(err)
	}
	if f.marked != 12 {
		t.Fatalf("MarkUsed 未调用: %d", f.marked)
	}
}

func TestKBToolsBoundToAllRoles(t *testing.T) {
	ts := NewToolSet(nil, "")
	for name, tools := range map[string][]actool.CoreTool{
		"worker":    ts.WorkerTools(),
		"planner":   ts.PlannerTools(),
		"mainagent": ts.MainAgentTools(),
	} {
		found := map[string]bool{}
		for _, tl := range tools {
			found[tl.Name()] = true
		}
		for _, want := range []string{"kb_search", "kb_get", "kb_mark_used"} {
			if !found[want] {
				t.Fatalf("%s 缺少工具 %s", name, want)
			}
		}
	}
}
