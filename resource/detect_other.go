//go:build !linux

package resource

import (
	"runtime"
	"time"
)

// Detect 在非 Linux 平台只探测 CPU 核数。内存/磁盘/负载没有可移植的免费探测
// 手段：这里如实标记为未知并让 Build 按 8G 保守估算，而不是假装探测成功。
func Detect(dataDir string) Capacity {
	return Capacity{
		CPUCores:   float64(runtime.NumCPU()),
		Warnings:   []string{"非 Linux 平台：未探测内存/磁盘/负载，内存按 8G 保守估算；资源计划仅供参考"},
		DetectedAt: time.Now(),
	}
}
