package resource

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// readCapacity 解析 /proc 与 cgroup 文件（纯文件读取，便于跨平台单测）。
// hostCPU 是宿主逻辑核数；cgroup 限额更小时取限额。
// 任何单个文件读不到都只影响对应维度并追加 warning，不返回 error。
func readCapacity(procRoot, sysRoot string, hostCPU float64) Capacity {
	c := Capacity{CPUCores: hostCPU, DetectedAt: time.Now()}
	if c.CPUCores <= 0 {
		c.CPUCores = 1
	}

	hostMem, hostAvail := readMeminfo(filepath.Join(procRoot, "meminfo"))
	c.MemTotal, c.MemAvailable = hostMem, hostAvail

	cgContent := ""
	if content, err := os.ReadFile(filepath.Join(procRoot, "self", "cgroup")); err == nil {
		cgContent = string(content)
		if strings.Contains(cgContent, "docker") || strings.Contains(cgContent, "kubepods") ||
			strings.Contains(cgContent, "containerd") || strings.Contains(cgContent, "libpod") ||
			strings.Contains(cgContent, "lxc") {
			c.InContainer = true
		}
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		c.InContainer = true
	}
	if v := strings.TrimSpace(os.Getenv("POJUN_RESOURCE_CONTAINER")); v == "1" {
		c.InContainer = true
	}

	// cgroup v2：systemd 服务位于 /system.slice/<unit>（/proc/self/cgroup 给出相对路径），
	// 容器内通常是命名空间根。只读根目录会漏掉 systemd 的 MemoryMax/CPUQuota。
	v2Base := sysRoot
	if rel := cgroupV2Rel(cgContent); rel != "" && rel != "/" {
		if cand, ok := joinCgroup(sysRoot, rel); ok && (fileExists(filepath.Join(cand, "memory.max")) ||
			fileExists(filepath.Join(cand, "cpu.max"))) {
			v2Base = cand
		}
	}
	if memRaw, err := os.ReadFile(filepath.Join(v2Base, "memory.max")); err == nil {
		c.CgroupV2 = true
		if limit := parseUintOrMax(string(memRaw)); limit > 0 && limit < 1<<60 {
			c.MemTotal = minU64(c.MemTotal, limit)
			c.MemFromCgroup = true
		}
		if cur, err := os.ReadFile(filepath.Join(v2Base, "memory.current")); err == nil {
			c.MemCurrent = parseUintOrMax(string(cur))
		}
	}
	if cpuRaw, err := os.ReadFile(filepath.Join(v2Base, "cpu.max")); err == nil {
		c.CgroupV2 = true
		if quotaCores := parseCPUMax(string(cpuRaw)); quotaCores > 0 && quotaCores < c.CPUCores {
			c.CPUCores = quotaCores
			c.CPUFromCgroup = true
		}
	}
	if !c.CgroupV2 {
		readCgroupV1(&c, cgContent, sysRoot)
	}

	if c.MemAvailable > c.MemTotal && c.MemTotal > 0 {
		c.MemAvailable = c.MemTotal
	}
	if c.MemTotal == 0 {
		c.Warnings = append(c.Warnings, "未读到内存总量（缺少 /proc/meminfo 或为非常规内核），计划将按 8G 保守估算")
	}
	if c.InContainer && c.MemTotal > 0 && !c.MemFromCgroup {
		c.Warnings = append(c.Warnings, "容器内未读到 cgroup 内存限额，按宿主内存估算（可能高估）；建议为容器设置 mem_limit")
	}
	if c.InContainer && !c.CPUFromCgroup {
		c.Warnings = append(c.Warnings, "容器内未读到 cgroup CPU 限额，按宿主核数估算（可能高估）；建议为容器设置 cpus 配额")
	}
	if raw, err := os.ReadFile(filepath.Join(procRoot, "loadavg")); err == nil {
		if f := strings.Fields(string(raw)); len(f) > 0 {
			c.Load1, _ = strconv.ParseFloat(f[0], 64)
		}
	}
	return c
}

// readCgroupV1 读取 v1 的 memory/cpu 控制器。分控制器解析，某个控制器缺失
// 只降级该维度。
func readCgroupV1(c *Capacity, cgContent, sysRoot string) {
	memBase := filepath.Join(sysRoot, "memory")
	if rel := cgroupV1Rel(cgContent, "memory"); rel != "" && rel != "/" {
		if cand, ok := joinCgroup(memBase, rel); ok && fileExists(filepath.Join(cand, "memory.limit_in_bytes")) {
			memBase = cand
		}
	}
	if raw, err := os.ReadFile(filepath.Join(memBase, "memory.limit_in_bytes")); err == nil {
		if limit := parseUintOrMax(string(raw)); limit > 0 && limit < 1<<60 {
			c.MemTotal = minU64(c.MemTotal, limit)
			c.MemFromCgroup = true
			c.InContainer = true
		}
		if cur, err := os.ReadFile(filepath.Join(memBase, "memory.usage_in_bytes")); err == nil {
			c.MemCurrent = parseUintOrMax(string(cur))
		}
	}
	cpuBase := filepath.Join(sysRoot, "cpu")
	if rel := cgroupV1Rel(cgContent, "cpu"); rel != "" && rel != "/" {
		if cand, ok := joinCgroup(cpuBase, rel); ok && fileExists(filepath.Join(cand, "cpu.cfs_quota_us")) {
			cpuBase = cand
		}
	}
	quotaRaw, qerr := os.ReadFile(filepath.Join(cpuBase, "cpu.cfs_quota_us"))
	periodRaw, perr := os.ReadFile(filepath.Join(cpuBase, "cpu.cfs_period_us"))
	if qerr != nil || perr != nil {
		return
	}
	quota, e1 := strconv.ParseInt(strings.TrimSpace(string(quotaRaw)), 10, 64)
	period, e2 := strconv.ParseInt(strings.TrimSpace(string(periodRaw)), 10, 64)
	if e1 != nil || e2 != nil || quota <= 0 || period <= 0 {
		return
	}
	cores := float64(quota) / float64(period)
	if cores > 0 && cores < c.CPUCores {
		c.CPUCores = cores
		c.CPUFromCgroup = true
		c.InContainer = true
	}
}

// joinCgroup 把 /proc/self/cgroup 里的相对路径安全地拼到控制器根下。
// 拒绝 .. 逃逸（回归文件来自本机，但保持路径拼接可预期，便于单测）。
func joinCgroup(root, rel string) (string, bool) {
	rel = strings.TrimSpace(rel)
	for _, part := range strings.Split(strings.ReplaceAll(rel, "\\", "/"), "/") {
		if part == ".." {
			return "", false
		}
	}
	clean := filepath.Clean("/" + rel)
	if clean == "/" {
		return "", false
	}
	return filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(clean, "/"))), true
}

// cgroupV2Rel 从 /proc/self/cgroup 取 v2 相对路径（`0::/path`）。
func cgroupV2Rel(content string) string {
	for _, line := range strings.Split(content, "\n") {
		f := strings.SplitN(line, ":", 3)
		if len(f) == 3 && f[0] == "0" && f[1] == "" {
			return strings.TrimSpace(f[2])
		}
	}
	return ""
}

// cgroupV1Rel 取指定控制器（memory/cpu）的相对路径。v1 的控制器是逗号列表
// （如 `4:cpu,cpuacct:/path`），必须按列表匹配而不是整串比较。
func cgroupV1Rel(content, controller string) string {
	for _, line := range strings.Split(content, "\n") {
		f := strings.SplitN(line, ":", 3)
		if len(f) != 3 {
			continue
		}
		for _, name := range strings.Split(f[1], ",") {
			if strings.TrimSpace(name) == controller {
				return strings.TrimSpace(f[2])
			}
		}
	}
	return ""
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readMeminfo(path string) (total, avail uint64) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		n, err := strconv.ParseUint(f[1], 10, 64) // kB
		if err != nil {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total = n << 10
		case "MemAvailable:":
			avail = n << 10
		}
	}
	return total, avail
}

// parseUintOrMax 解析 cgroup 的字节数/最大值。`max` 与负数（v1 的 -1）都表示
// 不限制，返回 0。
func parseUintOrMax(s string) uint64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "max" {
		return 0
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// parseCPUMax 解析 v2 的 `quota period`；quota 为 max 时返回 0（不限制）。
func parseCPUMax(s string) float64 {
	f := strings.Fields(strings.TrimSpace(s))
	if len(f) == 0 || f[0] == "max" {
		return 0
	}
	quota, err1 := strconv.ParseFloat(f[0], 64)
	period := 100000.0
	if len(f) > 1 {
		if v, err := strconv.ParseFloat(f[1], 64); err == nil && v > 0 {
			period = v
		}
	}
	if err1 != nil || quota <= 0 || period <= 0 {
		return 0
	}
	return quota / period
}

func minU64(a, b uint64) uint64 {
	if a == 0 {
		return b
	}
	if a < b {
		return a
	}
	return b
}

// envPath 允许用环境变量覆盖 /proc、cgroup 根路径（容器/测试/非常规挂载）。
// 只被 linux 构建的 detect_linux.go 引用；Windows 下看起来“未使用”，不要删。
func envPath(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}
