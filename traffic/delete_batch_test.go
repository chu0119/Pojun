package traffic

import (
	"context"
	"testing"
)

func TestDeleteHostsBatchedHonorsCutoffAndGuard(t *testing.T) {
	tr, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })

	insert := func(id, host string, ts int64) {
		t.Helper()
		if _, err := tr.db.Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, ts, host, "GET", "/", "http://"+host+"/", 200, "text/plain", 1, 1, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := tr.db.Exec(`INSERT INTO exchange_bodies(id,req_head,resp_head) VALUES(?,?,?)`, id, "GET / HTTP/1.1\n", "HTTP/1.1 200 OK\n"); err != nil {
			t.Fatal(err)
		}
	}
	insert("a-old-1", "a.example", 50)
	insert("a-old-2", "a.example", 60)
	insert("a-old-3", "a.example", 70)
	// Recorded after the deletion request: a new task on the same host must keep it.
	insert("a-new", "a.example", 500)
	insert("b-old", "b.example", 50)

	oldBatch := hostDeleteBatchSize
	hostDeleteBatchSize = 2
	t.Cleanup(func() { hostDeleteBatchSize = oldBatch })

	guardCalls := 0
	guard := func(_ context.Context, hosts []string) (map[string]bool, error) {
		guardCalls++
		allowed := make(map[string]bool, len(hosts))
		for _, host := range hosts {
			allowed[host] = true
		}
		return allowed, nil
	}
	var lastDone, lastTotal int64
	deleted, err := tr.DeleteHostsBatched(context.Background(), []string{"A.EXAMPLE", "a.example"}, 100, guard, func(done, total int64) {
		lastDone, lastTotal = done, total
	})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 3 {
		t.Fatalf("deleted=%d, want 3", deleted)
	}
	if guardCalls == 0 {
		t.Fatal("ownership guard was not called")
	}
	if lastDone != 3 || lastTotal != 3 {
		t.Fatalf("progress done=%d total=%d, want 3/3", lastDone, lastTotal)
	}
	var n int
	_ = tr.db.QueryRow(`SELECT count(*) FROM exchanges WHERE id='a-new'`).Scan(&n)
	if n != 1 {
		t.Fatalf("post-cutoff row deleted (n=%d)", n)
	}
	_ = tr.db.QueryRow(`SELECT count(*) FROM exchanges WHERE id='b-old'`).Scan(&n)
	if n != 1 {
		t.Fatalf("other host deleted (n=%d)", n)
	}
	_ = tr.db.QueryRow(`SELECT count(*) FROM exchange_bodies WHERE id LIKE 'a-old-%'`).Scan(&n)
	if n != 0 {
		t.Fatalf("deleted bodies remained: %d", n)
	}
}

func TestDeleteHostsBatchedDropsBlockedHosts(t *testing.T) {
	tr, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	if _, err := tr.db.Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
VALUES('blocked',100,'blocked.example','GET','/','http://blocked.example/',200,'text/plain',1,1,'')`); err != nil {
		t.Fatal(err)
	}
	guard := func(_ context.Context, hosts []string) (map[string]bool, error) {
		out := make(map[string]bool, len(hosts))
		for _, host := range hosts {
			out[host] = false // another live task now owns the host
		}
		return out, nil
	}
	deleted, err := tr.DeleteHostsBatched(context.Background(), []string{"blocked.example"}, 1000, guard, nil)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("blocked host deleted %d rows", deleted)
	}
	var n int
	_ = tr.db.QueryRow(`SELECT count(*) FROM exchanges WHERE id='blocked'`).Scan(&n)
	if n != 1 {
		t.Fatalf("blocked row count=%d, want 1", n)
	}
}
