"use client";

import * as React from "react";

import { RefreshCwIcon } from "lucide-react";

import { BUILD_POLL_INTERVAL_MS, normalizeBuildId, readEmbeddedBuildId, shouldAutoReload } from "@/lib/build-policy";
import { isAppDirty } from "@/lib/dirty-state";
import { reloadNow, reloadOnce } from "@/lib/reload-guard";

/**
 * 存活页面自检：本页前端代次是否仍是服务端当前代次。
 *
 * 换装后旧标签页的 chunk 已经不存在，用户点击时才失败，表现为"点了没反应"。
 * 这里在 mount / bfcache 恢复 / 重新获得焦点 / 可见时每 60s 各比一次，把
 * "静默失效"变成"可见并可一键恢复"。
 *
 * 只在服务端确实暴露了合法代次时工作：老版本服务端没有 meta 与 /_pojun_build.txt，
 * 此时完全不动，避免误报。
 */
export function BuildGuard() {
  const [stale, setStale] = React.useState(false);
  const [autoReloadFailed, setAutoReloadFailed] = React.useState(false);
  const embeddedRef = React.useRef<string | null>(null);
  const mountedAtRef = React.useRef(0);
  const inFlightRef = React.useRef(false);

  const check = React.useCallback(async () => {
    const mine = embeddedRef.current;
    if (!mine || inFlightRef.current) return;
    if (typeof navigator !== "undefined" && !navigator.onLine) return;
    inFlightRef.current = true;
    try {
      const res = await fetch("/_pojun_build.txt", { cache: "no-store" });
      if (!res.ok) return;
      const theirs = normalizeBuildId(await res.text());
      if (!theirs || theirs === mine) {
        setStale(false);
        return;
      }
      setStale(true);
      const shouldReload = shouldAutoReload({
        pathname: window.location.pathname,
        dirty: isAppDirty(),
        pageAgeMs: Date.now() - mountedAtRef.current,
      });
      if (shouldReload && !reloadOnce()) setAutoReloadFailed(true);
    } catch {
      // 离线或换装窗口内连不上：保持现状，下次触发再判。
    } finally {
      inFlightRef.current = false;
    }
  }, []);

  React.useEffect(() => {
    embeddedRef.current = readEmbeddedBuildId();
    mountedAtRef.current = Date.now();
    if (!embeddedRef.current) return;

    void check();

    const onFocus = () => void check();
    const onVisibility = () => {
      if (document.visibilityState === "visible") void check();
    };
    const onPageShow = (e: PageTransitionEvent) => {
      if (e.persisted) {
        // bfcache 恢复的页面可能是很久以前冻结的：重新计时，允许一次自动恢复。
        mountedAtRef.current = Date.now();
        void check();
      }
    };

    window.addEventListener("focus", onFocus);
    document.addEventListener("visibilitychange", onVisibility);
    window.addEventListener("pageshow", onPageShow as EventListener);

    const timer = window.setInterval(() => {
      if (document.visibilityState === "visible") void check();
    }, BUILD_POLL_INTERVAL_MS);

    return () => {
      window.removeEventListener("focus", onFocus);
      document.removeEventListener("visibilitychange", onVisibility);
      window.removeEventListener("pageshow", onPageShow as EventListener);
      window.clearInterval(timer);
    };
  }, [check]);

  if (!stale) return null;

  let notice = "";
  if (isAppDirty()) notice = "检测到未保存的修改，未自动刷新。";
  else if (autoReloadFailed) notice = "自动刷新未执行。";

  return (
    <div
      role="alert"
      aria-live="polite"
      className="fixed inset-x-4 bottom-4 z-[10000] mx-auto flex max-w-xl flex-wrap items-center gap-3 rounded-lg border bg-popover px-3.5 py-2.5 text-popover-foreground text-sm shadow-lg"
    >
      <span className="min-w-0 flex-1">页面已过期：服务端已换装，本标签页仍在运行旧前端。{notice}</span>
      <button
        type="button"
        onClick={reloadNow}
        className="inline-flex shrink-0 items-center gap-1.5 rounded-md bg-primary px-2.5 py-1.5 font-medium text-primary-foreground text-xs hover:opacity-90 focus-visible:ring-2 focus-visible:ring-ring"
      >
        <RefreshCwIcon className="size-3.5" aria-hidden="true" />
        立即重新加载
      </button>
    </div>
  );
}
