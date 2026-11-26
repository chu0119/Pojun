import type { ResourceManual, ResourcePlan } from "./types";

/** 字节数转人读大小；0/负数/缺失显示 "—"（探测失败时不要显示 0GB 误导）。 */
export function formatResourceBytes(v?: number | null): string {
  if (!v || v <= 0) return "—";
  const g = 1024 ** 3;
  if (v >= g) {
    const n = v / g;
    return `${n >= 10 ? Math.round(n) : Math.round(n * 10) / 10}G`;
  }
  const m = v / 1024 ** 2;
  return `${Math.max(1, Math.round(m))}M`;
}

/** 计划是否真正生效；老后端/未初始化时按 false 处理。 */
export function isResourcePlanActive(plan?: ResourcePlan | null): boolean {
  return !!plan?.enabled;
}

/** 校验两个阈值；返回错误文案，合法返回 null。 */
export function validateResourceThresholds(high: number, critical: number): string | null {
  if (!Number.isInteger(high) || high < 50 || high > 95) return "节流阈值必须在 50–95 之间";
  if (!Number.isInteger(critical) || critical < 60 || critical > 99) return "暂停阈值必须在 60–99 之间";
  if (critical <= high) return "暂停阈值必须大于节流阈值（critical > high）";
  return null;
}

type ManualField = keyof ResourceManual;
type ManualInput = Record<string, string>;

const MANUAL_INT_FIELDS: Record<string, { key: ManualField; lo: number; hi: number; label: string }> = {
  workers: { key: "workers", lo: 1, hi: 64, label: "worker 覆盖" },
  max_jobs: { key: "max_jobs", lo: 1, hi: 128, label: "作业上限覆盖" },
  job_max_per_task: { key: "job_max_per_task", lo: 1, hi: 32, label: "单任务作业上限覆盖" },
  job_memory_mb: { key: "job_memory_mb", lo: 128, hi: 131072, label: "单作业内存覆盖" },
  job_cpu_percent: { key: "job_cpu_percent", lo: 10, hi: 6400, label: "单作业 CPU 覆盖" },
  gomemlimit_mb: { key: "gomemlimit_mb", lo: 256, hi: 262144, label: "GOMEMLIMIT 覆盖" },
  gomaxprocs: { key: "gomaxprocs", lo: 1, hi: 256, label: "GOMAXPROCS 覆盖" },
  cracker_workload: { key: "cracker_workload", lo: 1, hi: 3, label: "破解负载覆盖" },
  cracker_runtime_s: { key: "cracker_runtime_s", lo: 60, hi: 86400, label: "破解时长覆盖" },
};

/**
 * 把表单里的字符串覆盖项归一成 ResourceManual。
 * 留空 = 不覆盖；非数字/越界返回 error，不把坏值发给后端。
 */
export function normalizeResourceManual(input: ManualInput): { manual: ResourceManual; error?: string } {
  const manual: ResourceManual = {};
  for (const [name, raw] of Object.entries(input)) {
    if (name === "scanner_scale") {
      if (!raw.trim()) continue;
      const n = Number(raw);
      if (!Number.isFinite(n) || n < 0.1 || n > 1) {
        return { manual: {}, error: "扫描缩放覆盖必须在 0.1–1 之间（留空表示跟随自动计划）" };
      }
      manual.scanner_scale = n;
      continue;
    }
    const field = MANUAL_INT_FIELDS[name];
    if (!field) continue;
    if (!raw.trim()) continue;
    const n = Number(raw);
    if (!Number.isInteger(n) || n < field.lo || n > field.hi) {
      return { manual: {}, error: `${field.label} 必须是 ${field.lo}–${field.hi} 的整数（留空表示跟随自动计划）` };
    }
    (manual as Record<string, number>)[field.key] = n;
  }
  return { manual };
}

/** 计划里真正会生效的 worker 数：显式配置优先，其次计划，最后默认 3。 */
export function effectiveWorkersLabel(plan?: ResourcePlan | null, effectiveWorkers?: number): string {
  if (effectiveWorkers && effectiveWorkers > 0) return String(effectiveWorkers);
  if (plan?.workers && plan.workers > 0) return String(plan.workers);
  return "3";
}
