package agent

import (
	"fmt"
	"strings"

	"github.com/chu0119/pojun/resource"
)

// ResourceCapabilityNote 把当前生效的资源计划翻译成给模型看的命令参数建议，
// 追加到 Bash 工具描述。与 EnvCapabilityNote 不同，它每次调用都重新读取计划
// （设置页改完/每小时重算后无需重启），且计划未启用时返回空串——默认部署的
// Bash 描述与升级前逐字一致。
//
// 这里只做「建议」不做命令改写：公开版没有内部版的 opsec 命令重写层，硬改用户
// 手敲的命令会影响审计与可复现性，也容易踩坏工具参数。真正强制的闸门是
// job_start 的全局/单任务上限（resource.AcquireJob），以及 scan_with_nuclei
// 自动带的 -rate-limit。
func ResourceCapabilityNote() string {
	p := resource.PlanNow()
	if !p.Enabled {
		return ""
	}
	var lines []string
	if p.MaxJobs > 0 {
		lines = append(lines, fmt.Sprintf(
			"后台作业上限：全局 %d 个、单任务 %d 个；超过时 job_start 会排队，不要把长任务拆成一堆并行 job。",
			p.MaxJobs, p.JobMaxPerTask))
	}
	if p.JobRuntimeS > 0 {
		lines = append(lines, fmt.Sprintf("单个后台作业墙钟上限约 %d 分钟，超时会被终止并写回退出码。", p.JobRuntimeS/60))
	}
	if scale := resource.ScannerScaleNow(); scale < 0.999 {
		if rl := resource.NucleiRateLimit(150); rl > 0 {
			lines = append(lines, fmt.Sprintf(
				"扫描限速：手工 nuclei 加 `-rate-limit %d`（scan_with_nuclei 会自动带），ffuf 用 `-rate %d -t 10`，不要开高并发。",
				rl, rl))
		}
	}
	if p.CrackerWorkload > 0 && p.CrackerRuntimeS > 0 {
		lines = append(lines, fmt.Sprintf(
			"离线破解低负载参数：hashcat 用 `-w %d --runtime %d`，john 用 `--max-run-time=%d`；避免长时间占满 CPU/GPU。",
			p.CrackerWorkload, p.CrackerRuntimeS, p.CrackerRuntimeS))
	}
	if len(lines) == 0 {
		return ""
	}
	return "\n\n**资源自适应**（当前机器的容量计划，已生效）：\n- " + strings.Join(lines, "\n- ")
}
