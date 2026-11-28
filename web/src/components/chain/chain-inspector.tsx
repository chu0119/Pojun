"use client";

import * as React from "react";

import { ArrowLeftIcon, TriangleAlertIcon } from "lucide-react";
import Link from "next/link";

import { Button } from "@/components/ui/button";
import { GROUP_LABEL, type ChainItem } from "@/lib/chain-model";
import type { AgentPromptChain as AgentPromptChainData } from "@/lib/types";
import { cn } from "@/lib/utils";

import {
  ConfigActions,
  DeferredDetail,
  ExcludedToolDetail,
  Kv,
  LLMDetail,
  MCPDetail,
  MemberList,
  ModelDetail,
  SegmentDetail,
  SkillDetail,
  ToolsDetail,
} from "./chain-details";
import { StatusWord } from "./chain-status";
import type { PromptChainController } from "./use-prompt-chain";

const linkCls =
  "inline-flex h-6 items-center rounded px-1.5 text-muted-foreground text-xs hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-foreground";

function agentHref(chain: AgentPromptChainData) {
  return `/system/agents/detail?key=${encodeURIComponent(chain.agent_key)}`;
}

function linkFor(chain: AgentPromptChainData, item: ChainItem): { label: string; href: string } {
  if (item.id === "llm" || item.id === "seg:profile_prompt") return { label: "LLM 配置", href: "/system/llm" };
  if (item.id === "skills" || item.id.startsWith("skill:")) return { label: "Skill", href: "/system/skills" };
  if (item.id === "seg:host_tools" || item.id.startsWith("xtool:")) return { label: "工具", href: "/system/tools" };
  if (item.id === "mcp" || item.id.startsWith("mcp:") || item.id === "seg:deferred_tools") {
    return { label: "MCP", href: "/system/mcp" };
  }
  return { label: "Agent 配置", href: agentHref(chain) };
}

export function ChainInspector({ pc }: { pc: PromptChainController }) {
  const { model, chain } = pc;
  if (!model || !chain) {
    return (
      <div className="grid gap-2 pt-1">
        {Array.from({ length: 5 }).map((_, i) => (
          <div key={i} className="h-9 animate-pulse rounded-md border bg-muted/40" />
        ))}
      </div>
    );
  }
  const item = pc.item(pc.sel);
  return item ? <DetailView pc={pc} item={item} chain={chain} /> : <Overview pc={pc} chain={chain} />;
}

function Overview({ pc, chain }: { pc: PromptChainController; chain: AgentPromptChainData }) {
  const model = pc.model!;
  const n = model.attention.length;
  return (
    <div className="grid content-start gap-3.5 pt-1">
      <h2 className="font-semibold text-base">{n > 0 ? `${n} 项需要注意` : "一切正常"}</h2>
      <p className="-mt-2 text-muted-foreground text-xs leading-relaxed">
        {n > 0 ? "其余各段状态正常。点带「查看」的条目定位到对应的对象。" : "没有发现异常。点清单或关系图里的任意对象查看详情。"}
      </p>
      {n > 0 && (
        <div className="grid gap-0.5">
          {model.attention.map((a, i) => {
            const target = a.itemId ? pc.item(a.itemId) : undefined;
            return (
              <button
                key={i}
                type="button"
                className={cn(
                  "flex w-full items-start gap-2 rounded-md px-2 py-1 text-left",
                  target ? "hover:bg-accent" : "cursor-default",
                  "focus-visible:ring-2 focus-visible:ring-foreground",
                )}
                onClick={() => (target ? pc.reveal(target.id) : undefined)}
              >
                <span className="min-w-0 flex-1 text-sm leading-5">{a.text}</span>
                {target && (
                  <span className="inline-flex shrink-0 items-center gap-1 pt-0.5 text-amber-700 text-xs dark:text-amber-400">
                    <TriangleAlertIcon className="size-3.5" aria-hidden="true" />
                    查看
                  </span>
                )}
              </button>
            );
          })}
        </div>
      )}
      {model.notes.length > 0 && (
        <div>
          <div className="mb-1.5 text-muted-foreground text-xs">说明 {model.notes.length} 条 · 不计入需注意</div>
          <div className="grid gap-1">
            {model.notes.map((t, i) => (
              <p key={i} className="px-2 text-muted-foreground text-xs leading-relaxed">
                {t}
              </p>
            ))}
          </div>
        </div>
      )}
      <div>
        <div className="mb-1 text-muted-foreground text-xs">解析摘要</div>
        <Kv
          rows={[
            ["模型", <span className="font-mono text-xs">{model.summary.model}</span>],
            ["配置来源", `${model.summary.profile} · ${model.summary.sourceLabel}`],
            ["独立提示词", model.summary.prompt],
            ["思考强度", `${model.summary.effort}（${model.summary.effortSource}）`],
          ]}
        />
      </div>
      <div>
        <div className="mb-1 text-muted-foreground text-xs">去配置</div>
        <div className="flex flex-wrap gap-1">
          {[
            ["Agent 配置", agentHref(chain)],
            ["LLM 配置", "/system/llm"],
            ["Skill", "/system/skills"],
            ["工具", "/system/tools"],
            ["MCP", "/system/mcp"],
          ].map(([label, href]) => (
            <Link key={href} href={href} className={linkCls}>
              {label} ↗
            </Link>
          ))}
        </div>
      </div>
      <p className="text-muted-foreground text-xs leading-relaxed">
        这是只读投影：不连接 MCP，不含凭据；运行时注入点只给位置与条件，不展开正文。
      </p>
    </div>
  );
}

function DetailView({
  pc,
  item,
  chain,
}: {
  pc: PromptChainController;
  item: ChainItem;
  chain: AgentPromptChainData;
}) {
  const model = pc.model!;
  const parent = pc.parentOf(item.id);
  const totalOrdered = model.items.filter((i) => i.order != null).length;
  const pos = item.order
    ? `第 ${item.order} 位 / 共 ${totalOrdered} 位`
    : parent
      ? `属于「${parent.title}」`
      : item.id === "model"
        ? "装配结果"
        : GROUP_LABEL[item.group].split(" · ")[0];
  const link = linkFor(chain, item);
  const related = pc.related(item.id);
  const configurable = item.id === "llm" || item.id === "seg:profile_prompt" || item.id === "seg:body";

  return (
    <div className="grid content-start gap-3 pt-1">
      <div className="-ml-1 flex items-center gap-2">
        <button
          type="button"
          className="inline-flex h-7 items-center gap-1 rounded-md px-1.5 text-muted-foreground text-xs hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-foreground"
          onClick={() => {
            if (!pc.confirmDiscard()) return;
            pc.select(null);
          }}
        >
          <ArrowLeftIcon className="size-3.5" /> 总览
        </button>
      </div>
      <div className="flex flex-wrap items-baseline gap-3">
        <h2 className="font-semibold text-base">{item.title}</h2>
        <span className="text-muted-foreground text-xs">{pos}</span>
      </div>
      <p className="-mt-2 flex flex-wrap items-center gap-1.5 text-muted-foreground text-xs">
        <StatusWord state={item.state} word={item.stateWord} />
        <span>{item.note}</span>
      </p>
      {parent && (
        <div className="flex flex-wrap gap-2">
          <Button
            size="sm"
            variant="outline"
            className="text-xs focus-visible:ring-2 focus-visible:ring-foreground"
            onClick={() => pc.reveal(parent.id)}
          >
            回到「{parent.title}」
          </Button>
        </div>
      )}
      {configurable && (
        <ConfigActions
          chain={chain}
          target={item.target}
          onReload={pc.reload}
          onDirtyChange={pc.setDirty}
        />
      )}
      <Body pc={pc} item={item} chain={chain} />
      {related.length > 0 && (
        <div>
          <div className="mb-1.5 text-muted-foreground text-xs">相关</div>
          <div className="flex flex-wrap gap-2">
            {related.map((r) => (
              <Button
                key={r.id}
                size="sm"
                variant="outline"
                className="text-xs focus-visible:ring-2 focus-visible:ring-foreground"
                onClick={() => pc.reveal(r.id)}
              >
                {r.title}
              </Button>
            ))}
          </div>
        </div>
      )}
      <div>
        <div className="mb-1 text-muted-foreground text-xs">去配置</div>
        <div className="flex flex-wrap gap-1">
          <Link href={link.href} className={linkCls}>
            {link.label} ↗
          </Link>
        </div>
      </div>
    </div>
  );
}

function GroupBody({
  pc,
  item,
  intro,
}: {
  pc: PromptChainController;
  item: ChainItem;
  intro: string;
}) {
  return (
    <div className="grid gap-3">
      <p className="text-muted-foreground text-xs leading-relaxed">{intro}</p>
      <MemberList item={item} onSelect={pc.reveal} />
    </div>
  );
}

function Body({ pc, item, chain }: { pc: PromptChainController; item: ChainItem; chain: AgentPromptChainData }) {
  const model = pc.model!;
  const t = item.target;
  if (t.kind === "segment") {
    return <SegmentDetail seg={chain.segments.find((s) => s.key === t.key)} onCopyText={pc.copyText} />;
  }
  switch (t.kind) {
    case "deferred":
      return <DeferredDetail chain={chain} onSelect={pc.reveal} />;
    case "llm":
      return <LLMDetail chain={chain} onCopyText={pc.copyText} />;
    case "skill":
      return <SkillDetail skill={chain.skills.find((s) => s.name === t.name)} />;
    case "skills":
      return (
        <GroupBody
          pc={pc}
          item={item}
          intro="宿主绑定；速查块只列名称与描述，正文在模型需要时按名加载。"
        />
      );
    case "code":
      return (
        <GroupBody
          pc={pc}
          item={item}
          intro="代码常量拼接，UI 不可修改；独立提示词不能替换它们。点击任一段查看正文。"
        />
      );
    case "runtime":
      return (
        <GroupBody
          pc={pc}
          item={item}
          intro="运行时按任务当时事实注入；静态视图只显示位置与门控，不展开正文。"
        />
      );
    case "tools":
      return <ToolsDetail chain={chain} />;
    case "mcp":
      return <MCPDetail mcp={chain.mcp.find((m) => m.id === t.id)} />;
    case "mcp-group":
      return (
        <GroupBody pc={pc} item={item} intro="这些服务器随请求一起下发；点开一台查看健康与工具缓存。只读缓存，不连接。" />
      );
    case "excluded":
      return <GroupBody pc={pc} item={item} intro="排除项常驻展示，不藏进提示层里。" />;
    case "excluded-tool":
      return <ExcludedToolDetail tool={chain.tools.excluded.find((x) => x.key === t.key)} />;
    case "model":
      return (
        <ModelDetail
          chain={chain}
          items={model.items}
          onSelect={pc.reveal}
          onCopyStatic={() => void pc.copyStatic()}
        />
      );
    default:
      return null;
  }
}
