"use client";

import * as React from "react";

import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";

import { ArrowLeftIcon, SlidersHorizontalIcon, XIcon } from "lucide-react";

import { AgentLLMRoutePanel } from "@/components/agent-llm-route";
import { AgentPromptChain } from "@/components/agent-prompt-chain";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { api } from "@/lib/api";
import type { Agent, LLMProfile } from "@/lib/types";

// 全链路只读页：图容器占满视口、图内单一纵向滚动（不再与页面双滚动）。
// URL 契约：/system/agents/chain?key=<agentKey>&profile=<profileId?>；
// profile 只作只读预览，不改变绑定；切换 Agent 时清空 profile。

const DEFAULT_AGENT = "worker";

/** 视口高度外壳：height = innerHeight - 顶边 - 16，下限 520；rAF 重算。 */
function useFillHeight(ref: React.RefObject<HTMLElement | null>) {
  const [height, setHeight] = React.useState(520);
  React.useEffect(() => {
    const el = ref.current;
    if (!el) return;
    let raf = 0;
    const read = () => {
      const rect = el.getBoundingClientRect();
      const h = Math.max(520, window.innerHeight - rect.top - 16);
      setHeight((prev) => (Math.abs(prev - h) > 1 ? h : prev));
    };
    const schedule = () => {
      cancelAnimationFrame(raf);
      raf = requestAnimationFrame(read);
    };
    read();
    // 只跟随视口尺寸；不要监听 scroll——rect.top 随滚动变化会让外壳高度单调增长。
    window.addEventListener("resize", schedule);
    return () => {
      cancelAnimationFrame(raf);
      window.removeEventListener("resize", schedule);
    };
  }, [ref]);
  return height;
}

function ChainPageInner() {
  const router = useRouter();
  const searchParams = useSearchParams();
  const key = searchParams.get("key")?.trim() || DEFAULT_AGENT;
  const profileParam = searchParams.get("profile")?.trim() || "";
  // 两个视图共用同一外壳与 URL；各自内部单一滚动，避免页面双滚动。
  const view = searchParams.get("view") === "route" ? "route" : "chain";

  const [agents, setAgents] = React.useState<Agent[]>([]);
  const [profiles, setProfiles] = React.useState<LLMProfile[]>([]);
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

  const current = agents.find((a) => a.key === key) ?? null;
  const builtin = agents.filter((a) => a.builtin);
  const custom = agents.filter((a) => !a.builtin);
  const selectedProfile = profiles.find((p) => p.id === profileParam) ?? null;

  const shellRef = React.useRef<HTMLDivElement>(null);
  const height = useFillHeight(shellRef);

  const baseQuery = (nextKey: string, nextProfile: string, nextView: string = view) =>
    `/system/agents/chain?key=${encodeURIComponent(nextKey)}${nextProfile ? `&profile=${encodeURIComponent(nextProfile)}` : ""}${nextView === "route" ? "&view=route" : ""}`;

  function switchTo(next: string) {
    // 切换 Agent：清空 profile 预览，组件重取数并复位路径/视口。
    router.replace(baseQuery(next, ""));
  }

  function setProfile(next: string) {
    router.replace(baseQuery(key, next === "__none__" ? "" : next));
  }

  function setView(next: "chain" | "route") {
    router.replace(baseQuery(key, profileParam, next));
  }

  return (
    <div ref={shellRef} className="flex flex-col" style={{ height }}>
      <div className="flex shrink-0 flex-wrap items-start justify-between gap-3">
        <div className="flex items-start gap-2">
          <Button asChild variant="ghost" size="icon-sm">
            <Link href="/system/agents" aria-label="返回 Agent 列表">
              <ArrowLeftIcon className="size-4" />
            </Link>
          </Button>
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-2">
              <h1 className="font-semibold text-xl tracking-tight">全链路</h1>
              <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
                只读
              </Badge>
              {current && <span className="font-mono text-muted-foreground text-xs">{current.key}</span>}
            </div>
            <p className="truncate text-muted-foreground text-sm">
              {current?.name ?? (agents.length > 0 ? "未知 Agent" : key)} · 一次 run 的系统提示词装配：LLM 独立提示词 →
              正文 → 技能 → 代码硬尾 → 工具 / MCP
            </p>
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <div className="inline-flex items-center rounded-md border p-0.5" role="tablist" aria-label="全链路视图">
            <Button
              size="sm"
              variant={view === "chain" ? "secondary" : "ghost"}
              className="h-7 px-2 text-xs"
              role="tab"
              aria-selected={view === "chain"}
              onClick={() => setView("chain")}
            >
              装配链
            </Button>
            <Button
              size="sm"
              variant={view === "route" ? "secondary" : "ghost"}
              className="h-7 px-2 text-xs"
              role="tab"
              aria-selected={view === "route"}
              onClick={() => setView("route")}
            >
              LLM 路由
            </Button>
          </div>
          <Select value={key} onValueChange={switchTo}>
            <SelectTrigger className="h-8 w-full sm:w-64" aria-label="切换 Agent">
              <SelectValue placeholder="选择 Agent" />
            </SelectTrigger>
            <SelectContent>
              {builtin.map((a) => (
                <SelectItem key={a.key} value={a.key}>
                  <span className="truncate">{a.name}</span>
                  <span className="ml-2 font-mono text-muted-foreground text-xs">{a.key}</span>
                </SelectItem>
              ))}
              {custom.map((a) => (
                <SelectItem key={a.key} value={a.key}>
                  <span className="truncate">{a.name}</span>
                  <span className="ml-2 font-mono text-muted-foreground text-xs">{a.key}</span>
                  <Badge variant="outline" className="ml-2 px-1 py-0 text-[10px]">
                    自定义
                  </Badge>
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <div className="flex flex-wrap items-center gap-1.5">
            <Select value={profileParam || "__none__"} onValueChange={setProfile}>
              <SelectTrigger className="h-8 w-full font-mono text-xs sm:w-56" aria-label="预览 LLM 配置（不改变绑定）">
                <SelectValue placeholder="不预览（按实际解析）" />
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
              <span
                className="inline-flex items-center gap-1 rounded border border-sky-500/40 bg-sky-500/5 px-1.5 py-0.5 text-[10px] text-sky-600 dark:text-sky-400"
                title="当前以选中的 LLM 配置解析装配链，只读预览，不改变 Agent 绑定"
              >
                预览中 · 不改变绑定
                <button
                  type="button"
                  aria-label="清除预览"
                  className="rounded-sm hover:bg-sky-500/10"
                  onClick={() => setProfile("__none__")}
                >
                  <XIcon className="size-3" />
                </button>
              </span>
            )}
          </div>
          <Button asChild size="sm" variant="outline">
            <Link href={`/system/agents/detail?key=${encodeURIComponent(key)}`}>
              <SlidersHorizontalIcon /> 去配置
            </Link>
          </Button>
        </div>
      </div>

      <div className="mt-3 flex min-h-0 flex-1 flex-col overflow-hidden">
        {view === "route" ? (
          <AgentLLMRoutePanel agentKey={key} active />
        ) : (
          <AgentPromptChain agentKey={key} active profileId={profileParam || undefined} />
        )}
      </div>
      {selectedProfile && (
        <p className="shrink-0 pt-1 text-right font-mono text-[10px] text-muted-foreground">
          预览配置：{selectedProfile.name} · #{selectedProfile.id} · {selectedProfile.model}
        </p>
      )}
    </div>
  );
}

// useSearchParams must sit under a Suspense boundary for static export.
export default function AgentChainPage() {
  return (
    <React.Suspense fallback={null}>
      <ChainPageInner />
    </React.Suspense>
  );
}
