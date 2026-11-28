"use client";

import * as React from "react";

import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";

import {
  ArrowLeftIcon,
  MoreHorizontalIcon,
  RefreshCwIcon,
  SlidersHorizontalIcon,
  TriangleAlertIcon,
  XIcon,
} from "lucide-react";
import { toast } from "sonner";

import { AgentLLMRoutePanel } from "@/components/agent-llm-route";
import { AgentPromptChain } from "@/components/agent-prompt-chain";
import { usePromptChain } from "@/components/chain/use-prompt-chain";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { api } from "@/lib/api";
import type { Agent, LLMProfile } from "@/lib/types";
import { cn } from "@/lib/utils";

// 全链路页：页头 + 舞台 + 检视器，单一纵向滚动。
// URL 契约：/system/agents/chain?key=<agentKey>&profile=<profileId>&view=list|map|route&sel=<对象 id>；
// profile 改变的是解析预览，不改绑定；在预览中保存独立提示词会写 (本 Agent × 该配置) 的覆盖。

const DEFAULT_AGENT = "worker";
type View = "list" | "map" | "route";

/**
 * 视口高度外壳：height = innerHeight − 顶边 − 主区下内边距。
 * 旧版按 16 计算，而主区下内边距是 24，整页多出 8px 滚动（N6）；
 * 这里读主区的实际下内边距，两种断点（16 / 24）都对。
 */
function useShellSize(ref: React.RefObject<HTMLElement | null>) {
  const [size, setSize] = React.useState({ h: 520, w: 0 });
  React.useEffect(() => {
    const el = ref.current;
    if (!el) return;
    let raf = 0;
    const read = () => {
      const rect = el.getBoundingClientRect();
      const parent = el.parentElement;
      const padBottom = parent ? Number.parseFloat(getComputedStyle(parent).paddingBottom) || 0 : 0;
      const h = Math.max(520, Math.round(window.innerHeight - rect.top - padBottom));
      const w = Math.round(rect.width);
      setSize((prev) => (prev.h === h && prev.w === w ? prev : { h, w }));
    };
    const schedule = () => {
      cancelAnimationFrame(raf);
      raf = requestAnimationFrame(read);
    };
    read();
    // 只跟随视口与主区尺寸；不要监听 scroll——rect.top 随滚动变化会让外壳高度单调增长。
    window.addEventListener("resize", schedule);
    const ro = new ResizeObserver(schedule);
    if (el.parentElement) ro.observe(el.parentElement);
    return () => {
      cancelAnimationFrame(raf);
      window.removeEventListener("resize", schedule);
      ro.disconnect();
    };
  }, [ref]);
  return size;
}

const focusRing = "focus-visible:ring-2 focus-visible:ring-foreground";

function ChainPageInner() {
  const router = useRouter();
  const searchParams = useSearchParams();
  const key = searchParams.get("key")?.trim() || DEFAULT_AGENT;
  const profileParam = searchParams.get("profile")?.trim() || "";
  const rawView = searchParams.get("view");
  // 旧链接里的 view=chain 视同清单。
  const view: View = rawView === "route" ? "route" : rawView === "map" ? "map" : "list";
  const sel = searchParams.get("sel")?.trim() || null;

  const [agents, setAgents] = React.useState<Agent[]>([]);
  const [profiles, setProfiles] = React.useState<LLMProfile[]>([]);
  const [stageWidth, setStageWidth] = React.useState(0);
  const [panelOpen, setPanelOpen] = React.useState(false);

  React.useEffect(() => {
    api
      .agents()
      .then(setAgents)
      .catch(() => setAgents([]));
  }, []);
  React.useEffect(() => {
    api
      .llmProfiles()
      .then(setProfiles)
      .catch(() => setProfiles([]));
  }, []);

  const url = React.useCallback(
    (next: { key?: string; profile?: string; view?: View; sel?: string | null }) => {
      const q = new URLSearchParams();
      q.set("key", next.key ?? key);
      const profile = next.profile ?? profileParam;
      if (profile) q.set("profile", profile);
      const v = next.view ?? view;
      if (v !== "list") q.set("view", v);
      const s = next.sel !== undefined ? next.sel : sel;
      if (s) q.set("sel", s);
      return `/system/agents/chain?${q.toString()}`;
    },
    [key, profileParam, view, sel],
  );

  const pc = usePromptChain({
    agentKey: key,
    profileId: profileParam || undefined,
    active: view !== "route",
    sel,
    onSelect: (id) => router.replace(url({ sel: id })),
  });

  const shellRef = React.useRef<HTMLDivElement>(null);
  const { h: height, w: shellWidth } = useShellSize(shellRef);
  const compact = shellWidth > 0 && shellWidth < 1100;
  // 舞台宽 = 内容宽 ≥ 900 时 clamp(560, 62%, 880)（≥560，总是够）；更窄时独占一栏。
  // 只有内容宽本身低于 520 时才真的放不下关系图。
  const mapUnavailable = stageWidth > 0 && stageWidth < 520;

  const guard = () => pc.confirmDiscard();

  async function switchAgent(next: string) {
    if (!(await guard())) return;
    setPanelOpen(false);
    router.replace(url({ key: next, profile: "", sel: null }));
  }

  async function setProfile(next: string) {
    if (!(await guard())) return;
    setPanelOpen(false);
    router.replace(url({ profile: next === "__none__" ? "" : next, sel: null }));
  }

  async function setView(next: View) {
    // 清单与关系图之间切换不丢草稿（检视器不重新挂载），不需要确认；
    // 切到 LLM 路由会卸载装配链与未保存的编辑区，要先确认。
    if (next === "route" && view !== "route" && !(await guard())) return;
    router.replace(url({ view: next }));
  }

  // 关系图放不下时不再用 disabled 静默挡掉点击：按钮仍可点，点了给出原因。
  async function requestView(next: View) {
    if (next === "map" && mapUnavailable) {
      toast.warning("窗口太窄：关系图至少需要 520px，请拉宽窗口或改用清单");
      return;
    }
    await setView(next);
  }

  // 旧链接直接带 ?view=map 时也要说明为什么被切回清单，而不是静默改写 URL。
  const mapFallbackNotified = React.useRef(false);
  React.useEffect(() => {
    if (view !== "map") {
      mapFallbackNotified.current = false;
      return;
    }
    if (!mapUnavailable) return;
    if (!mapFallbackNotified.current) {
      mapFallbackNotified.current = true;
      toast.warning("窗口太窄，关系图无法显示，已切回清单（至少需要 520px）");
    }
    router.replace(url({ view: "list" }));
  }, [view, mapUnavailable, router, url]);

  const builtin = agents.filter((a) => a.builtin);
  const custom = agents.filter((a) => !a.builtin);
  const attention = pc.model?.attention.length ?? 0;

  return (
    <div ref={shellRef} className="flex flex-col" style={{ height }}>
      <div className="flex min-h-12 shrink-0 flex-wrap items-center gap-3 border-b pb-2.5">
        <Button asChild variant="ghost" size="icon-sm" className={focusRing}>
          <Link href="/system/agents" aria-label="返回 Agent 列表">
            <ArrowLeftIcon className="size-4" />
          </Link>
        </Button>
        <h1 className="whitespace-nowrap font-semibold text-xl tracking-tight">全链路</h1>

        <Select value={key} onValueChange={switchAgent}>
          <SelectTrigger
            className={cn("h-8 w-full max-w-60 text-xs", focusRing)}
            aria-label="切换 Agent"
          >
            <SelectValue placeholder="选择 Agent" />
          </SelectTrigger>
          <SelectContent>
            {builtin.map((a) => (
              <SelectItem key={a.key} value={a.key}>
                <span className="truncate">{a.name}</span>
                {!compact && <span className="ml-2 font-mono text-muted-foreground text-xs">{a.key}</span>}
              </SelectItem>
            ))}
            {custom.map((a) => (
              <SelectItem key={a.key} value={a.key}>
                <span className="truncate">{a.name}</span>
                {!compact && <span className="ml-2 font-mono text-muted-foreground text-xs">{a.key}</span>}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>

        <Select value={profileParam || "__none__"} onValueChange={setProfile}>
          <SelectTrigger
            className={cn("h-8 w-full max-w-60 font-mono text-xs", focusRing)}
            aria-label="预览 LLM 配置（不改变绑定）"
            title="预览不改变 Agent 绑定；在预览中保存独立提示词会写入（本 Agent × 预览配置）的覆盖"
          >
            <span className="shrink-0 text-muted-foreground">
              {profileParam ? "预览中" : "预览配置"}
            </span>
            <SelectValue placeholder="不预览" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="__none__">不预览（按实际解析）</SelectItem>
            {profiles.map((p) => (
              <SelectItem key={p.id} value={p.id}>
                <span className="truncate">{p.name}</span>
                <span className="ml-2 font-mono text-muted-foreground text-xs">#{p.id}</span>
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        {profileParam && (
          <Button
            size="icon-sm"
            variant="ghost"
            aria-label="清除预览"
            className={focusRing}
            onClick={() => setProfile("__none__")}
          >
            <XIcon className="size-3.5" />
          </Button>
        )}

        <div className="inline-flex shrink-0 items-center gap-0.5 rounded-md border p-0.5" role="tablist" aria-label="视图">
          {(
            [
              ["list", "清单"],
              ["map", "关系图"],
              ["route", "LLM 路由"],
            ] as const
          ).map(([v, label]) => (
            <Button
              key={v}
              size="sm"
              variant={view === v ? "secondary" : "ghost"}
              className={cn("h-6 px-2.5 text-xs", focusRing)}
              role="tab"
              aria-selected={view === v}
              aria-disabled={v === "map" && mapUnavailable ? true : undefined}
              title={v === "map" && mapUnavailable ? "舞台太窄（小于 520px），关系图不提供" : undefined}
              onClick={() => requestView(v)}
            >
              {label}
            </Button>
          ))}
        </div>

        <span className="flex-1" />

        {view !== "route" && (
          <>
            <button
              type="button"
              className={cn(
                "inline-flex h-7 shrink-0 items-center gap-1.5 whitespace-nowrap rounded-full border px-2.5 text-xs",
                focusRing,
                attention > 0 ? "text-amber-700 dark:text-amber-400" : "text-muted-foreground",
              )}
              title="回到总览"
              onClick={() => {
                void (async () => {
                  if (!(await guard())) return;
                  void pc.select(null);
                  if (stageWidth > 0 && stageWidth < 900) setPanelOpen(true);
                })();
              }}
            >
              {attention > 0 ? (
                <>
                  <TriangleAlertIcon className="size-3.5" aria-hidden="true" />
                  需注意 {attention}
                </>
              ) : (
                "正常"
              )}
              {pc.stale && !compact && <span className="text-muted-foreground">· 数据可能已过期</span>}
            </button>
            <Button
              size="icon-sm"
              variant="ghost"
              aria-label="刷新"
              disabled={pc.loading}
              className={focusRing}
              onClick={() => {
                void (async () => {
                  if (!(await guard())) return;
                  pc.reload();
                })();
              }}
            >
              <RefreshCwIcon className={cn("size-4", pc.loading && "animate-spin")} />
            </Button>
          </>
        )}
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button size="icon-sm" variant="ghost" aria-label="更多" className={focusRing}>
              <MoreHorizontalIcon className="size-4" />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <DropdownMenuItem asChild>
              <Link href={`/system/agents/detail?key=${encodeURIComponent(key)}`}>
                <SlidersHorizontalIcon /> 去 Agent 配置
              </Link>
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>

      <div className="mt-3 flex min-h-0 flex-1 flex-col">
        {view === "route" ? (
          <AgentLLMRoutePanel agentKey={key} active />
        ) : (
          <AgentPromptChain
            pc={pc}
            view={view}
            panelOpen={panelOpen}
            onPanelOpenChange={setPanelOpen}
            onStageWidth={setStageWidth}
          />
        )}
      </div>
    </div>
  );
}

// useSearchParams must sit under a Suspense boundary for static export.
// 骨架而不是 null：换视图/首屏取数期间要看得见"正在发生什么"。
function ChainPageSkeleton() {
  return (
    <div className="flex flex-col gap-3" role="status" aria-busy="true" aria-label="加载中">
      <div className="h-8 w-40 animate-pulse rounded-md bg-muted" />
      <div className="h-20 animate-pulse rounded-lg bg-muted" />
      <div className="h-20 animate-pulse rounded-lg bg-muted" />
      <div className="h-20 animate-pulse rounded-lg bg-muted" />
    </div>
  );
}

export default function AgentChainPage() {
  return (
    <React.Suspense fallback={<ChainPageSkeleton />}>
      <ChainPageInner />
    </React.Suspense>
  );
}
