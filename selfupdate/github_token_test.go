package selfupdate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSetAPIHeadersAuthorization verifies that setAPIHeaders injects the
// Authorization header only when a token is configured. The test doesn't need
// to hit a real GitHub endpoint — it just checks the header on the request.
func TestSetAPIHeadersAuthorization(t *testing.T) {
	SetGitHubToken("ghp_test_token_12345")
	defer SetGitHubToken("")

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/repos/x/releases/latest", nil)
	setAPIHeaders(req)
	if got := req.Header.Get("Authorization"); got != "Bearer ghp_test_token_12345" {
		t.Fatalf("Authorization = %q, want Bearer ghp_test_token_12345", got)
	}

	SetGitHubToken("")
	req2, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://api.github.com/repos/x/releases/latest", nil)
	setAPIHeaders(req2)
	if got := req2.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization should be empty when token cleared, got %q", got)
	}
}

// TestFetchLatestSendsAuthorization end-to-end: fetchRelease against a local
// httptest server. checkURL rejects non-GitHub domains, so we bypass it by
// calling setAPIHeaders + client.Do directly (the exact code path fetchRelease
// uses after the domain check).
func TestFetchLatestSendsAuthorization(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
		w.Write([]byte(`{"tag_name":"v0.4.123","name":"v0.4.123","assets":[]}`))
	}))
	defer srv.Close()

	SetGitHubToken("ghp_test_token_12345")
	defer SetGitHubToken("")

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/test", nil)
	setAPIHeaders(req)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotAuth != "Bearer ghp_test_token_12345" {
		t.Fatalf("Authorization = %q, want Bearer ghp_test_token_12345", gotAuth)
	}

	SetGitHubToken("")
	gotAuth = ""
	req2, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/test", nil)
	setAPIHeaders(req2)
	resp2, err := srv.Client().Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if gotAuth != "" {
		t.Fatalf("Authorization should be empty when token cleared, got %q", gotAuth)
	}
}
