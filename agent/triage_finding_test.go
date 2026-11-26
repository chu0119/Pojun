package agent

import (
	"strings"
	"testing"

	"github.com/chu0119/pojun/db"
)

func TestTriageCorePass(t *testing.T) {
	ev := strings.Repeat("curl -s http://x/a?id=2 -> 200 用户B订单数据 ", 6) // ≥120 字
	f := &db.DBFinding{ID: 7, Severity: "high", Evidence: ev, Status: db.FindingPending}
	res := triageCore(f, map[string]any{"verification_strategy": "http_diff"}, 2, nil)
	if res.Verdict != TriagePass {
		t.Fatalf("verdict=%s checks=%+v", res.Verdict, res.Checks)
	}
	if len(res.Missing) != 0 {
		t.Fatalf("missing=%v", res.Missing)
	}
}

func TestTriageCoreNeedsEvidence(t *testing.T) {
	res := triageCore(&db.DBFinding{ID: 8, Severity: "high", Status: db.FindingPending}, nil, 0, nil)
	if res.Verdict != TriageNeedsEvidence {
		t.Fatalf("verdict=%s", res.Verdict)
	}
	joined := strings.Join(res.Missing, ",")
	if !strings.Contains(joined, "evidence_present") {
		t.Fatalf("missing=%v", res.Missing)
	}
}

func TestTriageCoreDuplicate(t *testing.T) {
	res := triageCore(&db.DBFinding{ID: 9, Status: db.FindingDuplicate, Evidence: "x"}, nil, 0, []int64{3})
	if res.Verdict != TriageDuplicate {
		t.Fatalf("verdict=%s", res.Verdict)
	}
	if len(res.Duplicates) != 1 || res.Duplicates[0] != 3 {
		t.Fatalf("duplicates=%v", res.Duplicates)
	}
}

func TestTriageCoreReviewOnMissingTraffic(t *testing.T) {
	ev := strings.Repeat("sqlmap -u http://x/?id=1 --batch 注入成功，回显数据库名 dvwa ", 6)
	f := &db.DBFinding{ID: 10, Severity: "high", Evidence: ev, Status: db.FindingPending}
	res := triageCore(f, map[string]any{"verification_strategy": "poc_replay"}, 0, nil)
	if res.Verdict != TriageReview {
		t.Fatalf("verdict=%s checks=%+v", res.Verdict, res.Checks)
	}
	found := false
	for _, c := range res.Checks {
		if c.Name == "traffic_bound" && c.Status == "warn" {
			found = true
		}
	}
	if !found {
		t.Fatalf("traffic warn missing: %+v", res.Checks)
	}
}

func TestTriageFindingNotGivenToWorker(t *testing.T) {
	for _, s := range BuiltinToolSeeds() {
		if s.Key != "triage_finding" {
			continue
		}
		for _, a := range s.Agents {
			if a == "worker" {
				t.Fatalf("triage_finding 不应绑定 worker：%v", s.Agents)
			}
		}
		hasPlanner := false
		for _, a := range s.Agents {
			if a == "planner" {
				hasPlanner = true
			}
		}
		if !hasPlanner {
			t.Fatalf("triage_finding 应绑定 planner：%v", s.Agents)
		}
		return
	}
	t.Fatal("triage_finding 未注册到 BuiltinToolSeeds")
}
