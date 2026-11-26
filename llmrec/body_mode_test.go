package llmrec

import (
	"strings"
	"testing"
)

func TestApplyBodyModePolicies(t *testing.T) {
	old := BodyMode
	t.Cleanup(func() { BodyMode = old })

	BodyMode = func() string { return "off" }
	req, resp, rawReq, rawResp := applyBodyMode("off", "ok", "REQ", "RESP", "RAWREQ", "RAWRESP")
	if req != "" || resp != "" || rawReq != "" || rawResp != "" {
		t.Fatalf("off must drop every body: %q %q %q %q", req, resp, rawReq, rawResp)
	}

	BodyMode = func() string { return "errors" }
	req, resp, _, _ = applyBodyMode("errors", "ok", "REQ", "RESP", "", "")
	if req != "" || resp != "" {
		t.Fatalf("errors mode must drop successful bodies: %q %q", req, resp)
	}
	req, resp, rawReq, rawResp = applyBodyMode("errors", "error", "REQ", "RESP", "RAWREQ", "RAWRESP")
	if req != "REQ" || resp != "RESP" || rawReq != "RAWREQ" || rawResp != "RAWRESP" {
		t.Fatalf("errors mode must keep failed bodies: %q %q %q %q", req, resp, rawReq, rawResp)
	}

	BodyMode = func() string { return "full" }
	req, resp, rawReq, rawResp = applyBodyMode("full", "ok", "REQ", "RESP", "RAWREQ", "RAWRESP")
	if req != "REQ" || resp != "RESP" || rawReq != "RAWREQ" || rawResp != "RAWRESP" {
		t.Fatalf("full mode must keep everything: %q %q %q %q", req, resp, rawReq, rawResp)
	}
}

func TestTruncateRecordedBodyKeepsUTF8(t *testing.T) {
	big := strings.Repeat("中", maxErrorBodyBytes) // 3 bytes each
	got := truncateRecordedBody(big)
	if len(got) >= len(big) || !strings.HasSuffix(got, "...[truncated]") {
		t.Fatalf("expected truncation marker, len=%d", len(got))
	}
	if !strings.Contains(got, "中") {
		t.Fatal("truncation cut away all content")
	}
}
