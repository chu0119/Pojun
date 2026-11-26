package traffic

import (
	"testing"
)

func TestEvidencePinsProtectPinnedTrafficFromPrune(t *testing.T) {
	tr, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })

	insert := func(id string, ts int64) {
		t.Helper()
		if _, err := tr.db.Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, ts, "evidence.example", "GET", "/", "http://evidence.example/", 200, "text/plain", 1, 1, ""); err != nil {
			t.Fatal(err)
		}
	}
	insert("pinned-old", 10)
	insert("unpinned-old", 10)
	insert("unpinned-new", 10_000)
	if err := tr.PinEvidence([]string{"pinned-old", "pinned-old"}); err != nil {
		t.Fatal(err)
	}
	preview, err := tr.PreviewUnpinnedBefore(100)
	if err != nil {
		t.Fatal(err)
	}
	if preview != 1 {
		t.Fatalf("preview=%d, want 1", preview)
	}
	deleted, err := tr.PruneUnpinnedBefore(100)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted=%d, want 1", deleted)
	}
	var n int
	_ = tr.db.QueryRow(`SELECT count(*) FROM exchanges WHERE id='pinned-old'`).Scan(&n)
	if n != 1 {
		t.Fatalf("pinned row deleted (n=%d)", n)
	}
	_ = tr.db.QueryRow(`SELECT count(*) FROM exchanges WHERE id='unpinned-old'`).Scan(&n)
	if n != 0 {
		t.Fatalf("unpinned old row remained (n=%d)", n)
	}
	_ = tr.db.QueryRow(`SELECT count(*) FROM exchanges WHERE id='unpinned-new'`).Scan(&n)
	if n != 1 {
		t.Fatalf("post-cutoff unpinned row deleted (n=%d)", n)
	}
	// Deleting a host must also drop its pin, otherwise a later evidence-mode
	// prune would leak pin rows forever.
	if _, err := tr.DeleteHost("evidence.example"); err != nil {
		t.Fatal(err)
	}
	_ = tr.db.QueryRow(`SELECT count(*) FROM evidence_pins`).Scan(&n)
	if n != 0 {
		t.Fatalf("pins remained after host delete: %d", n)
	}
}

func TestPruneUnpinnedToSizeNeverDropsPinnedRows(t *testing.T) {
	tr, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	insert := func(id string, ts int64) {
		t.Helper()
		if _, err := tr.db.Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, ts, "cap.example", "GET", "/", "http://cap.example/", 200, "text/plain", 1, 1, ""); err != nil {
			t.Fatal(err)
		}
	}
	insert("pinned", 1)
	for _, id := range []string{"u1", "u2", "u3"} {
		insert(id, 2)
	}
	if err := tr.PinEvidence([]string{"pinned"}); err != nil {
		t.Fatal(err)
	}
	// A one-byte cap cannot be met by deleting unpinned rows alone in a real
	// database, but pinned rows must still survive the sweep.
	if _, err := tr.PruneUnpinnedToSize(1); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = tr.db.QueryRow(`SELECT count(*) FROM exchanges WHERE id='pinned'`).Scan(&n)
	if n != 1 {
		t.Fatalf("pinned row deleted by size cap (n=%d)", n)
	}
}
