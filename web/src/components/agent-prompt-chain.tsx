"use client";

import * as React from "react";

import {
  AlertTriangleIcon,
  ArrowLeftIcon,
  CopyIcon,
  HomeIcon,
  RefreshCwIcon,
  XIcon,
} from "lucide-react";
import { toast } from "sonner";

import Link from "next/link";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { api } from "@/lib/api";
import {
  estimateTextWidth,
  fitText,
  layoutSignalFlow,
  type ChainMode,
  type Rect,
} from "@/lib/chain-layout";
import type {
  AgentPromptChain as AgentPromptChainData,
  LLMProfile,
  PromptChainMCP,
  PromptChainSegment,
  PromptChainTool,
} from "@/lib/types";
import { cn } from "@/lib/utils";

// Read-only assembly chain. The spatial model is the demo's final form:
// three columns (LLM/Skills, trunk rows, Tools/MCP), a terminal boundary,
// gates collected into the gaps, and every gate drawn AFTER every edge.
// Navigation is a path: clicking a node opens a detail overlay; Esc/back/root
// return one level, and the path bar stays visible while the graph scrolls.

type ColorKey =
  | "profile"
  | "body"
  | "skill"
  | "code"
  | "host"
  | "runtime"
  | "tool"
  | "model"
  | "danger"
  | "neutral";

const COLOR: Record<ColorKey, { stroke: string; fill: string; text: string; html: string }> = {
  profile: {
    stroke: "stroke-fuchsia-500",
    fill: "fill-fuchsia-500",
    text: "fill-fuchsia-600 dark:fill-fuchsia-400",
    html: "border-fuchsia-500/40 text-fuchsia-600 dark:text-fuchsia-400",
  },
  body: {
    stroke: "stroke-emerald-500",
    fill: "fill-emerald-500",
    text: "fill-emerald-600 dark:fill-emerald-400",
    html: "border-emerald-500/40 text-emerald-600 dark:text-emerald-400",
  },
  skill: {
    stroke: "stroke-emerald-500",
    fill: "fill-emerald-500",
    text: "fill-emerald-600 dark:fill-emerald-400",
    html: "border-emerald-500/40 text-emerald-600 dark:text-emerald-400",
  },
  code: {
    stroke: "stroke-amber-500",
    fill: "fill-amber-500",
    text: "fill-amber-600 dark:fill-amber-400",
    html: "border-amber-500/40 text-amber-600 dark:text-amber-400",
  },
  host: {
    stroke: "stroke-violet-500",
    fill: "fill-violet-500",
    text: "fill-violet-600 dark:fill-violet-400",
    html: "border-violet-500/40 text-violet-600 dark:text-violet-400",
  },
  runtime: {
    stroke: "stroke-muted-foreground",
    fill: "fill-muted-foreground",
    text: "fill-muted-foreground",
    html: "border-muted-foreground/40 text-muted-foreground",
  },
  tool: {
    stroke: "stroke-sky-500",
    fill: "fill-sky-500",
    text: "fill-sky-600 dark:fill-sky-400",
    html: "border-sky-500/40 text-sky-600 dark:text-sky-400",
  },
  model: {
    stroke: "stroke-sky-500",
    fill: "fill-sky-500",
    text: "fill-sky-600 dark:fill-sky-400",
    html: "border-sky-500/40 text-sky-600 dark:text-sky-400",
  },
  danger: {
    stroke: "stroke-destructive",
    fill: "fill-destructive",
    text: "fill-destructive",
    html: "border-destructive/40 text-destructive",
  },
  neutral: {
    stroke: "stroke-border",
    fill: "fill-muted-foreground",
    text: "fill-muted-foreground",
    html: "border-border text-muted-foreground",
  },
};

function modeLabel(mode: string) {
  switch (mode) {
    case "prepend":
      return "置于正文之前";
    case "append":
      return "置于正文之后";
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

function fmt(n: number) {
  return n.toLocaleString("en-US");
}

type Target =
  | { kind: "segment"; key: string }
  | { kind: "code" }
  | { kind: "runtime" }
  | { kind: "deferred" }
  | { kind: "llm" }
  | { kind: "skills" }
  | { kind: "tools" }
  | { kind: "mcp"; id: number }
  | { kind: "model" }
  | { kind: "excluded" };

type PathEntry = {
  id: string;
  title: string;
  anchor: string;
  target: Target;
};

type Row = {
  id: string;
  tag: string;
  title: string;
  sub: string;
  chars: number | null;
  color: ColorKey;
  present: boolean;
  available: boolean;
  gated: boolean;
  unknown: boolean;
  struck?: boolean;
  sockets?: number;
  sink?: boolean;
  h: number;
  gapBefore?: number;
  x: number;
  w: number;
  y: number;
  target: Target;
};

type Tray = { x: number; y: number; w: number; h: number };

// 文本按真实字体栈测量，保证裁切点与渲染一致。
const FONT_STACK_SANS =
  '-apple-system, "Segoe UI", "PingFang SC", "Microsoft YaHei", "Noto Sans SC", sans-serif';
const FONT_STACK_MONO =
  '"SFMono-Regular", "Cascadia Mono", "Segoe UI Mono", Consolas, "Courier New", monospace';
let measureCtx: CanvasRenderingContext2D | null = null;
function measureText(text: string, style: { px: number; mono?: boolean }): number {
  if (typeof document === "undefined") return estimateTextWidth(text, style);
  if (!measureCtx) measureCtx = document.createElement("canvas").getContext("2d");
  if (!measureCtx) return estimateTextWidth(text, style);
  measureCtx.font = `${style.px}px ${style.mono ? FONT_STACK_MONO : FONT_STACK_SANS}`;
  return measureCtx.measureText(text).width;
}

type Layout = {
  mode: ChainMode;
  w: number;
  rows: Row[];
  byId: Map<string, Row>;
  llm: Rect;
  skills: Rect;
  tools: Rect;
  mcp: Rect;
  tray: Tray | null;
  quarantine: Rect | null;
  trunkX: number;
  center: { x: number; w: number };
  height: number;
  boundary: { y: number; x1: number; x2: number } | null;
  measureMax: number;
  rowTitleMaxW: number;
  profileRow?: Row;
  runtimeRow?: Row;
  deferredRow?: Row;
  modelRow: Row;
  codeRow?: Row;
};

function targetTitle(chain: AgentPromptChainData, target: Target) {
  switch (target.kind) {
    case "segment":
      return chain.segments.find((s) => s.key === target.key)?.title ?? target.key;
    case "code":
      return "代码硬尾巴";
    case "runtime":
      return "运行时注入点";
    case "deferred":
      return "延迟工具块";
    case "llm":
      return "LLM 配置解析";
    case "skills":
      return "Skill 绑定";
    case "tools":
      return "工具 schema";
    case "mcp":
      return chain.mcp.find((m) => m.id === target.id)?.name ?? `MCP ${target.id}`;
    case "model":
      return "模型请求";
    case "excluded":
      return "未进入主干";
  }
}

function targetId(target: Target): string {
  switch (target.kind) {
    case "segment":
      return `segment:${target.key}`;
    case "mcp":
      return `mcp:${target.id}`;
    default:
      return target.kind;
  }
}

function anchorFor(chain: AgentPromptChainData, target: Target): string {
  switch (target.kind) {
    case "segment": {
      const seg = chain.segments.find((s) => s.key === target.key);
      switch (seg?.kind) {
        case "profile_prompt":
          return "profile";
        case "body":
          return "body";
        case "skill_index":
          return "skill";
        case "host":
          return "host";
        default:
          return seg?.key ?? target.key;
      }
    }
    case "mcp":
      return `mcp:${target.id}`;
    case "code":
      return "code";
    case "runtime":
      return "runtime";
    case "deferred":
      return "deferred";
    case "model":
      return "model";
    case "llm":
      return "llm";
    case "skills":
      return "skills";
    case "tools":
      return "tools";
    case "excluded":
      return "excluded";
  }
}

function buildLayout(chain: AgentPromptChainData, width: number): Layout {
  const find = (kind: string) => chain.segments.find((s) => s.kind === kind);
  const profile = find("profile_prompt");
  const body = find("body");
  const skill = find("skill_index");
  const host = find("host");
  const code = chain.segments.filter((s) => s.kind === "code_tail");
  const codePresent = code.filter((s) => s.present);
  const codeGated = code.filter((s) => !s.present);
  const deferredSeg = find("deferred_tools");
  const deferredNames = chain.tools.deferred_names;
  const deferredCount = Math.max(deferredNames.length, deferredSeg ? 1 : 0);
  const runtimeSegs = chain.segments.filter((s) => !s.available && s.kind !== "deferred_tools");
  const runtimeOwn = runtimeSegs.filter((s) => s.kind === "runtime").length;

  const mode = chain.llm.prompt_mode;
  const showProfile = !!profile && !!mode;
  const profileActive = !!profile?.present && !!profile.available && chain.llm.prompt_active;
  const replaceActive = mode === "replace" && profileActive;

  const rows: Row[] = [];
  const push = (row: Omit<Row, "y" | "x" | "w">) => rows.push({ ...row, x: 0, w: 0, y: 0 });

  push({
    id: "agent",
    tag: "Agent",
    title: `${chain.agent_key} · ${chain.agent_kind}`,
    sub: chain.agent_kind === "builtin" ? "内置 Agent" : "自定义 Agent",
    chars: null,
    color: "neutral",
    present: true,
    available: true,
    gated: false,
    unknown: false,
    h: 36,
    target: { kind: "model" },
  });

  if (showProfile && mode !== "append") {
    push({
      id: "profile",
      tag: "独立",
      title: "独立提示词",
      sub: `${modeLabel(mode)} · ${profileActive ? "用户自写" : "未生效"}`,
      chars: profileActive ? profile?.chars ?? 0 : 0,
      color: "profile",
      present: profileActive,
      available: true,
      gated: !profileActive,
      unknown: false,
      h: 36,
      gapBefore: 36,
      target: { kind: "segment", key: profile?.key ?? "profile_prompt" },
    });
  }

  if (body && !replaceActive) {
    push({
      id: "body",
      tag: "正文",
      title: "提示词正文",
      sub: body.present ? `${body.source_label || "提示词正文"} · 在配置中编辑` : "被独立提示词替换",
      chars: body.present ? body.chars : 0,
      color: "body",
      present: body.present,
      available: true,
      gated: !body.present,
      unknown: false,
      struck: !body.present,
      h: 36,
      target: { kind: "segment", key: body.key },
    });
  }

  if (skill) {
    push({
      id: "skill",
      tag: "技能",
      title: "技能速查",
      sub: "宿主注入 · Skill 绑定",
      chars: skill.present ? skill.chars : 0,
      color: "skill",
      present: skill.present,
      available: true,
      gated: !skill.present,
      unknown: false,
      h: 36,
      target: { kind: "segment", key: skill.key },
    });
  }

  if (showProfile && mode === "append") {
    push({
      id: "profile",
      tag: "独立",
      title: "独立提示词",
      sub: `${modeLabel(mode)} · ${profileActive ? "用户自写" : "未生效"}`,
      chars: profileActive ? profile?.chars ?? 0 : 0,
      color: "profile",
      present: profileActive,
      available: true,
      gated: !profileActive,
      unknown: false,
      h: 36,
      gapBefore: 36,
      target: { kind: "segment", key: profile?.key ?? "profile_prompt" },
    });
  }

  if (code.length > 0) {
    const codeChars = codePresent.reduce((n, s) => n + s.chars, 0);
    push({
      id: "code",
      tag: "硬尾",
      title: `代码硬尾巴 ×${codePresent.length}`,
      sub: "代码固定 · 只读",
      chars: codeChars,
      color: "code",
      present: codePresent.length > 0,
      available: true,
      gated: false,
      unknown: false,
      h: 36,
      gapBefore: 40,
      target: { kind: "code" },
    });
  }

  if (codeGated.length > 0) {
    push({
      id: "gated",
      tag: "门控",
      title: `门控未生效 ×${codeGated.length}`,
      sub: "条件未满足 · 不进入请求",
      chars: 0,
      color: "runtime",
      present: false,
      available: true,
      gated: true,
      unknown: false,
      h: 36,
      target: { kind: "code" },
    });
  }

  if (host) {
    push({
      id: "host",
      tag: "本机",
      title: "本机可用工具",
      sub: "宿主注入",
      chars: host.present ? host.chars : 0,
      color: "host",
      present: host.present,
      available: true,
      gated: !host.present,
      unknown: false,
      h: 36,
      target: { kind: "segment", key: host.key },
    });
  }

  if (runtimeSegs.length > 0) {
    push({
      id: "runtime",
      tag: "未采集",
      title: `未采集 ×${runtimeSegs.length}`,
      sub: `运行时 ${runtimeOwn} · 派生 ${runtimeSegs.length - runtimeOwn}`,
      chars: 0,
      color: "runtime",
      present: false,
      available: false,
      gated: false,
      unknown: true,
      sockets: Math.min(runtimeSegs.length, 12),
      h: 44,
      gapBefore: 36,
      target: { kind: "runtime" },
    });
  }

  if (deferredCount > 0) {
    push({
      id: "deferred",
      tag: "延迟",
      title: `延迟工具块 ×${deferredCount}`,
      sub: "第二 system（按需）",
      chars: 0,
      color: "runtime",
      present: false,
      available: false,
      gated: false,
      unknown: true,
      sockets: 1,
      h: 36,
      target: { kind: "deferred" },
    });
  }

  push({
    id: "model",
    tag: "请求",
    title: "模型请求",
    sub: "system + 工具 schema + 第二 system（按需）",
    chars: chain.totals.chars,
    color: "model",
    present: true,
    available: true,
    gated: false,
    unknown: false,
    sink: true,
    h: 48,
    gapBefore: 36,
    target: { kind: "model" },
  });

  const excluded = chain.tools.excluded.length;
  const outOfIndex = chain.skills.filter((s) => !s.in_index).length;
  const quarantine = excluded > 0 || outOfIndex > 0;
  const quarantineH = quarantine
    ? 30 + Math.min(excluded, 4) * 13 + (outOfIndex > 0 ? 18 : 0) + 10
    : 0;

  // 几何全部由容器宽度推导（chain-layout 纯模块）；这里只负责把语义行映射回去。
  const geom = layoutSignalFlow({
    width,
    rows: rows.map((row) => ({
      id: row.id,
      h: row.h,
      gapBefore: row.gapBefore,
      title: row.title,
      sub: row.sub,
      tag: row.tag,
      chars: row.chars,
    })),
    skills: chain.skills.length,
    mcpCount: chain.mcp.length,
    hasTray: replaceActive && !!body,
    quarantineH,
  });
  rows.forEach((row) => {
    const g = geom.byId.get(row.id);
    if (!g) return;
    row.x = g.x;
    row.w = g.w;
    row.y = g.y;
  });

  const byId = new Map(rows.map((row) => [row.id, row]));
  const modelRow = byId.get("model") as Row;
  const codeRow = byId.get("code");
  const measureMax = Math.max(
    1,
    ...rows.filter((row) => row.id !== "model" && row.present && (row.chars ?? 0) > 0).map((row) => row.chars ?? 0),
  );

  return {
    mode: geom.mode,
    w: geom.w,
    rows,
    byId,
    llm: geom.llm,
    skills: geom.skills,
    tools: geom.tools,
    mcp: geom.mcp,
    tray: geom.tray,
    quarantine: geom.quarantine,
    trunkX: geom.trunkX,
    center: geom.center,
    height: geom.h,
    boundary: geom.boundary,
    measureMax,
    rowTitleMaxW: geom.rowTitleMaxW,
    profileRow: byId.get("profile"),
    runtimeRow: byId.get("runtime"),
    deferredRow: byId.get("deferred"),
    modelRow,
    codeRow,
  };
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
  onOpen,
}: {
  chain: AgentPromptChainData;
  lampTest: boolean;
  onLampTest: () => void;
  onOpen: (target: Target) => void;
}) {
  const llm = chain.llm;
  const excluded = chain.tools.excluded.length;
  const outOfIndex = chain.skills.filter((s) => !s.in_index).length;
  const activeMCP = chain.mcp.filter((m) => m.enabled && m.mode !== "excluded");
  const mcpOk = activeMCP.filter((m) => m.health === "ok").length;
  const mcpFailed = activeMCP.filter((m) => m.health === "failed").length;
  const mcpNever = activeMCP.filter((m) => m.health === "never_checked").length;
  const mcpStale = activeMCP.filter((m) => m.cache_stale).length;
  const runtimeCount = chain.segments.filter((s) => !s.available).length;
  const overlayValue = llm.prompt_active
    ? `${modeLabel(llm.prompt_mode)} · ${llm.prompt_chars}c · 生效`
    : llm.prompt_mode
      ? `未生效 · 继承${llm.prompt_apply_inherited ? "开" : "关"}`
      : "未配置";
  const profileValue = llm.name
    ? `${llm.name}${llm.profile_id ? ` #${llm.profile_id}` : ""} · ${llm.source_label}`
    : "未解析";
  const firstFailedMCP = activeMCP.find((m) => m.health === "failed") ?? chain.mcp[0];
  const mcpTone: LampTone = mcpFailed > 0 ? "danger" : mcpStale > 0 ? "stale" : mcpNever > 0 ? "hollow" : "quiet";

  return (
    <div className="flex flex-wrap items-stretch gap-y-2 border-y bg-muted/15 py-2">
      <div className="flex min-w-0 flex-1 flex-wrap gap-y-2">
        <SituationUnit label="AGENT" value={`${chain.agent_key} · ${chain.agent_kind}`} onClick={() => onOpen({ kind: "model" })} />
        <SituationUnit
          label="PROFILE"
          value={profileValue}
          title={`${profileValue}（来源：${llm.source_label}）`}
          onClick={() => onOpen({ kind: "llm" })}
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
          onClick={() => onOpen({ kind: "llm" })}
        />
        <SituationUnit label="SCALE" value={`${chain.totals.segments} 段 · ${fmt(chain.totals.chars)}c · ≈${fmt(chain.totals.est_tokens)} tok`} onClick={() => onOpen({ kind: "model" })} />
        <SituationUnit
          label="INTEGRITY"
          value={`${chain.warnings.length} 告警 · ${excluded} 排除 · MCP ${mcpOk}/${activeMCP.length} 正常`}
          onClick={() => onOpen({ kind: "excluded" })}
        />
        <SituationUnit
          label="RUNTIME"
          value={`${runtimeCount} 注入点 · 未采集`}
          title="静态快照不展开运行时正文；仅显示注入位置与门控"
          onClick={() => onOpen({ kind: "runtime" })}
        />
      </div>
      <div className="flex items-center gap-1 px-2">
        <span className="mr-1 font-mono text-[7px] text-muted-foreground uppercase tracking-[0.12em]">LAMPS</span>
        <Lamp tone={llm.name ? "quiet" : "dangerOutline"} title={llm.name ? `已解析：${llm.name}` : "未解析到 LLM 配置"} lampTest={lampTest} onClick={() => onOpen({ kind: "llm" })} />
        <Lamp
          tone={llm.prompt_active ? "quiet" : llm.prompt_mode ? "dangerOutline" : "quiet"}
          title={llm.prompt_active ? "独立提示词生效" : llm.prompt_mode ? "独立提示词被扣留（未生效）" : "无独立提示词"}
          lampTest={lampTest}
          onClick={() => onOpen({ kind: "llm" })}
        />
        <Lamp tone={outOfIndex > 0 ? "hollow" : "quiet"} title={`Skill 索引外 ${outOfIndex}`} lampTest={lampTest} onClick={() => onOpen({ kind: "skills" })} />
        <Lamp tone={excluded > 0 ? "danger" : "quiet"} title={`排除 ${excluded} 项`} lampTest={lampTest} onClick={() => onOpen({ kind: "excluded" })} />
        <Lamp tone={mcpTone} title={`MCP：${mcpOk} ok / ${mcpFailed} failed / ${mcpNever} 未检查 / ${mcpStale} 缓存过期`} lampTest={lampTest} onClick={firstFailedMCP ? () => onOpen({ kind: "mcp", id: firstFailedMCP.id }) : undefined} />
        <Lamp tone="hollow" title={`运行时注入点 ${runtimeCount} 个 · 未采集`} lampTest={lampTest} onClick={() => onOpen({ kind: "runtime" })} />
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

function normalizeChain(data: AgentPromptChainData): AgentPromptChainData {
  // Go nil slices serialize as null. Normalize once so every downstream
  // .map/.length/filter sees an array, including older cached payloads.
  const tools = data.tools ?? {
    builtin: [],
    custom: [],
    traffic: [],
    shell: [],
    deferred_names: [],
    excluded: [],
    total: 0,
  };
  // Mock mode (NEXT_PUBLIC_MOCK=1) returns {} for unknown routes. Keep the tab
  // on an honest empty shell instead of crashing on chain.llm.name.
  const llm = data.llm ?? {
    profile_id: null,
    name: "",
    model: "",
    format: "",
    source: "none",
    source_label: "未解析",
    prompt_mode: "",
    prompt_chars: 0,
    prompt_apply_inherited: false,
    prompt_active: false,
  };
  return {
    ...data,
    agent_key: data.agent_key ?? "",
    agent_kind: data.agent_kind ?? "builtin",
    llm,
    totals: data.totals ?? { segments: 0, chars: 0, est_tokens: 0 },
    segments: data.segments ?? [],
    mcp: (data.mcp ?? []).map((m) => ({ ...m, tools: m.tools ?? [] })),
    skills: (data.skills ?? []).map((s) => ({ ...s, mcp_gated: s.mcp_gated ?? [] })),
    warnings: data.warnings ?? [],
    static_only: data.static_only ?? true,
    tools: {
      ...tools,
      builtin: tools.builtin ?? [],
      custom: tools.custom ?? [],
      traffic: tools.traffic ?? [],
      shell: tools.shell ?? [],
      deferred_names: tools.deferred_names ?? [],
      excluded: tools.excluded ?? [],
      total: tools.total ?? 0,
    },
  };
}

export function AgentPromptChain({
  agentKey,
  active,
  reloadKey,
  profileId,
}: {
  agentKey: string;
  active: boolean;
  reloadKey?: number;
  profileId?: string;
}) {
  const [chain, setChain] = React.useState<AgentPromptChainData | null>(null);
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState("");
  const [path, setPath] = React.useState<PathEntry[]>([]);
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
      const data = await api.agentPromptChain(agentKey, profileId);
      if (reqRef.current === seq) {
        setChain(normalizeChain(data));
        setPath([]);
      }
    } catch (e) {
      if (reqRef.current === seq) {
        setChain(null);
        setPath([]);
        setError((e as Error).message);
      }
    } finally {
      if (reqRef.current === seq) setLoading(false);
    }
  }, [agentKey, profileId]);

  React.useEffect(() => {
    if (active) void load();
  }, [active, load, reloadKey]);

  const copyText = React.useCallback(async (text: string, what: string) => {
    if (!text) {
      toast.error("没有可复制的内容");
      return;
    }
    try {
      await navigator.clipboard.writeText(text);
      toast.success(`已复制${what}`);
    } catch {
      toast.error("复制失败：浏览器拒绝了剪贴板访问");
    }
  }, []);

  const copyStatic = React.useCallback(async () => {
    if (!chain) return;
    const text = chain.segments
      .filter((s) => s.present && s.available && s.text)
      .map((s) => s.text)
      .join("");
    await copyText(text, "静态装配提示词（不含运行时段）");
  }, [chain, copyText]);

  const openTarget = React.useCallback(
    (target: Target, anchor?: string) => {
      if (!chain) return;
      setPath([
        {
          id: targetId(target),
          title: targetTitle(chain, target),
          anchor: anchor ?? anchorFor(chain, target),
          target,
        },
      ]);
    },
    [chain],
  );

  const drill = React.useCallback(
    (target: Target) => {
      if (!chain) return;
      setPath((prev) => {
        const last = prev[prev.length - 1];
        return [
          ...prev,
          {
            id: targetId(target),
            title: targetTitle(chain, target),
            anchor: last ? last.anchor : anchorFor(chain, target),
            target,
          },
        ];
      });
    },
    [chain],
  );

  const back = React.useCallback(() => {
    setPath((prev) => prev.slice(0, -1));
  }, []);

  const clear = React.useCallback(() => setPath([]), []);

  const truncatePath = React.useCallback((index: number) => {
    setPath((prev) => prev.slice(0, index + 1));
  }, []);

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
            <Badge variant="outline" className="px-1.5 py-0 font-mono text-[10px]">只读</Badge>
            <span className="ml-auto font-mono text-muted-foreground text-[10px]">快照时间未采集</span>
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
            onOpen={(target) => openTarget(target)}
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

      <ChainGraph
        chain={chain}
        path={path}
        onOpen={openTarget}
        onDrill={drill}
        onBack={back}
        onClear={clear}
        onTruncate={truncatePath}
        onCopyStatic={() => void copyStatic()}
        onCopyText={(text, what) => void copyText(text, what)}
        onReload={() => void load()}
      />

      <div className="flex flex-wrap items-center gap-2 text-muted-foreground text-[11px]">
        <button type="button" className="underline-offset-2 hover:underline" onClick={() => openTarget({ kind: "tools" })}>
          工具 {chain.tools.total}
        </button>
        <span>·</span>
        <button type="button" className="underline-offset-2 hover:underline" onClick={() => openTarget({ kind: "skills" })}>
          Skill {chain.skills.length}
        </button>
        <span>·</span>
        <button type="button" className="underline-offset-2 hover:underline" onClick={() => openTarget({ kind: "excluded" })}>
          排除 {chain.tools.excluded.length}
        </button>
        <span className="ml-auto text-right">
          已装配 实线框 · 实心来源条 / 门控未生效 虚线框 / 未采集 点线框 · 斜纹 / 被替换 删除线 · 已移出 / 行序从上到下 = 装配顺序
        </span>
      </div>
    </div>
  );
}

function MiniMap({
  layout,
  zoom,
  view,
  offsetRight,
  onJump,
}: {
  layout: Layout;
  zoom: number;
  view: { left: number; top: number; w: number; h: number };
  offsetRight: number;
  onJump: (left: number, top: number) => void;
}) {
  const mw = 168;
  const mh = Math.max(44, Math.min(120, Math.round((layout.height / layout.w) * mw)));
  const sx = mw / layout.w;
  const sy = mh / layout.height;
  const boxes: { r: Rect; className: string }[] = [
    { r: layout.llm, className: "fill-fuchsia-500/35" },
    { r: layout.skills, className: "fill-emerald-500/35" },
    { r: layout.tools, className: "fill-sky-500/35" },
    { r: layout.mcp, className: "fill-sky-500/25" },
    ...layout.rows.map((r) => ({ r, className: "fill-muted-foreground/45" })),
    ...(layout.tray ? [{ r: layout.tray, className: "fill-destructive/40" }] : []),
    ...(layout.quarantine ? [{ r: layout.quarantine, className: "fill-destructive/20" }] : []),
  ];
  const vx = (view.left / zoom) * sx;
  const vy = (view.top / zoom) * sy;
  const vw = Math.min(mw, (view.w / zoom) * sx);
  const vh = Math.min(mh, (view.h / zoom) * sy);
  return (
    <div
      className="pointer-events-none absolute bottom-2 z-10 hidden rounded-md border bg-background/90 p-1 shadow-sm backdrop-blur md:block"
      style={{ right: offsetRight }}
    >
      <svg
        width={mw}
        height={mh}
        viewBox={`0 0 ${mw} ${mh}`}
        className="pointer-events-auto cursor-pointer"
        role="img"
        aria-label="全链路缩略图，点击跳转"
        onClick={(e) => {
          const rect = e.currentTarget.getBoundingClientRect();
          const fx = e.clientX - rect.left;
          const fy = e.clientY - rect.top;
          const targetX = fx / sx - view.w / zoom / 2;
          const targetY = fy / sy - view.h / zoom / 2;
          onJump(Math.max(0, targetX) * zoom, Math.max(0, targetY) * zoom);
        }}
      >
        <rect x={0} y={0} width={mw} height={mh} className="fill-card" />
        {boxes.map((b, i) => (
          <rect
            key={i}
            x={b.r.x * sx}
            y={b.r.y * sy}
            width={Math.max(1, b.r.w * sx)}
            height={Math.max(1, b.r.h * sy)}
            className={b.className}
          />
        ))}
        <rect x={vx} y={vy} width={vw} height={vh} className="fill-foreground/10 stroke-foreground" strokeWidth={1} />
      </svg>
    </div>
  );
}

function ChainGraph({
  chain,
  path,
  onOpen,
  onDrill,
  onBack,
  onClear,
  onTruncate,
  onCopyStatic,
  onCopyText,
  onReload,
}: {
  chain: AgentPromptChainData;
  path: PathEntry[];
  onOpen: (target: Target, anchor?: string) => void;
  onDrill: (target: Target) => void;
  onBack: () => void;
  onClear: () => void;
  onTruncate: (index: number) => void;
  onCopyStatic: () => void;
  onCopyText: (text: string, what: string) => void;
  onReload: () => void;
}) {
  const wrapRef = React.useRef<HTMLDivElement>(null);
  const canvasRef = React.useRef<HTMLDivElement>(null);
  const backRef = React.useRef<HTMLButtonElement>(null);
  const [frameW, setFrameW] = React.useState(990);
  const open = path.length > 0;
  const last = path[path.length - 1];
  const anchor = last?.anchor ?? null;
  // 几何由容器实测宽推导：1 SVG 单位 = 1 CSS px，默认视图不缩放。
  const layout = React.useMemo(() => buildLayout(chain, frameW), [chain, frameW]);
  const narrow = layout.mode === "one";
  const anchorIdx = anchor ? layout.rows.findIndex((r) => r.id === anchor) : -1;
  const [zoom, setZoom] = React.useState(1);
  const zoomRef = React.useRef(1);
  zoomRef.current = zoom;
  const [view, setView] = React.useState({ left: 0, top: 0, w: 0, h: 0 });

  const syncView = React.useCallback(() => {
    const el = canvasRef.current;
    if (!el) return;
    setView({ left: el.scrollLeft, top: el.scrollTop, w: el.clientWidth, h: el.clientHeight });
  }, []);

  // 缩放只改渲染尺寸，文字仍是矢量与固定 CSS px，不靠缩小字号容纳。
  const applyZoom = React.useCallback(
    (next: number, ax?: number, ay?: number) => {
      const el = canvasRef.current;
      const z = Math.min(2.5, Math.max(0.5, Math.round(next * 100) / 100));
      if (!el) {
        setZoom(z);
        return;
      }
      const px = ax ?? el.clientWidth / 2;
      const py = ay ?? el.clientHeight / 2;
      const cx = (el.scrollLeft + px) / zoomRef.current;
      const cy = (el.scrollTop + py) / zoomRef.current;
      setZoom(z);
      requestAnimationFrame(() => {
        el.scrollLeft = cx * z - px;
        el.scrollTop = cy * z - py;
        syncView();
      });
    },
    [syncView],
  );

  React.useEffect(() => {
    const el = canvasRef.current;
    if (!el) return;
    const onWheel = (e: WheelEvent) => {
      if (!(e.ctrlKey || e.metaKey)) return;
      e.preventDefault();
      const rect = el.getBoundingClientRect();
      applyZoom(zoomRef.current * Math.exp(-e.deltaY * 0.002), e.clientX - rect.left, e.clientY - rect.top);
    };
    el.addEventListener("wheel", onWheel, { passive: false });
    return () => el.removeEventListener("wheel", onWheel);
  }, [applyZoom]);

  React.useEffect(() => {
    syncView();
  }, [syncView, frameW, layout.height]);

  React.useEffect(() => {
    // 测量画布本身（面板并排时画布宽度已扣除面板），而不是外层容器。
    const el = canvasRef.current;
    if (!el) return;
    const measure = () => {
      const node = canvasRef.current;
      if (!node) return;
      setFrameW(Math.max(320, Math.round(node.clientWidth - 16)));
    };
    measure();
    const ro = typeof ResizeObserver !== "undefined" ? new ResizeObserver(measure) : null;
    ro?.observe(el);
    window.addEventListener("resize", measure);
    return () => {
      window.removeEventListener("resize", measure);
      ro?.disconnect();
    };
  }, [path.length, open]);

  React.useEffect(() => {
    if (!open) {
      canvasRef.current?.scrollTo({ left: 0, top: 0 });
    }
    if (!open || !anchor) return;
    const raf = requestAnimationFrame(() => {
      const canvas = canvasRef.current;
      if (!canvas) return;
      const el = canvas.querySelector<SVGElement>(`[data-anchor="${anchor}"]`);
      if (!el) return;
      el.scrollIntoView({ block: "nearest", inline: "nearest", behavior: "smooth" });
      const cr = canvas.getBoundingClientRect();
      const er = el.getBoundingClientRect();
      const visibleRight = cr.right - 12;
      if (er.right > visibleRight) canvas.scrollLeft += er.right - visibleRight;
      else if (er.left < cr.left + 12) canvas.scrollLeft -= cr.left + 12 - er.left;
    });
    return () => cancelAnimationFrame(raf);
  }, [open, anchor, frameW]);

  React.useEffect(() => {
    if (open) backRef.current?.focus({ preventScroll: true });
  }, [open, path.length]);

  React.useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        onBack();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, onBack]);

  const dimmed = (id: string) => !!anchor && anchor !== id;

  return (
    <div className="grid gap-2">
      <div className="sticky top-0 z-20 flex flex-wrap items-center gap-1 rounded-md border bg-background/95 px-2 py-1.5 text-[11px] backdrop-blur">
        <button
          type="button"
          className={cn(
            "inline-flex items-center gap-1 rounded px-1.5 py-0.5 hover:bg-muted/60",
            path.length === 0 && "font-medium text-foreground",
          )}
          onClick={onClear}
        >
          <HomeIcon className="size-3" /> 全局预览
        </button>
        {path.map((entry, index) => (
          <React.Fragment key={`${entry.id}-${index}`}>
            <span className="text-muted-foreground">›</span>
            <button
              type="button"
              className={cn(
                "max-w-[180px] truncate rounded px-1 py-0.5 hover:bg-muted/60",
                index === path.length - 1 && "font-medium text-foreground",
              )}
              onClick={() => onTruncate(index)}
              title={entry.title}
            >
              {entry.title}
            </button>
          </React.Fragment>
        ))}
        <span className="ml-auto hidden pl-2 text-[10px] text-muted-foreground sm:inline">
          Esc 返回上一级
        </span>
        <span className="ml-auto flex items-center gap-0.5 pl-1 sm:ml-1">
          <button
            type="button"
            className="rounded px-1 py-0.5 hover:bg-muted/60"
            aria-label="缩小"
            onClick={() => applyZoom(zoom - 0.15)}
          >
            −
          </button>
          <span className="w-9 text-center tabular-nums text-[10px] text-muted-foreground">{Math.round(zoom * 100)}%</span>
          <button
            type="button"
            className="rounded px-1 py-0.5 hover:bg-muted/60"
            aria-label="放大"
            onClick={() => applyZoom(zoom + 0.15)}
          >
            +
          </button>
          <button type="button" className="rounded px-1 py-0.5 hover:bg-muted/60" onClick={() => applyZoom(1)}>
            1:1
          </button>
          <button
            type="button"
            className="rounded px-1 py-0.5 hover:bg-muted/60"
            onClick={() => {
              const el = canvasRef.current;
              if (!el) return;
              applyZoom(Math.min(1, (el.clientWidth - 16) / layout.w));
              el.scrollTo({ left: 0, top: 0 });
            }}
          >
            适应
          </button>
        </span>
      </div>

      <div ref={wrapRef} className="relative flex min-w-0 overflow-hidden rounded-lg border bg-card/40">
        <div
          ref={canvasRef}
          onScroll={syncView}
          className="min-w-0 flex-1 overflow-auto p-2"
        >
          <div style={{ width: layout.w * zoom }} className="mx-auto">
            <svg
              viewBox={`0 0 ${layout.w} ${layout.height}`}
              width={layout.w * zoom}
              height={layout.height * zoom}
              className="block max-w-none select-none"
              role="group"
              aria-label={`${chain.agent_key} 装配链门控信号流`}
              onClick={(e) => {
                if (path.length > 0 && e.target === e.currentTarget) onClear();
              }}
            >
              <defs>
                <marker id="ppm-arrow" viewBox="0 0 10 10" refX="8" refY="5" markerWidth="5" markerHeight="5" orient="auto">
                  <path d="M0,0 L10,5 L0,10 z" className="fill-muted-foreground" />
                </marker>
                <pattern id="ppm-hatch" width="6" height="6" patternUnits="userSpaceOnUse" patternTransform="rotate(45)">
                  <line x1="0" y1="0" x2="0" y2="6" className="stroke-muted-foreground" strokeWidth="1" opacity="0.25" />
                </pattern>
              </defs>

              {layout.boundary && (
                <rect
                  x={layout.boundary.x1}
                  y={layout.boundary.y + 2}
                  width={Math.max(0, layout.boundary.x2 - layout.boundary.x1)}
                  height={Math.max(0, layout.height - layout.boundary.y - 2)}
                  className="fill-background"
                  opacity={0.32}
                  pointerEvents="none"
                />
              )}

              <line
                x1={layout.trunkX}
                y1={layout.rows[0] ? layout.rows[0].y + layout.rows[0].h : 136}
                x2={layout.trunkX}
                y2={layout.modelRow.y + layout.modelRow.h / 2}
                className="stroke-border"
                strokeWidth={2}
              />

              {layout.rows.map((row) => {
                if (row.id === "agent") return null;
                return (
                  <line
                    key={`stub-${row.id}`}
                    x1={layout.trunkX}
                    y1={row.y + row.h / 2}
                    x2={layout.center.x}
                    y2={row.y + row.h / 2}
                    className="stroke-border"
                    strokeWidth={1.2}
                  />
                );
              })}

              {layout.tray && (
                <g
                  role="button"
                  tabIndex={0}
                  aria-label="被替换的提示词正文"
                  data-anchor="body"
                  className={cn("cursor-pointer outline-none", dimmed("body") && "opacity-40")}
                  onClick={() => onOpen({ kind: "segment", key: chain.segments.find((s) => s.kind === "body")?.key ?? "body" }, "body")}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" || e.key === " ") {
                      e.preventDefault();
                      onOpen({ kind: "segment", key: chain.segments.find((s) => s.kind === "body")?.key ?? "body" }, "body");
                    }
                  }}
                >
                  <rect
                    x={layout.tray.x}
                    y={layout.tray.y}
                    width={layout.tray.w}
                    height={layout.tray.h}
                    className="fill-card stroke-destructive"
                    strokeDasharray="5 4"
                  />
                  <rect x={layout.tray.x} y={layout.tray.y} width={4} height={layout.tray.h} className="fill-destructive" opacity={0.5} />
                  <text x={layout.tray.x + 10} y={layout.tray.y + 14} className="fill-destructive font-mono text-[8px]">被替换</text>
                  <text x={layout.tray.x + 10} y={layout.tray.y + 30} className="fill-foreground text-[11px] line-through opacity-70">提示词正文</text>
                  <text x={layout.tray.x + 10} y={layout.tray.y + 45} className="fill-muted-foreground text-[8px]">
                    {fmt(chain.segments.find((s) => s.kind === "body")?.chars ?? 0)} 字符 · 不进入本次请求
                  </text>
                </g>
              )}

              {layout.rows.map((row, i) => (
                <RowNode
                  key={row.id}
                  row={row}
                  maxChars={layout.measureMax}
                  anchor={anchor === row.id}
                  phase={
                    !anchor
                      ? "none"
                      : anchorIdx < 0
                        ? "related"
                        : i === anchorIdx
                          ? "current"
                          : i < anchorIdx
                            ? "past"
                            : "future"
                  }
                  onOpen={() => onOpen(row.target, row.id)}
                />
              ))}

              {layout.boundary && (
                <g className="pointer-events-none">
                  <line x1={layout.boundary.x1} y1={layout.boundary.y} x2={layout.boundary.x2} y2={layout.boundary.y} className="stroke-foreground/60" strokeWidth={2} />
                  <line x1={layout.boundary.x1} y1={layout.boundary.y - 4} x2={layout.boundary.x1} y2={layout.boundary.y + 4} className="stroke-foreground/60" strokeWidth={2} />
                  <line x1={layout.boundary.x2} y1={layout.boundary.y - 4} x2={layout.boundary.x2} y2={layout.boundary.y + 4} className="stroke-foreground/60" strokeWidth={2} />
                  <text x={layout.boundary.x2} y={layout.boundary.y - 6} textAnchor="end" className="fill-foreground/70 text-[8px]">
                    硬边界 · 配置层到此为止
                  </text>
                </g>
              )}

              <LLMRail
                chain={chain}
                layout={layout}
                active={anchor === "llm"}
                dimmed={dimmed("llm")}
                onOpen={() => onOpen({ kind: "llm" }, "llm")}
              />
              <SkillsRail
                chain={chain}
                layout={layout}
                active={anchor === "skills"}
                dimmed={dimmed("skills")}
                onOpen={() => onOpen({ kind: "skills" }, "skills")}
              />
              <ToolsRail
                chain={chain}
                layout={layout}
                active={anchor === "tools"}
                dimmed={dimmed("tools")}
                onOpen={() => onOpen({ kind: "tools" }, "tools")}
              />
              <MCPRail
                chain={chain}
                layout={layout}
                anchor={anchor}
                dimmed={dimmed}
                onOpen={onOpen}
              />

              {layout.mode !== "one" && layout.profileRow && (
                <path
                  d={`M${layout.llm.x + layout.llm.w},${layout.llm.y + 64} H${layout.center.x - 50} V${layout.profileRow.y - 18}`}
                  className={cn("fill-none", layout.profileRow.present ? "stroke-fuchsia-500" : "stroke-destructive")}
                  strokeWidth={1.6}
                  strokeDasharray={layout.profileRow.present ? undefined : "4 3"}
                  opacity={0.85}
                />
              )}
              {layout.mode !== "one" && layout.byId.get("skill") && (
                <path
                  d={`M${layout.skills.x + layout.skills.w},${layout.skills.y + 22} H${layout.center.x - 40} V${layout.byId.get("skill")!.y + layout.byId.get("skill")!.h / 2} H${layout.center.x}`}
                  className="fill-none stroke-emerald-500"
                  strokeWidth={1.2}
                  opacity={0.6}
                  markerEnd="url(#ppm-arrow)"
                />
              )}
              {layout.mode !== "one" && (
                <path
                  d={`M${layout.tools.x},${layout.tools.y + 24} H${layout.center.x + layout.center.w + 35} V${layout.modelRow.y + 17} H${layout.center.x + layout.center.w}`}
                  className="fill-none stroke-sky-500"
                  strokeWidth={1.2}
                  opacity={0.55}
                  markerEnd="url(#ppm-arrow)"
                />
              )}
              {layout.mode !== "one" && chain.mcp.length > 0 && (
                <line
                  x1={layout.center.x + layout.center.w + 35}
                  y1={layout.mcp.y + 18}
                  x2={layout.center.x + layout.center.w + 35}
                  y2={layout.tools.y + layout.tools.h}
                  className="stroke-sky-500"
                  strokeWidth={1}
                  opacity={0.35}
                />
              )}
              {layout.mode !== "one" && layout.runtimeRow && (
                <path
                  d={`M${layout.center.x + layout.center.w},${layout.runtimeRow.y + layout.runtimeRow.h / 2} H${layout.center.x + layout.center.w + 35} V${layout.modelRow.y + 12} H${layout.center.x + layout.center.w}`}
                  className="fill-none stroke-muted-foreground"
                  strokeDasharray="1 3"
                  strokeWidth={1.1}
                  opacity={0.65}
                  markerEnd="url(#ppm-arrow)"
                />
              )}
              {layout.mode !== "one" && layout.deferredRow && (
                <path
                  d={`M${layout.center.x + layout.center.w},${layout.deferredRow.y + layout.deferredRow.h / 2} H${layout.center.x + layout.center.w + 35} V${layout.modelRow.y + 40} H${layout.center.x + layout.center.w}`}
                  className="fill-none stroke-muted-foreground"
                  strokeDasharray="1 3"
                  strokeWidth={1.1}
                  opacity={0.65}
                  markerEnd="url(#ppm-arrow)"
                />
              )}

              <Quarantine
                chain={chain}
                layout={layout}
                active={anchor === "excluded"}
                dimmed={dimmed("excluded")}
                onOpen={() => onOpen({ kind: "excluded" }, "excluded")}
              />

              {layout.profileRow && (
                <g>
                  <rect
                    x={layout.trunkX - 50}
                    y={layout.profileRow.y - 26}
                    width={100}
                    height={16}
                    rx={8}
                    className={cn("fill-card", layout.profileRow.present ? "stroke-fuchsia-500" : "stroke-destructive")}
                    strokeDasharray={layout.profileRow.present ? undefined : "3 2"}
                  />
                  <text
                    x={layout.trunkX}
                    y={layout.profileRow.y - 15}
                    textAnchor="middle"
                    className={cn("text-[8px]", layout.profileRow.present ? "fill-fuchsia-600 dark:fill-fuchsia-400" : "fill-destructive")}
                  >
                    {layout.profileRow.present ? "注入" : "未注入"} · {modeLabel(chain.llm.prompt_mode)}
                  </text>
                </g>
              )}
              {layout.runtimeRow && (
                <g>
                  <rect x={layout.trunkX - 48} y={layout.runtimeRow.y - 26} width={96} height={16} rx={8} className="fill-card stroke-border" />
                  <text x={layout.trunkX} y={layout.runtimeRow.y - 15} textAnchor="middle" className="fill-muted-foreground text-[8px]">
                    运行时注入
                  </text>
                </g>
              )}
              <g>
                <rect x={layout.trunkX - 64} y={layout.modelRow.y - 26} width={128} height={16} rx={8} className="fill-card stroke-border" />
                <text x={layout.trunkX} y={layout.modelRow.y - 15} textAnchor="middle" className="fill-muted-foreground text-[8px]">
                  第二 system（按需）
                </text>
              </g>

              <text x={16} y={layout.height - 18} className="fill-muted-foreground text-[8px]">
                运行时注入在任务执行时生成；本视图只给位置与条件，不展开正文、不连接 MCP、不含凭据。
              </text>
              <text x={layout.w - 20} y={layout.height - 18} textAnchor="end" className="fill-muted-foreground text-[8px]">
                只读 · 静态
              </text>
            </svg>
          </div>
        </div>

        {layout.height > 640 && (
          <MiniMap
            offsetRight={open && !narrow ? 372 : 8}
            layout={layout}
            zoom={zoom}
            view={view}
            onJump={(left, top) => {
              const el = canvasRef.current;
              if (!el) return;
              el.scrollTo({ left, top });
              syncView();
            }}
          />
        )}

        {open && (
          <ChainDetail
            chain={chain}
            path={path}
            backRef={backRef}
            onBack={onBack}
            onClose={onClear}
            onDrill={onDrill}
            onCopyStatic={onCopyStatic}
            onCopyText={onCopyText}
            onReload={onReload}
            narrow={narrow}
          />
        )}
      </div>
    </div>
  );
}

function RowNode({
  row,
  maxChars,
  anchor,
  phase,
  onOpen,
}: {
  row: Row;
  maxChars: number;
  anchor: boolean;
  phase: "none" | "related" | "past" | "current" | "future";
  onOpen: () => void;
}) {
  const color = COLOR[row.color];
  const chars =
    row.chars === null ? "" : row.gated || row.unknown ? "—" : `${fmt(row.chars)}c`;
  const measure =
    row.present && row.chars !== null && row.chars > 0
      ? Math.max(2, Math.round((row.chars / maxChars) * (row.w - 40)))
      : 0;
  // 文案按当前行宽裁切，不再用「18 个字」这类与容器无关的魔法数。
  const charsW = chars ? Math.max(26, measureText(chars, { px: 8, mono: true }) + 8) : 0;
  const title = fitText(row.title, row.w - 20 - charsW, { px: 11 }, measureText);
  const sub = fitText(row.sub, Math.max(60, row.w * 0.55), { px: 8 }, measureText);
  return (
    <g
      role="button"
      tabIndex={0}
      data-anchor={row.id}
      aria-label={`${row.title}，${row.chars === null ? "" : row.unknown ? "运行时未采集" : row.present ? `${row.chars} 字符` : "未生效"}`}
      className={cn("cursor-pointer outline-none", phase === "future" && "opacity-50", phase === "past" && "opacity-90", phase === "related" && "opacity-75")}
      onClick={onOpen}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onOpen();
        }
      }}
    >
      <title>{`${row.title} · ${row.sub}${row.chars === null ? "" : ` · ${row.chars} 字符`}`}</title>
      <rect
        x={row.x}
        y={row.y}
        width={row.w}
        height={row.h}
        className={cn(
          "fill-card",
          anchor ? "stroke-foreground" : row.unknown ? "stroke-muted-foreground" : row.gated ? color.stroke : color.stroke,
        )}
        strokeWidth={anchor ? 2 : 1}
        strokeDasharray={row.unknown ? "1 3" : row.gated ? "4 3" : undefined}
      />
      {row.unknown && (
        <rect x={row.x + 1} y={row.y + 1} width={row.w - 2} height={row.h - 2} fill="url(#ppm-hatch)" className="pointer-events-none" />
      )}
      <rect x={row.x} y={row.y} width={4} height={row.h} className={color.fill} opacity={row.present ? 1 : 0.4} />
      <text x={row.x + 10} y={row.y + 14} className={cn("font-mono text-[8px]", color.text)}>{row.tag}</text>
      <text x={row.x + row.w - 10} y={row.y + 14} textAnchor="end" className="fill-muted-foreground font-mono text-[8px] tabular-nums">
        {chars}
      </text>
      <text
        x={row.x + 10}
        y={row.y + 29}
        className={cn("fill-foreground text-[11px]", row.struck && "line-through opacity-60")}
      >
        {title}
      </text>
      {row.unknown ? (
        <g>
          {Array.from({ length: row.sockets ?? 1 }).map((_, i) => (
            <rect key={i} x={row.x + row.w - 11 - i * 9} y={row.y + row.h - 11} width={6} height={6} className="fill-none stroke-muted-foreground" strokeWidth={1} />
          ))}
        </g>
      ) : (
        <text x={row.x + row.w - 10} y={row.y + 28} textAnchor="end" className="fill-muted-foreground text-[8px]">
          {sub}
        </text>
      )}
      {measure > 0 && (
        <rect x={row.x + 10} y={row.y + row.h - 4} width={measure} height={2} className={color.fill} opacity={0.4} />
      )}
    </g>
  );
}

function LLMRail({
  chain,
  layout,
  active,
  dimmed,
  onOpen,
}: {
  chain: AgentPromptChainData;
  layout: Layout;
  active: boolean;
  dimmed: boolean;
  onOpen: () => void;
}) {
  const llm = chain.llm;
  const source = llm.source;
  const steps = [
    {
      label: "① Agent 显式绑定",
      state: source === "agent_binding" ? "命中" : source === "preview_override" ? "预览指定" : source === "none" ? "未绑定" : "未命中",
    },
    {
      label: "② 任务链",
      state: source === "agent_binding" ? "未参与（绑定已定）" : source === "none" ? "运行时解析" : "未参与",
    },
    {
      label: "③ 全局激活",
      state: source === "global_profile" ? "命中" : source === "global_profile_inactive" ? "未生效" : source === "agent_binding" ? "未参与（绑定已定）" : "未命中",
    },
  ];
  const tone = (state: string) =>
    state === "命中" ? "fill-fuchsia-600 dark:fill-fuchsia-400" : state === "预览指定" ? "fill-sky-600 dark:fill-sky-400" : state === "未生效" ? "fill-destructive" : "fill-muted-foreground";
  const sourceWord =
    source === "agent_binding"
      ? "显式绑定"
      : source === "preview_override"
        ? "预览配置"
        : source === "global_profile"
          ? "全局继承"
          : source === "global_profile_inactive"
            ? "全局未生效"
            : "配置";
  const proof = llm.profile_id
    ? `${sourceWord} ${chain.agent_key} → #${llm.profile_id} · ${fmt(chain.llm.prompt_chars)} 字符`
    : source === "preview_override"
      ? "预览配置不存在"
      : source === "agent_binding"
        ? "绑定配置不存在"
        : "未绑定 · 未解析到 LLM 配置";
  const { x, y, w, h } = layout.llm;
  return (
    <g
      role="button"
      tabIndex={0}
      data-anchor="llm"
      aria-label="LLM 配置解析"
      className={cn("cursor-pointer outline-none", dimmed && "opacity-40")}
      onClick={onOpen}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onOpen();
        }
      }}
    >
      <rect x={x} y={y} width={w} height={h} className={cn("fill-card", active ? "stroke-foreground" : "stroke-border")} strokeWidth={active ? 2 : 1} />
      <rect x={x} y={y} width={4} height={h} className="fill-fuchsia-500" />
      <text x={x + 12} y={y + 15} className="fill-fuchsia-600 font-mono text-[8px] dark:fill-fuchsia-400">LLM 配置解析</text>
      <text x={x + 12} y={y + 31} className="fill-foreground text-[11px]">{llm.name || "未解析"}</text>
      <text x={x + 12} y={y + 45} className="fill-muted-foreground text-[8px]">
        {llm.profile_id ? `#${llm.profile_id} · ` : ""}{llm.format} · {llm.model || "—"}
      </text>
      <line x1={x + 12} y1={y + 52} x2={x + w - 12} y2={y + 52} className="stroke-border" />
      {steps.map((row, i) => (
        <g key={row.label}>
          <text x={x + 12} y={y + 68 + i * 17} className="fill-muted-foreground text-[8px]">{row.label}</text>
          <text x={x + w - 12} y={y + 68 + i * 17} textAnchor="end" className={cn("text-[7px]", tone(row.state))}>{row.state}</text>
        </g>
      ))}
      <text x={x + 12} y={y + 120} className="fill-muted-foreground text-[8px]">
        继承时也生效：{llm.prompt_apply_inherited ? "开" : "关"} · 显式绑定不受影响
      </text>
      <text x={x + 12} y={y + 134} className="fill-muted-foreground text-[7px]">{trimText(proof, 34)}</text>
      {source === "preview_override" && (
        <text x={x + w - 12} y={y + 15} textAnchor="end" className="fill-sky-600 font-mono text-[7px] dark:fill-sky-400">预览</text>
      )}
    </g>
  );
}

function SkillsRail({
  chain,
  layout,
  active,
  dimmed,
  onOpen,
}: {
  chain: AgentPromptChainData;
  layout: Layout;
  active: boolean;
  dimmed: boolean;
  onOpen: () => void;
}) {
  const shown = chain.skills.slice(0, 6);
  const hidden = chain.skills.length - shown.length;
  const inIndex = chain.skills.filter((s) => s.in_index).length;
  const { x, y, w, h } = layout.skills;
  const nameMaxW = w - 50;
  return (
    <g
      role="button"
      tabIndex={0}
      data-anchor="skills"
      aria-label="Skill 绑定"
      className={cn("cursor-pointer outline-none", dimmed && "opacity-40")}
      onClick={onOpen}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onOpen();
        }
      }}
    >
      <rect x={x} y={y} width={w} height={h} className={cn("fill-card", active ? "stroke-foreground" : "stroke-border")} strokeWidth={active ? 2 : 1} />
      <rect x={x} y={y} width={4} height={h} className="fill-emerald-500" />
      <text x={x + 12} y={y + 15} className="fill-emerald-600 font-mono text-[8px] dark:fill-emerald-400">Skill 绑定 · {chain.skills.length}</text>
      <text x={x + w - 10} y={y + 15} textAnchor="end" className="fill-muted-foreground text-[8px]">已进速查 {inIndex} / {chain.skills.length}</text>
      {shown.length === 0 && (
        <text x={x + 12} y={y + 31} className="fill-muted-foreground text-[8px]">（无 Skill）</text>
      )}
      {shown.map((s, i) => (
        <text key={s.name} x={x + 12} y={y + 33 + i * 15} className={cn("text-[8px]", s.in_index ? "fill-foreground" : "fill-muted-foreground")}>
          <title>{s.name}</title>
          {s.in_index ? "●" : "○"} {fitText(s.name, nameMaxW, { px: 8 }, measureText)}
        </text>
      ))}
      {hidden > 0 && (
        <text x={x + w - 10} y={y + h - 6} textAnchor="end" className="fill-muted-foreground text-[7px]">+{hidden}</text>
      )}
    </g>
  );
}

function ToolsRail({
  chain,
  layout,
  active,
  dimmed,
  onOpen,
}: {
  chain: AgentPromptChainData;
  layout: Layout;
  active: boolean;
  dimmed: boolean;
  onOpen: () => void;
}) {
  const { x, y, w, h } = layout.tools;
  const t = chain.tools;
  return (
    <g
      role="button"
      tabIndex={0}
      data-anchor="tools"
      aria-label="工具 schema"
      className={cn("cursor-pointer outline-none", dimmed && "opacity-40")}
      onClick={onOpen}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onOpen();
        }
      }}
    >
      <rect x={x} y={y} width={w} height={h} className={cn("fill-card", active ? "stroke-foreground" : "stroke-border")} strokeWidth={active ? 2 : 1} />
      <rect x={x} y={y} width={4} height={h} className="fill-sky-500" />
      <text x={x + 12} y={y + 15} className="fill-sky-600 font-mono text-[8px] dark:fill-sky-400">工具 · {t.total}</text>
      <text x={x + w - 12} y={y + 15} textAnchor="end" className="fill-muted-foreground text-[8px]">
        {fitText("随请求单独下发，不进 system", Math.max(80, w - 120), { px: 8 }, measureText)}
      </text>
      <text x={x + 12} y={y + 33} className="fill-muted-foreground text-[8px]">
        内置 {t.builtin.length} · 自定义 {t.custom.length} · 流量 {t.traffic.length} · Shell {t.shell.length}
      </text>
      <text x={x + 12} y={y + 48} className="fill-muted-foreground text-[8px]">延迟 {t.deferred_names.length} → 第二 system</text>
      <text x={x + 12} y={y + 63} className="fill-destructive text-[8px]">未进主干 {t.excluded.length}</text>
      <line x1={x + 12} y1={y + 74} x2={x + w - 12} y2={y + 74} className="stroke-border" />
      <text x={x + 12} y={y + 90} className="fill-muted-foreground text-[8px]">MCP 工具按需进入第二 system</text>
    </g>
  );
}

function MCPRail({
  chain,
  layout,
  anchor,
  dimmed,
  onOpen,
}: {
  chain: AgentPromptChainData;
  layout: Layout;
  anchor: string | null;
  dimmed: (id: string) => boolean;
  onOpen: (target: Target, anchor?: string) => void;
}) {
  const { mcp } = layout;
  const tools = mcp;
  const mcpY = mcp.y;
  const disabled = chain.mcp.filter((m) => !m.enabled).length;
  return (
    <g>
      <text x={tools.x} y={mcpY - 8} className="fill-sky-600 font-mono text-[8px] dark:fill-sky-400">
        MCP 服务器 · {chain.mcp.length} · {disabled} 未启用
      </text>
      {chain.mcp.length === 0 && (
        <text x={tools.x} y={mcpY + 12} className="fill-muted-foreground text-[8px]">（无 MCP 服务器）</text>
      )}
      {chain.mcp.map((m, i) => {
        const y = mcpY + i * 38 + 10;
        const id = `mcp:${m.id}`;
        const active = anchor === id;
        const tone = !m.enabled || m.mode === "excluded"
          ? "fill-transparent stroke-muted-foreground/60"
          : m.health === "ok"
            ? "fill-emerald-500 stroke-emerald-500"
            : m.health === "failed"
              ? "fill-destructive stroke-destructive"
              : "fill-transparent stroke-muted-foreground";
        const status = !m.enabled
          ? "未启用 · 不进入请求"
          : m.mode === "excluded"
            ? "不进入本 Agent"
            : m.mode === "skill_gated"
              ? `由 Skill「${m.skill}」解锁`
              : m.health === "failed"
                ? "上次失败 · 缓存可能过期"
                : m.health === "never_checked"
                  ? "未检查"
                  : "全局可见";
        const cached = !m.enabled || m.mode === "excluded"
          ? "未进入本 Agent"
          : `已缓存 ${m.tools.length} / 共 ${m.tool_count}`;
        return (
          <g
            key={m.id}
            role="button"
            tabIndex={0}
            data-anchor={id}
            aria-label={`MCP ${m.name}，${healthLabel(m.health)}`}
            className={cn("cursor-pointer outline-none", dimmed(id) && "opacity-40")}
            onClick={() => onOpen({ kind: "mcp", id: m.id }, id)}
            onKeyDown={(e) => {
              if (e.key === "Enter" || e.key === " ") {
                e.preventDefault();
                onOpen({ kind: "mcp", id: m.id }, id);
              }
            }}
          >
            <rect x={tools.x} y={y} width={tools.w} height={34} className={cn("fill-card", active ? "stroke-foreground" : "stroke-border")} strokeWidth={active ? 2 : 1} />
            {m.cache_stale && <rect x={tools.x + tools.w - 3} y={y} width={3} height={34} className="fill-amber-500" opacity={0.7} />}
            <circle cx={tools.x + 12} cy={y + 17} r={3} className={tone} strokeWidth={1} />
            <text x={tools.x + 24} y={y + 13} className="fill-foreground text-[10px]">
              <title>{m.name}</title>
              {fitText(m.name, tools.w - 120, { px: 10 }, measureText)}
            </text>
            <text x={tools.x + 24} y={y + 27} className="fill-muted-foreground text-[8px]">{fitText(`${m.transport} · ${status}`, tools.w - 120, { px: 8 }, measureText)}</text>
            <text x={tools.x + tools.w - 10} y={y + 13} textAnchor="end" className="fill-muted-foreground text-[7px]">{cached}</text>
          </g>
        );
      })}
    </g>
  );
}

function Quarantine({
  chain,
  layout,
  active,
  dimmed,
  onOpen,
}: {
  chain: AgentPromptChainData;
  layout: Layout;
  active: boolean;
  dimmed: boolean;
  onOpen: () => void;
}) {
  const excluded = chain.tools.excluded;
  const outOfIndex = chain.skills.filter((s) => !s.in_index);
  if (!layout.quarantine) return null;
  const { x, y, w, h } = layout.quarantine;
  return (
    <g
      role="button"
      tabIndex={0}
      data-anchor="excluded"
      aria-label="未进入主干的项"
      className={cn("cursor-pointer outline-none", dimmed && "opacity-40")}
      onClick={onOpen}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onOpen();
        }
      }}
    >
      <rect x={x} y={y} width={w} height={h} className={cn("fill-card", active ? "stroke-foreground" : "stroke-destructive")} strokeDasharray="5 4" strokeWidth={active ? 2 : 1} />
      <text x={x + 12} y={y + 16} className="fill-destructive font-mono text-[8px]">未进入主干 · {excluded.length + outOfIndex.length}</text>
      {excluded.slice(0, 4).map((t, i) => (
        <text key={t.key} x={x + 12} y={y + 32 + i * 13} className="fill-destructive text-[8px]">
          <title>{`${t.key} — ${t.reason}`}</title>
          ✕ {fitText(`${t.key} — ${t.reason}`, w - 24, { px: 8 }, measureText)}
        </text>
      ))}
      {outOfIndex.length > 0 && (
        <text x={x + 12} y={y + h - 12} className="fill-muted-foreground text-[8px]">
          索引外 Skill：{outOfIndex.map((s) => s.name).join("、")}（仍可 Skill(name=...) 加载）
        </text>
      )}
    </g>
  );
}


const EFFORT_CHOICES = ["", "low", "medium", "high", "xhigh", "max"] as const;

type Stage = { id: string; label: string; target: Target; offTrunk?: boolean };

function stageTargets(chain: AgentPromptChainData): Stage[] {
  const find = (kind: string) => chain.segments.find((s) => s.kind === kind);
  const out: Stage[] = [{ id: "agent", label: "Agent", target: { kind: "model" } }];
  const profile = find("profile_prompt");
  if (profile && chain.llm.prompt_mode) {
    out.push({ id: "profile", label: "独立提示词", target: { kind: "segment", key: profile.key } });
  }
  const body = find("body");
  if (body && chain.llm.prompt_mode !== "replace") {
    out.push({ id: "body", label: "正文", target: { kind: "segment", key: body.key } });
  }
  const skill = find("skill_index");
  if (skill) out.push({ id: "skill", label: "技能速查", target: { kind: "segment", key: skill.key } });
  if (chain.segments.some((s) => s.kind === "code_tail" && s.present)) {
    out.push({ id: "code", label: "代码硬尾", target: { kind: "code" } });
  }
  if (chain.segments.some((s) => s.kind === "code_tail" && !s.present)) {
    out.push({ id: "gated", label: "门控未生效", target: { kind: "code" } });
  }
  const host = find("host");
  if (host) out.push({ id: "host", label: "本机工具", target: { kind: "segment", key: host.key } });
  if (chain.segments.some((s) => !s.available && s.kind !== "deferred_tools")) {
    out.push({ id: "runtime", label: "运行时注入", target: { kind: "runtime" } });
  }
  if (chain.tools.deferred_names.length > 0) {
    out.push({ id: "deferred", label: "延迟工具", target: { kind: "deferred" } });
  }
  out.push({ id: "model", label: "模型请求", target: { kind: "model" } });
  out.push({ id: "llm", label: "LLM 配置", target: { kind: "llm" }, offTrunk: true });
  out.push({ id: "skills", label: "Skill 轨", target: { kind: "skills" }, offTrunk: true });
  out.push({ id: "tools", label: "工具轨", target: { kind: "tools" }, offTrunk: true });
  out.push({ id: "mcp", label: "MCP 轨", target: { kind: "mcp", id: chain.mcp[0]?.id ?? 0 }, offTrunk: true });
  return out;
}

function activeStage(chain: AgentPromptChainData, target: Target): string {
  switch (target.kind) {
    case "code":
      return "code";
    case "runtime":
      return "runtime";
    case "deferred":
      return "deferred";
    case "model":
      return "model";
    case "llm":
      return "llm";
    case "skills":
      return "skills";
    case "tools":
      return "tools";
    case "mcp":
      return "mcp";
    case "segment": {
      const seg = chain.segments.find((s) => s.key === target.key);
      switch (seg?.kind) {
        case "profile_prompt":
          return "profile";
        case "body":
          return "body";
        case "skill_index":
          return "skill";
        case "host":
          return "host";
        default:
          return "agent";
      }
    }
    default:
      return "agent";
  }
}

// 全链路态势：把当前点选节点放回它在装配顺序里的位置，前后段一眼可见。
function FocusStages({
  chain,
  target,
  onDrill,
}: {
  chain: AgentPromptChainData;
  target: Target;
  onDrill: (t: Target) => void;
}) {
  const stages = stageTargets(chain);
  const activeId = activeStage(chain, target);
  const activeIdx = stages.findIndex((s) => s.id === activeId);
  return (
    <div className="mb-3 rounded-md border bg-muted/20 p-2">
      <div className="mb-1.5 font-mono text-[9px] text-muted-foreground uppercase tracking-[0.12em]">
        全链路态势 · 第 {activeIdx + 1} / {stages.length} 段
      </div>
      <div className="flex flex-wrap items-center gap-1">
        {stages.map((stage, i) => {
          const active = stage.id === activeId;
          const past = activeIdx < 0 ? false : i < activeIdx;
          return (
            <React.Fragment key={stage.id}>
              {i > 0 && <span className="text-[9px] text-muted-foreground/60">→</span>}
              <button
                type="button"
                title={`${stage.label}${stage.offTrunk ? "（侧轨）" : ""}`}
                onClick={() => onDrill(stage.target)}
                className={cn(
                  "rounded border px-1.5 py-0.5 text-[10px] transition-colors",
                  active
                    ? "border-foreground bg-foreground font-medium text-background"
                    : stage.offTrunk
                      ? "border-dashed text-muted-foreground hover:bg-muted/60"
                      : past
                        ? "text-muted-foreground hover:bg-muted/60"
                        : "text-foreground/80 hover:bg-muted/60",
                )}
              >
                {stage.label}
              </button>
            </React.Fragment>
          );
        })}
      </div>
      <p className="mt-1.5 text-[10px] text-muted-foreground">
        高亮为当前节点；左侧标签是它之前已经进入提示词的段，右侧是之后仍会追加/按需进入的段。
      </p>
    </div>
  );
}

// 右侧面板的可配置区：思考强度覆盖 + 独立提示词直接编辑 + 各段配置入口。
function ConfigActions({
  chain,
  target,
  onReload,
}: {
  chain: AgentPromptChainData;
  target: Target;
  onReload: () => void;
}) {
  const [profiles, setProfiles] = React.useState<LLMProfile[]>([]);
  const [busy, setBusy] = React.useState(false);
  const [prompt, setPrompt] = React.useState("");
  const [mode, setMode] = React.useState<string>("");
  const [inherit, setInherit] = React.useState(false);
  const pid = chain.llm.profile_id ?? 0;
  const profile = pid ? profiles.find((p) => Number(p.id) === pid) : undefined;

  React.useEffect(() => {
    api
      .llmProfiles()
      .then(setProfiles)
      .catch(() => setProfiles([]));
  }, []);

  React.useEffect(() => {
    if (!profile) return;
    setPrompt(profile.prompt ?? "");
    setMode(profile.prompt_mode ?? "");
    setInherit(!!profile.prompt_apply_inherited);
  }, [profile]);

  const isPromptSurface =
    target.kind === "llm" ||
    (target.kind === "segment" &&
      chain.segments.some(
        (s) => s.key === target.key && (s.kind === "profile_prompt" || s.kind === "body"),
      ));

  const setEffort = async (value: string) => {
    if (!pid) return;
    setBusy(true);
    try {
      if (!value) await api.deleteAgentEffortOverride(chain.agent_key, pid);
      else await api.setAgentEffortOverride(chain.agent_key, pid, value);
      toast.success("已更新思考强度覆盖（下一次模型调用生效）");
      onReload();
    } catch (e) {
      toast.error((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const savePrompt = async () => {
    if (!profile) return;
    setBusy(true);
    try {
      await api.saveLLMProfile({
        id: Number(profile.id),
        name: profile.name,
        format: profile.format,
        model: profile.model,
        base_url: profile.base_url,
        proxy: profile.proxy,
        rate_per_second: profile.rate_per_second,
        rate_per_minute: profile.rate_per_minute,
        context_window_k: profile.context_window_k,
        thinking_type: profile.thinking_type,
        reasoning_effort: profile.reasoning_effort,
        priority: profile.priority,
        pool_exclude: profile.pool_exclude,
        streaming: profile.streaming,
        max_tokens: profile.max_tokens,
        max_tokens_field: profile.max_tokens_field,
        session_header_key: profile.session_header_key,
        user_agent: profile.user_agent,
        preset_id: profile.preset_id,
        prompt,
        prompt_mode: mode,
        prompt_apply_inherited: inherit,
      });
      toast.success("已保存配置提示词；运行中的会话沿用原 system prompt");
      onReload();
    } catch (e) {
      toast.error((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const links: { label: string; href: string }[] = [
    { label: "Agent 配置", href: `/system/agents/detail?key=${encodeURIComponent(chain.agent_key)}` },
    { label: "LLM 配置", href: "/system/llm" },
    { label: "Skill", href: "/system/skills" },
    { label: "工具", href: "/system/tools" },
    { label: "MCP", href: "/system/mcp" },
  ];

  return (
    <div className="mb-3 rounded-md border border-sky-500/30 bg-sky-500/[0.03] p-2">
      <div className="mb-1.5 font-mono text-[9px] text-sky-600 uppercase tracking-[0.12em] dark:text-sky-400">
        可配置 · 下一次模型调用生效
      </div>

      {isPromptSurface && pid > 0 && (
        <div className="mb-2">
          <label className="mb-1 block text-[10px] text-muted-foreground">
            思考强度覆盖（{chain.agent_key} × 本配置）
          </label>
          <select
            className="w-full rounded border bg-background px-1.5 py-1 text-xs"
            value={chain.llm.effort_source === "override" ? (chain.llm.effort ?? "") : ""}
            disabled={busy}
            onChange={(e) => void setEffort(e.target.value)}
          >
            <option value="">继承配置（默认）</option>
            {EFFORT_CHOICES.filter(Boolean).map((v) => (
              <option key={v} value={v}>
                {v}
              </option>
            ))}
          </select>
          <p className="mt-1 text-[9px] text-muted-foreground">
            当前生效：{chain.llm.effort || "未设置"}（
            {chain.llm.effort_source === "override"
              ? "overrides 覆盖"
              : chain.llm.effort_source === "profile"
                ? "配置自身"
                : "未设置"}
            ）
          </p>
        </div>
      )}

      {isPromptSurface && profile && (
        <details className="mb-2 rounded border bg-background/60 p-1.5" open={target.kind === "segment"}>
          <summary className="cursor-pointer text-[10px] text-muted-foreground">
            独立提示词（{profile.name} #{profile.id}）
            {profile.prompt_apply_inherited ? " · 继承时也生效" : ""}
          </summary>
          <div className="mt-1.5 grid gap-1.5">
            <textarea
              className="min-h-24 w-full resize-y rounded border bg-background px-1.5 py-1 font-mono text-[11px]"
              value={prompt}
              placeholder="纯文本，不做模板变量渲染"
              onChange={(e) => setPrompt(e.target.value)}
            />
            <div className="flex flex-wrap items-center gap-2 text-[10px]">
              <select
                className="rounded border bg-background px-1 py-0.5"
                value={mode}
                onChange={(e) => setMode(e.target.value)}
              >
                <option value="">不注入</option>
                <option value="prepend">prepend（正文最前）</option>
                <option value="append">append（硬尾之前）</option>
                <option value="replace">replace（替换正文）</option>
              </select>
              <label className="inline-flex items-center gap-1">
                <input type="checkbox" checked={inherit} onChange={(e) => setInherit(e.target.checked)} />
                继承时也生效
              </label>
              <span className="ml-auto font-mono text-muted-foreground">{prompt.length} 字符</span>
            </div>
            <button
              type="button"
              className="justify-self-start rounded bg-foreground px-2 py-1 font-medium text-[10px] text-background disabled:opacity-50"
              disabled={busy}
              onClick={() => void savePrompt()}
            >
              保存提示词
            </button>
          </div>
        </details>
      )}

      <div className="flex flex-wrap gap-1">
        {links.map((l) => (
          <Link
            key={l.href}
            href={l.href}
            className="rounded border px-1.5 py-0.5 text-[10px] text-muted-foreground hover:bg-muted/60 hover:text-foreground"
          >
            {l.label} ↗
          </Link>
        ))}
      </div>
    </div>
  );
}

function ChainDetail({
  chain,
  path,
  backRef,
  onBack,
  onClose,
  onDrill,
  onCopyStatic,
  onCopyText,
  onReload,
  narrow,
}: {
  chain: AgentPromptChainData;
  path: PathEntry[];
  backRef: React.RefObject<HTMLButtonElement | null>;
  onBack: () => void;
  onClose: () => void;
  onDrill: (target: Target) => void;
  onCopyStatic: () => void;
  onCopyText: (text: string, what: string) => void;
  onReload: () => void;
  narrow: boolean;
}) {
  const entry = path[path.length - 1];
  if (!entry) return null;
  const target = entry.target;
  const backLabel = path.length > 1 ? `返回 ${path[path.length - 2].title}` : "返回 全局预览";
  const codeSegs = chain.segments.filter((s) => s.kind === "code_tail");
  const runtimeSegs = chain.segments.filter((s) => !s.available && s.kind !== "deferred_tools");
  const selectedMCP = target.kind === "mcp" ? chain.mcp.find((m) => m.id === target.id) : undefined;

  return (
    <aside
      className={cn(
        "z-20 flex max-w-full flex-col border-l bg-card shadow-2xl",
        narrow ? "absolute inset-y-0 right-0 w-full" : "relative w-[360px] shrink-0",
      )}
      aria-label="装配链只读详情"
    >
      <div className="border-b p-3">
        <div className="flex items-center gap-2">
          <button
            ref={backRef}
            type="button"
            className="inline-flex items-center gap-1 rounded border px-2 py-1 text-xs hover:bg-muted/50"
            onClick={onBack}
          >
            <ArrowLeftIcon className="size-3.5" /> {backLabel}
          </button>
          <Button size="icon" variant="ghost" className="ml-auto size-6" onClick={onClose} aria-label="关闭详情">
            <XIcon className="size-3.5" />
          </Button>
        </div>
        <div className="mt-2 font-mono text-[9px] text-muted-foreground uppercase tracking-[0.12em]">
          只读详情 · 第 {path.length} 层
        </div>
        <h3 className="truncate text-sm font-medium" title={entry.title}>{entry.title}</h3>
      </div>
      <div className="min-h-0 flex-1 overflow-y-auto p-3">
        <FocusStages chain={chain} target={target} onDrill={onDrill} />
        <ConfigActions chain={chain} target={target} onReload={onReload} />
        {target.kind === "segment" && (
          <SegmentDetail
            seg={chain.segments.find((s) => s.key === target.key)}
            onCopyText={onCopyText}
          />
        )}
        {target.kind === "code" && (
          <CodeDetail
            chain={chain}
            segs={codeSegs}
            onDrill={(s) => onDrill({ kind: "segment", key: s.key })}
          />
        )}
        {target.kind === "runtime" && (
          <RuntimeDetail
            segs={runtimeSegs}
            onDrill={(s) => onDrill({ kind: "segment", key: s.key })}
          />
        )}
        {target.kind === "deferred" && (
          <DeferredDetail
            chain={chain}
            onDrill={(s) => onDrill({ kind: "segment", key: s.key })}
          />
        )}
        {target.kind === "llm" && (
          <LLMDetail chain={chain} onCopyText={onCopyText} />
        )}
        {target.kind === "skills" && <SkillsDetail chain={chain} />}
        {target.kind === "tools" && <ToolsDetail chain={chain} />}
        {target.kind === "mcp" && <MCPDetail mcp={selectedMCP} />}
        {target.kind === "model" && (
          <ModelDetail chain={chain} onCopyStatic={onCopyStatic} />
        )}
        {target.kind === "excluded" && <ExcludedDetail chain={chain} />}
      </div>
    </aside>
  );
}

function SegmentDetail({
  seg,
  onCopyText,
}: {
  seg?: PromptChainSegment;
  onCopyText: (text: string, what: string) => void;
}) {
  if (!seg) return <p className="text-muted-foreground text-xs">（段不存在）</p>;
  const text = seg.text.replace(/^[\r\n]+/, "");
  // A configured-but-inactive profile prompt still carries the user's text so
  // this detail can show what was written; it stays marked 未生效.
  const showText = seg.available && text !== "" && (seg.present || seg.kind === "profile_prompt");
  const colorKey: ColorKey =
    seg.kind === "profile_prompt"
      ? "profile"
      : seg.kind === "body"
        ? "body"
        : seg.kind === "skill_index"
          ? "skill"
          : seg.kind === "code_tail"
            ? "code"
            : seg.kind === "host"
              ? "host"
              : "runtime";
  const lead =
    seg.kind === "profile_prompt"
      ? "这是用户在 LLM 配置里自写的纯文本，不是平台的固定字段。平台不解析变量，也没有优先级概念；是否生效只看绑定与注入模式。"
      : seg.kind === "body"
        ? seg.present
          ? `提示词正文；来源：${sourceLabel(seg)}，可在 Agent 设置中编辑。`
          : "已被独立提示词替换；技能索引与代码硬尾巴仍然保留。"
        : seg.kind === "skill_index"
          ? "宿主从已绑定 Skill 生成的速查块；只列名称与描述，正文按需加载。"
          : seg.kind === "host"
            ? "宿主按部署环境注入的本机工具清单；配置层不覆盖。"
            : seg.kind === "code_tail"
              ? "代码常量，UI 不可改；配置层不能越过硬边界。"
              : "只读投影；运行时段不展开正文。";
  return (
    <div className="grid gap-2">
      <div className="flex flex-wrap gap-1">
        <Badge variant="outline" className={cn("px-1.5 py-0 text-[10px]", COLOR[colorKey].html)}>
          {sourceLabel(seg)}
        </Badge>
        <Badge variant={seg.editable ? "secondary" : "outline"} className="px-1.5 py-0 text-[10px]">
          {seg.editable ? "可编辑" : "只读"}
        </Badge>
        {!seg.available && <Badge variant="secondary" className="px-1.5 py-0 text-[10px]">运行时</Badge>}
        {!seg.present && <Badge variant="outline" className="px-1.5 py-0 text-[10px]">未生效</Badge>}
      </div>
      <div className="grid grid-cols-2 gap-1.5">
        <Meta k="来源" v={sourceLabel(seg)} />
        <Meta k="字符" v={seg.chars ? fmt(seg.chars) : "0"} />
        <Meta k="顺序" v={`${seg.order}`} />
        <Meta k="状态" v={seg.present ? "参与装配" : "未生效"} />
      </div>
      <p className="text-muted-foreground text-xs leading-relaxed">{lead}</p>
      {seg.gate && <p className="text-muted-foreground text-xs">门控：{seg.gate}</p>}
      {!seg.present && seg.reason && <p className="text-muted-foreground text-xs">{seg.reason}</p>}
      {!seg.present && seg.kind === "profile_prompt" && text !== "" && (
        <p className="text-muted-foreground text-[10px]">以下为配置中的原文，本次装配未注入。</p>
      )}
      <p className="rounded-md border bg-muted/20 p-2 text-[10px] text-muted-foreground">
        只读投影：不连接 MCP、不含凭据；运行时段只显示注入位置与条件，不展开正文。
      </p>
      {showText && (
        <>
          <div className="flex items-center gap-2">
            <Button
              size="sm"
              variant="outline"
              onClick={() => onCopyText(text, seg.present ? "这段提示词" : "配置原文")}
            >
              <CopyIcon className="size-3.5" /> {seg.present ? "复制这段" : "复制原文"}
            </Button>
            <span className="text-muted-foreground text-[10px]">{fmt(seg.chars)} 字符</span>
          </div>
          <pre className="max-h-64 overflow-auto whitespace-pre-wrap break-words rounded-md bg-muted/40 p-2 font-mono text-[11px]">
            {text}
          </pre>
        </>
      )}
    </div>
  );
}

function CodeDetail({
  chain,
  segs,
  onDrill,
}: {
  chain: AgentPromptChainData;
  segs: PromptChainSegment[];
  onDrill: (seg: PromptChainSegment) => void;
}) {
  return (
    <div className="grid gap-2">
      <p className="text-muted-foreground text-xs leading-relaxed">
        代码常量拼接，UI 不可修改；独立提示词不能替换它们。点击任一段查看正文。
      </p>
      <div className="grid gap-1">
        {segs.map((seg) => (
          <button
            key={seg.key}
            type="button"
            className="flex items-center gap-2 rounded-md border px-2 py-1.5 text-left text-xs hover:bg-accent"
            onClick={() => onDrill(seg)}
          >
            <span className="size-1.5 rounded-full bg-amber-500" />
            <span className="min-w-0 flex-1 truncate">{seg.title}</span>
            <span className="font-mono text-[10px] text-muted-foreground">
              {seg.present ? `${fmt(seg.chars)}c` : "未生效"}
            </span>
          </button>
        ))}
      </div>
      {chain.agent_kind === "custom" && (
        <p className="text-muted-foreground text-xs">自定义 Agent 的代码硬尾巴可能更短；以列表为准。</p>
      )}
    </div>
  );
}

function RuntimeDetail({
  segs,
  onDrill,
}: {
  segs: PromptChainSegment[];
  onDrill: (seg: PromptChainSegment) => void;
}) {
  return (
    <div className="grid gap-2">
      <p className="text-muted-foreground text-xs leading-relaxed">
        运行时按任务当时事实注入；静态视图只显示位置与门控，不展开正文。点击任一段查看条件。
      </p>
      <div className="grid gap-1">
        {segs.map((seg) => (
          <button
            key={seg.key}
            type="button"
            className="flex items-center gap-2 rounded-md border px-2 py-1.5 text-left text-xs hover:bg-accent"
            onClick={() => onDrill(seg)}
          >
            <span className="size-1.5 rounded-full bg-muted-foreground" />
            <span className="min-w-0 flex-1 truncate">{seg.title}</span>
            <span className="max-w-[45%] truncate font-mono text-[10px] text-muted-foreground">{seg.gate || "按态势"}</span>
          </button>
        ))}
      </div>
    </div>
  );
}

function DeferredDetail({
  chain,
  onDrill,
}: {
  chain: AgentPromptChainData;
  onDrill: (seg: PromptChainSegment) => void;
}) {
  const seg = chain.segments.find((s) => s.kind === "deferred_tools");
  return (
    <div className="grid gap-2">
      <p className="text-muted-foreground text-xs leading-relaxed">
        延迟工具的 schema 作为独立的第二 system 段随请求下发；静态视图不连接 MCP。
      </p>
      {chain.tools.deferred_names.length > 0 && (
        <div>
          <p className="mb-1 font-medium text-xs">延迟工具（{chain.tools.deferred_names.length}）</p>
          <div className="flex flex-wrap gap-1">
            {chain.tools.deferred_names.map((name) => (
              <Badge key={name} variant="outline" className="px-1.5 py-0 font-mono text-[10px]">{name}</Badge>
            ))}
          </div>
        </div>
      )}
      {seg && (
        <button
          type="button"
          className="rounded-md border px-2 py-1.5 text-left text-xs hover:bg-accent"
          onClick={() => onDrill(seg)}
        >
          <span className="block truncate">{seg.title}</span>
          <span className="mt-0.5 block text-muted-foreground text-[10px]">{seg.gate || seg.reason || "条件注入"}</span>
        </button>
      )}
    </div>
  );
}

function LLMDetail({
  chain,
  onCopyText,
}: {
  chain: AgentPromptChainData;
  onCopyText: (text: string, what: string) => void;
}) {
  const llm = chain.llm;
  const profile = chain.segments.find((s) => s.kind === "profile_prompt");
  const profileText = (profile?.text ?? "").replace(/^[\r\n]+|[\r\n]+$/g, "");
  const proof = `${chain.agent_key} → #${llm.profile_id ?? "?"} · ${llm.source_label} · ${modeLabel(llm.prompt_mode)} · inherit ${llm.prompt_apply_inherited ? "on" : "off"} · ${fmt(llm.prompt_chars)}c`;
  return (
    <div className="grid gap-2">
      <div className="grid grid-cols-2 gap-1.5">
        <Meta k="配置" v={`${llm.name || "未解析"}${llm.profile_id ? ` · #${llm.profile_id}` : ""}`} />
        <Meta k="模型" v={`${llm.format} · ${llm.model || "—"}`} />
        <Meta k="来源" v={llm.source_label} />
        <Meta k="独立提示词" v={`${modeLabel(llm.prompt_mode)} · ${llm.prompt_active ? "生效" : "未生效"}`} />
      </div>
      <p className="text-muted-foreground text-xs leading-relaxed">
        显式绑定始终生效；任务链与全局激活只有在该配置勾选“继承时也生效”时才携带独立提示词。
      </p>
      {profile && !profile.present && profile.reason && (
        <p className="text-muted-foreground text-xs">{profile.reason}</p>
      )}
      <div className="flex items-center gap-2">
        <Button size="sm" variant="outline" onClick={() => onCopyText(proof, "解析证明")}>
          <CopyIcon className="size-3.5" /> 复制证明串
        </Button>
        <code className="min-w-0 flex-1 truncate rounded border bg-muted/30 px-2 py-1 font-mono text-[10px] text-muted-foreground" title={proof}>
          {proof}
        </code>
      </div>
      {profileText && (
        <>
          <p className="text-muted-foreground text-[10px]">
            {profile?.present ? "已进入装配的独立提示词原文" : "配置中的原文，本次装配未注入"}
          </p>
          <pre className="max-h-64 overflow-auto whitespace-pre-wrap break-words rounded-md bg-muted/40 p-2 font-mono text-[11px]">
            {profileText}
          </pre>
        </>
      )}
    </div>
  );
}

function SkillsDetail({ chain }: { chain: AgentPromptChainData }) {
  return (
    <div className="grid gap-1">
      <p className="text-muted-foreground text-xs">宿主绑定；只有进入索引的 Skill 才会出现在速查块。</p>
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
  );
}

function ToolsDetail({ chain }: { chain: AgentPromptChainData }) {
  return (
    <div className="grid gap-2">
      <p className="text-muted-foreground text-xs leading-relaxed">
        工具 schema 不进入 system 文本，随请求单独下发；延迟工具走第二 system 段。
      </p>
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
  );
}

function MCPDetail({ mcp }: { mcp?: PromptChainMCP }) {
  if (!mcp) return <p className="text-muted-foreground text-xs">（MCP 不存在）</p>;
  return (
    <div className="grid gap-2">
      <div className="grid grid-cols-2 gap-1.5">
        <Meta k="transport" v={mcp.transport} />
        <Meta k="health" v={healthLabel(mcp.health)} />
        <Meta k="tools" v={`${mcp.tool_count} 个`} />
        <Meta k="cache" v={mcp.cache_stale ? "可能过期" : "新鲜"} />
      </div>
      <p className="text-muted-foreground text-xs leading-relaxed">
        模式 {mcp.mode}{mcp.skill ? ` · 随 Skill「${mcp.skill}」解锁` : ""}；只读缓存，不连接。
        {mcp.last_error ? ` 上次错误：${mcp.last_error}` : ""}
      </p>
      <ul className="grid gap-0.5">
        {mcp.tools.map((t) => (
          <li key={t.name} className="font-mono text-[11px]">
            {t.name}
            {t.description && <span className="ml-1 font-sans text-muted-foreground">— {t.description}</span>}
          </li>
        ))}
      </ul>
    </div>
  );
}

function ModelDetail({
  chain,
  onCopyStatic,
}: {
  chain: AgentPromptChainData;
  onCopyStatic: () => void;
}) {
  return (
    <div className="grid gap-2">
      <div className="grid grid-cols-2 gap-1.5">
        <Meta k="system" v={`${fmt(chain.totals.chars)} 字符`} />
        <Meta k="tokens" v={`约 ${fmt(chain.totals.est_tokens)}`} />
        <Meta k="tools" v={`${chain.tools.total} 个`} />
        <Meta k="第二 system" v="条件注入" />
      </div>
      {chain.warnings.length > 0 && (
        <ul className="list-disc pl-5 text-muted-foreground text-xs">
          {chain.warnings.map((w, i) => <li key={i}>{w}</li>)}
        </ul>
      )}
      <Button size="sm" variant="outline" onClick={onCopyStatic}>
        <CopyIcon className="size-3.5" /> 复制静态提示词
      </Button>
    </div>
  );
}

function ExcludedDetail({ chain }: { chain: AgentPromptChainData }) {
  return (
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