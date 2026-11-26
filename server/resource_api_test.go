package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chu0119/pojun/resource"
)

func TestResourceAPIRequiresExplicitEnablePG(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}

	keys := []string{
		settingResourceAutoTune, settingResourceMode, settingResourceHigh,
		settingResourceCritical, settingResourceAutoPause, settingResourceManual,
		settingWorkers,
	}
	type saved struct {
		value string
		ok    bool
	}
	old := map[string]saved{}
	for _, k := range keys {
		v, ok, gerr := m.pg.GetSetting(k)
		if gerr != nil {
			t.Fatalf("snapshot %s: %v", k, gerr)
		}
		old[k] = saved{v, ok}
	}
	// 恢复必须先于 m.Close（defer LIFO），所以包在同一个 defer 里。
	defer func() {
		for k, s := range old {
			if s.ok {
				_ = m.pg.SetSetting(k, s.value)
			} else {
				_, _ = m.pg.Exec(`DELETE FROM settings WHERE key=$1`, k)
			}
		}
		resource.SetPlan(resource.NeutralPlan())
		m.Close()
	}()
	// 清空资源设置与 workers，模拟「从未配置过」的新库。
	for _, k := range keys {
		if _, err := m.pg.Exec(`DELETE FROM settings WHERE key=$1`, k); err != nil {
			t.Fatalf("delete %s: %v", k, err)
		}
	}

	td := t.TempDir()
	s := New(context.Background(), m, td, td, td)
	defer s.Close()
	// 测试结束后恢复进程级 Go 运行时（GOMAXPROCS/内存上限），避免影响同包其他测试。
	defer s.applyGoRuntime(resource.NeutralPlan())
	h := s.Handler()
	tok, err := signJWT(s.currentJWTKey())
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}

	do := func(method, path string, body any) (int, map[string]any) {
		var r *http.Request
		if body != nil {
			b, _ := json.Marshal(body)
			r = httptest.NewRequest(method, path, bytes.NewReader(b))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	planOf := func(out map[string]any) map[string]any {
		p, _ := out["plan"].(map[string]any)
		return p
	}
	settingsOf := func(out map[string]any) map[string]any {
		p, _ := out["settings"].(map[string]any)
		return p
	}

	// 1) 默认：自动调优关闭、计划未生效、有效 workers 仍是 3。
	code, out := do("GET", "/api/resource", nil)
	if code != 200 {
		t.Fatalf("GET /api/resource = %d (%v)", code, out)
	}
	if got := settingsOf(out)["auto_tune"]; got != false {
		t.Fatalf("default auto_tune = %v, want false", got)
	}
	if got := planOf(out)["enabled"]; got != false {
		t.Fatalf("default plan.enabled = %v, want false", got)
	}
	if got := out["effective"].(map[string]any)["workers"]; got != float64(3) {
		t.Fatalf("default effective workers = %v, want 3", got)
	}
	if got := planOf(out)["max_jobs"]; got != float64(0) {
		t.Fatalf("default plan.max_jobs = %v, want 0 (no gate)", got)
	}
	if m.Workers() != 3 {
		t.Fatalf("Manager.Workers() = %d, want 3", m.Workers())
	}

	// 2) 开启自动调优：新库（无 workers 记录）由计划推导 worker。
	code, out = do("PUT", "/api/resource", map[string]any{"auto_tune": true, "mode": "auto"})
	if code != 200 {
		t.Fatalf("PUT enable = %d (%v)", code, out)
	}
	p := planOf(out)
	if p["enabled"] != true || p["mode"] != "auto" {
		t.Fatalf("enabled plan mismatch: %v", p)
	}
	derived := int(p["workers"].(float64))
	if derived <= 0 {
		t.Fatalf("derived workers = %d", derived)
	}
	if p["max_jobs"].(float64) <= 0 {
		t.Fatalf("enabled auto plan must have max_jobs: %v", p)
	}
	if m.Workers() != derived {
		t.Fatalf("new library should follow plan workers %d, got %d", derived, m.Workers())
	}

	// 3) 存量显式 workers 不得被自动计划覆盖。
	if err := m.pg.SetSetting(settingWorkers, "7"); err != nil {
		t.Fatal(err)
	}
	s.applyResourcePlan()
	if m.Workers() != 7 {
		t.Fatalf("explicit workers=7 was overridden to %d", m.Workers())
	}

	// 4) 手工覆盖：worker 写穿到常规设置，作业上限进入计划。
	code, out = do("PUT", "/api/resource", map[string]any{
		"manual": map[string]any{"workers": 2, "max_jobs": 3, "scanner_scale": 0.5},
	})
	if code != 200 {
		t.Fatalf("PUT manual = %d (%v)", code, out)
	}
	p = planOf(out)
	if p["workers"] != float64(2) || p["max_jobs"] != float64(3) || p["scanner_scale"] != 0.5 {
		t.Fatalf("manual overrides not applied: %v", p)
	}
	if got, ok, _ := m.pg.GetSetting(settingWorkers); !ok || got != "2" {
		t.Fatalf("manual worker override not written through: %q ok=%v", got, ok)
	}
	if m.Workers() != 2 {
		t.Fatalf("Manager.Workers() = %d, want manual 2", m.Workers())
	}
	if resource.ScannerScaleNow() != 0.5 {
		t.Fatalf("scanner scale not applied: %v", resource.ScannerScaleNow())
	}

	// 5) 清空手工覆盖：计划回到自动推导，但已写穿的 workers 仍是显式配置。
	code, out = do("PUT", "/api/resource", map[string]any{"manual": map[string]any{}})
	if code != 200 {
		t.Fatalf("PUT clear manual = %d (%v)", code, out)
	}
	if got := planOf(out)["workers"]; int(got.(float64)) != derived {
		t.Fatalf("after clearing manual plan workers = %v, want derived %d", got, derived)
	}
	if m.Workers() != 2 {
		t.Fatalf("clearing manual must not silently rewrite explicit workers=2, got %d", m.Workers())
	}

	// 6) 校验：阈值/模式/覆盖项越界都返回 400，且不落库。
	code, _ = do("PUT", "/api/resource", map[string]any{"threshold_high": 96})
	if code != http.StatusBadRequest {
		t.Fatalf("high=96 should be 400, got %d", code)
	}
	code, _ = do("PUT", "/api/resource", map[string]any{"threshold_high": 90, "threshold_critical": 88})
	if code != http.StatusBadRequest {
		t.Fatalf("critical<=high should be 400, got %d", code)
	}
	code, _ = do("PUT", "/api/resource", map[string]any{"mode": "turbo"})
	if code != http.StatusBadRequest {
		t.Fatalf("bad mode should be 400, got %d", code)
	}
	code, _ = do("PUT", "/api/resource", map[string]any{"manual": map[string]any{"scanner_scale": 2}})
	if code != http.StatusBadRequest {
		t.Fatalf("scanner_scale=2 should be 400, got %d", code)
	}
	code, _ = do("PUT", "/api/resource", map[string]any{"manual": map[string]any{"workers": 0}})
	if code != http.StatusBadRequest {
		t.Fatalf("manual workers=0 should be 400, got %d", code)
	}
	if got := s.loadResourceSettings().High; got != defaultResourceHigh {
		t.Fatalf("invalid PUT changed threshold_high to %d", got)
	}
	// 部分字段非法时整单不落库：auto_tune 不能被顺手改掉。
	if code, _ = do("PUT", "/api/resource", map[string]any{"auto_tune": false}); code != 200 {
		t.Fatalf("reset auto_tune = %d", code)
	}
	code, _ = do("PUT", "/api/resource", map[string]any{
		"auto_tune": true, "manual": map[string]any{"workers": 0},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("invalid manual with auto_tune=true should be 400, got %d", code)
	}
	_, out = do("GET", "/api/resource", nil)
	if got := settingsOf(out)["auto_tune"]; got != false {
		t.Fatalf("invalid PUT must not persist auto_tune, got %v", got)
	}

	// 7) 关闭自动调优但保留手工覆盖：计划仍按 manual 生效（作业闸门可单独启用）。
	code, out = do("PUT", "/api/resource", map[string]any{
		"auto_tune": false, "manual": map[string]any{"max_jobs": 5, "job_max_per_task": 2},
	})
	if code != 200 {
		t.Fatalf("PUT auto off + manual = %d (%v)", code, out)
	}
	p = planOf(out)
	if p["enabled"] != true || p["mode"] != "manual" || p["max_jobs"] != float64(5) || p["job_max_per_task"] != float64(2) {
		t.Fatalf("manual plan while auto off mismatch: %v", p)
	}
	if !resource.JobGateEnabled() {
		t.Fatalf("manual max_jobs should enable job gate")
	}

	// 8) 全部关闭：计划中性，作业闸门关闭，扫描不缩放。
	code, out = do("PUT", "/api/resource", map[string]any{
		"auto_tune": false, "mode": "auto", "manual": map[string]any{},
	})
	if code != 200 {
		t.Fatalf("PUT reset = %d (%v)", code, out)
	}
	p = planOf(out)
	if p["enabled"] != false || p["max_jobs"] != float64(0) {
		t.Fatalf("reset plan not neutral: %v", p)
	}
	if resource.JobGateEnabled() || resource.ScannerScaleNow() != 1 {
		t.Fatalf("reset must disable gate and scale")
	}

	// 9) retune 往返。
	if code, out = do("POST", "/api/resource/retune", map[string]any{}); code != 200 {
		t.Fatalf("POST retune = %d (%v)", code, out)
	}
	if _, ok := out["capacity"].(map[string]any); !ok {
		t.Fatalf("retune response missing capacity: %v", out)
	}
}

func TestValidateResourceManual(t *testing.T) {
	i := func(n int) *int { return &n }
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name string
		m    resource.Manual
		ok   bool
	}{
		{"empty", resource.Manual{}, true},
		{"workers ok", resource.Manual{Workers: i(1)}, true},
		{"workers high", resource.Manual{Workers: i(65)}, false},
		{"max jobs low", resource.Manual{MaxJobs: i(0)}, false},
		{"job mem low", resource.Manual{JobMemoryMB: i(1)}, false},
		{"scale low", resource.Manual{ScannerScale: f(0.05)}, false},
		{"scale high", resource.Manual{ScannerScale: f(1.5)}, false},
		{"scale ok", resource.Manual{ScannerScale: f(0.3)}, true},
		{"procs high", resource.Manual{GOMAXPROCS: i(257)}, false},
		{"cracker ok", resource.Manual{CrackerWorkload: i(2), CrackerRuntimeS: i(1800)}, true},
		{"cracker runtime low", resource.Manual{CrackerRuntimeS: i(10)}, false},
	}
	for _, tc := range cases {
		err := validateResourceManual(tc.m)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err=%v want ok=%v", tc.name, err, tc.ok)
		}
	}
}
