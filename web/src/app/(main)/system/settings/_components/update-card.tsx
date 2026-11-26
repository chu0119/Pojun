"use client";

import * as React from "react";

import {
  CheckCircle2Icon,
  DownloadIcon,
  ExternalLinkIcon,
  Loader2Icon,
  PlugZapIcon,
  RefreshCwIcon,
  RotateCcwIcon,
  TriangleAlertIcon,
} from "lucide-react";
import Link from "next/link";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Progress } from "@/components/ui/progress";
import { api, sseUrl } from "@/lib/api";
import type { UpdateCheck, UpdateProgress, UpdateProxyTestResult } from "@/lib/types";

/** 等待新版本上线的最长时间。一次升级要经过三次进程启动（暂存 → 换装 → 新版），
 *  每次都是秒级，三分钟足够覆盖慢磁盘和 Docker 容器重建。 */
const RESTART_TIMEOUT_MS = 180_000;

const PROXY_SOURCE_LABEL: Record<string, string> = {
  update: "更新专用代理",
  env: "环境变量 POJUN_UPDATE_PROXY",
  install: "装机代理",
  global: "全局代理",
  direct: "直连",
};

function humanSize(n?: number): string {
  if (!n || n <= 0) return "";
  const units = ["B", "KB", "MB", "GB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** 上次更新结果的人话描述；跨重启保留的记录让页面不是只看到一个变化的版本号。 */
function describeLastUpdate(info: UpdateCheck): string | null {
  const last = info.last_update;
  if (!last) return null;
  const when = last.at ? new Date(last.at).toLocaleString() : "";
  const suffix = when ? `（${when}）` : "";
  if (last.status === "failed") {
    return `上次更新 ${last.from} → ${last.to} 失败：${last.error || "未知原因"}${suffix}`;
  }
  if (last.status === "staged" && last.to === info.current) {
    return `已更新到 ${last.to}${suffix}`;
  }
  if (last.status === "running" || last.status === "staged") {
    return `上次更新到 ${last.to} 未完成（当前 ${info.current}）${suffix}`;
  }
  return `上次更新：${last.from} → ${last.to}${suffix}`;
}

export function UpdateCard() {
  const [info, setInfo] = React.useState<UpdateCheck | null>(null);
  const [checking, setChecking] = React.useState(true);
  const [progress, setProgress] = React.useState<UpdateProgress | null>(null);
  // 与 progress 分开：暂存完成后进程就没了，SSE 会断，此时要切到轮询 /api/health。
  const [restarting, setRestarting] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  // 更新专用代理：只作用于查版本/下载 Release，与目标流量出口互不影响。
  const [proxyInput, setProxyInput] = React.useState("");
  const [proxyCheck, setProxyCheck] = React.useState<UpdateProxyTestResult | null>(null);
  const [testingProxy, setTestingProxy] = React.useState(false);
  const [savingProxy, setSavingProxy] = React.useState(false);
  const proxyLoaded = React.useRef(false);
  // 刷新后自动接回进行中的更新：attached 防重复，streamRef 保证只有一条 SSE。
  const attachedRef = React.useRef(false);
  const streamRef = React.useRef<EventSource | null>(null);

  // quiet 同时决定要不要绕过后端缓存：进页面时的自动检查用缓存（顶栏刚查过），
  // 用户手动点「检查更新」则强制回源，否则刚发布的版本要等缓存过期才看得到。
  const check = React.useCallback((quiet = false) => {
    setChecking(true);
    api
      .checkUpdate(!quiet)
      .then((r) => {
        setInfo(r);
        // 只在首次加载时回填，避免后台刷新覆盖用户正在编辑的输入。
        if (!proxyLoaded.current) {
          proxyLoaded.current = true;
          setProxyInput(r.update_proxy ?? "");
        }
        if (!quiet) {
          if (r.error) toast.error(`检查更新失败：${r.error}`);
          else if (r.has_update) toast.success(`发现新版本 ${r.latest}`);
          else if (r.comparable) toast.success("当前已是最新版本");
        }
      })
      .catch((e) => {
        if (!quiet) toast.error(`检查更新失败：${(e as Error).message}`);
      })
      .finally(() => setChecking(false));
  }, []);

  React.useEffect(() => {
    check(true);
  }, [check]);

  // 轮询 /api/version（鉴权）直到版本号变化。
  //
  // 判据必须是"版本变了"而不是"能连上了"：换装过程中旧版本会短暂地重新起来一次
  // （那一次只负责把 pojun.new 换上去然后立刻退出），只看连通性会误判成功。
  const waitForNewVersion = React.useCallback(async (fromVersion: string) => {
    setRestarting(true);
    const deadline = Date.now() + RESTART_TIMEOUT_MS;
    while (Date.now() < deadline) {
      await sleep(2000);
      try {
        // /api/health 已去身份（公网指纹治理）：版本查询走鉴权的 /api/version。
        const r = await fetch("/api/version", { cache: "no-store" });
        if (r.ok) {
          const j = (await r.json()) as { version?: string };
          if (j.version && j.version !== fromVersion) {
            toast.success(`已更新到 ${j.version}，正在重新加载页面`);
            await sleep(800);
            window.location.reload();
            return;
          }
        }
      } catch {
        // 重启窗口内连不上是预期的，继续轮询。
      }
    }
    setRestarting(false);
    setBusy(false);
    toast.error("等待服务重启超时。若本次升级启用了 HTTPS，请改用 https:// 地址访问确认版本；否则检查后端日志，或确认 pojun 是通过 start.sh / start.bat 启动的。");
  }, []);

  // 订阅更新进度。SSE 不走 Next 的 /api 重写（那层会缓冲，事件推不出来）。
  const openStream = React.useCallback(
    (fromVersion: string) => {
      streamRef.current?.close();
      const es = new EventSource(sseUrl("/api/update/stream"));
      streamRef.current = es;
      let settled = false;
      let watchdog = 0;
      const armWatchdog = () => {
        window.clearTimeout(watchdog);
        // 每次收到进度都重新计时：下载大包可能超过 90s，但完全无事件就不能
        // 一直把 busy 挂住（后端崩溃/SSE 400 时按钮会永久禁用）。
        watchdog = window.setTimeout(() => fail("更新进度中断：90 秒没有收到任何进度，请检查后端日志"), 90_000);
      };
      const finish = () => {
        settled = true;
        window.clearTimeout(watchdog);
        es.close();
        if (streamRef.current === es) streamRef.current = null;
      };
      const fail = (message: string) => {
        if (settled) return;
        finish();
        setBusy(false);
        setProgress(null);
        toast.error(message);
      };
      armWatchdog();
      es.onmessage = (ev) => {
        if (settled) return;
        armWatchdog();
        let p: UpdateProgress;
        try {
          p = JSON.parse(ev.data) as UpdateProgress;
        } catch {
          return;
        }
        setProgress(p);
        if (p.phase === "failed") {
          finish();
          setBusy(false);
          toast.error(`更新失败：${p.error || p.message}`);
          return;
        }
        if (p.phase === "staged") {
          finish();
          void waitForNewVersion(fromVersion);
        }
      };
      es.onerror = () => {
        if (settled) return;
        // 进程退出时 SSE 必然断开。先等几秒确认服务是否已经换成新版本：
        // 换上了就按成功走，没换上才把 busy 复位，避免按钮永久禁用。
        window.setTimeout(async () => {
          if (settled) return;
          try {
            const r = await fetch("/api/version", { cache: "no-store" });
            if (r.ok) {
              const j = (await r.json()) as { version?: string };
              if (j.version && j.version !== fromVersion) {
                finish();
                setRestarting(true);
                toast.success(`已更新到 ${j.version}，正在重新加载页面`);
                await sleep(800);
                window.location.reload();
                return;
              }
            }
          } catch {
            // 重启窗口内连不上是预期的，交给下面的失败复位。
          }
          fail("更新进度连接中断。若服务正在重启，请稍后刷新页面确认版本；否则请检查后端日志。");
        }, 3000);
      };
      return es;
    },
    [waitForNewVersion],
  );

  // 刷新/换标签页后：check 返回 running 就自动接回 SSE；staged 但进程还没退时直接等新版本。
  React.useEffect(() => {
    const cur = info;
    if (!cur || attachedRef.current) return;
    const st = cur.update;
    if (!st) return;
    if (st.running) {
      attachedRef.current = true;
      setBusy(true);
      setProgress(st);
      openStream(cur.current);
    } else if (st.phase === "staged") {
      attachedRef.current = true;
      setBusy(true);
      setProgress(st);
      void waitForNewVersion(cur.current);
    }
  }, [info, openStream, waitForNewVersion]);

  const doUpdate = () => {
    if (!info) return;
    const from = info.current;
    const ok = window.confirm(
      `确定更新到 ${info.latest}？\n\n` +
        "更新会重启程序，正在运行的任务会被中断。\n" +
        (info.mode === "docker"
          ? "\n注意：容器内更新只替换程序本身，不会更新镜像里的 playwright / nmap 等工具链；" +
            "若新版本依赖新工具，请改用 docker compose pull。"
          : ""),
    );
    if (!ok) return;

    setBusy(true);
    setProgress({ phase: "downloading", percent: 0, message: "准备中…" });
    const es = openStream(from);
    api.applyUpdate().catch((e) => {
      es.close();
      if (streamRef.current === es) streamRef.current = null;
      setBusy(false);
      setProgress(null);
      const message = (e as Error).message || "";
      // 刷新前一刻刚好开始了另一次更新：接回它的进度，而不是只弹一句 409。
      if (message.includes("已有一个更新")) {
        toast.info("已有更新正在进行，正在接入进度…");
        attachedRef.current = false; // 允许 effect 重新接入（此前可能因断流已放弃）
        check(true); // 静默刷新 check，上面的 effect 会自动订阅 SSE
        return;
      }
      toast.error(`启动更新失败：${message}`);
    });
  };

  const doRollback = () => {
    if (!info) return;
    if (
      !window.confirm(
        "回滚到上一版本？\n\n程序会重启，正在运行的任务会被中断。\n注意：数据库结构不会回退，旧版本可能无法识别新版写入的数据。",
      )
    )
      return;
    const from = info.current;
    setBusy(true);
    api
      .rollbackUpdate()
      .then(() => {
        toast.success("已切换到上一版本，正在重启…");
        void waitForNewVersion(from);
      })
      .catch((e) => {
        setBusy(false);
        toast.error(`回滚失败：${(e as Error).message}`);
      });
  };

  const testUpdateProxy = () => {
    setTestingProxy(true);
    setProxyCheck(null);
    api
      .testUpdateProxy(proxyInput.trim())
      .then(setProxyCheck)
      .catch((e) => toast.error(`测试失败：${(e as Error).message}`))
      .finally(() => setTestingProxy(false));
  };

  const saveUpdateProxy = () => {
    setSavingProxy(true);
    api
      .setSettings({ update_proxy: proxyInput.trim() })
      .then(() => {
        setProxyInput(proxyInput.trim());
        toast.success("更新代理已保存，正在验证更新链路…");
        // 强制回源：顺便让新代理下的版本信息立即刷新（失败会显示在卡片里）。
        check(false);
      })
      .catch((e) => toast.error(`保存失败：${(e as Error).message}`))
      .finally(() => setSavingProxy(false));
  };

  const phase = progress?.phase;
  const showProgress = busy || restarting;
  // 失败可能来自本次会话的 SSE（progress），也可能来自刷新后的 check（info.update）。
  const failedProgress =
    !busy && !restarting
      ? progress?.phase === "failed"
        ? progress
        : info?.update?.phase === "failed"
          ? info.update
          : null
      : null;
  // 只有下载阶段拿得到真实百分比（按 Content-Length 算）。校验/解压/等待重启都是
  // 时长不可知的阶段，进度条填满并加个脉冲动画表示"在忙但说不准还要多久"。
  const downloading = !restarting && phase === "downloading";
  const pct = downloading ? Math.max(progress?.percent ?? 0, 0) : 100;
  const lastLine = info ? describeLastUpdate(info) : null;

  return (
    // 设置页是多列瀑布流布局，卡片自己负责行间距并禁止跨列断开（见 page.tsx 的注释）。
    <Card className="mb-4 break-inside-avoid md:mb-6">
      <CardHeader>
        <CardTitle className="flex items-center gap-2 text-base">
          <DownloadIcon className="size-4" />
          版本与更新
        </CardTitle>
        <CardDescription>从 GitHub 检查并安装新版本。更新会重启程序，正在运行的任务会被中断。</CardDescription>
      </CardHeader>
      <CardContent className="space-y-4">
        {/* 更新专用代理：只影响查版本/下载 Release，与目标流量/装机安装互不影响。 */}
        <div className="flex flex-col gap-2 rounded-lg border bg-muted/30 p-3">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <Label htmlFor="update-proxy" className="text-sm">
              更新专用代理
            </Label>
            <div className="flex items-center gap-2">
              {proxyCheck && (
                <Badge variant={proxyCheck.ok ? "default" : "destructive"} title={proxyCheck.error || ""}>
                  {proxyCheck.ok ? `可用${proxyCheck.latency_ms ? ` · ${proxyCheck.latency_ms}ms` : ""}` : "不可用"}
                </Badge>
              )}
              <span className="text-muted-foreground text-xs">
                留空 = 环境变量 → 装机代理 → 全局代理 → 直连
              </span>
            </div>
          </div>
          <div className="flex items-center gap-2">
            <Input
              id="update-proxy"
              autoComplete="off"
              className="font-mono text-xs"
              placeholder="http://127.0.0.1:7890 或 socks5://host:1080（留空=跟随回退链）"
              value={proxyInput}
              disabled={!info || savingProxy}
              onChange={(e) => {
                setProxyInput(e.target.value);
                setProxyCheck(null);
              }}
            />
            <Button
              type="button"
              size="sm"
              variant="outline"
              onClick={testUpdateProxy}
              disabled={!info || testingProxy || savingProxy}
            >
              {testingProxy ? <Loader2Icon className="animate-spin" /> : <PlugZapIcon />}
              测试
            </Button>
            <Button
              type="button"
              size="sm"
              variant="outline"
              onClick={saveUpdateProxy}
              disabled={!info || savingProxy || testingProxy}
            >
              保存
            </Button>
          </div>
          {proxyCheck && !proxyCheck.ok && (
            <p className="text-destructive text-xs">{proxyCheck.error || "代理不可用"}</p>
          )}
          {proxyCheck?.ok && (
            <p className="text-muted-foreground text-xs">
              可访问 GitHub{proxyCheck.latest ? `，最新版本 ${proxyCheck.latest}` : ""}
              {proxyCheck.latency_ms ? ` · ${proxyCheck.latency_ms}ms` : ""}
            </p>
          )}
          {info && (
            <p className="text-muted-foreground text-xs">
              当前生效：
              <span className="font-mono">{info.update_proxy_effective || "直连"}</span>
              （来源：{PROXY_SOURCE_LABEL[info.update_proxy_source || "direct"] || "直连"}）
            </p>
          )}
          <p className="text-muted-foreground text-xs">
            只作用于版本检查、整包下载与 hostd 跟随升级；不改变任务的目标流量出口。
            也可用 <code>POJUN_UPDATE_PROXY</code> 环境变量提供。
          </p>
        </div>

        <div className="flex flex-wrap items-center gap-2 text-sm">
          <span className="text-muted-foreground">当前版本</span>
          <Badge variant="secondary" className="font-mono">
            {info?.current ?? "…"}
          </Badge>
          {info && (
            <>
              <Badge variant="outline" className="font-mono">
                {info.os}/{info.arch}
              </Badge>
              <Badge variant="outline">{info.mode === "docker" ? "Docker" : "独立程序"}</Badge>
            </>
          )}
          {info?.latest && (
            <>
              <span className="text-muted-foreground">最新版本</span>
              <Badge variant={info.has_update ? "default" : "secondary"} className="font-mono">
                {info.latest}
              </Badge>
            </>
          )}
          {info?.html_url && (
            <a
              href={info.html_url}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 text-muted-foreground text-xs underline-offset-4 hover:underline"
            >
              更新日志 <ExternalLinkIcon className="size-3" />
            </a>
          )}
        </div>

        {info?.boot_notice && (
          <p className="flex items-start gap-2 rounded-md border border-amber-500/40 bg-amber-500/10 p-2 text-amber-700 text-xs dark:text-amber-400">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.boot_notice}
          </p>
        )}

        {info?.error && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-destructive text-xs">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            检查更新失败：{info.error}
          </p>
        )}

        {/* 上次更新结果（跨重启保留）：成功/失败/未完成都能在刷新后看到。 */}
        {lastLine && (
          <p
            className={
              info?.last_update?.status === "failed"
                ? "flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-destructive text-xs"
                : "flex items-start gap-2 text-muted-foreground text-xs"
            }
          >
            {info?.last_update?.status === "failed" ? (
              <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            ) : (
              <CheckCircle2Icon className="mt-0.5 size-3.5 shrink-0 text-emerald-600" />
            )}
            {lastLine}
          </p>
        )}

        <p className="text-muted-foreground text-xs">
          检查和下载更新优先使用「
          <Link href="/system/hostenv#install-proxy-settings" className="underline underline-offset-4">
            宿主环境 → 装机代理
          </Link>
          」（也可由 <code>POJUN_INSTALL_PROXY</code> 提供）。未配置装机代理时才使用全局代理，两者都未配置时直连。
          {info?.mode === "docker" &&
            "Docker 桥接网络下，127.0.0.1 指向容器自身。代理若运行在宿主机上，请填容器可访问的地址。"}
        </p>

        {info && !info.comparable && info.reason && <p className="text-muted-foreground text-xs">{info.reason}</p>}

        {info?.has_update && info.asset_available === false && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-destructive text-xs">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {info.latest} 没有提供 {info.os}/{info.arch} 的发布包（缺少 {info.asset}），无法自动更新。
          </p>
        )}

        {info?.has_update && info.asset_available !== false && (
          <p className="text-muted-foreground text-xs">
            将下载 <span className="font-mono">{info.asset}</span>
            {info.size ? `（${humanSize(info.size)}）` : ""}，校验 SHA256 并冒烟测试后才会替换，失败自动保留当前版本。
          </p>
        )}

        {info && !info.has_update && info.comparable && !info.error && (
          <p className="flex items-center gap-2 text-muted-foreground text-xs">
            <CheckCircle2Icon className="size-3.5 text-emerald-600" />
            当前已是最新版本。
          </p>
        )}

        {info?.mode === "docker" && info.has_update && (
          <p className="text-muted-foreground text-xs">
            Docker 下的更新只替换程序本身，不更新镜像里的 playwright / nmap 等工具链，且
            <span className="font-mono"> docker compose up -d </span>
            重建容器后会退回镜像自带的版本。需要连镜像一起升级请执行
            <span className="font-mono"> docker compose pull pojun &amp;&amp; docker compose up -d pojun</span>。
          </p>
        )}

        {showProgress && (
          <div className="space-y-1.5">
            <Progress value={pct} className={downloading ? undefined : "animate-pulse"} />
            <p className="text-muted-foreground text-xs">
              {restarting ? "正在重启并应用新版本，请稍候（页面会自动刷新）…" : progress?.message}
            </p>
          </div>
        )}

        {failedProgress && (
          <p className="flex items-start gap-2 rounded-md border border-destructive/40 bg-destructive/10 p-2 text-destructive text-xs">
            <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" />
            {failedProgress.message || "更新失败"}
            {failedProgress.error ? `：${failedProgress.error}` : ""}
          </p>
        )}

        <div className="flex flex-wrap gap-2">
          <Button variant="outline" size="sm" onClick={() => check(false)} disabled={checking || busy || restarting}>
            <RefreshCwIcon className={checking ? "size-4 animate-spin" : "size-4"} />
            检查更新
          </Button>
          <Button
            size="sm"
            onClick={doUpdate}
            disabled={busy || restarting || !info?.has_update || info?.asset_available === false}
          >
            <DownloadIcon className="size-4" />
            {info?.has_update ? `更新到 ${info.latest}` : "立即更新"}
          </Button>
          {info?.has_backup && (
            <Button variant="ghost" size="sm" onClick={doRollback} disabled={busy || restarting}>
              <RotateCcwIcon className="size-4" />
              回滚到上一版本
            </Button>
          )}
        </div>

        <p className="text-muted-foreground text-xs">
          一键更新依赖守护脚本重启程序。请通过 <span className="font-mono">start.sh</span>（Windows 为
          <span className="font-mono"> start.bat</span>）启动 POJUN；直接运行 pojun 本体时，程序退出后不会被自动拉起。
        </p>
      </CardContent>
    </Card>
  );
}
