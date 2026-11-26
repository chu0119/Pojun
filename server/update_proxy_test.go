package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chu0119/pojun/db"
	"github.com/chu0119/pojun/selfupdate"
)

func TestPickUpdateProxyOrder(t *testing.T) {
	cases := []struct {
		name                         string
		saved, env, install, global string
		want, source                string
	}{
		{"update wins", " http://update:1 ", "http://env:2", "http://install:3", "http://global:4", "http://update:1", "update"},
		{"env second", "", "http://env:2", "http://install:3", "http://global:4", "http://env:2", "env"},
		{"install third", "", "", "http://install:3", "http://global:4", "http://install:3", "install"},
		{"global fourth", "", "", "", "http://global:4", "http://global:4", "global"},
		{"direct", "", "", "", "", "", "direct"},
	}
	for _, c := range cases {
		got, src := pickUpdateProxy(c.saved, c.env, c.install, c.global)
		if got != c.want || src != c.source {
			t.Errorf("%s: got (%q,%q) want (%q,%q)", c.name, got, src, c.want, c.source)
		}
	}
}

func TestUpdateProxyInfoReadsSettingsAndEnv(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	s := &Server{m: &Manager{pg: d}}
	t.Setenv("POJUN_UPDATE_PROXY", "")
	t.Cleanup(func() {
		_ = d.SetSetting(settingUpdateProxy, "")
		_ = d.SetSetting(settingInstallProxy, "")
	})
	_ = d.SetSetting(settingUpdateProxy, "")
	_ = d.SetSetting(settingInstallProxy, "")

	saved, effective, source := s.updateProxyInfo()
	if saved != "" || effective != "" || source != "direct" {
		t.Fatalf("无任何代理配置时应为 direct，得到 (%q,%q,%q)", saved, effective, source)
	}
	t.Setenv("POJUN_UPDATE_PROXY", "http://env-proxy:8080")
	// Manager.GlobalProxy 需要初始化过设置；无配置时为空，installProxy 也为空，
	// 因此这里应落到 env。
	_, effective, source = s.updateProxyInfo()
	if effective != "http://env-proxy:8080" || source != "env" {
		t.Fatalf("env 代理未生效: (%q,%q)", effective, source)
	}
	if err := s.saveUpdateProxy("http://update-proxy:3128"); err != nil {
		t.Fatal(err)
	}
	saved, effective, source = s.updateProxyInfo()
	if saved != "http://update-proxy:3128" || effective != saved || source != "update" {
		t.Fatalf("更新专用代理未优先: (%q,%q,%q)", saved, effective, source)
	}
	// 清空设置 → 回到 env。
	if err := s.saveUpdateProxy(""); err != nil {
		t.Fatal(err)
	}
	_, effective, source = s.updateProxyInfo()
	if effective != "http://env-proxy:8080" || source != "env" {
		t.Fatalf("清空后应回到 env: (%q,%q)", effective, source)
	}
}

func TestSaveUpdateProxyValidation(t *testing.T) {
	s := &Server{m: &Manager{}}
	if err := s.saveUpdateProxy("ftp://bad"); err == nil || !strings.Contains(err.Error(), "更新代理无效") {
		t.Fatalf("非法代理应被拒绝: %v", err)
	}
	// 空串是合法输入（清除）。
	if err := s.saveUpdateProxy(""); err == nil || !strings.Contains(err.Error(), "设置存储不可用") {
		t.Fatalf("无设置存储时应报错（空串也应先通过校验）: %v", err)
	}
}

func TestUpdateLastRunRoundTrip(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	s := &Server{m: &Manager{pg: d}}
	t.Cleanup(func() { _ = d.SetSetting(settingUpdateLastRun, "") })
	_ = d.SetSetting(settingUpdateLastRun, "")
	if _, ok := s.loadUpdateLast(); ok {
		t.Fatal("空设置不应返回记录")
	}
	at := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	s.saveUpdateLast(updateLastRun{From: "0.9.73", To: "0.9.74", Status: "staged", At: at})
	got, ok := s.loadUpdateLast()
	if !ok || got.Status != "staged" || got.To != "0.9.74" || !got.At.Equal(at) {
		t.Fatalf("round trip: %+v ok=%v", got, ok)
	}
	s.saveUpdateLast(updateLastRun{From: "0.9.73", To: "0.9.74", Status: "failed", Error: "下载超时", At: at})
	got, ok = s.loadUpdateLast()
	if !ok || got.Status != "failed" || got.Error != "下载超时" {
		t.Fatalf("failed round trip: %+v ok=%v", got, ok)
	}
}

func TestUpdateProxyTestUsesInjectedProbe(t *testing.T) {
	dsn, _, err := db.DSN()
	if err == nil {
		if d, derr := db.Open(dsn); derr == nil {
			defer d.Close()
			_ = d.SetSetting(settingUpdateProxy, "")
		}
	}
	old := updateProxyProbe
	t.Cleanup(func() { updateProxyProbe = old })
	var gotProxy string
	updateProxyProbe = func(ctx context.Context, c *http.Client) (*selfupdate.Release, error) {
		gotProxy = "called"
		return &selfupdate.Release{TagName: "v9.9.9"}, nil
	}
	s := &Server{m: &Manager{}}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/update/proxy-test", strings.NewReader(`{"proxy":"http://127.0.0.1:7890"}`))
	s.updateProxyTest(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		OK      bool   `json:"ok"`
		Proxy   string `json:"proxy"`
		Latest  string `json:"latest"`
		Latency int64  `json:"latency_ms"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.Proxy != "http://127.0.0.1:7890" || out.Latest != "v9.9.9" || gotProxy != "called" {
		t.Fatalf("out=%+v called=%q", out, gotProxy)
	}

	// 非法代理在调用探测前就被挡掉。
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/update/proxy-test", strings.NewReader(`{"proxy":"ftp://bad"}`))
	s.updateProxyTest(rr, req)
	if rr.Code != 400 {
		t.Fatalf("invalid proxy status=%d body=%s", rr.Code, rr.Body.String())
	}
}

// 更新进度状态机：begin 互斥，publish 推进，finish 收尾；快照可用于刷新后接回。
func TestUpdateHubStateMachine(t *testing.T) {
	h := &updateHub{cur: updateProgress{Phase: selfupdate.PhaseIdle, Percent: -1}, subs: map[chan updateProgress]struct{}{}}
	if !h.begin("v1.2.3") {
		t.Fatal("first begin should win")
	}
	if h.begin("v1.2.3") {
		t.Fatal("second begin must be rejected (409 path)")
	}
	cur, running := h.snapshot()
	if !running || cur.Version != "v1.2.3" || cur.Phase != selfupdate.PhaseDownload {
		t.Fatalf("snapshot after begin: %+v running=%v", cur, running)
	}
	ch, unsub := h.subscribe()
	defer unsub()
	h.publish(selfupdate.PhaseVerify, -1, "校验中")
	p := <-ch
	if p.Phase != selfupdate.PhaseVerify || p.Message != "校验中" || p.Version != "v1.2.3" {
		t.Fatalf("published=%+v", p)
	}
	h.finish(nil)
	cur, running = h.snapshot()
	if running || cur.Phase != selfupdate.PhaseStaged || cur.Percent != 100 {
		t.Fatalf("snapshot after finish: %+v running=%v", cur, running)
	}
	if !h.begin("v1.2.4") {
		t.Fatal("after finish a new run must be allowed")
	}
	h.finish(context.DeadlineExceeded)
	cur, running = h.snapshot()
	if running || cur.Phase != selfupdate.PhaseFailed || cur.Error == "" {
		t.Fatalf("failed snapshot: %+v running=%v", cur, running)
	}
}

// updateCheck 必须把进行中状态、更新代理与上次结果一并返回，页面刷新后才能接回。
func TestUpdateCheckReturnsRuntimeAndProxy(t *testing.T) {
	old := relCache.fetch
	t.Cleanup(func() {
		relCache.fetch = old
		relCache.invalidate()
	})
	relCache.invalidate()
	relCache.fetch = func(ctx context.Context, c *http.Client) (*selfupdate.Release, error) {
		return &selfupdate.Release{TagName: "v99.0.0", Body: "notes"}, nil
	}
	// 全局 hub 状态在包内共享：备份并在测试后复位。
	prev, prevRunning := updHub.snapshot()
	t.Cleanup(func() {
		updHub.mu.Lock()
		updHub.running = prevRunning
		updHub.cur = prev
		updHub.mu.Unlock()
	})
	updHub.mu.Lock()
	updHub.running = false
	updHub.cur = updateProgress{Phase: selfupdate.PhaseIdle, Percent: -1}
	updHub.mu.Unlock()

	s := &Server{m: &Manager{}}
	rr := httptest.NewRecorder()
	s.updateCheck(rr, httptest.NewRequest(http.MethodGet, "/api/update/check", nil))
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Update map[string]any `json:"update"`
		Proxy  string        `json:"update_proxy"`
		Eff    string        `json:"update_proxy_effective"`
		Source string        `json:"update_proxy_source"`
		Latest string        `json:"latest"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Latest != "v99.0.0" {
		t.Fatalf("latest=%q body=%s", out.Latest, rr.Body.String())
	}
	if out.Update == nil {
		t.Fatal("update runtime snapshot missing")
	}
	if running, _ := out.Update["running"].(bool); running {
		t.Fatalf("update.running should be false: %+v", out.Update)
	}
	if out.Source == "" {
		t.Fatalf("update_proxy_source missing: %s", rr.Body.String())
	}
}
