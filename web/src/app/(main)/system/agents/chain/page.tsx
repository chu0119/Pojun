"use client";

import * as React from "react";

import Link from "next/link";
import { useRouter, useSearchParams } from "next/navigation";

import { ArrowLeftIcon, SlidersHorizontalIcon } from "lucide-react";

import { AgentPromptChain } from "@/components/agent-prompt-chain";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { api } from "@/lib/api";
import type { Agent } from "@/lib/types";

// Full-width read-only page for one agent's prompt assembly chain. Replaces the
// tab that used to live inside the agent drawer: the graph is wide, so it gets
// its own route and its own agent switcher. Switching only changes what is
// displayed; editing the binding stays in the agent config editor.
const DEFAULT_AGENT = "worker";

function ChainPageInner() {
  const router = useRouter();
  const searchParams = useSearchParams();
  const key = searchParams.get("key")?.trim() || DEFAULT_AGENT;

  const [agents, setAgents] = React.useState<Agent[]>([]);
  React.useEffect(() => {
    api.agents().then(setAgents).catch(() => setAgents([]));
  }, []);

  const current = agents.find((a) => a.key === key) ?? null;
  const builtin = agents.filter((a) => a.builtin);
  const custom = agents.filter((a) => !a.builtin);

  function switchTo(next: string) {
    router.replace(`/system/agents/chain?key=${encodeURIComponent(next)}`);
  }

  return (
    <div className="flex flex-1 flex-col gap-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex items-start gap-2">
          <Button asChild variant="ghost" size="icon-sm">
            <Link href="/system/agents" aria-label="返回 Agent 列表">
              <ArrowLeftIcon className="size-4" />
            </Link>
          </Button>
          <div>
            <div className="flex flex-wrap items-center gap-2">
              <h1 className="font-semibold text-xl tracking-tight">全链路</h1>
              <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
                只读
              </Badge>
              {current && (
                <span className="font-mono text-muted-foreground text-xs">{current.key}</span>
              )}
            </div>
            <p className="text-muted-foreground text-sm">
              {current?.name ?? (agents.length > 0 ? "未知 Agent" : key)} · 一次 run 的系统提示词装配：LLM 独立提示词 → 正文 → 技能 → 代码硬尾 → 工具 / MCP
            </p>
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Select value={key} onValueChange={switchTo}>
            <SelectTrigger className="h-8 w-64" aria-label="切换 Agent">
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
          <Button asChild size="sm" variant="outline">
            <Link href={`/system/agents/detail?key=${encodeURIComponent(key)}`}>
              <SlidersHorizontalIcon /> 去配置
            </Link>
          </Button>
        </div>
      </div>

      <div className="flex min-h-0 flex-1 flex-col overflow-y-auto rounded-lg border p-4">
        <AgentPromptChain agentKey={key} active />
      </div>
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
