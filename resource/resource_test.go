package resource

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func memInfo(totalKB, availKB int) string {
	return "MemTotal:       " + itoa(totalKB) + " kB\nMemAvailable:   " + itoa(availKB) + " kB\n"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [24]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestReadCapacityCgroupV2ServicePath(t *testing.T) {
	proc := t.TempDir()
	sys := t.TempDir()
	writeFixture(t, filepath.Join(proc, "meminfo"), memInfo(8*1024*1024, 6*1024*1024))
	writeFixture(t, filepath.Join(proc, "self", "cgroup"), "0::/system.slice/pojun.service\n")
	writeFixture(t, filepath.Join(proc, "loadavg"), "1.25 1.00 0.75 2/100 1234\n")
	base := filepath.Join(sys, "system.slice", "pojun.service")
	writeFixture(t, filepath.Join(base, "memory.max"), "2147483648\n")
	writeFixture(t, filepath.Join(base, "memory.current"), "536870912\n")
	writeFixture(t, filepath.Join(base, "cpu.max"), "150000 100000\n")

	c := readCapacity(proc, sys, 8)
	if !c.CgroupV2 || !c.MemFromCgroup || !c.CPUFromCgroup {
		t.Fatalf("cgroup flags not set: %+v", c)
	}
	if c.MemTotal != 2<<30 {
		t.Fatalf("mem total = %d, want 2GiB (cgroup limit)", c.MemTotal)
	}
	if c.MemCurrent != 512<<20 {
		t.Fatalf("mem current = %d, want 512MiB", c.MemCurrent)
	}
	if c.CPUCores != 1.5 {
		t.Fatalf("cpu cores = %v, want 1.5", c.CPUCores)
	}
	if c.Load1 != 1.25 {
		t.Fatalf("load1 = %v", c.Load1)
	}
}

func TestReadCapacityCgroupV2RootUnlimited(t *testing.T) {
	proc := t.TempDir()
	sys := t.TempDir()
	writeFixture(t, filepath.Join(proc, "meminfo"), memInfo(4*1024*1024, 2*1024*1024))
	writeFixture(t, filepath.Join(proc, "self", "cgroup"), "0::/\n")
	writeFixture(t, filepath.Join(sys, "memory.max"), "max\n")
	writeFixture(t, filepath.Join(sys, "cpu.max"), "max 100000\n")

	c := readCapacity(proc, sys, 4)
	if !c.CgroupV2 {
		t.Fatalf("want cgroup v2 detected from max files")
	}
	if c.MemFromCgroup || c.CPUFromCgroup {
		t.Fatalf("unlimited cgroup must not be treated as a limit: %+v", c)
	}
	if c.MemTotal != 4<<30 || c.CPUCores != 4 {
		t.Fatalf("got mem=%d cpu=%v, want host values", c.MemTotal, c.CPUCores)
	}
}

func TestReadCapacityCgroupV1Controllers(t *testing.T) {
	proc := t.TempDir()
	sys := t.TempDir()
	writeFixture(t, filepath.Join(proc, "meminfo"), memInfo(16*1024*1024, 12*1024*1024))
	writeFixture(t, filepath.Join(proc, "self", "cgroup"),
		"12:memory:/docker/abc\n4:cpu,cpuacct:/docker/abc\n0::/\n")
	writeFixture(t, filepath.Join(sys, "memory", "docker", "abc", "memory.limit_in_bytes"), "3221225472\n")
	writeFixture(t, filepath.Join(sys, "memory", "docker", "abc", "memory.usage_in_bytes"), "1073741824\n")
	writeFixture(t, filepath.Join(sys, "cpu", "docker", "abc", "cpu.cfs_quota_us"), "200000\n")
	writeFixture(t, filepath.Join(sys, "cpu", "docker", "abc", "cpu.cfs_period_us"), "100000\n")

	c := readCapacity(proc, sys, 8)
	if c.CgroupV2 {
		t.Fatalf("v1 fixture must not be reported as v2")
	}
	if !c.InContainer || !c.MemFromCgroup || !c.CPUFromCgroup {
		t.Fatalf("v1 flags not set: %+v", c)
	}
	if c.MemTotal != 3<<30 {
		t.Fatalf("mem total = %d, want 3GiB", c.MemTotal)
	}
	if c.CPUCores != 2 {
		t.Fatalf("cpu cores = %v, want 2", c.CPUCores)
	}
	if c.MemCurrent != 1<<30 {
		t.Fatalf("mem current = %d", c.MemCurrent)
	}
}

func TestReadCapacityV1UnlimitedQuota(t *testing.T) {
	proc := t.TempDir()
	sys := t.TempDir()
	writeFixture(t, filepath.Join(proc, "meminfo"), memInfo(8*1024*1024, 4*1024*1024))
	writeFixture(t, filepath.Join(proc, "self", "cgroup"), "5:memory:/x\n5:cpu:/x\n")
	writeFixture(t, filepath.Join(sys, "cpu", "x", "cpu.cfs_quota_us"), "-1\n")
	writeFixture(t, filepath.Join(sys, "cpu", "x", "cpu.cfs_period_us"), "100000\n")
	writeFixture(t, filepath.Join(sys, "memory", "x", "memory.limit_in_bytes"), "9223372036854771712\n")

	c := readCapacity(proc, sys, 8)
	if c.CPUFromCgroup || c.MemFromCgroup {
		t.Fatalf("unlimited v1 values must be ignored: %+v", c)
	}
	if c.CPUCores != 8 || c.MemTotal != 8<<30 {
		t.Fatalf("host fallback mismatch: cpu=%v mem=%d", c.CPUCores, c.MemTotal)
	}
}

func TestReadCapacityDegradesWithoutProc(t *testing.T) {
	c := readCapacity(filepath.Join(t.TempDir(), "missing"), filepath.Join(t.TempDir(), "missing"), 0)
	if c.CPUCores != 1 {
		t.Fatalf("cpu cores = %v, want 1 fallback", c.CPUCores)
	}
	if c.MemTotal != 0 {
		t.Fatalf("mem total = %d, want 0 (unknown)", c.MemTotal)
	}
	if len(c.Warnings) == 0 || !strings.Contains(strings.Join(c.Warnings, "|"), "内存总量") {
		t.Fatalf("missing meminfo must produce an actionable warning, got %v", c.Warnings)
	}
}

func TestReadCapacityDoesNotEscapeCgroupRoot(t *testing.T) {
	proc := t.TempDir()
	sys := t.TempDir()
	writeFixture(t, filepath.Join(proc, "meminfo"), memInfo(1024*1024, 512*1024))
	writeFixture(t, filepath.Join(proc, "self", "cgroup"), "0::/../../etc\n")
	writeFixture(t, filepath.Join(sys, "memory.max"), "1073741824\n")

	c := readCapacity(proc, sys, 2)
	if c.MemTotal != 1<<30 {
		t.Fatalf("mem total = %d, want root memory.max fallback", c.MemTotal)
	}
	if _, ok := joinCgroup(sys, "../../etc"); ok {
		t.Fatalf("joinCgroup must reject parent escapes")
	}
}

func TestBuildTierTable(t *testing.T) {
	cases := []struct {
		cpu   float64
		memGB uint64
		want  int
	}{
		{1, 2, 2},
		{2, 4, 2},
		{4, 8, 3},
		{8, 16, 4},
		{16, 32, 6},
		{32, 64, 8},
	}
	for _, tc := range cases {
		p := Build(Capacity{CPUCores: tc.cpu, MemTotal: tc.memGB << 30})
		if !p.Enabled || p.Mode != "auto" {
			t.Fatalf("cpu=%v: plan must be enabled/auto: %+v", tc.cpu, p)
		}
		if p.Workers != tc.want {
			t.Fatalf("cpu=%v mem=%dG: workers=%d want %d", tc.cpu, tc.memGB, p.Workers, tc.want)
		}
		usableMem := float64(tc.memGB<<30) * 0.70
		if p.JobMemoryMB <= 0 || float64(p.JobMemoryMB)*float64(1<<20) > usableMem/float64(p.MaxJobs) {
			t.Fatalf("cpu=%v: job memory %dMB exceeds per-job budget", tc.cpu, p.JobMemoryMB)
		}
		if p.GoMemLimitMB <= 0 || uint64(p.GoMemLimitMB)<<20 > uint64(usableMem*0.55)+1<<20 {
			t.Fatalf("cpu=%v: gomemlimit %dMB above 55%% usable", tc.cpu, p.GoMemLimitMB)
		}
		if p.ScannerScale < 0.3 || p.ScannerScale > 1 {
			t.Fatalf("cpu=%v: scanner scale %v out of range", tc.cpu, p.ScannerScale)
		}
		if p.GOMAXPROCS < 1 || p.TaskConcurrency < 1 || p.MaxJobs < 1 {
			t.Fatalf("cpu=%v: zero-valued plan fields: %+v", tc.cpu, p)
		}
		if p.MaxJobs < p.Workers && tc.cpu > 2 {
			t.Fatalf("cpu=%v: max_jobs %d below workers %d", tc.cpu, p.MaxJobs, p.Workers)
		}
	}
}

func TestBuildUnknownMemoryFallsBack(t *testing.T) {
	p := Build(Capacity{CPUCores: 8})
	if p.Workers == 0 || p.GoMemLimitMB == 0 {
		t.Fatalf("unknown memory must still produce a plan: %+v", p)
	}
	joined := strings.Join(p.Reasons, "|")
	if !strings.Contains(joined, "8G") {
		t.Fatalf("reasons must explain the 8G fallback: %v", p.Reasons)
	}
}

func TestBuildSmallMemoryCapsWorkers(t *testing.T) {
	p := Build(Capacity{CPUCores: 16, MemTotal: 4 << 30})
	if p.Workers != 2 {
		t.Fatalf("4G host must cap workers at 2, got %d", p.Workers)
	}
	if p.MaxJobs != 4 {
		t.Fatalf("max jobs = %d, want 4", p.MaxJobs)
	}
}

func TestReplanModes(t *testing.T) {
	c := Capacity{CPUCores: 8, MemTotal: 16 << 30}

	auto := Replan(c, Manual{}, "auto")
	if !auto.Enabled || auto.Workers == 0 || auto.ScannerScale == 0 {
		t.Fatalf("auto plan must be enabled: %+v", auto)
	}

	manualEmpty := Replan(c, Manual{}, "manual")
	if manualEmpty.Enabled {
		t.Fatalf("manual mode without overrides must stay neutral: %+v", manualEmpty)
	}
	if manualEmpty.MaxJobs != 0 || manualEmpty.ScannerScale != 1 || manualEmpty.GOMAXPROCS != 0 {
		t.Fatalf("neutral plan fields changed: %+v", manualEmpty)
	}

	maxJobs := 4
	scale := 0.5
	manual := Replan(c, Manual{MaxJobs: &maxJobs, ScannerScale: &scale}, "manual")
	if !manual.Enabled || manual.MaxJobs != 4 || manual.ScannerScale != 0.5 {
		t.Fatalf("manual overrides not applied: %+v", manual)
	}
}

func TestApplyManualClamps(t *testing.T) {
	workers := 999
	scale := 9.0
	procs := 999
	p := applyManualForTest(Build(Capacity{CPUCores: 8, MemTotal: 16 << 30}), Manual{
		Workers: &workers, ScannerScale: &scale, GOMAXPROCS: &procs,
	})
	if p.Workers != 64 || p.ScannerScale != 1 || p.GOMAXPROCS != 256 {
		t.Fatalf("manual values not clamped: %+v", p)
	}
}

// applyManualForTest 暴露内部的 applyManual 给同包测试，生产代码不要用。
func applyManualForTest(p Plan, m Manual) Plan {
	applyManual(&p, m)
	return p
}

func TestHasManualOverrides(t *testing.T) {
	if HasManualOverrides(Manual{}) {
		t.Fatalf("empty manual must be false")
	}
	n := 0
	if HasManualOverrides(Manual{MaxJobs: &n}) {
		t.Fatalf("zero override must be ignored")
	}
	n = 1
	if !HasManualOverrides(Manual{MaxJobs: &n}) {
		t.Fatalf("positive override must count")
	}
}

func TestPlanSerializationRoundTrip(t *testing.T) {
	p := Build(Capacity{CPUCores: 4, MemTotal: 8 << 30})
	raw := MarshalPlan(p)
	if !strings.Contains(raw, `"enabled":true`) || !strings.Contains(raw, `"mode":"auto"`) {
		t.Fatalf("marshaled plan missing fields: %s", raw)
	}
	m := ParseManual(`{"workers":5,"scanner_scale":0.6,"max_jobs":7}`)
	if m.Workers == nil || *m.Workers != 5 || m.ScannerScale == nil || *m.ScannerScale != 0.6 || m.MaxJobs == nil || *m.MaxJobs != 7 {
		t.Fatalf("parse manual: %+v", m)
	}
	if bad := ParseManual("{not json"); HasManualOverrides(bad) {
		t.Fatalf("invalid manual must parse to empty")
	}
}

func resetGate() {
	jobGate.mu.Lock()
	jobGate.inflight = 0
	jobGate.perTask = map[int64]int{}
	jobGate.mu.Unlock()
}

func TestJobGateDisabledIsNoop(t *testing.T) {
	resetGate()
	SetPlan(NeutralPlan())
	defer SetPlan(NeutralPlan())
	release, err := AcquireJob(context.Background(), 1)
	if err != nil {
		t.Fatalf("disabled gate must not error: %v", err)
	}
	release()
	release() // idempotent
	if jobGate.inflight != 0 {
		t.Fatalf("disabled gate changed inflight: %d", jobGate.inflight)
	}
}

func TestJobGateGlobalAndPerTaskLimits(t *testing.T) {
	resetGate()
	SetPlan(Plan{Enabled: true, MaxJobs: 2, JobMaxPerTask: 1, GeneratedAt: time.Now()})
	defer func() { resetGate(); SetPlan(NeutralPlan()) }()

	r1, err := AcquireJob(context.Background(), 1)
	if err != nil {
		t.Fatalf("first slot: %v", err)
	}
	defer r1()
	r2, err := AcquireJob(context.Background(), 2)
	if err != nil {
		t.Fatalf("second slot: %v", err)
	}

	// 全局已满：第三个槽位必须排队，ctx 取消后返回错误。
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := AcquireJob(ctx, 3); err == nil {
		t.Fatalf("third slot must block while global limit is reached")
	}

	// 释放一个后可以拿到；再验证单任务上限。
	r2()
	r2again, err := AcquireJob(context.Background(), 2)
	if err != nil {
		t.Fatalf("slot after release: %v", err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel2()
	if _, err := AcquireJob(ctx2, 2); err == nil {
		t.Fatalf("per-task limit must block a second job for task 2")
	}
	r2again()

	// 全局上限之外的释放在关闭计划后也要能安全调用。
	SetPlan(NeutralPlan())
	r1()
	r1()
	resetGate()
}

func TestScannerScaleAndNucleiRate(t *testing.T) {
	SetPlan(NeutralPlan())
	defer SetPlan(NeutralPlan())
	if got := ScannerScaleNow(); got != 1 {
		t.Fatalf("disabled scanner scale = %v, want 1", got)
	}
	if got := NucleiRateLimit(150); got != 0 {
		t.Fatalf("disabled nuclei rate = %d, want 0 (no flag)", got)
	}

	SetPlan(Plan{Enabled: true, ScannerScale: 0.5})
	if got := ScannerScaleNow(); got != 0.5 {
		t.Fatalf("scanner scale = %v", got)
	}
	if got := NucleiRateLimit(150); got != 75 {
		t.Fatalf("nuclei rate = %d, want 75", got)
	}
	SetPlan(Plan{Enabled: true, ScannerScale: 0.01})
	if got := NucleiRateLimit(150); got != 10 {
		t.Fatalf("nuclei rate floor = %d, want 10", got)
	}
	SetPlan(Plan{Enabled: true, ScannerScale: 1})
	if got := NucleiRateLimit(150); got != 0 {
		t.Fatalf("full-scale rate must not add a flag, got %d", got)
	}
}

func TestWrapJobCommandDisabledPassthrough(t *testing.T) {
	SetPlan(NeutralPlan())
	defer SetPlan(NeutralPlan())
	name, args := WrapJobCommand("bash", []string{"-c", "true"})
	if name != "bash" || len(args) != 2 {
		t.Fatalf("disabled wrap changed command: %s %v", name, args)
	}
}

func TestJobGateEnabled(t *testing.T) {
	SetPlan(NeutralPlan())
	if JobGateEnabled() {
		t.Fatalf("neutral plan must not enable job gate")
	}
	SetPlan(Plan{Enabled: true, MaxJobs: 0})
	if JobGateEnabled() {
		t.Fatalf("zero max jobs must not enable job gate")
	}
	SetPlan(Plan{Enabled: true, MaxJobs: 1})
	if !JobGateEnabled() {
		t.Fatalf("enabled plan with max jobs must enable job gate")
	}
	SetPlan(NeutralPlan())
}
