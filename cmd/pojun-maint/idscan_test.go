package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// TestRunIDScanReadsBlobViaDataDir covers the production layout: --data derives
// <data>/traffic/_index/index.sqlite and <data>/traffic/_blobs, and a spilled
// body is read from the blob file rather than the inline preview.
func TestRunIDScanReadsBlobViaDataDir(t *testing.T) {
	dataDir := t.TempDir()
	idxDir := filepath.Join(dataDir, "traffic", "_index")
	if err := os.MkdirAll(idxDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := append([]byte(strings.Repeat("x", 100*1024)), []byte(" pojun ")...)
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	blobDir := filepath.Join(dataDir, "traffic", "_blobs", "sha256", hash[:2])
	if err := os.MkdirAll(blobDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobDir, hash+".bin"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(idxDir, "index.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
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
		VALUES('e1',1700000000,'blob.example.com','GET','/','http://blob.example.com/',200,'text/plain',0,?,'')`, len(body)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO exchange_bodies(id,req_head,req_body,req_blob,resp_head,resp_body,resp_blob)
		VALUES('e1','GET / HTTP/1.1' || char(10),NULL,NULL,'HTTP 200' || char(10),NULL,?)`, hash); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "report.json")
	runIDScan([]string{"--data", dataDir, "--tiers", "A", "--out", out})

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var rep traffic.IdentityReport
	if err := json.Unmarshal(raw, &rep); err != nil {
		t.Fatal(err)
	}
	if rep.FindingsTotal == 0 {
		t.Fatal("CLI 应从 blob 读到深部命中")
	}
	if rep.Scan.BlobMissing != 0 {
		t.Fatalf("blob_missing=%d", rep.Scan.BlobMissing)
	}
	if rep.Coverage.WithBody != 1 || rep.Coverage.ScanCoverage != 1 {
		t.Fatalf("coverage=%+v", rep.Coverage)
	}
}
