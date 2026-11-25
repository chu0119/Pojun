package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/chu0119/pojun/db"
	"github.com/chu0119/pojun/traffic"
)

func TestTrafficRetentionStatusEndpoint(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil || dsn == "" {
		t.Skip("no test database configured")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pg.Close() })
	t.Cleanup(func() { _ = pg.SetSetting(settingTrafficRetentionDays, "") })
	if err := pg.SetSetting(settingTrafficRetentionDays, "30"); err != nil {
		t.Fatal(err)
	}
	tr, err := traffic.Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	s := &Server{m: &Manager{pg: pg, traffic: tr}}

	rr := httptest.NewRecorder()
	s.getTrafficRetention(rr, httptest.NewRequest("GET", "/api/traffic/retention", nil))
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var got struct {
		Days       int  `json:"days"`
		Enabled    bool `json:"enabled"`
		Candidates int  `json:"candidates"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Days != 30 || !got.Enabled {
		t.Fatalf("unexpected status: %+v", got)
	}
}
