package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/chu0119/pojun/db"
	"github.com/chu0119/pojun/resource"
)

// 资源自适应：容量探测、计划推导与设置读写。
//
// 公开版的默认值与内部版不同：auto_tune 默认 false。未开启时不会改 worker、
// 不会限制后台作业、不会缩放扫描、不会碰 Go 运行时，升级前后行为一致。
// 用户显式开启或在设置页填写手工覆盖后，计划才生效。
const (
	settingResourceAutoTune  = "resource_auto_tune"
	settingResourceMode      = "resource_mode"
	settingResourceHigh      = "resource_threshold_high"
	settingResourceCritical  = "resource_threshold_critical"
	settingResourceAutoPause = "resource_auto_pause_scans"
	settingResourceManual    = "resource_manual"

	defaultResourceHigh     = 85
	defaultResourceCritical = 95
)

type resourceSettings struct {
	AutoTune  bool            `json:"auto_tune"`
	Mode      string          `json:"mode"`
	High      int             `json:"threshold_high"`
	Critical  int             `json:"threshold_critical"`
	AutoPause bool            `json:"auto_pause_scans"`
	Manual    resource.Manual `json:"manual"`
}

func (s *Server) loadResourceSettings() resourceSettings {
	rs := resourceSettings{
		AutoTune: false, Mode: "auto", High: defaultResourceHigh,
		Critical: defaultResourceCritical, AutoPause: true,
	}
	if s == nil || s.m == nil || s.m.pg == nil {
		return rs
	}
	pg := s.m.pg
	rs.AutoTune = pg.GetBool(settingResourceAutoTune, false)
	rs.AutoPause = pg.GetBool(settingResourceAutoPause, true)
	if v, ok, err := pg.GetSetting(settingResourceMode); err == nil && ok && strings.TrimSpace(v) != "" {
		mode := strings.TrimSpace(v)
		if mode == "auto" || mode == "manual" {
			rs.Mode = mode
		}
	}
	if n, ok := settingIntFromPG(pg, settingResourceHigh); ok {
		rs.High = clampInt(n, 50, 95)
	}
	if n, ok := settingIntFromPG(pg, settingResourceCritical); ok {
		rs.Critical = clampInt(n, 60, 99)
	}
	if rs.Critical <= rs.High {
		rs.Critical = rs.High + 5
		if rs.Critical > 99 {
			rs.Critical = 99
			rs.High = 94
		}
	}
	if raw, ok, err := pg.GetSetting(settingResourceManual); err == nil && ok {
		rs.Manual = resource.ParseManual(raw)
	}
	return rs
}

// settingIntFromPG 读取整数设置；不存在/非法返回 ok=false。
func settingIntFromPG(pg *db.DB, key string) (int, bool) {
	raw, ok, err := pg.GetSetting(key)
	if err != nil || !ok {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, false
	}
	return n, true
}

// applyResourcePlan 重新探测并把计划落到运行时（Go 堆/P 数、worker、作业闸门）。
func (s *Server) applyResourcePlan() (resource.Capacity, resource.Plan, resourceSettings) {
	rs := s.loadResourceSettings()
	dataDir := ""
	if s != nil && s.m != nil {
		dataDir = s.m.dir
	}
	c := resource.Detect(dataDir)
	mode := "auto"
	if !rs.AutoTune || rs.Mode == "manual" {
		mode = "manual"
	}
	p := resource.Replan(c, rs.Manual, mode)
	s.applyGoRuntime(p)
	if p.Enabled {
		log.Printf("[resource] 计划生效 mode=%s workers=%d max_jobs=%d job_max_per_task=%d scanner_scale=%.2f gomemlimit=%dMB gomaxprocs=%d",
			p.Mode, p.Workers, p.MaxJobs, p.JobMaxPerTask, p.ScannerScale, p.GoMemLimitMB, p.GOMAXPROCS)
	} else {
		log.Printf("[resource] 自动调优未开启（容量已探测：CPU=%.2f 内存=%dMB 容器=%v）；不改变 worker/作业/扫描/Go 运行时",
			c.CPUCores, c.MemTotal>>20, c.InContainer)
	}
	if len(c.Warnings) > 0 {
		log.Printf("[resource] 探测降级：%s", strings.Join(c.Warnings, "；"))
	}
	return c, p, rs
}

// captureBaseRuntime 记下进程启动时的 Go 运行时参数。关闭资源自适应时恢复这两
// 个值，而不是假设「默认值 = 不限/NumCPU」——用户可能显式设了 GOMEMLIMIT，Go
// 1.25+ 也可能已按 cgroup 配额算好了 GOMAXPROCS。
func (s *Server) captureBaseRuntime() {
	s.baseGOMAXPROCS = runtime.GOMAXPROCS(0)
	s.baseMemLimit = debug.SetMemoryLimit(-1)
}

// applyGoRuntime 只在计划生效时设置 Go 堆上限与 P 数；关闭时恢复进程启动值。
func (s *Server) applyGoRuntime(p resource.Plan) {
	if p.Enabled {
		if p.GoMemLimitMB > 0 {
			debug.SetMemoryLimit(int64(p.GoMemLimitMB) << 20)
		}
		if p.GOMAXPROCS > 0 {
			runtime.GOMAXPROCS(p.GOMAXPROCS)
		}
		return
	}
	baseMem := int64(math.MaxInt64)
	baseProcs := runtime.NumCPU()
	if s != nil {
		if s.baseMemLimit > 0 {
			baseMem = s.baseMemLimit
		}
		if s.baseGOMAXPROCS > 0 {
			baseProcs = s.baseGOMAXPROCS
		}
	}
	debug.SetMemoryLimit(baseMem)
	runtime.GOMAXPROCS(baseProcs)
}

func (s *Server) resourceStatus() map[string]any {
	c, p, ok := resource.Snapshot()
	rs := s.loadResourceSettings()
	effective := map[string]any{
		"job_gate":      resource.JobGateEnabled(),
		"scanner_scale": resource.ScannerScaleNow(),
		"gomaxprocs":    runtime.GOMAXPROCS(0),
		"gomemlimit_mb": debug.SetMemoryLimit(-1) >> 20,
	}
	if s != nil && s.m != nil {
		effective["workers"] = s.m.Workers()
		on, limit := s.m.ConcurrencyLimit()
		effective["task_concurrency_enabled"] = on
		effective["task_concurrency_limit"] = limit
	}
	return map[string]any{
		"capacity":    c,
		"plan":        p,
		"initialized": ok,
		"settings":    rs,
		"effective":   effective,
		"usage": map[string]any{
			"mem_current": c.MemCurrent,
			"mem_total":   c.MemTotal,
			"load1":       c.Load1,
			"cgroup_v2":   c.CgroupV2,
			"container":   c.InContainer,
		},
		"generated_at": time.Now(),
	}
}

func (s *Server) resourceGet(w http.ResponseWriter, r *http.Request) {
	if s == nil || s.m == nil || s.m.pg == nil {
		writeErr(w, http.StatusServiceUnavailable, "数据库未就绪")
		return
	}
	// GET 时做一次轻量刷新（内存/负载会变），但不重建计划。
	c := resource.Detect(s.m.dir)
	resource.SetCapacity(c)
	writeJSON(w, http.StatusOK, s.resourceStatus())
}

func (s *Server) resourceRetune(w http.ResponseWriter, r *http.Request) {
	if s == nil || s.m == nil || s.m.pg == nil {
		writeErr(w, http.StatusServiceUnavailable, "数据库未就绪")
		return
	}
	s.applyResourcePlan()
	writeJSON(w, http.StatusOK, s.resourceStatus())
}

func (s *Server) resourcePut(w http.ResponseWriter, r *http.Request) {
	if s == nil || s.m == nil || s.m.pg == nil {
		writeErr(w, http.StatusServiceUnavailable, "数据库未就绪")
		return
	}
	var body struct {
		AutoTune  *bool            `json:"auto_tune"`
		Mode      *string          `json:"mode"`
		High      *int             `json:"threshold_high"`
		Critical  *int             `json:"threshold_critical"`
		AutoPause *bool            `json:"auto_pause_scans"`
		Manual    *resource.Manual `json:"manual"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON："+err.Error())
		return
	}
	// 先整体校验再落库：部分字段非法时不能留下「auto_tune 已改、manual 没改」
	// 的半写状态。
	mode := ""
	if body.Mode != nil {
		mode = strings.TrimSpace(*body.Mode)
		if mode != "auto" && mode != "manual" {
			writeErr(w, http.StatusBadRequest, "mode 只能是 auto 或 manual")
			return
		}
	}
	high, critical := 0, 0
	thresholdsChanged := body.High != nil || body.Critical != nil
	if thresholdsChanged {
		rs := s.loadResourceSettings()
		high, critical = rs.High, rs.Critical
		if body.High != nil {
			high = *body.High
		}
		if body.Critical != nil {
			critical = *body.Critical
		}
		if high < 50 || high > 95 {
			writeErr(w, http.StatusBadRequest, "节流阈值 threshold_high 必须在 50–95 之间")
			return
		}
		if critical < 60 || critical > 99 {
			writeErr(w, http.StatusBadRequest, "暂停阈值 threshold_critical 必须在 60–99 之间")
			return
		}
		if critical <= high {
			writeErr(w, http.StatusBadRequest, "暂停阈值必须大于节流阈值（critical > high）")
			return
		}
	}
	if body.Manual != nil {
		if err := validateResourceManual(*body.Manual); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	pg := s.m.pg
	if body.AutoTune != nil {
		if err := pg.SetBool(settingResourceAutoTune, *body.AutoTune); err != nil {
			writeInternalError(w, "api", err)
			return
		}
	}
	if body.AutoPause != nil {
		if err := pg.SetBool(settingResourceAutoPause, *body.AutoPause); err != nil {
			writeInternalError(w, "api", err)
			return
		}
	}
	if mode != "" {
		if err := pg.SetSetting(settingResourceMode, mode); err != nil {
			writeInternalError(w, "api", err)
			return
		}
	}
	if thresholdsChanged {
		if err := pg.SetSetting(settingResourceHigh, strconv.Itoa(high)); err != nil {
			writeInternalError(w, "api", err)
			return
		}
		if err := pg.SetSetting(settingResourceCritical, strconv.Itoa(critical)); err != nil {
			writeInternalError(w, "api", err)
			return
		}
	}
	if body.Manual != nil {
		raw, err := json.Marshal(body.Manual)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "manual 参数格式错误")
			return
		}
		if err := pg.SetSetting(settingResourceManual, string(raw)); err != nil {
			writeInternalError(w, "api", err)
			return
		}
		// worker 覆盖同时写穿常规设置：老库常已有显式 workers，只改计划不改这条
		// 会看起来“没生效”。这是用户显式填写覆盖触发的，不是自动改写。
		if body.Manual.Workers != nil {
			if err := s.m.SetWorkers(*body.Manual.Workers); err != nil {
				writeErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
	}
	s.applyResourcePlan()
	writeJSON(w, http.StatusOK, s.resourceStatus())
}

// validateResourceManual 对手工覆盖做显式范围校验。留空（字段省略）表示跟随
// 自动计划；显式给 0/负数视为非法而不是“取消覆盖”，避免前端误清空。
func validateResourceManual(m resource.Manual) error {
	check := func(name string, v *int, lo, hi int) error {
		if v == nil {
			return nil
		}
		if *v < lo || *v > hi {
			return fmt.Errorf("%s 必须在 %d–%d 之间（留空表示跟随自动计划）", name, lo, hi)
		}
		return nil
	}
	if err := check("workers", m.Workers, 1, 64); err != nil {
		return err
	}
	if err := check("max_jobs", m.MaxJobs, 1, 128); err != nil {
		return err
	}
	if err := check("job_max_per_task", m.JobMaxPerTask, 1, 32); err != nil {
		return err
	}
	if err := check("job_memory_mb", m.JobMemoryMB, 128, 128*1024); err != nil {
		return err
	}
	if err := check("job_cpu_percent", m.JobCPUPercent, 10, 6400); err != nil {
		return err
	}
	if m.ScannerScale != nil && (*m.ScannerScale < 0.1 || *m.ScannerScale > 1) {
		return errors.New("scanner_scale 必须在 0.1–1 之间（1=不缩放）")
	}
	if err := check("gomemlimit_mb", m.GoMemLimitMB, 256, 256*1024); err != nil {
		return err
	}
	if err := check("gomaxprocs", m.GOMAXPROCS, 1, 256); err != nil {
		return err
	}
	if err := check("cracker_workload", m.CrackerWorkload, 1, 3); err != nil {
		return err
	}
	if err := check("cracker_runtime_s", m.CrackerRuntimeS, 60, 24*3600); err != nil {
		return err
	}
	return nil
}

// resourceLoop 每小时重算一次：容器/虚机扩容、systemd 限额调整后不必重启服务。
// 只重算计划，不写系统配置；失败也不影响主流程。
func (s *Server) resourceLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.applyResourcePlan()
		}
	}
}
