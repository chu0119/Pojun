package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/chu0119/pojun/traffic"
	_ "modernc.org/sqlite"
)

// TestRunIDScanCLIWritesReport exercises the pojun-maint entry point end to end
// against a hand-built traffic index: no PostgreSQL, no proxy, no network.
func TestRunIDScanCLIWritesReport(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "index.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
CREATE TABLE exchanges (
  id TEXT PRIMARY KEY, ts INTEGER, host TEXT, method TEXT, url_template TEXT,
  url TEXT, status INTEGER, content_type TEXT, req_len INTEGER, resp_len INTEGER, path TEXT
);
CREATE TABLE exchange_bodies (
  id TEXT PRIMARY KEY, req_head TEXT NOT NULL DEFAULT '', req_body BLOB, req_blob TEXT,
  resp_head TEXT NOT NULL DEFAULT '', resp_body BLOB, resp_blob TEXT
);`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO exchanges(id,ts,host,method,url_template,url,status,content_type,req_len,resp_len,path)
		VALUES('e1',1700000000,'cli.example.com','GET','/','http://cli.example.com/',200,'text/plain',0,5,'')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO exchange_bodies(id,req_head,req_body,req_blob,resp_head,resp_body,resp_blob)
		VALUES('e1','GET / HTTP/1.1' || char(10) || 'Host: cli.example.com' || char(10),NULL,NULL,
		       'HTTP 200' || char(10) || 'Content-Type: text/plain' || char(10),'pojun',NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "report.json")
	runIDScan([]string{"--db", dbPath, "--tiers", "A", "--out", out})

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var rep traffic.IdentityReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.FindingsTotal == 0 {
		t.Fatal("CLI 报告应包含 A 命中")
	}
	if rep.DB.Mode != "ro" {
		t.Fatalf("db.mode=%q", rep.DB.Mode)
	}
	if rep.Scan.Scanned != 1 || rep.Coverage.WithBody != 1 {
		t.Fatalf("scan=%+v coverage=%+v", rep.Scan, rep.Coverage)
	}
}
