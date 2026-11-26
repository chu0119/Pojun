"use client";

import * as React from "react";

import { RefreshCwIcon } from "lucide-react";

import { ChainInspector } from "@/components/chain/chain-inspector";
import { ChainList } from "@/components/chain/chain-list";
import { ChainMap } from "@/components/chain/chain-map";
import type { PromptChainController } from "@/components/chain/use-prompt-chain";
import { Button } from "@/components/ui/button";
import { Sheet, SheetContent, SheetTitle } from "@/components/ui/sheet";
import { cn } from "@/lib/utils";

// 装配链外壳：舞台（清单或关系图）+ 检视器。两者读同一份模型、同一个选中。
// 内容容器宽 ≥ 900 时并排常驻；更窄时检视器改为右侧滑出的模态面板。
// 测量容器必须始终在 DOM 里：ResizeObserver 挂在条件渲染的节点上时，首次渲染
// 拿到 null 就再也不会观察，断点与舞台宽度会永远停在初值。

export function AgentPromptChain({
  pc,
  view,
  panelOpen,
  onPanelOpenChange,
  onStageWidth,
}: {
  pc: PromptChainController;
  view: "list" | "map";
  panelOpen: boolean;
  onPanelOpenChange: (open: boolean) => void;
  onStageWidth: (width: number) => void;
}) {
  const wrapRef = React.useRef<HTMLDivElement>(null);
  const [wide, setWide] = React.useState(true);
  const widthCb = React.useRef(onStageWidth);
  widthCb.current = onStageWidth;

  React.useEffect(() => {
    const el = wrapRef.current;
    if (!el) return;
    const ro = new ResizeObserver((entries) => {
      const w = Math.round(entries[0]?.contentRect.width ?? 0);
      setWide(w >= 900);
      widthCb.current(w);
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  // 窄容器下选中对象就打开面板；「总览」与关闭由用户操作。
  React.useEffect(() => {
    if (!wide && pc.sel) onPanelOpenChange(true);
  }, [wide, pc.sel, onPanelOpenChange]);

  // Esc 清空选中；焦点在输入控件里时归控件自己。有未保存草稿先确认。
  React.useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "Escape" || !pc.sel) return;
      const t = e.target as HTMLElement | null;
      if (t && (/^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName) || t.isContentEditable)) return;
      // 弹层（窄容器检视器、下拉、菜单）自己处理 Esc：只关弹层，不清选中。
      if (t?.closest('[role="dialog"],[role="menu"],[role="listbox"],[data-radix-popper-content-wrapper]')) return;
      e.preventDefault();
      const id = pc.sel;
      void (async () => {
        if (!(await pc.confirmDiscard())) return;
        void pc.select(null);
        requestAnimationFrame(() => {
          pc.stageRef.current?.querySelector<HTMLElement>(`[data-id="${CSS.escape(id)}"]`)?.focus();
        });
      })();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [pc]);

  const onStageClick = (e: React.MouseEvent) => {
    if (!pc.sel) return;
    const t = e.target as HTMLElement;
    if (t.closest("[data-chain-item]")) return;
    void (async () => {
      if (!(await pc.confirmDiscard())) return;
      void pc.select(null);
    })();
  };

  const body =
    pc.loading && !pc.chain ? (
      <div className="grid gap-2">
        {Array.from({ length: 3 }).map((_, i) => (
          <div key={i} className="grid gap-2">
            <div className="h-6 w-40 animate-pulse rounded bg-muted/60" />
            {Array.from({ length: 4 }).map((__, j) => (
              <div key={j} className="h-9 animate-pulse rounded-md border bg-muted/40" />
            ))}
          </div>
        ))}
      </div>
    ) : pc.error ? (
      <div className="rounded-lg border border-red-700/40 bg-red-700/5 p-4 dark:border-red-400/40 dark:bg-red-400/5">
        <p className="font-medium text-red-700 text-sm dark:text-red-400">装配链读取失败</p>
        <p className="mt-1 text-muted-foreground text-xs">{pc.error}</p>
        <Button
          className="mt-3 text-xs focus-visible:ring-2 focus-visible:ring-foreground"
          size="sm"
          variant="outline"
          onClick={pc.reload}
        >
          <RefreshCwIcon className="size-3.5" /> 重试
        </Button>
      </div>
    ) : !pc.chain ? (
      <p className="p-4 text-center text-muted-foreground text-sm">（暂无装配数据）</p>
    ) : (
      <div
        className={cn(
          "grid min-h-0 flex-1 gap-0",
          wide ? "grid-cols-[clamp(560px,62%,880px)_minmax(0,1fr)]" : "grid-cols-1",
        )}
      >
        <div
          ref={pc.stageRef}
          role="group"
          aria-label="装配舞台"
          className="relative min-w-0 overflow-auto"
          onClick={onStageClick}
        >
          {view === "map" ? <ChainMap pc={pc} /> : <ChainList pc={pc} />}
        </div>
        {wide && (
          <aside aria-label="检视器" className="min-h-0 min-w-0 overflow-auto border-l pb-4 pl-5">
            <div className="max-w-[720px]">
              <ChainInspector pc={pc} />
            </div>
          </aside>
        )}
      </div>
    );

  return (
    <div ref={wrapRef} className="flex min-h-0 flex-1 flex-col">
      {body}
      {!wide && !!pc.chain && (
        <Sheet open={panelOpen} onOpenChange={onPanelOpenChange}>
          <SheetContent side="right" className="w-full overflow-y-auto p-4 sm:max-w-[480px]">
            {/* Radix 要求对话框有可访问名称，否则读屏拿到一个无名 dialog。 */}
            <SheetTitle className="sr-only">装配链检视器</SheetTitle>
            <ChainInspector pc={pc} />
          </SheetContent>
        </Sheet>
      )}
    </div>
  );
}