"use client";

import * as React from "react";

import { AlertTriangleIcon, Loader2Icon, ShieldCheckIcon, TimerResetIcon } from "lucide-react";
import { toast } from "sonner";

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { api } from "@/lib/api";
import type { TrafficRetentionResult, TrafficRetentionStatus } from "@/lib/types";

function fmt(n: number) {
  return n.toLocaleString("zh-CN");
}

function cutoffText(ts: number) {
  if (!ts) return "未开启";
  const d = new Date(ts * 1000);
  return Number.isNaN(d.getTime()) ? "—" : d.toLocaleString("zh-CN");
}

export function TrafficRetentionDialog({
  open,
  onOpenChange,
  onChanged,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onChanged: () => void;
}) {
  const [status, setStatus] = React.useState<TrafficRetentionStatus | null>(null);
  const [days, setDays] = React.useState("0");
  const [loading, setLoading] = React.useState(false);
  const [saving, setSaving] = React.useState(false);
  const [previewing, setPreviewing] = React.useState(false);
  const [running, setRunning] = React.useState(false);
  const [result, setResult] = React.useState<TrafficRetentionResult | null>(null);
  const [confirmOpen, setConfirmOpen] = React.useState(false);

  const load = React.useCallback(async () => {
    setLoading(true);
    try {
      const s = await api.trafficRetention();
      setStatus(s);
      setDays(String(s.days ?? 0));
    } catch (e) {
      toast.error(`读取保留策略失败：${(e as Error).message}`);
    } finally {
      setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    if (!open) return;
    setResult(null);
    void load();
  }, [open, load]);

  async function saveDays(): Promise<boolean> {
    const n = Number(days);
    if (!Number.isInteger(n) || n < 0 || n > 3650) {
      toast.error("保留天数必须是 0-3650 的整数（0=关闭）");
      return false;
    }
    setSaving(true);
    try {
      await api.setSettings({ traffic_retention_days: n });
      const s = await api.trafficRetention();
      setStatus(s);
      toast.success(n > 0 ? `已开启：未引用流量保留 ${n} 天` : "已关闭自动清理");
      return true;
    } catch (e) {
      toast.error(`保存失败：${(e as Error).message}`);
      return false;
    } finally {
      setSaving(false);
    }
  }

  async function preview() {
    if (!(await saveDays())) return;
    setPreviewing(true);
    try {
      const r = await api.trafficRetentionRun(true);
      setResult(r);
      toast.success(`预览：预计清理 ${fmt(r.deletable ?? 0)} 条`);
    } catch (e) {
      toast.error(`预览失败：${(e as Error).message}`);
    } finally {
      setPreviewing(false);
    }
  }

  async function execute() {
    if (!(await saveDays())) return;
    setConfirmOpen(false);
    setRunning(true);
    try {
      const r = await api.trafficRetentionRun(false);
      setResult(r);
      toast.success(`已清理 ${fmt(r.deleted ?? 0)} 条未引用流量`);
      onChanged();
      await load();
    } catch (e) {
      toast.error(`清理失败：${(e as Error).message}`);
    } finally {
      setRunning(false);
    }
  }

  const enabled = status?.enabled ?? false;
  const deletable = result?.deletable ?? status?.deletable ?? 0;
  const protectedCount = result?.protected ?? status?.protected ?? 0;

  return (
    <>
      <Dialog open={open} onOpenChange={onOpenChange}>
        <DialogContent className="sm:max-w-[560px]">
          <DialogHeader>
            <DialogTitle className="flex items-center gap-2">
              <TimerResetIcon className="size-4 text-primary" /> 流量保留策略
              <Badge variant={enabled ? "secondary" : "outline"}>{enabled ? "已开启" : "关闭"}</Badge>
            </DialogTitle>
            <DialogDescription>
              只清理超过保留期限、且未被漏洞证据 / 回归 spec / 探索图引用的流量；受保护流量永不删除。
            </DialogDescription>
          </DialogHeader>

          <div className="flex flex-col gap-4 text-sm">
            <div className="rounded-lg border bg-muted/30 p-3 text-xs text-muted-foreground">
              <div className="flex items-center gap-2 font-medium text-foreground">
                <ShieldCheckIcon className="size-3.5 text-emerald-500" /> 保护集 fail-closed
              </div>
              <p className="mt-1.5">
                清理前从 PostgreSQL 读取 evidence snapshot、finding regression、finding 时间线和探索图引用；
                读取失败会直接中止本轮，不会在保护集为空时误删。
              </p>
            </div>

            <div className="grid gap-3 sm:grid-cols-[1fr_auto] sm:items-end">
              <label className="flex flex-col gap-1.5">
                <span className="text-muted-foreground text-xs">保留天数（0=关闭，最多 3650）</span>
                <Input
                  type="number"
                  min={0}
                  max={3650}
                  value={days}
                  onChange={(e) => setDays(e.target.value)}
                  placeholder="30"
                  disabled={loading || saving}
                />
              </label>
              <Button variant="outline" onClick={() => void saveDays()} disabled={loading || saving}>
                {saving ? <Loader2Icon className="animate-spin" /> : null} 保存策略
              </Button>
            </div>

            <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
              {[
                ["总流量", status?.total ?? 0],
                ["候选", status?.candidates ?? 0],
                ["受保护", protectedCount],
                ["可清理", deletable],
              ].map(([label, value]) => (
                <div key={String(label)} className="rounded-md border p-2.5">
                  <div className="text-muted-foreground text-[11px]">{label}</div>
                  <div className="mt-0.5 font-semibold tabular-nums">{fmt(Number(value))}</div>
                </div>
              ))}
            </div>

            <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-muted-foreground text-xs">
              <span>截止时间：{cutoffText(result?.cutoff || status?.cutoff || 0)}</span>
              {result?.dry_run ? <Badge variant="outline">dry-run 预览</Badge> : null}
              {result?.deleted != null ? <span>本次删除 {fmt(result.deleted)} 条</span> : null}
              {result?.legacy_dirs ? <span>清理旧目录 {fmt(result.legacy_dirs)} 个</span> : null}
            </div>

            {enabled && deletable > 0 ? (
              <div className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-3 text-xs">
                <AlertTriangleIcon className="mt-0.5 size-3.5 shrink-0 text-amber-600" />
                <span>
                  执行后会删除 SQLite 索引行、正文、FTS 与可回收 blob，并释放磁盘。该动作不可撤销；
                  建议先点「预览」核对候选数与保护数。
                </span>
              </div>
            ) : null}
          </div>

          <DialogFooter className="gap-2">
            <Button variant="outline" onClick={() => void preview()} disabled={loading || previewing || saving}>
              {previewing ? <Loader2Icon className="animate-spin" /> : null} 预览
            </Button>
            <Button
              variant="destructive"
              onClick={() => setConfirmOpen(true)}
              disabled={loading || running || saving || !enabled || deletable <= 0}
            >
              {running ? <Loader2Icon className="animate-spin" /> : null} 执行清理
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <AlertDialog open={confirmOpen} onOpenChange={setConfirmOpen}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>执行不可逆的流量清理？</AlertDialogTitle>
            <AlertDialogDescription>
              将删除 {fmt(deletable)} 条超过 {days} 天且未被引用的流量。受保护的 {fmt(protectedCount)} 条不会删除。
              删除后无法恢复，但已绑定到漏洞的证据快照会保留。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction onClick={() => void execute()}>确认清理</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}
