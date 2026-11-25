package server

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/chu0119/pojun/agent"
)

func TestBuildReflectionLesson(t *testing.T) {
	done := buildReflectionLesson("done", "", "test authz", "read one record",
		agent.WriteCounts{Facts: 1, Findings: 1, Assets: 2}, 7, 1)
	for _, want := range []string{"完成且有写回", "事实1", "漏洞1", "工具7次", "验收标准"} {
		if !strings.Contains(done, want) {
			t.Fatalf("done lesson missing %q: %s", want, done)
		}
	}
	ex := buildReflectionLesson("exhausted", "max_turns", "sqli probe", "",
		agent.WriteCounts{}, 12, 3)
	if !strings.Contains(ex, "耗尽无产出") || !strings.Contains(ex, "max_turns") {
		t.Fatalf("exhausted lesson: %s", ex)
	}
	bl := buildReflectionLesson("blocked", "error: connection refused", "upload probe", "",
		agent.WriteCounts{}, 2, 2)
	if !strings.Contains(bl, "阻塞") || !strings.Contains(bl, "error") {
		t.Fatalf("blocked lesson: %s", bl)
	}
}

// #75: PostgreSQL rejects text that ends in the middle of a multi-byte rune.
// The lesson/summary/criteria truncation must stay on UTF-8 boundaries.
func TestReflectionLessonKeepsUTF8Boundaries(t *testing.T) {
	longZH := strings.Repeat("中文摘要与验收标准", 30) + "🙂"
	lesson := buildReflectionLesson("done", "error: "+longZH, longZH, longZH,
		agent.WriteCounts{Facts: 1}, 3, 1)
	if !utf8.ValidString(lesson) {
		t.Fatalf("lesson is not valid UTF-8: %q", lesson)
	}
	reason := shortReflectionReason(longZH)
	if !utf8.ValidString(reason) || len(reason) > 80 {
		t.Fatalf("reason invalid or too long: %q (%d bytes)", reason, len(reason))
	}
	line := firstLineText(longZH, 120)
	if !utf8.ValidString(line) || len(line) > 120 {
		t.Fatalf("summary invalid or too long: %q (%d bytes)", line, len(line))
	}

	// A 4-byte emoji starting exactly at the byte cut must be dropped whole.
	s := strings.Repeat("a", 119) + "🙂"
	if got := truncateUTF8Bytes(s, 120); got != strings.Repeat("a", 119) {
		t.Fatalf("truncate split the emoji: %q", got)
	}
}

func TestReflectionTextSanitizesInvalidUTF8(t *testing.T) {
	bad := string([]byte{0xe5, 0x8f, 0xe3, 0x80}) // the SQLSTATE 22021 shape from #75
	if utf8.ValidString(bad) {
		t.Fatal("test fixture must be invalid UTF-8")
	}
	if got := sanitizeReflectionText(bad); !utf8.ValidString(got) {
		t.Fatalf("sanitize left invalid UTF-8: %q", got)
	}
	lesson := buildReflectionLesson("blocked", bad, bad, bad, agent.WriteCounts{}, 1, 1)
	if !utf8.ValidString(lesson) {
		t.Fatalf("lesson with invalid input is not valid UTF-8: %q", lesson)
	}
}

func TestCoverageReportRows(t *testing.T) {
	// guards the shape the DOCX/Markdown renderers rely on
	rows := []string{agent.DimensionLabel(agent.DimensionOrder()[0])}
	if rows[0] == "" {
		t.Fatal("dimension label empty")
	}
}
