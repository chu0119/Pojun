//go:build linux

package resource

import (
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

// Detect 探测本机/容器容量。所有失败都转成 warning，不返回 error：启动流程
// 不能因为探测不到某个维度就停下。
func Detect(dataDir string) Capacity {
	procRoot := envPath("POJUN_RESOURCE_PROC", "/proc")
	sysRoot := envPath("POJUN_RESOURCE_SYSFS", "/sys/fs/cgroup")
	c := readCapacity(procRoot, sysRoot, float64(runtime.NumCPU()))
	if dataDir != "" {
		if st, ok := statfsNearest(dataDir); ok {
			c.DiskTotal = st.Total
			c.DiskFree = st.Free
		} else {
			c.Warnings = append(c.Warnings, "磁盘容量未探测：数据目录不可访问（"+dataDir+"），不影响启动，仅计划里缺少磁盘依据")
		}
	}
	if c.DetectedAt.IsZero() {
		c.DetectedAt = time.Now()
	}
	return c
}

// statfsNearest 从目标目录向上找到第一个可 statfs 的祖先。启动早期 data/ 可能
// 还没创建，直接 statfs 会失败并让磁盘维度永久缺失。
func statfsNearest(path string) (struct{ Total, Free uint64 }, bool) {
	for i := 0; i < 8; i++ {
		if st, err := statfs(path); err == nil {
			return st, true
		}
		parent := filepath.Dir(path)
		if parent == path || parent == "" || parent == "." {
			break
		}
		path = parent
	}
	return struct{ Total, Free uint64 }{}, false
}

func statfs(path string) (struct{ Total, Free uint64 }, error) {
	var out struct{ Total, Free uint64 }
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return out, err
	}
	bs := uint64(st.Bsize)
	out.Total = st.Blocks * bs
	out.Free = st.Bavail * bs
	return out, nil
}
