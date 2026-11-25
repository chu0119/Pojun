package traffic

import (
	"testing"
	"time"
)

func TestRetentionPreviewAndDeleteProtectsReferenced(t *testing.T) {
	tr, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })

	now := time.Now()
	old := now.Add(-10 * 24 * time.Hour).Unix()
	recent := now.Add(-2 * 24 * time.Hour).Unix()
	insert := func(id string, ts int64) {
		if _, err := tr.db.Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, ts, "retention.example", "GET", "/", "http://retention.example/", 200, "text/plain", 1, 1, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := tr.db.Exec(`INSERT INTO exchange_bodies(id,req_head,resp_head) VALUES(?,?,?)`, id, "GET / HTTP/1.1\nHost: retention.example\n", "HTTP/1.1 200 OK\n"); err != nil {
			t.Fatal(err)
		}
	}
	insert("old-free", old)
	insert("old-protected", old)
	insert("recent", recent)
	protected := map[string]struct{}{"old-protected": {}}
	cutoff := now.Add(-7 * 24 * time.Hour)

	preview, err := tr.RetentionPreview(cutoff, protected)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Candidates != 2 || preview.Protected != 1 || preview.Deletable != 1 {
		t.Fatalf("preview=%+v", preview)
	}

	res, err := tr.DeleteUnreferencedBefore(cutoff, protected)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted != 1 || res.Protected != 1 {
		t.Fatalf("delete result=%+v", res)
	}
	var n int
	if err := tr.db.QueryRow(`SELECT COUNT(*) FROM exchanges WHERE id='old-free'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("old-free should be deleted: n=%d err=%v", n, err)
	}
	if err := tr.db.QueryRow(`SELECT COUNT(*) FROM exchanges WHERE id='old-protected'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("protected should remain: n=%d err=%v", n, err)
	}
	if err := tr.db.QueryRow(`SELECT COUNT(*) FROM exchanges WHERE id='recent'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("recent should remain: n=%d err=%v", n, err)
	}
}

func TestRetentionPreviewEmptyProtection(t *testing.T) {
	tr, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	if _, err := tr.db.Exec(`INSERT INTO exchanges(id,ts,host,method,url,status,resp_len,path) VALUES('x',1,'h','GET','http://h/',200,1,'')`); err != nil {
		t.Fatal(err)
	}
	// ts=1 is older than any cutoff; an empty map still means "nothing protected",
	// which is why the server must fail closed before calling this method.
	preview, err := tr.RetentionPreview(time.Unix(100, 0), map[string]struct{}{})
	if err != nil || preview.Candidates != 1 || preview.Deletable != 1 {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
}
