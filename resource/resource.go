// Package resource 探测宿主/容器的真实容量，并据此推导保守的资源计划。
//
// 设计约束（公开版下放）：探测永远是只读的，失败只降级对应维度并留下可操作
// 警告，绝不阻断启动；计划只有在设置页显式开启「资源自适应」或填写了手工覆盖
// 后才生效，未开启时所有调用方看到的都是中性值（不限制作业、不缩放扫描）。
// 这样升级前后公开版的默认行为完全一致。
package resource

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Capacity 是一次资源探测的结果。MemTotal/MemCurrent 在容器内取 cgroup 限额。
// 派生字段（MemFromCgroup/CPUFromCgroup）用于设置页解释「依据」。
type Capacity struct {
	CPUCores      float64   `json:"cpu_cores"`
	MemTotal      uint64    `json:"mem_total"`
	MemAvailable  uint64    `json:"mem_available"`
	MemCurrent    uint64    `json:"mem_current"`
	DiskTotal     uint64    `json:"disk_total"`
	DiskFree      uint64    `json:"disk_free"`
	Load1         float64   `json:"load1"`
	CgroupV2      bool      `json:"cgroup_v2"`
	InContainer   bool      `json:"in_container"`
	MemFromCgroup bool      `json:"mem_from_cgroup"`
	CPUFromCgroup bool      `json:"cpu_from_cgroup"`
	Warnings      []string  `json:"warnings,omitempty"`
	DetectedAt    time.Time `json:"detected_at"`
}

// Manual 是用户对自动计划的覆盖项（nil = 不覆盖）。手工覆盖是整对象替换：
// PUT /api/resource 里省略的字段即「恢复跟随自动计划」。
type Manual struct {
	Workers         *int     `json:"workers,omitempty"`
	MaxJobs         *int     `json:"max_jobs,omitempty"`
	JobMaxPerTask   *int     `json:"job_max_per_task,omitempty"`
	JobMemoryMB     *int     `json:"job_memory_mb,omitempty"`
	JobCPUPercent   *int     `json:"job_cpu_percent,omitempty"`
	ScannerScale    *float64 `json:"scanner_scale,omitempty"`
	GoMemLimitMB    *int     `json:"gomemlimit_mb,omitempty"`
	GOMAXPROCS      *int     `json:"gomaxprocs,omitempty"`
	CrackerWorkload *int     `json:"cracker_workload,omitempty"`
	CrackerRuntimeS *int     `json:"cracker_runtime_s,omitempty"`
}

// Plan 是落到运行时各处的资源上限。
type Plan struct {
	// Enabled 表示计划是否真正生效。未开启自动调优且没有手工覆盖时为 false，
	// 所有闸门/缩放都按中性值处理（与升级前完全一致）。
	Enabled         bool      `json:"enabled"`
	Mode            string    `json:"mode"` // auto|manual
	Workers         int       `json:"workers"`
	TaskConcurrency int       `json:"task_concurrency"`
	MaxJobs         int       `json:"max_jobs"`
	JobMaxPerTask   int       `json:"job_max_per_task"`
	JobMemoryMB     int       `json:"job_memory_mb"`
	JobCPUPercent   int       `json:"job_cpu_percent"`
	JobRuntimeS     int       `json:"job_runtime_s"`
	ScannerScale    float64   `json:"scanner_scale"`
	GoMemLimitMB    int       `json:"gomemlimit_mb"`
	GOMAXPROCS      int       `json:"gomaxprocs"`
	CrackerWorkload int       `json:"cracker_workload"`
	CrackerRuntimeS int       `json:"cracker_runtime_s"`
	Reasons         []string  `json:"reasons,omitempty"`
	GeneratedAt     time.Time `json:"generated_at"`
}

var (
	stateMu     sync.RWMutex
	capacity    Capacity
	plan        Plan
	initialized bool
)

// Init 在进程启动早期调用：只探测容量并装上中性计划，不改任何运行时限制。
// 真正按设置生成计划由 server 在读到数据库后调用 Replan（见 applyResourcePlan）。
func Init(dataDir string) Plan {
	c := Detect(dataDir)
	stateMu.Lock()
	capacity = c
	plan = NeutralPlan()
	initialized = true
	p := plan
	stateMu.Unlock()
	return p
}

// NeutralPlan 是「未开启自动调优且没有手工覆盖」时的中性计划：不限制作业、
// 不缩放扫描、不改 Go 运行时。
func NeutralPlan() Plan {
	return Plan{Mode: "manual", ScannerScale: 1, GeneratedAt: time.Now()}
}

// SetCapacity 更新容量快照（不重算计划）。
func SetCapacity(c Capacity) {
	stateMu.Lock()
	capacity = c
	initialized = true
	stateMu.Unlock()
}

// Replan 用新容量 + 手工覆盖重新生成计划。mode 只接受 auto|manual，空值按 auto。
// manual 模式下先取中性计划再套覆盖：用户选择手动就只用手工值。
func Replan(c Capacity, m Manual, mode string) Plan {
	if mode != "auto" && mode != "manual" {
		mode = "auto"
	}
	var p Plan
	if mode == "manual" {
		p = NeutralPlan()
	} else {
		p = Build(c)
	}
	p.Mode = mode
	p.Enabled = mode != "manual" || HasManualOverrides(m)
	applyManual(&p, m)
	if p.Enabled && p.MaxJobs > 0 && p.JobRuntimeS <= 0 {
		p.JobRuntimeS = 6 * 3600
	}
	p.GeneratedAt = time.Now()
	stateMu.Lock()
	capacity = c
	plan = p
	initialized = true
	stateMu.Unlock()
	return p
}

// Snapshot 返回当前容量、计划与是否已初始化。
func Snapshot() (Capacity, Plan, bool) {
	stateMu.RLock()
	defer stateMu.RUnlock()
	return capacity, plan, initialized
}

// PlanNow 返回当前计划（零值表示未初始化）。
func PlanNow() Plan {
	stateMu.RLock()
	defer stateMu.RUnlock()
	return plan
}

// Initialized 报告 Init/Replan 是否已经跑过。
func Initialized() bool {
	stateMu.RLock()
	defer stateMu.RUnlock()
	return initialized
}

// SetPlan 仅供测试/重算使用。
func SetPlan(p Plan) {
	stateMu.Lock()
	plan = p
	initialized = true
	stateMu.Unlock()
}

// Build 按容量推导保守计划（不含手工覆盖）。整机规模分档而不是连续公式：
// 小机器保守、大机器可控，也避免在档位边界抖动。
func Build(c Capacity) Plan {
	effCPU := c.CPUCores
	if effCPU <= 0 {
		effCPU = float64(runtime.NumCPU())
	}
	if effCPU <= 0 {
		effCPU = 2
	}
	effMem := c.MemTotal
	reasons := append([]string{}, c.Warnings...)
	if effMem == 0 {
		effMem = 8 << 30
		reasons = append(reasons, "未探测到内存总量，按 8G 保守估算")
	}
	usableCPU := effCPU * 0.80
	usableMem := float64(effMem) * 0.70

	workers := 2
	switch {
	case effCPU <= 2:
		workers = 2
	case effCPU <= 4:
		workers = 3
	case effCPU <= 8:
		workers = 4
	case effCPU <= 16:
		workers = 6
	default:
		workers = 8
	}
	if effMem <= 4<<30 && workers > 2 {
		workers = 2
	}
	maxJobs := clampInt(workers*2, 2, 12)
	if effCPU <= 2 {
		maxJobs = workers // 2 核机器上每个 worker 一个作业，避免扫描互相抢核
	}
	taskConc := clampInt(workers/2, 1, 4)
	// 单作业内存/CPU 建议值只用于提示与破解器低负载参数，不做硬性 cgroup 限制。
	jobMem := clampInt(int(usableMem/float64(maxJobs)/2)>>20, 256, 8192)
	if capMB := int(usableMem / float64(maxJobs) / float64(1<<20)); capMB > 0 && jobMem > capMB {
		jobMem = capMB
	}
	jobCPU := clampInt(int(usableCPU*100/float64(maxJobs)), 100, 400)
	scale := usableCPU / 8
	if scale > 1 {
		scale = 1
	}
	if scale < 0.3 {
		scale = 0.3
	}
	goMem := clampInt(int(usableMem*0.55)>>20, 512, 28*1024)
	procs := int(math.Ceil(effCPU))
	if procs < 1 {
		procs = 1
	}
	crackW, crackT := 1, 1800
	if effCPU >= 8 && effMem >= 16<<30 {
		crackW, crackT = 2, 3600
	}
	reasons = append(reasons,
		"CPU "+formatFloat(effCPU)+" 核（含 cgroup 限额）",
		"内存 "+formatBytes(effMem)+"（含 cgroup 限额）",
	)
	if c.DiskFree > 0 {
		reasons = append(reasons, "数据目录磁盘剩余 "+formatBytes(c.DiskFree))
	}
	return Plan{
		Enabled: true, Mode: "auto", Workers: workers, TaskConcurrency: taskConc,
		MaxJobs: maxJobs, JobMaxPerTask: clampInt(maxJobs/2, 1, 6),
		JobMemoryMB: jobMem, JobCPUPercent: jobCPU, JobRuntimeS: 6 * 3600,
		ScannerScale: scale, GoMemLimitMB: goMem, GOMAXPROCS: procs,
		CrackerWorkload: crackW, CrackerRuntimeS: crackT,
		Reasons: reasons, GeneratedAt: time.Now(),
	}
}

// applyManual 把手工覆盖应用到计划上。超界值在这里钳制（server 的 API 层
// 会先做显式校验并返回 400，这里只兜底数据库里的脏数据）。
func applyManual(p *Plan, m Manual) {
	if m.Workers != nil && *m.Workers > 0 {
		p.Workers = clampInt(*m.Workers, 1, 64)
	}
	if m.MaxJobs != nil && *m.MaxJobs > 0 {
		p.MaxJobs = clampInt(*m.MaxJobs, 1, 128)
	}
	if m.JobMaxPerTask != nil && *m.JobMaxPerTask > 0 {
		p.JobMaxPerTask = clampInt(*m.JobMaxPerTask, 1, 32)
	}
	if m.JobMemoryMB != nil && *m.JobMemoryMB > 0 {
		p.JobMemoryMB = clampInt(*m.JobMemoryMB, 128, 128*1024)
	}
	if m.JobCPUPercent != nil && *m.JobCPUPercent > 0 {
		p.JobCPUPercent = clampInt(*m.JobCPUPercent, 10, 6400)
	}
	if m.ScannerScale != nil && *m.ScannerScale > 0 {
		p.ScannerScale = clampFloat(*m.ScannerScale, 0.1, 1)
	}
	if m.GoMemLimitMB != nil && *m.GoMemLimitMB > 0 {
		p.GoMemLimitMB = clampInt(*m.GoMemLimitMB, 256, 256*1024)
	}
	if m.GOMAXPROCS != nil && *m.GOMAXPROCS > 0 {
		p.GOMAXPROCS = clampInt(*m.GOMAXPROCS, 1, 256)
	}
	if m.CrackerWorkload != nil && *m.CrackerWorkload > 0 {
		p.CrackerWorkload = clampInt(*m.CrackerWorkload, 1, 3)
	}
	if m.CrackerRuntimeS != nil && *m.CrackerRuntimeS > 0 {
		p.CrackerRuntimeS = clampInt(*m.CrackerRuntimeS, 60, 24*3600)
	}
}

// HasManualOverrides 报告手工覆盖里是否至少有一项有效值。
func HasManualOverrides(m Manual) bool {
	return (m.Workers != nil && *m.Workers > 0) ||
		(m.MaxJobs != nil && *m.MaxJobs > 0) ||
		(m.JobMaxPerTask != nil && *m.JobMaxPerTask > 0) ||
		(m.JobMemoryMB != nil && *m.JobMemoryMB > 0) ||
		(m.JobCPUPercent != nil && *m.JobCPUPercent > 0) ||
		(m.ScannerScale != nil && *m.ScannerScale > 0) ||
		(m.GoMemLimitMB != nil && *m.GoMemLimitMB > 0) ||
		(m.GOMAXPROCS != nil && *m.GOMAXPROCS > 0) ||
		(m.CrackerWorkload != nil && *m.CrackerWorkload > 0) ||
		(m.CrackerRuntimeS != nil && *m.CrackerRuntimeS > 0)
}

// ScannerScaleNow 返回扫描器速率缩放；未启用计划时返回 1（行为不变）。
func ScannerScaleNow() float64 {
	stateMu.RLock()
	defer stateMu.RUnlock()
	if !initialized || !plan.Enabled || plan.ScannerScale <= 0 {
		return 1
	}
	return plan.ScannerScale
}

// NucleiRateLimit 把调用方的基准速率（req/s）按计划缩放；未启用时返回 0，
// 表示调用方不要添加 -rate-limit，命令与升级前完全一致。
func NucleiRateLimit(base int) int {
	if base <= 0 {
		return 0
	}
	scale := ScannerScaleNow()
	if scale >= 1 {
		return 0
	}
	n := int(math.Round(float64(base) * scale))
	if n < 10 {
		n = 10
	}
	if n > base {
		n = base
	}
	return n
}

// MemoryTotal 返回探测到的内存总量（0=未知）。
func MemoryTotal() uint64 {
	stateMu.RLock()
	defer stateMu.RUnlock()
	return capacity.MemTotal
}

// JobGateEnabled 报告后台作业闸门是否生效。
func JobGateEnabled() bool {
	stateMu.RLock()
	defer stateMu.RUnlock()
	return initialized && plan.Enabled && plan.MaxJobs > 0
}

// ---- 作业闸门 ----

// jobQueueTimeout 是排队等槽位的上限：长时间拿不到槽位说明并发配置不合理，
// 返回可行动的错误比让工具调用无限挂起更好。
const jobQueueTimeout = 10 * time.Minute

var jobGate = struct {
	mu       sync.Mutex
	inflight int
	perTask  map[int64]int
}{perTask: map[int64]int{}}

// AcquireJob 获取一个后台作业槽位；超过计划上限时排队等待。
// 返回的 release 必须在作业进程退出后调用（job_start 在 Wait 之后调用）。
// 计划未启用时立即返回空 release，调用方无需分支。
func AcquireJob(ctx context.Context, taskID int64) (func(), error) {
	deadline := time.Now().Add(jobQueueTimeout)
	for {
		p := PlanNow()
		if !Initialized() || !p.Enabled || p.MaxJobs <= 0 {
			return func() {}, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("后台作业排队超过 %s 仍未获得槽位（全局上限 %d，单任务上限 %d）", jobQueueTimeout, p.MaxJobs, p.JobMaxPerTask)
		}
		jobGate.mu.Lock()
		ok := jobGate.inflight < p.MaxJobs
		if ok && p.JobMaxPerTask > 0 && jobGate.perTask[taskID] >= p.JobMaxPerTask {
			ok = false
		}
		if ok {
			jobGate.inflight++
			jobGate.perTask[taskID]++
			jobGate.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					jobGate.mu.Lock()
					jobGate.inflight--
					jobGate.perTask[taskID]--
					if jobGate.perTask[taskID] <= 0 {
						delete(jobGate.perTask, taskID)
					}
					jobGate.mu.Unlock()
				})
			}, nil
		}
		jobGate.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// WrapJobCommand 给后台作业套 nice（Linux），降低与主服务/扫描器的抢核。
// 未启用资源计划时原样返回，保证升级前后完全一致。
func WrapJobCommand(name string, args []string) (string, []string) {
	if !JobGateEnabled() {
		return name, args
	}
	if runtime.GOOS != "linux" {
		return name, args
	}
	if _, err := exec.LookPath("nice"); err != nil {
		return name, args
	}
	out := append([]string{"nice", "-n", "10", name}, args...)
	return out[0], out[1:]
}

var (
	timeoutForegroundOnce sync.Once
	timeoutForegroundOK   bool
)

// TimeoutPrefix 返回套在作业内层命令前的 timeout 前缀（含尾随空格），不支持时返回空串。
// 必须用 --foreground：默认模式下 GNU timeout 会把子命令放进新的进程组，
// 任务暂停/删除时按原进程组 kill 就漏掉真正的扫描进程；--foreground 保持同组。
// BusyBox 等无该选项的 timeout 直接放弃墙钟（返回空串），避免作业启动即报参数错误。
func TimeoutPrefix(secs int) string {
	if secs <= 0 {
		return ""
	}
	path, err := exec.LookPath("timeout")
	if err != nil {
		return ""
	}
	timeoutForegroundOnce.Do(func() {
		out, err := exec.Command(path, "--help").CombinedOutput()
		timeoutForegroundOK = err == nil && strings.Contains(string(out), "--foreground") &&
			strings.Contains(string(out), "--kill-after")
	})
	if !timeoutForegroundOK {
		return ""
	}
	return fmt.Sprintf("timeout --foreground --kill-after=30 %d ", secs)
}

// PlanTimeoutPrefix 只在后台作业闸门生效时返回 timeout 前缀；手工只覆盖
// worker/扫描等字段、没有作业上限时不应顺手给作业加墙钟。
func PlanTimeoutPrefix() string {
	if !JobGateEnabled() {
		return ""
	}
	return TimeoutPrefix(PlanNow().JobRuntimeS)
}

// MarshalPlan 便于把计划写入 settings / 日志。
func MarshalPlan(p Plan) string {
	b, _ := json.Marshal(p)
	return string(b)
}

// ParseManual 解析 settings 里的手工覆盖。坏数据返回空 Manual（等价于跟随自动）。
func ParseManual(raw string) Manual {
	var m Manual
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &m)
	}
	return m
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func formatFloat(v float64) string {
	if v == math.Trunc(v) {
		return strconv.Itoa(int(v))
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

func formatBytes(v uint64) string {
	const g = 1 << 30
	if v >= g {
		return formatFloat(float64(v)/float64(g)) + "G"
	}
	return formatFloat(float64(v)/(1<<20)) + "M"
}
