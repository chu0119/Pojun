"use client";

import * as React from "react";

import { ActivityIcon, Loader2Icon, RefreshCwIcon, ServerIcon, TriangleAlertIcon } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { api } from "@/lib/api";
import {
  effectiveWorkersLabel,
  formatResourceBytes,
  normalizeResourceManual,
  validateResourceThresholds,
} from "@/lib/resource-plan";
import type { ResourceStatus } from "@/lib/types";

function planBadgeLabel(plan?: ResourceStatus["plan"] | null): string {
  if (!plan) return "";
  if (!plan.enabled) return "仅展示（未启用）";
  return plan.mode === "manual" ? "手工覆盖生效" : "自动计划生效";
}

/** 资源自适应设置卡片：容量/计划/依据 + 自动/手动 + 阈值 + 手工覆盖。 */
export function ResourceCard({ className }: { className?: string }) {
  const [status, setStatus] = React.useState<ResourceStatus | null>(null);
  const [loading, setLoading] = React.useState(true);
  const [loadError, setLoadError] = React.useState<string | null>(null);
  const [busy, setBusy] = React.useState<"save" | "retune" | "concurrency" | null>(null);

  const [autoTune, setAutoTune] = React.useState(false);
  const [mode, setMode] = React.useState<"auto" | "manual">("auto");
  const [high, setHigh] = React.useState("85");
  const [critical, setCritical] = React.useState("95");
  const [autoPause, setAutoPause] = React.useState(true);
  const [manualWorkers, setManualWorkers] = React.useState("");
  const [manualMaxJobs, setManualMaxJobs] = React.useState("");
  const [manualScale, setManualScale] = React.useState("");
  const [manualGoMem, setManualGoMem] = React.useState("");

  const applyStatus = React.useCallback((r: ResourceStatus) => {
    // 旧后端/演示模式可能返回 {}：缺字段时按「未就绪」展示，避免整页崩。
    if (!r?.settings || !r.plan || !r.capacity) {
      setStatus(null);
      return;
    }
    setStatus(r);
    setAutoTune(!!r.settings.auto_tune);
    setMode(r.settings.mode === "manual" ? "manual" : "auto");
    setHigh(String(r.settings.threshold_high ?? 85));
    setCritical(String(r.settings.threshold_critical ?? 95));
    setAutoPause(!!r.settings.auto_pause_scans);
    const m = r.settings.manual ?? {};
    setManualWorkers(m.workers ? String(m.workers) : "");
    setManualMaxJobs(m.max_jobs ? String(m.max_jobs) : "");
    setManualScale(m.scanner_scale ? String(m.scanner_scale) : "");
    setManualGoMem(m.gomemlimit_mb ? String(m.gomemlimit_mb) : "");
  }, []);

  const load = React.useCallback(() => {
    setLoading(true);
    setLoadError(null);
    api
      .resourceStatus()
      .then(applyStatus)
      .catch((e) => setLoadError((e as Error).message))
      .finally(() => setLoading(false));
  }, [applyStatus]);

  React.useEffect(() => {
    load();
  }, [load]);

  const save = () => {
    const highN = Number(high);
    const criticalN = Number(critical);
    const err = validateResourceThresholds(highN, criticalN);
    if (err) {
      toast.error(err);
      return;
    }
    const { manual, error } = normalizeResourceManual({
      workers: manualWorkers,
      max_jobs: manualMaxJobs,
      scanner_scale: manualScale,
      gomemlimit_mb: manualGoMem,
    });
    if (error) {
      toast.error(error);
      return;
    }
    setBusy("save");
    api
      .updateResource({
        auto_tune: autoTune,
        mode,
        threshold_high: highN,
        threshold_critical: criticalN,
        auto_pause_scans: autoPause,
        manual,
      })
      .then((r) => {
        applyStatus(r);
        toast.success("已保存资源设置并重新应用计划");
      })
      .catch((e) => toast.error(`保存失败：${(e as Error).message}`))
      .finally(() => setBusy(null));
  };

  const retune = () => {
    setBusy("retune");
    api
      .retuneResource()
      .then((r) => {
        applyStatus(r);
        toast.success("已重新探测容量并应用计划");
      })
      .catch((e) => toast.error(`重新探测失败：${(e as Error).message}`))
      .finally(() => setBusy(null));
  };

  // 任务并发上限有独立开关：只给建议值，点了才写入（不静默改显式配置）。
  const applyTaskConcurrency = () => {
    const limit = status?.plan?.task_concurrency ?? 0;
    if (limit <= 0) return;
    setBusy("concurrency");
    api
      .setSettings({ task_concurrency_enabled: true, task_concurrency_limit: limit })
      .then(() => {
        toast.success(`已启用任务并发上限：${limit}`);
        load();
      })
      .catch((e) => toast.error(`保存失败：${(e as Error).message}`))
      .finally(() => setBusy(null));
  };

  const plan = status?.plan;
  const capacity = status?.capacity;
  const effective = status?.effective;
  const disabled = loading || busy !== null;

  return (
    <Card className={className}>
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <ServerIcon className="size-4" />
          资源自适应
          {plan ? (
            <span className="ml-1 inline-flex items-center gap-1 rounded-full border px-2 py-0.5 font-normal text-muted-foreground text-xs">
              <ActivityIcon className="size-3" />
              {planBadgeLabel(plan)}
            </span>
          ) : null}
        </CardTitle>
        <CardDescription>
          启动时探测宿主与容器配额（容器内以 cgroup 限额为准），据此推导 worker 数、后台作业并发、扫描速率、 破解负载与
          Go 堆上限。<b>默认关闭</b>：不开启时 worker 仍为现有设置/默认 3，不限制作业、不缩放扫描。 已有显式 workers
          配置永远优先于计划。
        </CardDescription>
      </CardHeader>
      <CardContent className="flex flex-1 flex-col gap-4">
        {loadError && !plan ? (
          <div className="flex items-center gap-2 text-destructive text-sm">
            <TriangleAlertIcon className="size-4" />
            读取资源状态失败：{loadError}
            <Button variant="outline" size="sm" onClick={load}>
              重试
            </Button>
          </div>
        ) : null}

        {capacity && plan ? (
          <>
            <div className="rounded-lg border bg-muted/30 p-3 text-sm">
              <div className="flex flex-wrap items-center gap-x-4 gap-y-1">
                <span className="font-medium">CPU {capacity.cpu_cores} 核</span>
                <span className="text-muted-foreground">内存 {formatResourceBytes(capacity.mem_total)}</span>
                <span className="text-muted-foreground">可用 {formatResourceBytes(capacity.mem_available)}</span>
                <span className="text-muted-foreground">
                  磁盘剩余 {formatResourceBytes(capacity.disk_free)} / {formatResourceBytes(capacity.disk_total)}
                </span>
                <span className="text-muted-foreground">负载 {capacity.load1.toFixed(2)}</span>
                <span className="text-muted-foreground">
                  {capacity.cgroup_v2 ? "cgroup v2" : "cgroup v1/无"}
                  {capacity.in_container ? " · 容器内" : ""}
                </span>
              </div>
              <p className="mt-1 text-muted-foreground text-xs">
                依据：{capacity.mem_from_cgroup ? "内存取 cgroup 限额" : "内存取宿主 /proc/meminfo"}
                {capacity.cpu_from_cgroup ? " · CPU 取 cgroup 配额" : " · CPU 取宿主核数"}
              </p>
              {capacity.warnings?.length ? (
                <p className="mt-2 text-amber-600 text-xs dark:text-amber-400">{capacity.warnings.join("；")}</p>
              ) : null}
            </div>

            <div className="grid gap-x-4 gap-y-1 text-sm sm:grid-cols-2 lg:grid-cols-3">
              <span>
                worker 并发 推荐 <b className="font-mono">{plan.workers}</b> / 生效{" "}
                <b className="font-mono">{effectiveWorkersLabel(plan, effective?.workers)}</b>
              </span>
              <span>
                任务并发建议 <b className="font-mono">{plan.task_concurrency}</b>
              </span>
              <span>
                后台作业上限{" "}
                <b className="font-mono">
                  {plan.max_jobs > 0 ? `${plan.max_jobs}（每任务 ${plan.job_max_per_task}）` : "不限制"}
                </b>
              </span>
              <span>
                单作业内存建议 <b className="font-mono">{plan.job_memory_mb}MB</b>
              </span>
              <span>
                单作业 CPU 建议 <b className="font-mono">{plan.job_cpu_percent}%</b>
              </span>
              <span>
                作业最长运行 <b className="font-mono">{Math.round(plan.job_runtime_s / 60)} 分钟</b>
              </span>
              <span>
                扫描速率缩放 <b className="font-mono">{plan.scanner_scale.toFixed(2)}×</b>
              </span>
              <span>
                GOMEMLIMIT <b className="font-mono">{plan.gomemlimit_mb > 0 ? `${plan.gomemlimit_mb}MB` : "不设置"}</b>
              </span>
              <span>
                GOMAXPROCS <b className="font-mono">{plan.gomaxprocs > 0 ? plan.gomaxprocs : "运行时默认"}</b>
              </span>
              <span>
                破解负载{" "}
                <b className="font-mono">
                  {plan.cracker_workload > 0
                    ? `-w ${plan.cracker_workload} / ${Math.round(plan.cracker_runtime_s / 60)} 分钟`
                    : "—"}
                </b>
              </span>
            </div>
            {plan.reasons?.length ? (
              <p className="text-muted-foreground text-xs">推导依据：{plan.reasons.join("；")}</p>
            ) : null}

            <div className="flex flex-wrap items-center gap-4">
              <div className="flex items-center gap-2">
                <Switch id="resource-auto" checked={autoTune} disabled={disabled} onCheckedChange={setAutoTune} />
                <Label htmlFor="resource-auto" className="text-sm">
                  启用自动调优
                </Label>
              </div>
              <Select
                value={mode}
                onValueChange={(v) => setMode(v === "manual" ? "manual" : "auto")}
                disabled={disabled || !autoTune}
              >
                <SelectTrigger className="w-48">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="auto">自动（按容量推导）</SelectItem>
                  <SelectItem value="manual">手动（仅用手工覆盖）</SelectItem>
                </SelectContent>
              </Select>
              <div className="flex items-center gap-2">
                <Switch
                  id="resource-autopause"
                  checked={autoPause}
                  disabled={disabled}
                  onCheckedChange={setAutoPause}
                />
                <Label htmlFor="resource-autopause" className="text-sm">
                  临界时暂停扫描/破解（预留，当前仅保存）
                </Label>
              </div>
            </div>

            <div className="space-y-3">
              <div className="grid gap-3 sm:grid-cols-2">
                <label htmlFor="resource-high" className="flex flex-col gap-1 text-sm">
                  <span>节流阈值（%）</span>
                  <Input
                    id="resource-high"
                    type="number"
                    min={50}
                    max={95}
                    className="w-full font-mono text-sm"
                    value={high}
                    disabled={disabled}
                    onChange={(e) => setHigh(e.target.value)}
                  />
                </label>
                <label htmlFor="resource-critical" className="flex flex-col gap-1 text-sm">
                  <span>暂停阈值（%）</span>
                  <Input
                    id="resource-critical"
                    type="number"
                    min={60}
                    max={99}
                    className="w-full font-mono text-sm"
                    value={critical}
                    disabled={disabled}
                    onChange={(e) => setCritical(e.target.value)}
                  />
                </label>
              </div>
              <div className="space-y-2 border-t pt-3">
                <p className="font-medium text-muted-foreground text-xs">手动覆盖（留空 = 跟随自动计划）</p>
                <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
                  <label htmlFor="resource-manual-workers" className="flex flex-col gap-1 text-sm">
                    <span>worker 覆盖</span>
                    <Input
                      id="resource-manual-workers"
                      type="number"
                      min={1}
                      className="w-full font-mono text-sm"
                      placeholder={String(plan.workers)}
                      value={manualWorkers}
                      disabled={disabled}
                      onChange={(e) => setManualWorkers(e.target.value)}
                    />
                  </label>
                  <label htmlFor="resource-manual-maxjobs" className="flex flex-col gap-1 text-sm">
                    <span>作业上限覆盖</span>
                    <Input
                      id="resource-manual-maxjobs"
                      type="number"
                      min={1}
                      className="w-full font-mono text-sm"
                      placeholder={String(plan.max_jobs || 1)}
                      value={manualMaxJobs}
                      disabled={disabled}
                      onChange={(e) => setManualMaxJobs(e.target.value)}
                    />
                  </label>
                  <label htmlFor="resource-manual-scale" className="flex flex-col gap-1 text-sm">
                    <span>扫描缩放覆盖</span>
                    <Input
                      id="resource-manual-scale"
                      type="number"
                      min={0.1}
                      max={1}
                      step={0.05}
                      className="w-full font-mono text-sm"
                      placeholder={plan.scanner_scale.toFixed(2)}
                      value={manualScale}
                      disabled={disabled}
                      onChange={(e) => setManualScale(e.target.value)}
                    />
                  </label>
                  <label htmlFor="resource-manual-gomem" className="flex flex-col gap-1 text-sm">
                    <span>GOMEMLIMIT（MB）</span>
                    <Input
                      id="resource-manual-gomem"
                      type="number"
                      min={256}
                      className="w-full font-mono text-sm"
                      placeholder={String(plan.gomemlimit_mb || 512)}
                      value={manualGoMem}
                      disabled={disabled}
                      onChange={(e) => setManualGoMem(e.target.value)}
                    />
                  </label>
                </div>
                <p className="text-muted-foreground text-xs">
                  手动填写 worker 会同时写入「工作并发」设置（显式配置优先于计划）；其他覆盖只影响计划。
                  阈值与「临界暂停」当前仅保存，不会主动暂停作业。
                </p>
              </div>
            </div>

            <div className="flex flex-wrap items-center gap-2">
              <Button onClick={save} disabled={disabled}>
                {busy === "save" ? <Loader2Icon className="size-4 animate-spin" /> : null}
                保存并应用
              </Button>
              <Button variant="outline" onClick={retune} disabled={disabled}>
                {busy === "retune" ? (
                  <Loader2Icon className="size-4 animate-spin" />
                ) : (
                  <RefreshCwIcon className="size-4" />
                )}
                重新探测
              </Button>
              <Button variant="outline" onClick={applyTaskConcurrency} disabled={disabled || !plan.task_concurrency}>
                应用推荐任务并发（{plan.task_concurrency}）
              </Button>
              <span className="text-muted-foreground text-xs">
                worker/任务并发对之后启动的任务生效；运行中的作业不受影响，计划每小时自动重算一次。
              </span>
            </div>
          </>
        ) : (
          <p className="text-muted-foreground text-sm">
            {loading ? "正在读取资源状态…" : "尚未读取到资源状态（服务端未就绪或仍在启动）。"}
          </p>
        )}
      </CardContent>
    </Card>
  );
}
