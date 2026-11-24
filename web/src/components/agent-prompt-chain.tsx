"use client";

import * as React from "react";

import {
  AlertTriangleIcon,
  CopyIcon,
  RefreshCwIcon,
  XIcon,
} from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { api } from "@/lib/api";
import type {
  AgentPromptChain as AgentPromptChainData,
  PromptChainMCP,
  PromptChainSegment,
  PromptChainTool,
} from "@/lib/types";
import { cn } from "@/lib/utils";

// Gate Signal Flow: a read-only vertical trunk of 34px work rows. Nodes are
// deliberately not cards: the trunk, the gaps, and the gate pills carry the
// order. The profile overlay is a branch from the LLM rail into the exact gap
// where it enters; runtime-only segments collapse into a dashed bus.

const SOURCE_STROKE: Record<string, string> = {
  profile_prompt: "stroke-fuchsia-500",
  db_template: "stroke-emerald-500",
  builtin_default: "stroke-sky-500",
  skill_index: "stroke-emerald-500",
  code: "stroke-amber-500",
  host: "stroke-violet-500",
  runtime: "stroke-muted-foreground",
};

const SOURCE_FILL: Record<string, string> = {
  profile_prompt: "fill-fuchsia-500",
  db_template: "fill-emerald-500",
  builtin_default: "fill-sky-500",
  skill_index: "fill-emerald-500",
  code: "fill-amber-500",
  host: "fill-violet-500",
  runtime: "fill-muted-foreground",
};

const SOURCE_TEXT: Record<string, string> = {
  profile_prompt: "fill-fuchsia-600 dark:fill-fuchsia-400",
  db_template: "fill-emerald-600 dark:fill-emerald-400",
  builtin_default: "fill-sky-600 dark:fill-sky-400",
  skill_index: "fill-emerald-600 dark:fill-emerald-400",
  code: "fill-amber-600 dark:fill-amber-400",
  host: "fill-violet-600 dark:fill-violet-400",
  runtime: "fill-muted-foreground",
};

const SOURCE_HTML_TEXT: Record<string, string> = {
  profile_prompt: "text-fuchsia-600 dark:text-fuchsia-400 border-fuchsia-500/40",
  db_template: "text-emerald-600 dark:text-emerald-400 border-emerald-500/40",
  builtin_default: "text-sky-600 dark:text-sky-400 border-sky-500/40",
  skill_index: "text-emerald-600 dark:text-emerald-400 border-emerald-500/40",
  code: "text-amber-600 dark:text-amber-400 border-amber-500/40",
  host: "text-violet-600 dark:text-violet-400 border-violet-500/40",
  runtime: "text-muted-foreground border-muted-foreground/40",
};

const TAG_BY_KIND: Record<string, string> = {
  profile_prompt: "PT",
  body: "BD",
  skill_index: "SK",
  host: "HT",
  code_tail: "CT",
};

function modeLabel(mode: string) {
  switch (mode) {
    case "prepend":
      return "前置";
    case "append":
      return "追加";
    case "replace":
      return "替换正文";
    default:
      return "未配置";
  }
}

function healthLabel(h: PromptChainMCP["health"]) {
  switch (h) {
    case "ok":
      return "健康";
    case "failed":
      return "上次失败";
    default:
      return "未检查";
  }
}

function sourceLabel(seg: PromptChainSegment) {
  return seg.source_label || seg.source;
}

function trimText(value: string, max: number) {
  return value.length > max ? `${value.slice(0, max - 1)}…` : value;
}

const MODE_MARK: Record<string, string> = {
  prepend: "ƒ+",
  append: "ƒ−",
  replace: "ƒ!",
};

type RowKind = "profile" | "body" | "skill" | "code" | "host" | "runtime" | "segment";

type GraphRow = {
  id: string;
  kind: RowKind;
  tag: string;
  title: string;
  sub: string;
  chars: number;
  rawChars: number;
  mark: string;
  source: string;
  present: boolean;
  available: boolean;
  editable: boolean;
  gate?: string;
  reason?: string;
  keys: string[];
  order: number;
  h: number;
  y: number;
};

type Selection =
  | { kind: "segment"; key: string }
  | { kind: "code" }
  | { kind: "runtime" }
  | { kind: "llm" }
  | { kind: "skills" }
  | { kind: "tools" }
  | { kind: "mcp"; id: number }
  | { kind: "model" }
  | { kind: "excluded" };

function buildRows(chain: AgentPromptChainData): GraphRow[] {
  const rows: GraphRow[] = [];
  const code: PromptChainSegment[] = [];
  const runtime: PromptChainSegment[] = [];
  const generic: PromptChainSegment[] = [];

  for (const seg of chain.segments) {
    if (seg.kind === "code_tail") {
      code.push(seg);
    } else if (!seg.available) {
      runtime.push(seg);
    } else if (seg.kind === "profile_prompt" || seg.kind === "body" || seg.kind === "skill_index" || seg.kind === "host") {
      rows.push({
        id: seg.key,
        kind: seg.kind === "profile_prompt" ? "profile" : seg.kind === "body" ? "body" : seg.kind === "skill_index" ? "skill" : "host",
        tag: TAG_BY_KIND[seg.kind] || "SEG",
        title: seg.title,
        sub: sourceLabel(seg),
        chars: seg.present ? seg.chars : 0,
        rawChars: seg.chars,
        mark:
          seg.kind === "profile_prompt"
            ? MODE_MARK[chain.llm.prompt_mode] || "ƒ?"
            : seg.kind === "body"
              ? chain.llm.prompt_active && chain.llm.prompt_mode === "replace"
                ? "ƒ!"
                : "＝"
              : seg.kind === "skill_index" || seg.kind === "host"
                ? "⊕"
                : "＝",
        source: seg.source,
        present: seg.present,
        available: seg.available,
        editable: seg.editable,
        gate: seg.gate,
        reason: seg.reason,
        keys: [seg.key],
        order: seg.order,
        h: 34,
        y: 0,
      });
    } else {
      generic.push(seg);
    }
  }

  if (code.length > 0) {
    const present = code.filter((s) => s.present).length;
    const inactive = code.length - present;
    rows.push({
      id: "code",
      kind: "code",
      tag: "CT",
      title: `代码硬尾巴 ×${code.length}`,
      sub: inactive > 0 ? `${inactive} 段门控未触发 · UI 不可改` : "locked · UI 不可改",
      chars: code.reduce((n, s) => n + (s.present ? s.chars : 0), 0),
      rawChars: code.reduce((n, s) => n + s.chars, 0),
      mark: "▣",
      source: "code",
      present: present > 0,
      available: true,
      editable: false,
      keys: code.map((s) => s.key),
      order: Math.min(...code.map((s) => s.order)),
      h: 34,
      y: 0,
    });
  }

  for (const seg of generic) {
    rows.push({
      id: seg.key,
      kind: "segment",
      tag: TAG_BY_KIND[seg.kind] || "SEG",
      title: seg.title,
      sub: sourceLabel(seg),
      chars: seg.present ? seg.chars : 0,
      rawChars: seg.chars,
      mark: "＝",
      source: seg.source,
      present: seg.present,
      available: seg.available,
      editable: seg.editable,
      gate: seg.gate,
      reason: seg.reason,
      keys: [seg.key],
      order: seg.order,
      h: 34,
      y: 0,
    });
  }

  if (runtime.length > 0) {
    rows.push({
      id: "runtime",
      kind: "runtime",
      tag: "RT",
      title: `运行时总线 ×${runtime.length}`,
      sub: "运行时注入 · 只显示门控位置",
      chars: 0,
      rawChars: 0,
      mark: "?",
      source: "runtime",
      present: false,
      available: false,
      editable: false,
      keys: runtime.map((s) => s.key),
      order: Math.min(...runtime.map((s) => s.order)),
      h: 44,
      y: 0,
    });
  }

  rows.sort((a, b) => a.order - b.order || a.id.localeCompare(b.id));
  let y = 100;
  for (const row of rows) {
    row.y = y;
    y += row.h + 20;
  }
  return rows;
}

function ToolLine({ item }: { item: PromptChainTool }) {
  return (
    <li className="rounded-md border px-2 py-1.5">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-mono text-xs">{item.key}</span>
        {item.deferred && (
          <Badge variant="outline" className="px-1.5 py-0 text-[10px]">延迟 schema</Badge>
        )}
        {item.gate && (
          <Badge variant="outline" className="px-1.5 py-0 text-[10px] text-muted-foreground">{item.gate}</Badge>
        )}
      </div>
      {item.description && (
        <p className="mt-0.5 line-clamp-2 text-muted-foreground text-[11px]">{item.description}</p>
      )}
    </li>
  );
}

type LampTone = "quiet" | "hollow" | "warn" | "danger" | "dangerOutline" | "stale";

const LAMP_TONE_CLASS: Record<LampTone, string> = {
  quiet: "border border-muted-foreground/25 bg-transparent",
  hollow: "border border-muted-foreground/60 bg-transparent",
  warn: "border border-amber-500 bg-amber-500/20",
  danger: "border border-destructive bg-destructive",
  dangerOutline: "border border-destructive bg-transparent",
  stale: "border border-amber-500 bg-transparent",
};

function Lamp({
  tone,
  title,
  lampTest,
  onClick,
}: {
  tone: LampTone;
  title: string;
  lampTest: boolean;
  onClick?: () => void;
}) {
  const dot = (
    <span
      className={cn(
        "inline-block size-[10px] rounded-[2px]",
        LAMP_TONE_CLASS[tone],
        lampTest && "!border-foreground !bg-foreground/70",
      )}
      style={
        tone === "stale" && !lampTest
          ? { backgroundImage: "repeating-linear-gradient(45deg, rgba(245,158,11,.65) 0 2px, transparent 2px 5px)" }
          : undefined
      }
      aria-hidden
    />
  );
  if (onClick) {
    return (
      <button
        type="button"
        className="flex size-4 items-center justify-center rounded-[2px] hover:bg-muted/60"
        title={title}
        aria-label={title}
        onClick={onClick}
      >
        {dot}
      </button>
    );
  }
  return (
    <span className="flex size-4 items-center justify-center" title={title} aria-label={title}>
      {dot}
    </span>
  );
}

function SituationUnit({
  label,
  value,
  title,
  onClick,
}: {
  label: string;
  value: string;
  title?: string;
  onClick?: () => void;
}) {
  const inner = (
    <>
      <div className="font-mono text-[7px] text-muted-foreground uppercase tracking-[0.12em]">{label}</div>
      <div className="mt-0.5 truncate text-[11px] tabular-nums" title={title ?? value}>{value}</div>
    </>
  );
  const cls = "min-w-[140px] flex-1 border-r border-border/60 px-2 text-left last:border-r-0";
  if (onClick) {
    return (
      <button type="button" className={cn(cls, "hover:bg-muted/30")} onClick={onClick} title={title}>
        {inner}
      </button>
    );
  }
  return <div className={cls} title={title}>{inner}</div>;
}

function SituationStrip({
  chain,
  lampTest,
  onLampTest,
  onSelect,
}: {
  chain: AgentPromptChainData;
  lampTest: boolean;
  onLampTest: () => void;
  onSelect: (s: Selection) => void;
}) {
  const llm = chain.llm;
  const excluded = chain.tools.excluded.length;
  const outOfIndex = chain.skills.filter((s) => !s.in_index).length;
  const enabledMCP = chain.mcp.filter((m) => m.enabled);
  const mcpOk = enabledMCP.filter((m) => m.health === "ok").length;
  const mcpFailed = enabledMCP.filter((m) => m.health === "failed").length;
  const mcpNever = enabledMCP.filter((m) => m.health === "never_checked").length;
  const mcpStale = chain.mcp.filter((m) => m.cache_stale).length;
  const runtimeCount = chain.segments.filter((s) => !s.available).length;
  const overlayValue = llm.prompt_active
    ? `${MODE_MARK[llm.prompt_mode] ?? ""} ${modeLabel(llm.prompt_mode)} · ${llm.prompt_chars}c · ACTIVE`
    : llm.prompt_mode
      ? `WITHHELD · inherit=${llm.prompt_apply_inherited ? "on" : "off"}`
      : "NONE";
  const profileValue = llm.name
    ? `${llm.name}${llm.profile_id ? ` #${llm.profile_id}` : ""} · ${llm.source_label}`
    : "未解析";
  const firstFailedMCP = chain.mcp.find((m) => m.health === "failed") ?? chain.mcp[0];
  const mcpTone: LampTone = mcpFailed > 0 ? "danger" : mcpStale > 0 ? "stale" : mcpNever > 0 ? "hollow" : "quiet";

  return (
    <div className="flex flex-wrap items-stretch gap-y-2 border-y bg-muted/15 py-2">
      <div className="flex min-w-0 flex-1 flex-wrap gap-y-2">
        <SituationUnit label="AGENT" value={`${chain.agent_key} · ${chain.agent_kind}`} onClick={() => onSelect({ kind: "model" })} />
        <SituationUnit
          label="PROFILE"
          value={profileValue}
          title={`${profileValue}（来源：${llm.source_label}）`}
          onClick={() => onSelect({ kind: "llm" })}
        />
        <SituationUnit
          label="OVERLAY"
          value={overlayValue}
          title={
            llm.prompt_active
              ? `独立提示词已生效：${modeLabel(llm.prompt_mode)}，${llm.prompt_chars} 字符`
              : llm.prompt_mode
                ? "配置了独立提示词，但当前解析路径不生效（继承开关或绑定原因）"
                : "该配置没有独立提示词"
          }
          onClick={() => onSelect({ kind: "llm" })}
        />
        <SituationUnit label="SCALE" value={`${chain.totals.segments} 段 · ${chain.totals.chars}c · ≈${chain.totals.est_tokens} tok`} onClick={() => onSelect({ kind: "model" })} />
        <SituationUnit
          label="INTEGRITY"
          value={`${chain.warnings.length} 告警 · ${excluded} 排除 · MCP ${mcpOk}/${enabledMCP.length} ok`}
          onClick={() => onSelect({ kind: "excluded" })}
        />
        <SituationUnit
          label="RUNTIME"
          value={`${runtimeCount} 注入点 · 未采集`}
          title="静态快照不展开运行时正文；仅显示注入位置与门控"
          onClick={() => onSelect({ kind: "runtime" })}
        />
      </div>
      <div className="flex items-center gap-1 px-2">
        <span className="mr-1 font-mono text-[7px] text-muted-foreground uppercase tracking-[0.12em]">LAMPS</span>
        <Lamp tone={llm.name ? "quiet" : "dangerOutline"} title={llm.name ? `已解析：${llm.name}` : "未解析到 LLM 配置"} lampTest={lampTest} onClick={() => onSelect({ kind: "llm" })} />
        <Lamp
          tone={llm.prompt_active ? "quiet" : llm.prompt_mode ? "dangerOutline" : "quiet"}
          title={llm.prompt_active ? "独立提示词生效" : llm.prompt_mode ? "独立提示词被扣留（未生效）" : "无独立提示词"}
          lampTest={lampTest}
          onClick={() => onSelect({ kind: "llm" })}
        />
        <Lamp tone={outOfIndex > 0 ? "hollow" : "quiet"} title={`Skill 索引外 ${outOfIndex}`} lampTest={lampTest} onClick={() => onSelect({ kind: "skills" })} />
        <Lamp tone={excluded > 0 ? "danger" : "quiet"} title={`排除 ${excluded} 项`} lampTest={lampTest} onClick={() => onSelect({ kind: "excluded" })} />
        <Lamp tone={mcpTone} title={`MCP：${mcpOk} ok / ${mcpFailed} failed / ${mcpNever} 未检查 / ${mcpStale} 缓存过期`} lampTest={lampTest} onClick={firstFailedMCP ? () => onSelect({ kind: "mcp", id: firstFailedMCP.id }) : undefined} />
        <Lamp tone="hollow" title={`运行时注入点 ${runtimeCount} 个 · 未采集`} lampTest={lampTest} onClick={() => onSelect({ kind: "runtime" })} />
        <Lamp tone={chain.warnings.length > 0 ? "warn" : "quiet"} title={chain.warnings.length > 0 ? `未分类告警 ${chain.warnings.length} 条` : "无告警"} lampTest={lampTest} />
        <button
          type="button"
          className="ml-1 border px-1.5 py-0.5 font-mono text-[8px] text-muted-foreground tracking-wider hover:bg-muted/40"
          onClick={onLampTest}
          title="一次性点亮全部指示灯，证明暗是真的没有信号"
        >
          LAMP TEST
        </button>
      </div>
    </div>
  );
}

export function AgentPromptChain({
  agentKey,
  active,
  reloadKey,
}: {
  agentKey: string;
  active: boolean;
  reloadKey?: number;
}) {
  const [chain, setChain] = React.useState<AgentPromptChainData | null>(null);
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState("");
  const [selection, setSelection] = React.useState<Selection | null>(null);
  const [lampTest, setLampTest] = React.useState(false);
  const reqRef = React.useRef(0);

  React.useEffect(() => {
    if (!lampTest) return;
    const t = window.setTimeout(() => setLampTest(false), 180);
    return () => window.clearTimeout(t);
  }, [lampTest]);

  const load = React.useCallback(async () => {
    const seq = ++reqRef.current;
    setLoading(true);
    setError("");
    try {
      const data = await api.agentPromptChain(agentKey);
      if (reqRef.current === seq) {
        setChain(data);
        const profile = data.segments.find((s) => s.kind === "profile_prompt");
        const body = data.segments.find((s) => s.kind === "body");
        const initial = profile?.present ? profile : body;
        setSelection(initial ? { kind: "segment", key: initial.key } : null);
      }
    } catch (e) {
      if (reqRef.current === seq) {
        setChain(null);
        setSelection(null);
        setError((e as Error).message);
      }
    } finally {
      if (reqRef.current === seq) setLoading(false);
    }
  }, [agentKey]);

  React.useEffect(() => {
    if (active) void load();
  }, [active, load, reloadKey]);

  const copyStatic = React.useCallback(async () => {
    if (!chain) return;
    const text = chain.segments
      .filter((s) => s.present && s.available && s.text)
      .map((s) => s.text)
      .join("");
    try {
      await navigator.clipboard.writeText(text);
      toast.success("已复制静态装配提示词（不含运行时注入段）");
    } catch {
      toast.error("复制失败：浏览器拒绝了剪贴板访问");
    }
  }, [chain]);

  if (!active) return null;

  if (loading && !chain) {
    return (
      <div className="grid gap-2">
        {Array.from({ length: 6 }).map((_, i) => (
          <div key={i} className="h-10 animate-pulse rounded-md border bg-muted/40" />
        ))}
      </div>
    );
  }

  if (error) {
    return (
      <div className="rounded-lg border border-destructive/40 bg-destructive/5 p-4">
        <p className="font-medium text-destructive text-sm">装配链读取失败</p>
        <p className="mt-1 text-muted-foreground text-xs">{error}</p>
        <Button className="mt-3" size="sm" variant="outline" onClick={() => void load()}>
          <RefreshCwIcon className="size-3.5" /> 重试
        </Button>
      </div>
    );
  }

  if (!chain) {
    return <p className="p-4 text-center text-muted-foreground text-sm">（暂无装配数据）</p>;
  }

  return (
    <div className="grid gap-3">
      <Card>
        <CardHeader className="pb-2">
          <div className="flex flex-wrap items-center gap-2">
            <CardTitle className="text-sm">装配链 · 门控信号流</CardTitle>
            <Badge variant="outline" className="px-1.5 py-0 text-[10px]">
              {chain.agent_kind === "builtin" ? "内置 Agent" : "自定义 Agent"}
            </Badge>
            <Badge variant="outline" className="px-1.5 py-0 font-mono text-[10px]">STATIC</Badge>
            <span className="ml-auto font-mono text-muted-foreground text-[10px]">SNAPSHOT 未采集</span>
          </div>
          <CardDescription className="text-xs">
            {chain.llm.name
              ? `${chain.llm.name}（${chain.llm.format} · ${chain.llm.model}） · ${chain.llm.source_label}`
              : "未解析到 LLM 配置"}
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-2">
          <SituationStrip
            chain={chain}
            lampTest={lampTest}
            onLampTest={() => setLampTest(true)}
            onSelect={setSelection}
          />
          <div className="flex flex-wrap items-center gap-2">
            <Button size="sm" variant="outline" onClick={() => void load()} disabled={loading}>
              <RefreshCwIcon className={cn("size-3.5", loading && "animate-spin")} /> 刷新
            </Button>
            <Button size="sm" variant="outline" onClick={() => void copyStatic()}>
              <CopyIcon className="size-3.5" /> 复制静态提示词
            </Button>
            <span className="text-muted-foreground text-xs">
              只读 · 不连接 MCP · 运行时段只展示注入点
            </span>
          </div>
        </CardContent>
      </Card>

      {chain.warnings.length > 0 && (
        <div className="rounded-md border border-amber-500/40 bg-amber-500/5 p-3">
          <div className="mb-1 flex items-center gap-1.5 font-medium text-amber-700 text-xs dark:text-amber-400">
            <AlertTriangleIcon className="size-3.5" /> 注意
          </div>
          <ul className="list-disc pl-5 text-muted-foreground text-xs">
            {chain.warnings.map((w, i) => (
              <li key={i}>{w}</li>
            ))}
          </ul>
        </div>
      )}

      <div className={cn("grid gap-3", selection && "lg:grid-cols-[minmax(0,1fr)_340px]")}>
        <div className="min-w-0 overflow-x-auto rounded-lg border bg-card/40">
          <GateFlow chain={chain} selection={selection} onSelect={setSelection} />
        </div>
        {selection && (
          <DetailPanel
            chain={chain}
            selection={selection}
            onSelect={setSelection}
            onClose={() => setSelection(null)}
            onCopy={() => void copyStatic()}
          />
        )}
      </div>

      <div className="flex flex-wrap items-center gap-2 text-muted-foreground text-[11px]">
        <button type="button" className="underline-offset-2 hover:underline" onClick={() => setSelection({ kind: "tools" })}>
          工具 {chain.tools.total}
        </button>
        <span>·</span>
        <button type="button" className="underline-offset-2 hover:underline" onClick={() => setSelection({ kind: "skills" })}>
          Skill {chain.skills.length}
        </button>
        <span>·</span>
        <span>MCP {chain.mcp.length}</span>
        <span>·</span>
        <button type="button" className="underline-offset-2 hover:underline" onClick={() => setSelection({ kind: "excluded" })}>
          排除 {chain.tools.excluded.length}
        </button>
        <span className="ml-auto">实线=已装配 · 4-3 虚线=门控投影 · 1-3 点线=运行时未知 · 删除线=被替换；点击节点查看原文。</span>
      </div>
    </div>
  );
}

function GateFlow({
  chain,
  selection,
  onSelect,
}: {
  chain: AgentPromptChainData;
  selection: Selection | null;
  onSelect: (s: Selection) => void;
}) {
  const rows = React.useMemo(() => buildRows(chain), [chain]);
  const byId = React.useMemo(() => new Map(rows.map((r) => [r.id, r])), [rows]);
  const trunkX = 450;
  const nodeX = 470;
  const nodeW = 300;
  const rootY = 44;
  const rootH = 34;
  const modelY = rows.length > 0 ? rows[rows.length - 1].y + rows[rows.length - 1].h + 26 : 440;
  const modelH = 46;
  const measureMax = Math.max(1, ...rows.filter((r) => r.available && r.chars > 0).map((r) => r.chars));

  // Left rail (LLM + Skills) and right rail (Tools + MCP) are independent
  // columns. Heights are computed from their contents so the rails can never
  // overlap each other or the quarantine rack.
  const llmY = 96;
  const llmH = 136;
  const skillsY = llmY + llmH + 16;
  const skillsH = 40 + Math.min(chain.skills.length, 6) * 14;
  const leftBottom = skillsY + skillsH;
  const toolsY = 96;
  const toolsH = 86;
  const mcpY = toolsY + toolsH + 16;
  const mcpH = chain.mcp.length > 0 ? 18 + chain.mcp.length * 36 : 40;
  const rightBottom = mcpY + mcpH;
  const bodyBottom = modelY + modelH;

  const excludedCount = chain.tools.excluded.length;
  const outOfIndex = chain.skills.filter((s) => !s.in_index).length;
  const quarantine = excludedCount > 0 || outOfIndex > 0;
  const quarantineH = quarantine ? 30 + Math.min(excludedCount, 4) * 13 + (outOfIndex > 0 ? 18 : 0) + 10 : 0;
  const quarantineY = Math.max(leftBottom, rightBottom, bodyBottom) + 24;
  const height = Math.max(720, quarantineY + quarantineH + 40);

  const profileRow = rows.find((r) => r.kind === "profile");
  const bodyRow = byId.get("body");
  const skillRow = byId.get("skill_index");
  const codeRow = byId.get("code");
  const hostRow = byId.get("host_tools");
  const runtimeRow = byId.get("runtime");

  const selectedKey = selection?.kind === "segment" ? selection.key : null;
  const selectedGroup =
    selection?.kind === "llm" ? "llm" :
    selection?.kind === "skills" ? "skills" :
    selection?.kind === "tools" ? "tools" :
    selection?.kind === "excluded" ? "excluded" :
    selection?.kind === "model" ? "model" :
    selection?.kind === "code" ? "code" :
    selection?.kind === "runtime" ? "runtime" :
    selection?.kind === "mcp" ? `mcp:${selection.id}` : null;

  const rowSelected = (row: GraphRow) => {
    if (selectedKey && row.keys.includes(selectedKey)) return true;
    return selectedGroup === row.id;
  };

  const overlayActive = !!profileRow?.present;
  const injectLabel = `${overlayActive ? "INJECT" : "WITHHELD"} · ${modeLabel(chain.llm.prompt_mode)}`;
  const injectRow = profileRow;
  const injectAbove = profileRow && bodyRow && profileRow.order > bodyRow.order;
  const injectY = injectRow
    ? injectAbove
      ? injectRow.y - 10
      : injectRow.y + injectRow.h + 10
    : 0;
  const ladderIndex =
    chain.llm.source === "global_profile" || chain.llm.source === "global_profile_inactive" ? 2 : 0;
  const injectStartY =
    chain.llm.source === "preview_override" ? llmY + 18 : llmY + 64 + ladderIndex * 16;

  return (
    <svg
      viewBox={`0 0 1200 ${height}`}
      className="block h-auto w-full min-w-[1100px]"
      role="group"
      aria-label={`${chain.agent_key} 装配链门控信号流`}
    >
      <defs>
        <marker id="gate-arrow" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="5" markerHeight="5" orient="auto">
          <path d="M0,0 L10,5 L0,10 z" className="fill-muted-foreground" />
        </marker>
        <pattern id="gate-hatch" width="6" height="6" patternUnits="userSpaceOnUse" patternTransform="rotate(45)">
          <line x1="0" y1="0" x2="0" y2="6" className="stroke-muted-foreground" strokeWidth="1" opacity="0.25" />
        </pattern>
      </defs>

      <text x={40} y={30} className="fill-muted-foreground font-mono text-[9px]" letterSpacing="0.16em">
        PROMPT GATE FLOW · {chain.agent_key.toUpperCase()} · STATIC
      </text>
      <text x={40} y={48} className="fill-muted-foreground text-[8px]">
        实线=已装配 · 4-3 虚线=门控投影 · 1-3 点线=运行时未知 · 删除线=被替换
      </text>

      <line
        x1={trunkX}
        y1={rootY + rootH}
        x2={trunkX}
        y2={modelY + 12}
        className="stroke-border"
        strokeWidth={2}
      />
      <RootNode chain={chain} x={nodeX} y={rootY} w={nodeW} />

      {rows.map((row) => (
        <GraphRowNode
          key={row.id}
          row={row}
          x={nodeX}
          trunkX={trunkX}
          measureMax={measureMax}
          selected={rowSelected(row)}
          onSelect={() => {
            if (row.kind === "code") onSelect({ kind: "code" });
            else if (row.kind === "runtime") onSelect({ kind: "runtime" });
            else onSelect({ kind: "segment", key: row.keys[0] });
          }}
        />
      ))}

      <line
        x1={trunkX}
        y1={modelY}
        x2={nodeX}
        y2={modelY + modelH / 2}
        className="stroke-border"
        strokeWidth={1.2}
      />
      <ModelNode chain={chain} x={nodeX} y={modelY} w={nodeW} h={modelH} selected={selection?.kind === "model"} onSelect={() => onSelect({ kind: "model" })} />

      {codeRow && <TerminalBoundary y={codeRow.y - 10} />}

      {injectRow && (
        <>
          <path
            d={
              overlayActive
                ? `M340,${injectStartY} H440 V${injectY} H${trunkX}`
                : `M340,${injectStartY} H440 V${injectY}`
            }
            className={cn("fill-none", overlayActive ? "stroke-fuchsia-500" : "stroke-destructive")}
            strokeWidth={1.6}
            strokeDasharray={overlayActive ? undefined : "4 3"}
            opacity={overlayActive ? 0.85 : 0.7}
          />
          {!overlayActive && (
            <text x={444} y={injectY + 3} className="fill-destructive font-mono text-[9px]">⊘</text>
          )}
          <rect
            x={trunkX - 48}
            y={injectY - 7}
            width={96}
            height={14}
            rx={7}
            className={cn("fill-card", overlayActive ? "stroke-fuchsia-500" : "stroke-destructive")}
            strokeWidth={1}
            strokeDasharray={overlayActive ? undefined : "3 2"}
          />
          <text
            x={trunkX}
            y={injectY + 2}
            textAnchor="middle"
            className={cn(
              "font-mono text-[7px]",
              overlayActive ? "fill-fuchsia-600 dark:fill-fuchsia-400" : "fill-destructive",
            )}
          >
            {injectLabel}
          </text>
        </>
      )}
      {bodyRow && profileRow && chain.llm.prompt_mode === "replace" && chain.llm.prompt_active && (
        <>
          <path
            d={`M${trunkX + 30},${profileRow.y + profileRow.h / 2 + 8} V${bodyRow.y + bodyRow.h / 2} H${nodeX}`}
            className="fill-none stroke-destructive"
            strokeDasharray="2 3"
            strokeWidth={1}
          />
          <text x={trunkX + 34} y={bodyRow.y - 4} className="fill-destructive text-[8px]">
            SUPERSEDED{bodyRow.rawChars > 0 ? ` ${bodyRow.rawChars}c` : ""}
          </text>
        </>
      )}

      {hostRow && <GatePill x={trunkX} y={hostRow.y - 10} label="GATE · HOST" />}
      {runtimeRow && <GatePill x={trunkX} y={runtimeRow.y - 10} label="RUNTIME" />}
      {runtimeRow && <GatePill x={trunkX} y={modelY - 10} label="2ND SYSTEM" />}

      {skillRow && (
        <path
          d={`M340,${skillsY + 20} H410 V${skillRow.y + skillRow.h / 2} H${trunkX}`}
          className="fill-none stroke-emerald-500"
          strokeWidth={1.2}
          opacity={0.6}
        />
      )}
      <path
        d={`M810,${toolsY + 20} H790 V${modelY + 17} H${nodeX + nodeW}`}
        className="fill-none stroke-sky-500"
        strokeWidth={1.2}
        opacity={0.55}
      />
      {runtimeRow && (
        <path
          d={`M${nodeX + nodeW},${runtimeRow.y + runtimeRow.h / 2} H790 V${modelY + 40} H${nodeX + nodeW}`}
          className="fill-none stroke-muted-foreground"
          strokeDasharray="1 3"
          strokeWidth={1.1}
          opacity={0.6}
        />
      )}
      {chain.mcp.map((m, i) => (
        <path
          key={m.id}
          d={`M810,${mcpY + 18 + i * 36 + 16} H790 V${modelY + 17} H${nodeX + nodeW}`}
          className={cn(
            "fill-none",
            m.enabled && m.health === "failed" ? "stroke-destructive" : "stroke-sky-500",
          )}
          strokeDasharray={m.enabled && m.health === "failed" ? "2 3" : m.cache_stale ? "4 3" : undefined}
          strokeWidth={1}
          opacity={0.3}
        />
      ))}

      <LLMRail chain={chain} y={llmY} selected={selection?.kind === "llm"} onSelect={() => onSelect({ kind: "llm" })} />
      <SkillsRail chain={chain} y={skillsY} h={skillsH} selected={selection?.kind === "skills"} onSelect={() => onSelect({ kind: "skills" })} />
      <ToolsRail chain={chain} y={toolsY} selected={selection?.kind === "tools"} onSelect={() => onSelect({ kind: "tools" })} />
      <MCPRail chain={chain} y={mcpY} selected={selection} onSelect={onSelect} />

      {quarantine && (
        <Quarantine chain={chain} y={quarantineY} h={quarantineH} selected={selection?.kind === "excluded"} onSelect={() => onSelect({ kind: "excluded" })} />
      )}

      <text x={40} y={height - 24} className="fill-muted-foreground text-[8px]">
        运行时注入点按任务当时事实渲染；本视图只显示位置与门控，不展开正文、不连接 MCP。
      </text>
      <text x={1160} y={height - 24} textAnchor="end" className="fill-muted-foreground text-[8px]">
        STATIC · NO MCP · NO SECRETS
      </text>
    </svg>
  );
}

function RootNode({ chain, x, y, w }: { chain: AgentPromptChainData; x: number; y: number; w: number }) {
  return (
    <g>
      <rect x={x} y={y} width={w} height={34} className="fill-card stroke-border" />
      <rect x={x} y={y} width={4} height={34} className="fill-muted-foreground/60" />
      <text x={x + 14} y={y + 14} className="fill-muted-foreground font-mono text-[8px]">AG</text>
      <text x={x + 14} y={y + 28} className="fill-foreground text-[11px]">
        {chain.agent_key} · {chain.agent_kind}
      </text>
      <text x={x + w - 10} y={y + 14} textAnchor="end" className="fill-muted-foreground font-mono text-[8px]">AGENT</text>
    </g>
  );
}

function GraphRowNode({
  row,
  x,
  trunkX,
  measureMax,
  selected,
  onSelect,
}: {
  row: GraphRow;
  x: number;
  trunkX: number;
  measureMax: number;
  selected: boolean;
  onSelect: () => void;
}) {
  const colorKey = row.kind === "skill" ? "skill_index" : row.source;
  const stroke = SOURCE_STROKE[colorKey] || "stroke-border";
  const fill = SOURCE_FILL[colorKey] || "fill-muted-foreground";
  const text = SOURCE_TEXT[colorKey] || "fill-muted-foreground";
  // Three independent channels: presence (line style), source (left bar + tag
  // color), and editability (pencil/lock). Source color never encodes state.
  const gated = row.available && !row.present;
  const unknown = !row.available;
  const cut = row.kind === "body" && gated;
  const measure = row.available && row.chars > 0 ? Math.max(2, Math.round((row.chars / measureMax) * 264)) : 0;
  return (
    <g
      role="button"
      tabIndex={0}
      aria-label={`${row.title}，${
        row.available ? (row.present ? `${row.chars} 字符` : "未生效") : "运行时未采集"
      }`}
      className="cursor-pointer outline-none"
      onClick={onSelect}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onSelect();
        }
      }}
    >
      <line x1={trunkX} y1={row.y + row.h / 2} x2={x} y2={row.y + row.h / 2} className="stroke-border" strokeWidth={1.2} />
      <rect
        x={x}
        y={row.y}
        width={300}
        height={row.h}
        className={cn("fill-card", stroke, selected && "stroke-2")}
        strokeWidth={selected ? 2 : 1}
        strokeDasharray={unknown ? "1 3" : gated ? "4 3" : undefined}
      />
      {unknown && (
        <rect x={x + 1} y={row.y + 1} width={298} height={row.h - 2} fill="url(#gate-hatch)" className="pointer-events-none" />
      )}
      <rect x={x} y={row.y} width={4} height={row.h} className={fill} opacity={row.present ? 1 : 0.4} />
      <text x={x + 10} y={row.y + 14} className={cn("font-mono text-[8px]", text)}>{row.tag}</text>
      {row.editable && (
        <text x={x + 34} y={row.y + 14} className="fill-sky-600 dark:fill-sky-400 text-[7px]">✎ 可编辑</text>
      )}
      {!row.editable && row.kind === "code" && (
        <text x={x + 34} y={row.y + 14} className="fill-amber-600 dark:fill-amber-400 text-[7px]">▣ 只读</text>
      )}
      <text x={x + 286} y={row.y + 14} textAnchor="end" className="fill-muted-foreground font-mono text-[8px] tabular-nums">
        {row.available ? (row.present ? `${row.chars}c` : "0c") : "—"}
      </text>
      <text x={x + 10} y={row.y + 29} className={cn("font-mono text-[9px]", text)}>{row.mark}</text>
      <text
        x={x + 30}
        y={row.y + 28}
        className={cn("fill-foreground text-[11px]", cut && "line-through opacity-60")}
      >
        {trimText(row.title, 16)}
      </text>
      {row.kind !== "runtime" && (
        <text x={x + 286} y={row.y + 27} textAnchor="end" className="fill-muted-foreground text-[8px]">
          {trimText(row.sub, 10)}
        </text>
      )}
      {row.kind === "runtime" && (
        <g>
          {Array.from({ length: Math.min(row.keys.length, 12) }).map((_, i) => (
            <rect key={i} x={x + 286 - 11 - i * 9} y={row.y + 33} width={6} height={6} className="fill-none stroke-muted-foreground" strokeWidth={1} />
          ))}
        </g>
      )}
      {cut && (
        <line x1={x + 30} y1={row.y + 24} x2={x + 290} y2={row.y + 24} className="stroke-destructive" strokeWidth={1.2} />
      )}
      {row.available && row.chars > 0 && (
        <rect x={x + 10} y={row.y + row.h - 2} width={measure} height={2} className={fill} opacity={0.4} />
      )}
    </g>
  );
}

function TerminalBoundary({ y }: { y: number }) {
  return (
    <g className="pointer-events-none">
      <line x1={410} y1={y} x2={760} y2={y} className="stroke-foreground/60" strokeWidth={2} />
      <text x={760} y={y - 4} textAnchor="end" className="fill-foreground/70 font-mono text-[8px]">
        TERMINAL · 配置层止于此处
      </text>
    </g>
  );
}

function GatePill({ x, y, label }: { x: number; y: number; label: string }) {
  return (
    <g>
      <rect x={x - 80} y={y - 7} width={80} height={14} rx={7} className="fill-card stroke-border" strokeWidth={1} />
      <text x={x - 40} y={y + 2} textAnchor="middle" className="fill-muted-foreground font-mono text-[7px]">{label}</text>
    </g>
  );
}

function ModelNode({
  chain,
  x,
  y,
  w,
  h,
  selected,
  onSelect,
}: {
  chain: AgentPromptChainData;
  x: number;
  y: number;
  w: number;
  h: number;
  selected: boolean;
  onSelect: () => void;
}) {
  return (
    <g
      role="button"
      tabIndex={0}
      aria-label="Provider 请求"
      className="cursor-pointer outline-none"
      onClick={onSelect}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onSelect();
        }
      }}
    >
      <rect x={x} y={y} width={w} height={h} className={cn("fill-card stroke-sky-500", selected && "stroke-2")} strokeWidth={selected ? 2 : 1} />
      <rect x={x} y={y} width={4} height={h} className="fill-sky-500" />
      <text x={x + 14} y={y + 14} className="fill-sky-600 dark:fill-sky-400 font-mono text-[8px]">MC</text>
      <text x={x + 14} y={y + 28} className="fill-foreground text-[11px]">MODEL CALL</text>
      <text x={x + 14} y={y + 40} className="fill-muted-foreground text-[8px]">system + 2nd system + tools</text>
      <text x={x + w - 10} y={y + 16} textAnchor="end" className="fill-muted-foreground font-mono text-[8px] tabular-nums">
        {chain.totals.chars}c
      </text>
      <text x={x + w - 10} y={y + 28} textAnchor="end" className="fill-muted-foreground text-[8px] tabular-nums">
        ≈{chain.totals.est_tokens} tok
      </text>
      <text x={x + w - 10} y={y + 40} textAnchor="end" className="fill-muted-foreground text-[8px]">{chain.tools.total} tools</text>
    </g>
  );
}

function LLMRail({ chain, y, selected, onSelect }: { chain: AgentPromptChainData; y: number; selected: boolean; onSelect: () => void }) {
  const llm = chain.llm;
  const source = llm.source;
  const ladder = [
    {
      label: "1 AGENT BINDING",
      state: source === "agent_binding" ? "MATCH" : source === "preview_override" ? "PREVIEW" : "—",
    },
    {
      label: "2 TASK LLM CHAIN",
      state: source === "none" ? "—" : "RUNTIME?",
    },
    {
      label: "3 GLOBAL ACTIVE",
      state: source === "global_profile" ? "MATCH" : source === "global_profile_inactive" ? "WITHHELD" : "—",
    },
  ];
  const stateClass = (state: string) => {
    switch (state) {
      case "MATCH":
        return "fill-fuchsia-500/20 stroke-fuchsia-500";
      case "PREVIEW":
        return "fill-sky-500/20 stroke-sky-500";
      case "WITHHELD":
        return "fill-transparent stroke-destructive";
      case "RUNTIME?":
        return "fill-transparent stroke-muted-foreground";
      default:
        return "fill-transparent stroke-border";
    }
  };
  const stateText = (state: string) => {
    switch (state) {
      case "MATCH":
        return "fill-fuchsia-600 dark:fill-fuchsia-400";
      case "PREVIEW":
        return "fill-sky-600 dark:fill-sky-400";
      case "WITHHELD":
        return "fill-destructive";
      default:
        return "fill-muted-foreground";
    }
  };
  const proof = llm.profile_id
    ? `binding:${chain.agent_key} → #${llm.profile_id} · ${llm.prompt_mode || "no overlay"} · inherit ${llm.prompt_apply_inherited ? "on" : "off"} · ${llm.prompt_chars}c`
    : source === "global_profile_inactive"
      ? `inherit:global_active → ${llm.name || "?"} · WITHHELD · apply_inherited=false`
      : "not bound · 未解析到 LLM 配置";
  return (
    <g
      role="button"
      tabIndex={0}
      aria-label="LLM 配置解析梯"
      className="cursor-pointer outline-none"
      onClick={onSelect}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onSelect();
        }
      }}
    >
      <rect x={40} y={y} width={300} height={136} className={cn("fill-card stroke-border", selected && "stroke-2")} />
      <rect x={40} y={y} width={4} height={136} className="fill-fuchsia-500" />
      <text x={52} y={y + 14} className="fill-fuchsia-600 dark:fill-fuchsia-400 font-mono text-[8px]">SOURCE · LLM PROFILE</text>
      <text x={52} y={y + 29} className="fill-foreground text-[11px]">{llm.name || "未解析"}</text>
      <text x={52} y={y + 42} className="fill-muted-foreground text-[8px]">
        {llm.profile_id ? `#${llm.profile_id} · ` : ""}{llm.format} · {llm.model || "—"}
      </text>
      <line x1={52} y1={y + 50} x2={326} y2={y + 50} className="stroke-border" strokeWidth={1} />
      {ladder.map((row, i) => (
        <g key={row.label}>
          <text x={52} y={y + 64 + i * 16} className="fill-muted-foreground font-mono text-[7px]">{row.label}</text>
          <rect
            x={268}
            y={y + 56 + i * 16}
            width={58}
            height={11}
            rx={5.5}
            className={cn("fill-transparent", stateClass(row.state))}
            strokeWidth={1}
            strokeDasharray={row.state === "RUNTIME?" || row.state === "WITHHELD" ? "2 2" : undefined}
          />
          <text x={297} y={y + 64 + i * 16} textAnchor="middle" className={cn("font-mono text-[6px]", stateText(row.state))}>
            {row.state}
          </text>
        </g>
      ))}
      <text x={52} y={y + 114} className="fill-muted-foreground text-[7px]">
        PROMPT_APPLY_INHERITED {llm.prompt_apply_inherited ? "on" : "off"} · 仅继承路径需要
      </text>
      <text x={52} y={y + 128} className="fill-muted-foreground font-mono text-[7px]">{trimText(proof, 34)}</text>
      {source === "preview_override" && (
        <text x={326} y={y + 14} textAnchor="end" className="fill-sky-600 dark:fill-sky-400 font-mono text-[7px]">PREVIEW</text>
      )}
      <circle cx={326} cy={y + 26} r={2} className="fill-fuchsia-500" />
    </g>
  );
}

function SkillsRail({ chain, y, h, selected, onSelect }: { chain: AgentPromptChainData; y: number; h: number; selected: boolean; onSelect: () => void }) {
  const shown = chain.skills.slice(0, 6);
  const hidden = chain.skills.length - shown.length;
  const inIndex = chain.skills.filter((s) => s.in_index).length;
  return (
    <g
      role="button"
      tabIndex={0}
      aria-label="Skill 库"
      className="cursor-pointer outline-none"
      onClick={onSelect}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onSelect();
        }
      }}
    >
      <rect x={40} y={y} width={300} height={h} className={cn("fill-card stroke-border", selected && "stroke-2")} />
      <rect x={40} y={y} width={4} height={h} className="fill-emerald-500" />
      <text x={52} y={y + 15} className="fill-emerald-600 dark:fill-emerald-400 font-mono text-[8px]">
        SKILLS · {chain.skills.length} · 索引 {inIndex}
      </text>
      {shown.length === 0 && (
        <text x={52} y={y + 31} className="fill-muted-foreground text-[8px]">（无 Skill）</text>
      )}
      {shown.map((s, i) => (
        <g key={s.name}>
          <text x={52} y={y + 31 + i * 14} className={cn("text-[8px]", s.in_index ? "fill-foreground" : "fill-muted-foreground")}>
            {s.in_index ? "●" : "○"} {trimText(s.name, 20)}
          </text>
          {s.mcp_gated.length > 0 && (
            <text x={326} y={y + 31 + i * 14} textAnchor="end" className="fill-amber-600 dark:fill-amber-400 text-[7px]">
              ⛓ {trimText(s.mcp_gated.join(","), 10)}
            </text>
          )}
        </g>
      ))}
      {hidden > 0 && (
        <text x={326} y={y + h - 6} textAnchor="end" className="fill-muted-foreground text-[7px]">+{hidden}</text>
      )}
    </g>
  );
}

function ToolsRail({ chain, y, selected, onSelect }: { chain: AgentPromptChainData; y: number; selected: boolean; onSelect: () => void }) {
  return (
    <g
      role="button"
      tabIndex={0}
      aria-label="工具 schema"
      className="cursor-pointer outline-none"
      onClick={onSelect}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onSelect();
        }
      }}
    >
      <rect x={810} y={y} width={350} height={86} className={cn("fill-card stroke-border", selected && "stroke-2")} />
      <rect x={810} y={y} width={4} height={86} className="fill-sky-500" />
      <text x={822} y={y + 15} className="fill-sky-600 dark:fill-sky-400 font-mono text-[8px]">TOOLS · {chain.tools.total}</text>
      <text x={822} y={y + 31} className="fill-muted-foreground text-[8px]">
        builtin {chain.tools.builtin.length} · custom {chain.tools.custom.length} · traffic {chain.tools.traffic.length} · shell {chain.tools.shell.length}
      </text>
      <text x={822} y={y + 45} className="fill-muted-foreground text-[8px]">deferred {chain.tools.deferred_names.length} → 2nd system</text>
      <text x={822} y={y + 59} className="fill-destructive text-[8px]">excluded {chain.tools.excluded.length} → quarantine</text>
    </g>
  );
}

function MCPRail({
  chain,
  y,
  selected,
  onSelect,
}: {
  chain: AgentPromptChainData;
  y: number;
  selected: Selection | null;
  onSelect: (s: Selection) => void;
}) {
  const okCount = chain.mcp.filter((m) => m.enabled && m.health === "ok").length;
  const failedCount = chain.mcp.filter((m) => m.enabled && m.health === "failed").length;
  return (
    <g>
      <text x={810} y={y - 8} className="fill-sky-600 dark:fill-sky-400 font-mono text-[8px]">
        MCP RACK · {chain.mcp.length} · ok {okCount} · failed {failedCount}
      </text>
      {chain.mcp.length === 0 && (
        <text x={810} y={y + 12} className="fill-muted-foreground text-[8px]">（无 MCP 服务器）</text>
      )}
      {chain.mcp.map((m, i) => {
        const cy = y + 18 + i * 36;
        const isSel = selected?.kind === "mcp" && selected.id === m.id;
        const filled = m.enabled && (m.health === "ok" || m.health === "failed");
        const dotClass = !m.enabled
          ? "fill-transparent stroke-muted-foreground/60"
          : m.health === "ok"
            ? "fill-emerald-500 stroke-emerald-500"
            : m.health === "failed"
              ? "fill-destructive stroke-destructive"
              : "fill-transparent stroke-muted-foreground";
        return (
          <g
            key={m.id}
            role="button"
            tabIndex={0}
            aria-label={`MCP ${m.name}，${healthLabel(m.health)}`}
            className="cursor-pointer outline-none"
            onClick={() => onSelect({ kind: "mcp", id: m.id })}
            onKeyDown={(e) => {
              if (e.key === "Enter" || e.key === " ") {
                e.preventDefault();
                onSelect({ kind: "mcp", id: m.id });
              }
            }}
          >
            <rect x={810} y={cy} width={350} height={32} className={cn("fill-card stroke-border", isSel && "stroke-2")} />
            {m.cache_stale && <rect x={1157} y={cy} width={3} height={32} className="fill-amber-500" opacity={0.7} />}
            <circle cx={822} cy={cy + 16} r={3} className={dotClass} strokeWidth={1} fillOpacity={filled ? 1 : 0} />
            <text x={834} y={cy + 13} className="fill-foreground text-[10px]">{trimText(m.name, 22)}</text>
            <text x={834} y={cy + 25} className="fill-muted-foreground text-[8px]">
              {trimText(`${m.transport} · ${m.mode}${m.skill ? ` · ${m.skill}` : ""} · ${m.tool_count} tools`, 44)}
            </text>
            <text x={1150} y={cy + 13} textAnchor="end" className="fill-muted-foreground font-mono text-[7px]">
              {m.last_check_at ? trimText(m.last_check_at, 10) : "未检查"}
            </text>
            {m.cache_stale && (
              <text x={1150} y={cy + 25} textAnchor="end" className="fill-amber-600 dark:fill-amber-400 text-[7px]">cache 过期</text>
            )}
          </g>
        );
      })}
    </g>
  );
}

function Quarantine({
  chain,
  y,
  h,
  selected,
  onSelect,
}: {
  chain: AgentPromptChainData;
  y: number;
  h: number;
  selected: boolean;
  onSelect: () => void;
}) {
  const excluded = chain.tools.excluded;
  const outOfIndex = chain.skills.filter((s) => !s.in_index);
  return (
    <g
      role="button"
      tabIndex={0}
      aria-label="未进入主干的项"
      className="cursor-pointer outline-none"
      onClick={onSelect}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onSelect();
        }
      }}
    >
      <rect x={40} y={y} width={700} height={h} className={cn("fill-card stroke-destructive", selected && "stroke-2")} strokeDasharray="5 4" />
      <text x={52} y={y + 16} className="fill-destructive font-mono text-[8px]">QUARANTINE · 未进入主干</text>
      {excluded.slice(0, 3).map((t, i) => (
        <text key={t.key} x={52} y={y + 32 + i * 13} className="fill-destructive text-[8px]">
          ✕ {t.key} — {trimText(t.reason, 46)}
        </text>
      ))}
      {excluded.length > 3 && (
        <text x={52} y={y + 32 + 3 * 13} className="fill-muted-foreground text-[8px]">
          +{excluded.length - 3} 项（打开详情查看）
        </text>
      )}
      {outOfIndex.length > 0 && (
        <text x={52} y={y + h - 12} className="fill-muted-foreground text-[8px]">
          索引外 Skill：{outOfIndex.map((s) => s.name).join("、")}（仍可 Skill(name=...) 加载）
        </text>
      )}
    </g>
  );
}

function DetailPanel({
  chain,
  selection,
  onSelect,
  onClose,
  onCopy,
}: {
  chain: AgentPromptChainData;
  selection: Selection;
  onSelect: (s: Selection) => void;
  onClose: () => void;
  onCopy: () => void;
}) {
  const codeSegs = chain.segments.filter((s) => s.kind === "code_tail");
  const runtimeSegs = chain.segments.filter((s) => !s.available);
  const profile = chain.segments.find((s) => s.kind === "profile_prompt");
  const selectedMCP = selection.kind === "mcp" ? chain.mcp.find((m) => m.id === selection.id) : undefined;

  const title =
    selection.kind === "segment" ? chain.segments.find((s) => s.key === selection.key)?.title :
    selection.kind === "llm" ? "LLM 配置" :
    selection.kind === "skills" ? "Skill 索引" :
    selection.kind === "tools" ? "工具 schema" :
    selection.kind === "mcp" ? selectedMCP?.name ?? "MCP" :
    selection.kind === "code" ? "代码硬尾巴" :
    selection.kind === "runtime" ? "运行时总线" :
    selection.kind === "model" ? "Provider 请求" : "未进入主干";

  return (
    <aside className="min-w-0 rounded-lg border bg-card/40 p-3">
      <div className="mb-2 flex items-center gap-2 border-b pb-2">
        <span className="font-mono text-[10px] text-muted-foreground uppercase">Datasheet</span>
        <span className="truncate font-medium text-sm">{title}</span>
        <Button size="icon" variant="ghost" className="ml-auto size-6" onClick={onClose} aria-label="关闭详情">
          <XIcon className="size-3.5" />
        </Button>
      </div>
      <div className="max-h-[60vh] overflow-auto">
        {selection.kind === "segment" && (
          <SegmentDetail seg={chain.segments.find((s) => s.key === selection.key)} />
        )}
        {selection.kind === "code" && (
          <div className="grid gap-1">
            <p className="text-muted-foreground text-xs">代码常量拼接，UI 不可修改；独立提示词不能替换它们。</p>
            {codeSegs.map((s) => (
              <button key={s.key} type="button" className="flex items-center gap-2 rounded-md border px-2 py-1.5 text-left text-xs hover:bg-accent" onClick={() => onSelect({ kind: "segment", key: s.key })}>
                <span className="size-1.5 rounded-full bg-amber-500" />
                <span className="truncate">{s.title}</span>
                <span className="ml-auto font-mono text-muted-foreground text-[10px]">{s.present ? `${s.chars}c` : "未生效"}</span>
              </button>
            ))}
          </div>
        )}
        {selection.kind === "runtime" && (
          <div className="grid gap-1">
            <p className="text-muted-foreground text-xs">运行时按任务当时事实注入；静态视图只显示位置与门控。</p>
            {runtimeSegs.map((s) => (
              <button key={s.key} type="button" className="flex items-center gap-2 rounded-md border px-2 py-1.5 text-left text-xs hover:bg-accent" onClick={() => onSelect({ kind: "segment", key: s.key })}>
                <span className="size-1.5 rounded-full bg-muted-foreground" />
                <span className="truncate">{s.title}</span>
                <span className="ml-auto max-w-[45%] truncate font-mono text-muted-foreground text-[10px]">{s.gate || "按态势"}</span>
              </button>
            ))}
          </div>
        )}
        {selection.kind === "llm" && (
          <div className="grid gap-2">
            <div className="grid grid-cols-2 gap-1.5">
              <Meta k="配置" v={`${chain.llm.name || "未解析"}${chain.llm.profile_id ? ` · #${chain.llm.profile_id}` : ""}${chain.llm.format ? ` · ${chain.llm.format}` : ""}`} />
              <Meta k="模型" v={chain.llm.model} />
              <Meta k="来源" v={chain.llm.source_label} />
              <Meta k="模式" v={`${modeLabel(chain.llm.prompt_mode)} · ${chain.llm.prompt_apply_inherited ? "继承生效" : "仅显式绑定"}`} />
            </div>
            <p className="text-muted-foreground text-xs">独立提示词随该配置装载；显式绑定始终生效，继承路径需勾选“继承时也生效”。</p>
            {profile?.text && <pre className="max-h-64 overflow-auto whitespace-pre-wrap break-words rounded-md bg-muted/40 p-2 font-mono text-[11px]">{profile.text}</pre>}
          </div>
        )}
        {selection.kind === "skills" && (
          <div className="grid gap-1">
            {chain.skills.map((s) => (
              <div key={s.name} className="rounded-md border px-2 py-1.5">
                <div className="flex flex-wrap items-center gap-1.5">
                  <span className="font-mono text-xs">{s.name}</span>
                  <Badge variant={s.in_index ? "secondary" : "outline"} className="px-1.5 py-0 text-[10px]">
                    {s.in_index ? "已进速查块" : "未进速查块"}
                  </Badge>
                  {s.mcp_gated.map((m) => (
                    <Badge key={m} variant="outline" className="px-1.5 py-0 text-[10px] text-muted-foreground">解锁 MCP {m}</Badge>
                  ))}
                </div>
                {s.description && <p className="mt-0.5 line-clamp-2 text-muted-foreground text-[11px]">{s.description}</p>}
              </div>
            ))}
          </div>
        )}
        {selection.kind === "tools" && (
          <div className="grid gap-2">
            <p className="text-muted-foreground text-xs">工具 schema 不进入 system 文本，随请求单独下发；延迟工具走第二 system 段。</p>
            <ToolGroup name="内置" items={chain.tools.builtin} />
            <ToolGroup name="自定义" items={chain.tools.custom} />
            <ToolGroup name="流量" items={chain.tools.traffic} />
            <ToolGroup name="Shell" items={chain.tools.shell} />
            {chain.tools.deferred_names.length > 0 && (
              <div>
                <p className="mb-1 font-medium text-xs">延迟 schema（{chain.tools.deferred_names.length}）</p>
                <p className="font-mono text-muted-foreground text-[11px]">{chain.tools.deferred_names.join("、")}</p>
              </div>
            )}
          </div>
        )}
        {selection.kind === "mcp" && selectedMCP && (
          <div className="grid gap-2">
            <div className="grid grid-cols-2 gap-1.5">
              <Meta k="transport" v={selectedMCP.transport} />
              <Meta k="health" v={healthLabel(selectedMCP.health)} />
              <Meta k="tools" v={`${selectedMCP.tool_count} 个`} />
              <Meta k="cache" v={selectedMCP.cache_stale ? "可能过期" : "新鲜"} />
            </div>
            <p className="text-muted-foreground text-xs">
              模式 {selectedMCP.mode}{selectedMCP.skill ? ` · 随 Skill「${selectedMCP.skill}」解锁` : ""}；只读缓存，不连接。
              {selectedMCP.last_error ? ` 上次错误：${selectedMCP.last_error}` : ""}
            </p>
            <ul className="grid gap-0.5">
              {selectedMCP.tools.map((t) => (
                <li key={t.name} className="font-mono text-[11px]">
                  {t.name}
                  {t.description && <span className="ml-1 font-sans text-muted-foreground">— {t.description}</span>}
                </li>
              ))}
            </ul>
          </div>
        )}
        {selection.kind === "model" && (
          <div className="grid gap-2">
            <div className="grid grid-cols-2 gap-1.5">
              <Meta k="system" v={`${chain.totals.chars} 字符`} />
              <Meta k="tokens" v={`约 ${chain.totals.est_tokens}`} />
              <Meta k="tools" v={`${chain.tools.total} 个`} />
              <Meta k="2nd system" v="条件注入" />
            </div>
            {chain.warnings.length > 0 && (
              <ul className="list-disc pl-5 text-muted-foreground text-xs">
                {chain.warnings.map((w, i) => <li key={i}>{w}</li>)}
              </ul>
            )}
            <Button size="sm" variant="outline" onClick={onCopy}>
              <CopyIcon className="size-3.5" /> 复制静态提示词
            </Button>
          </div>
        )}
        {selection.kind === "excluded" && (
          <div className="grid gap-1">
            <p className="text-muted-foreground text-xs">排除项常驻展示，不藏在 tooltip 里。</p>
            {chain.tools.excluded.map((t) => (
              <div key={t.key} className="rounded-md border px-2 py-1.5 text-xs">
                <span className="font-mono">{t.key}</span>
                <span className="ml-1 text-muted-foreground">— {t.reason}</span>
              </div>
            ))}
            {chain.skills.some((s) => !s.in_index) && (
              <p className="text-muted-foreground text-xs">
                索引外 Skill：{chain.skills.filter((s) => !s.in_index).map((s) => s.name).join("、")}
              </p>
            )}
          </div>
        )}
      </div>
    </aside>
  );
}

function SegmentDetail({ seg }: { seg?: PromptChainSegment }) {
  if (!seg) return <p className="text-muted-foreground text-xs">（段不存在）</p>;
  const text = seg.text.replace(/^[\r\n]+/, "");
  const discipline = (() => {
    switch (seg.kind) {
      case "profile_prompt":
        return {
          discipline: "可改写正文与注入位置；replace 会移除正文（技能索引与硬尾保留）。",
          opsec: "配置层不能越过 TERMINAL 硬边界。",
        };
      case "body":
        return {
          discipline: "Agent DB 模板可改；若被独立提示词 replace，会标记为 SUPERSEDED。",
          opsec: "纯文本，不渲染该 Agent 之外的运行时事实。",
        };
      case "code_tail":
        return {
          discipline: "代码常量，UI 不可改。",
          opsec: "配置层不可越过；保证平台硬约束。",
        };
      case "skill_index":
        return {
          discipline: "由宿主技能目录生成；是否进入索引取决于 Skill 配置。",
          opsec: "只列名称与描述，不含凭据。",
        };
      case "host":
        return {
          discipline: "宿主注入段，随部署环境变化。",
          opsec: "运行时事实，配置不覆盖。",
        };
      default:
        return seg.available
          ? { discipline: "只读投影。", opsec: "不连接外部服务。" }
          : { discipline: "需要任务上下文，静态视图不展开。", opsec: "只显示注入位置与门控，不显示正文。" };
    }
  })();
  const badgeColorKey = seg.kind === "skill_index" ? "skill_index" : seg.source;
  return (
    <div className="grid gap-2">
      <div className="flex flex-wrap gap-1">
        <Badge variant="outline" className={cn("px-1.5 py-0 text-[10px]", SOURCE_HTML_TEXT[badgeColorKey])}>
          {seg.source_label || seg.source}
        </Badge>
        {seg.editable ? (
          <Badge variant="secondary" className="px-1.5 py-0 text-[10px]">可编辑</Badge>
        ) : (
          <Badge variant="outline" className="px-1.5 py-0 text-[10px]">只读</Badge>
        )}
        {!seg.available && <Badge variant="secondary" className="px-1.5 py-0 text-[10px]">运行时</Badge>}
        {!seg.present && <Badge variant="outline" className="px-1.5 py-0 text-[10px]">未生效</Badge>}
      </div>
      <div className="grid grid-cols-2 gap-1.5">
        <Meta k="来源" v={seg.source_label || seg.source} />
        <Meta k="字符" v={`${seg.chars}`} />
        <Meta k="order" v={`${seg.order}`} />
        <Meta k="状态" v={seg.present ? "参与装配" : "未生效"} />
      </div>
      {seg.gate && <p className="text-muted-foreground text-xs">门控：{seg.gate}</p>}
      {!seg.present && seg.reason && <p className="text-muted-foreground text-xs">{seg.reason}</p>}
      <div className="rounded-md border bg-muted/20 p-2">
        <p className="text-[10px] text-muted-foreground">
          <span className="font-medium text-foreground">Discipline</span> · {discipline.discipline}
        </p>
        <p className="mt-0.5 text-[10px] text-muted-foreground">
          <span className="font-medium text-foreground">OPSEC</span> · {discipline.opsec}
        </p>
      </div>
      {seg.present && seg.available && text !== "" && (
        <pre className="max-h-64 overflow-auto whitespace-pre-wrap break-words rounded-md bg-muted/40 p-2 font-mono text-[11px]">
          {text}
        </pre>
      )}
    </div>
  );
}

function Meta({ k, v }: { k: string; v: string }) {
  return (
    <div className="rounded-md border bg-muted/20 px-2 py-1">
      <div className="text-[9px] text-muted-foreground uppercase">{k}</div>
      <div className="break-all font-mono text-[10px]">{v}</div>
    </div>
  );
}

function ToolGroup({ name, items }: { name: string; items: PromptChainTool[] }) {
  if (items.length === 0) return null;
  return (
    <div>
      <p className="mb-1 font-medium text-xs">{name}（{items.length}）</p>
      <ul className="grid gap-1">
        {items.map((t) => <ToolLine key={t.key} item={t} />)}
      </ul>
    </div>
  );
}