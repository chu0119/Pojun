"use client";

import * as React from "react";

import { RefreshCwIcon } from "lucide-react";

import { isChunkLoadFailure, isNextAssetUrl } from "@/lib/build-policy";
import { reloadNow, reloadOnce } from "@/lib/reload-guard";

/**
 * 真实资源加载失败（脚本/样式/chunk）的兜底。
 *
 * 与 BuildGuard 分工：BuildGuard 处理"页面是旧的但还能跑"，这里处理"新代码
 * 根本没加载成功"——动态 import 的 chunk 404、入口脚本被拦、CSS 取不到。
 * 离线时不重载（重载只会再失败一次），只把失败摆到界面上。
 */
export function AssetFailureGuard() {
  const [failed, setFailed] = React.useState(false);

  React.useEffect(() => {
    const handle = () => {
      if (typeof navigator !== "undefined" && !navigator.onLine) {
        setFailed(true);
        return;
      }
      if (!reloadOnce()) setFailed(true);
    };

    // 资源加载失败不会冒泡，必须在 capture 阶段监听。
    const onError = (e: Event) => {
      const target = e.target;
      if (target instanceof HTMLScriptElement) {
        if (isNextAssetUrl(target.src)) handle();
        return;
      }
      if (target instanceof HTMLLinkElement) {
        if (isNextAssetUrl(target.href)) handle();
        return;
      }
      const err = e as ErrorEvent;
      if (isChunkLoadFailure(err.message)) handle();
    };

    const onRejection = (e: PromiseRejectionEvent) => {
      const reason = e.reason as { message?: string; name?: string } | string | null | undefined;
      const message = typeof reason === "string" ? reason : (reason?.message ?? "");
      const name = typeof reason === "object" && reason ? reason.name : "";
      if (isChunkLoadFailure(message, name)) handle();
    };

    const onOffline = () => setFailed(false);

    window.addEventListener("error", onError, true);
    window.addEventListener("unhandledrejection", onRejection);
    window.addEventListener("online", onOffline);
    return () => {
      window.removeEventListener("error", onError, true);
      window.removeEventListener("unhandledrejection", onRejection);
      window.removeEventListener("online", onOffline);
    };
  }, []);

  if (!failed) return null;

  return (
    <div
      role="alert"
      className="fixed inset-x-4 bottom-4 z-[10000] mx-auto flex max-w-xl flex-wrap items-center gap-3 rounded-lg border border-destructive/40 bg-popover px-3.5 py-2.5 text-popover-foreground text-sm shadow-lg"
    >
      <span className="min-w-0 flex-1">前端资源加载失败，页面可能不完整（网络中断或服务端刚换装）。</span>
      <button
        type="button"
        onClick={() => {
          setFailed(false);
          reloadNow();
        }}
        className="inline-flex shrink-0 items-center gap-1.5 rounded-md bg-primary px-2.5 py-1.5 font-medium text-primary-foreground text-xs hover:opacity-90 focus-visible:ring-2 focus-visible:ring-ring"
      >
        <RefreshCwIcon className="size-3.5" aria-hidden="true" />
        重新加载
      </button>
    </div>
  );
}
