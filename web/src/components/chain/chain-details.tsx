"use client";

import * as React from "react";

import { CopyIcon } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { api } from "@/lib/api";
import type { ChainItem, Target } from "@/lib/chain-model";
import { modeLabel } from "@/lib/chain-model";
import { EFFORT_LEVELS, fromStore, toStore } from "@/lib/effort";
import type {
  AgentPromptChain as AgentPromptChainData,
  LLMProfile,
  PromptChainMCP,
  PromptChainSegment,
  PromptChainSkill,
  PromptChainTool,
} from "@/lib/types";
import { cn } from "@/lib/utils";

import { StatusWord } from "./chain-status";

const fmt = (n: number) => n.toLocaleString("en-US");
const copyCls = "text-xs focus-visible:ring-2 focus-visible:ring-foreground";

/** 键值：88px 的键列 + 弹性值列；键用 12px，值用 14px。 */
export function Kv({ rows }: { rows: [string, React.ReactNode][] }) {
  return (
    <dl className="mb-3.5 grid grid-cols-[88px_minmax(0,1fr)] gap-x-3 gap-y-1.5">
      {rows.map(([k, v], i) => (
        <React.Fragment key={`${k}-${i}`}>
          <dt className="text-muted-foreground text-xs leading-5">{k}</dt>
          <dd className="m-0 break-words text-sm leading-5">{v}</dd>
        </React.Fragment>
      ))}
    </dl>
  );
}

/** 分组详情共用的成员列表：成员状态与顺序都来自模型，不各自解释接口数据。 */
export function MemberList({
  item,
  onSelect,
}: {
  item: ChainItem;
  onSelect: (id: string) => void;
}) {
  const kids = item.children ?? [];
  if (kids.length === 0) return null;
  return (
    <div>
      <div className="mb-1.5 text-muted-foreground text-xs">成员 {kids.length} 项</div>
      <div className="grid gap-0.5">
        {kids.map((c) => (
          <button
            key={c.id}
            type="button"
            className="flex min-h-8 w-full items-center gap-2 rounded-md px-2 py-1 text-left hover:bg-accent focus-visible:ring-2 focus-visible:ring-foreground"
            onClick={() => onSelect(c.id)}
          >
            <span className="min-w-0 flex-1 truncate text-sm" title={c.title}>
              {c.title}
            </span>
            <StatusWord state={c.state} word={c.stateWord} />
            {c.chars != null && (
              <span className="font-mono text-muted-foreground text-xs tabular-nums">{fmt(c.chars)}</span>
            )}
          </button>
        ))}
      </div>
    </div>
  );
}

function leadOf(seg: PromptChainSegment): string {
  switch (seg.kind) {
    case "profile_prompt":
      return "这是用户在 LLM 配置里自写的纯文本，不是平台的固定字段。平台不解析变量，也没有优先级概念；是否生效只看绑定与注入模式。";
    case "body":
      return seg.present
        ? `提示词正文；来源：${seg.source_label || seg.source}，可在 Agent 设置中编辑。`
        : "已被独立提示词替换；技能索引与代码硬尾巴仍然保留。";
    case "skill_index":
      return "宿主从已绑定 Skill 生成的速查块；只列名称与描述，正文按需加载。";
    case "host":
      return "宿主按部署环境注入的本机工具清单；配置层不覆盖。";
    case "code_tail":
      return "代码常量，UI 不可改；配置层不能越过硬边界。";
    default:
      return "只读投影；运行时段不展开正文。";
  }
}

export function SegmentDetail({
  seg,
  onCopyText,
}: {
  seg?: PromptChainSegment;
  onCopyText: (text: string, what: string) => void;
}) {
  if (!seg) return <p className="text-muted-foreground text-sm">（段不存在）</p>;
  const text = seg.text.replace(/^[\r\n]+/, "");
  // 配置了但未生效的独立提示词仍带原文：详情要能看出用户写了什么，同时如实标未生效。
  const showText = seg.available && text !== "" && (seg.present || seg.kind === "profile_prompt");
  return (
    <div className="grid gap-3">
      <p className="text-muted-foreground text-xs leading-relaxed">{leadOf(seg)}</p>
      <Kv
        rows={[
          ["来源", seg.source_label || seg.source],
          ["字符", seg.chars ? fmt(seg.chars) : "0"],
          ["状态", seg.present ? "参与装配" : "未生效"],
        ]}
      />
      {seg.gate && <p className="text-muted-foreground text-xs">门控：{seg.gate}</p>}
      {!seg.present && seg.reason && <p className="text-muted-foreground text-xs">{seg.reason}</p>}
      {!seg.present && seg.kind === "profile_prompt" && text !== "" && (
        <p className="text-muted-foreground text-xs">以下为配置中的原文，本次装配未注入。</p>
      )}
      {showText && (
        <>
          <Button
            size="sm"
            variant="outline"
            className={cn("justify-self-start", copyCls)}
            onClick={() => onCopyText(text, seg.present ? "这段提示词" : "配置原文")}
          >
            <CopyIcon className="size-3.5" /> {seg.present ? "复制这段" : "复制原文"}
          </Button>
          <pre className="max-h-80 overflow-auto whitespace-pre-wrap break-words rounded-md border bg-muted/40 p-3 font-mono text-xs leading-relaxed">
            {text}
          </pre>
        </>
      )}
      <p className="rounded-md border bg-muted/20 p-3 text-muted-foreground text-xs leading-relaxed">
        只读投影：不连接 MCP、不含凭据；运行时段只显示注入位置与条件，不展开正文。
      </p>
    </div>
  );
}

export function DeferredDetail({
  chain,
  onSelect,
}: {
  chain: AgentPromptChainData;
  onSelect: (id: string) => void;
}) {
  const seg = chain.segments.find((s) => s.kind === "deferred_tools");
  return (
    <div className="grid gap-3">
      <p className="text-muted-foreground text-xs leading-relaxed">
        延迟工具的 schema 作为独立的第二 system 段随请求下发；静态视图不连接 MCP。
      </p>
      {chain.tools.deferred_names.length > 0 && (
        <div>
          <div className="mb-1.5 text-muted-foreground text-xs">延迟工具（{chain.tools.deferred_names.length}）</div>
          <div className="flex flex-wrap gap-1">
            {chain.tools.deferred_names.map((name) => (
              <Badge key={name} variant="outline" className="px-1.5 py-0 font-mono text-xs">
                {name}
              </Badge>
            ))}
          </div>
        </div>
      )}
      {seg && (
        <button
          type="button"
          className="rounded-md border px-2 py-1.5 text-left hover:bg-accent focus-visible:ring-2 focus-visible:ring-foreground"
          onClick={() => onSelect(`seg:${seg.key}`)}
        >
          <span className="block truncate text-sm">{seg.title}</span>
          <span className="mt-0.5 block text-muted-foreground text-xs">{seg.gate || seg.reason || "条件注入"}</span>
        </button>
      )}
    </div>
  );
}

export function LLMDetail({
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
  const effortSource =
    llm.effort_source === "override" ? "覆盖（本 Agent）" : llm.effort_source === "profile" ? "配置自身" : "未设置";
  return (
    <div className="grid gap-3">
      <Kv
        rows={[
          ["配置", `${llm.name || "未解析"}${llm.profile_id ? ` · #${llm.profile_id}` : ""}`],
          ["模型", <span className="font-mono text-xs">{`${llm.format} · ${llm.model || "—"}`}</span>],
          ["来源", llm.source_label],
          ["独立提示词", `${modeLabel(llm.prompt_mode)} · ${llm.prompt_active ? "生效" : "未生效"}`],
          ["思考强度", `${llm.effort || "未设置"}（${effortSource}）`],
        ]}
      />
      <p className="text-muted-foreground text-xs leading-relaxed">
        显式绑定始终生效；任务链与全局激活只有在该配置勾选「继承时也生效」时才携带独立提示词。
      </p>
      {profile && !profile.present && profile.reason && (
        <p className="text-muted-foreground text-xs">{profile.reason}</p>
      )}
      <div className="flex flex-wrap items-center gap-2">
        <Button size="sm" variant="outline" className={copyCls} onClick={() => onCopyText(proof, "解析证明")}>
          <CopyIcon className="size-3.5" /> 复制证明串
        </Button>
        <code
          className="min-w-0 flex-1 truncate rounded-md border bg-muted/30 px-2 py-1 font-mono text-muted-foreground text-xs"
          title={proof}
        >
          {proof}
        </code>
      </div>
      {profileText && (
        <>
          <p className="text-muted-foreground text-xs">
            {profile?.present ? "已进入装配的独立提示词原文" : "配置中的原文，本次装配未注入"}
          </p>
          <pre className="max-h-80 overflow-auto whitespace-pre-wrap break-words rounded-md border bg-muted/40 p-3 font-mono text-xs leading-relaxed">
            {profileText}
          </pre>
        </>
      )}
    </div>
  );
}

const SKILL_STATE_LABEL = { described: "有描述", name_only: "仅名称（预算用尽）", absent: "不在速查块" } as const;

export function SkillDetail({ skill }: { skill?: PromptChainSkill }) {
  if (!skill) return <p className="text-muted-foreground text-sm">（Skill 不存在）</p>;
  return (
    <div className="grid gap-3">
      <p className="text-muted-foreground text-xs leading-relaxed">
        Skill 是按需加载的能力包：速查块只列名称与描述，正文在模型需要时按名加载。
      </p>
      <Kv
        rows={[
          ["速查块", SKILL_STATE_LABEL[skill.index_state] ?? "不在速查块"],
          ["MCP 门控", skill.mcp_gated.length ? skill.mcp_gated.join("、") : "无"],
          ["绑定", skill.in_index ? "已绑定 · 进速查" : "已绑定 · 索引外"],
        ]}
      />
      {skill.description ? (
        <div>
          <div className="mb-1.5 text-muted-foreground text-xs">描述全文</div>
          <p className="text-sm leading-relaxed">{skill.description}</p>
        </div>
      ) : (
        <p className="text-muted-foreground text-xs">（无描述）</p>
      )}
    </div>
  );
}

export function ToolsDetail({ chain }: { chain: AgentPromptChainData }) {
  return (
    <div className="grid gap-3">
      <p className="text-muted-foreground text-xs leading-relaxed">
        工具 schema 不进入 system 文本，随请求单独下发；延迟工具走第二 system 段。
      </p>
      <ToolGroup name="内置" items={chain.tools.builtin} />
      <ToolGroup name="自定义" items={chain.tools.custom} />
      <ToolGroup name="流量" items={chain.tools.traffic} />
      <ToolGroup name="Shell" items={chain.tools.shell} />
      {chain.tools.deferred_names.length > 0 && (
        <div>
          <div className="mb-1.5 text-muted-foreground text-xs">
            延迟 schema（{chain.tools.deferred_names.length}）
          </div>
          <p className="font-mono text-muted-foreground text-xs leading-relaxed">
            {chain.tools.deferred_names.join("、")}
          </p>
        </div>
      )}
    </div>
  );
}

function ToolLine({ item }: { item: PromptChainTool }) {
  return (
    <li className="rounded-md border px-2 py-1.5">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-mono text-sm">{item.key}</span>
        {item.deferred && (
          <Badge variant="outline" className="px-1.5 py-0 text-xs">
            延迟 schema
          </Badge>
        )}
        {item.gate && (
          <Badge variant="outline" className="px-1.5 py-0 text-muted-foreground text-xs">
            {item.gate}
          </Badge>
        )}
      </div>
      {item.description && <p className="mt-0.5 line-clamp-2 text-muted-foreground text-xs">{item.description}</p>}
    </li>
  );
}

function ToolGroup({ name, items }: { name: string; items: PromptChainTool[] }) {
  if (items.length === 0) return null;
  return (
    <div>
      <div className="mb-1.5 text-muted-foreground text-xs">
        {name}（{items.length}）
      </div>
      <ul className="grid gap-1">
        {items.map((t) => (
          <ToolLine key={t.key} item={t} />
        ))}
      </ul>
    </div>
  );
}

export function MCPDetail({ mcp }: { mcp?: PromptChainMCP }) {
  if (!mcp) return <p className="text-muted-foreground text-sm">（MCP 不存在）</p>;
  const health = mcp.health === "ok" ? "健康" : mcp.health === "failed" ? "上次失败" : "未检查";
  return (
    <div className="grid gap-3">
      <Kv
        rows={[
          ["transport", mcp.transport],
          ["健康", health],
          ["工具", `${mcp.tool_count} 个`],
          ["缓存", mcp.cache_stale ? "可能过期" : "新鲜"],
        ]}
      />
      <p className="text-muted-foreground text-xs leading-relaxed">
        模式 {mcp.mode}
        {mcp.skill ? ` · 随 Skill「${mcp.skill}」解锁` : ""}；只读缓存，不连接。
        {mcp.last_error ? ` 上次错误：${mcp.last_error}` : ""}
      </p>
      <ul className="grid gap-0.5">
        {mcp.tools.map((t) => (
          <li key={t.name} className="font-mono text-xs leading-relaxed">
            {t.name}
            {t.description && <span className="ml-1 font-sans text-muted-foreground">— {t.description}</span>}
          </li>
        ))}
      </ul>
    </div>
  );
}

/** 模型请求的装配结果预览：按拼接顺序列出每段开头，点击定位到该段。 */
export function ModelDetail({
  chain,
  items,
  onSelect,
  onCopyStatic,
}: {
  chain: AgentPromptChainData;
  items: ChainItem[];
  onSelect: (id: string) => void;
  onCopyStatic: () => void;
}) {
  const ordered = items.filter((i) => i.group === "system" && i.order != null);
  const previewOf = (it: ChainItem): string => {
    const t = it.target;
    if (t.kind === "segment") {
      const seg = chain.segments.find((s) => s.key === t.key);
      if (!seg || !seg.available) return "〔运行时才有，未采集〕";
      const line = seg.text.replace(/^\s+/, "").split("\n")[0]?.trim();
      return line || `（${fmt(seg.chars)} 字符）`;
    }
    if (t.kind === "code") {
      const first = chain.segments
        .filter((s) => s.kind === "code_tail" && s.present && s.available)
        .map((s) => s.text.replace(/^\s+/, "").split("\n")[0]?.trim())
        .find(Boolean);
      return first || `〔代码常量 · ${fmt(it.chars ?? 0)} 字符〕`;
    }
    if (t.kind === "runtime") return "〔运行时才有，未采集〕";
    return it.note;
  };
  return (
    <div className="grid gap-3">
      <Kv
        rows={[
          ["system", `${fmt(chain.totals.chars)} 字符`],
          ["tokens", `约 ${fmt(chain.totals.est_tokens)}`],
          ["tools", `${chain.tools.total} 个`],
          ["第二 system", "条件注入"],
        ]}
      />
      <div>
        <div className="mb-1.5 text-muted-foreground text-xs">装配结果预览 · 点击定位到该段</div>
        <div className="overflow-hidden rounded-md border">
          {ordered.map((it) => (
            <button
              key={it.id}
              type="button"
              className="grid w-full grid-cols-[104px_minmax(0,1fr)] items-start border-t text-left first:border-t-0 hover:bg-accent focus-visible:ring-2 focus-visible:ring-foreground"
              onClick={() => onSelect(it.id)}
            >
              <span className="h-full border-r px-2 py-1.5 text-muted-foreground text-xs">
                {it.order} {it.title}
              </span>
              <span className="min-w-0 truncate px-2.5 py-1.5 font-mono text-xs">{previewOf(it)}</span>
            </button>
          ))}
        </div>
      </div>
      <Button size="sm" variant="outline" className={cn("justify-self-start", copyCls)} onClick={onCopyStatic}>
        <CopyIcon className="size-3.5" /> 复制静态提示词
      </Button>
      {chain.warnings.length > 0 && (
        <ul className="list-disc pl-5 text-muted-foreground text-xs">
          {chain.warnings.map((w, i) => (
            <li key={i}>{w}</li>
          ))}
        </ul>
      )}
    </div>
  );
}

export function ExcludedToolDetail({ tool }: { tool?: { key: string; reason: string } }) {
  if (!tool) return <p className="text-muted-foreground text-sm">（工具不存在）</p>;
  return (
    <Kv
      rows={[
        ["工具", <span className="font-mono text-xs">{tool.key}</span>],
        ["状态", "未进入本次请求"],
        ["原因", tool.reason],
      ]}
    />
  );
}

export function ConfigActions({
  chain,
  target,
  onReload,
  onDirtyChange,
}: {
  chain: AgentPromptChainData;
  target: Target;
  onReload: () => void;
  onDirtyChange: (dirty: boolean) => void;
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

  const segKey = target.kind === "segment" ? target.key : "";
  const surface = chain.segments.find((s) => s.key === segKey);
  const showPromptSurface =
    target.kind === "llm" || (surface != null && (surface.kind === "profile_prompt" || surface.kind === "body"));
  // 切换对象时重置草稿；否则在 LLM 配置与独立提示词之间切换会看到上一份未保存文本。
  const targetKey = target.kind === "segment" ? `${target.kind}:${target.key}` : target.kind;

  React.useEffect(() => {
    if (!profile) return;
    setPrompt(profile.prompt ?? "");
    setMode(profile.prompt_mode ?? "");
    setInherit(!!profile.prompt_apply_inherited);
    onDirtyChange(false);
  }, [profile, targetKey, onDirtyChange]);

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
    // 服务端规则：文本非空而 mode 为空时会强制回落到 prepend。前端先拦住，
    // 避免用户以为「停用」却变成「正文最前注入」。
    if (!mode && prompt.trim()) {
      toast.error("「不注入」需要先清空提示词；非空文本请选择 prepend / append / replace");
      return;
    }
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
        retry: profile.retry,
        preset_id: profile.preset_id,
        prompt,
        prompt_mode: mode,
        prompt_apply_inherited: inherit,
      });
      onDirtyChange(false);
      toast.success("已保存配置提示词；运行中的会话沿用原 system prompt");
      onReload();
    } catch (e) {
      toast.error((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  if (!pid) {
    return (
      <p className="rounded-md border bg-muted/20 p-3 text-muted-foreground text-xs">
        未解析到 LLM 配置，没有可在这里修改的项。
      </p>
    );
  }

  return (
    <div className="mb-3.5 grid gap-3 rounded-md border p-3">
      <div className="text-sm">可配置 · 下一次模型调用生效</div>
      {chain.llm.source === "preview_override" && profile && (
        <p className="-mt-2 text-muted-foreground text-xs">正在修改预览中的配置：{profile.name}</p>
      )}
      {showPromptSurface && (
        <label className="grid gap-1 text-muted-foreground text-xs">
          思考强度覆盖（{chain.agent_key} × 本配置）
          <select
            className="h-7 max-w-full rounded-md border bg-background px-1.5 text-foreground text-xs focus-visible:ring-2 focus-visible:ring-foreground"
            value={fromStore(chain.llm.effort_source === "override" ? (chain.llm.effort ?? "") : "")}
            disabled={busy}
            onChange={(e) => void setEffort(toStore(e.target.value))}
          >
            {EFFORT_LEVELS.map((o) => (
              <option key={o.value} value={o.value}>
                {o.label}
              </option>
            ))}
          </select>
        </label>
      )}
      {showPromptSurface && (
        <p className="text-muted-foreground text-xs">
          当前生效：{chain.llm.effort || "未设置"}（
          {chain.llm.effort_source === "override"
            ? "overrides 覆盖"
            : chain.llm.effort_source === "profile"
              ? "配置自身"
              : "未设置"}
          ）
        </p>
      )}
      {showPromptSurface && profile && (
        <details className="rounded-md border bg-background/60 p-2" open={surface?.kind === "profile_prompt"}>
          <summary className="cursor-pointer text-muted-foreground text-xs">
            独立提示词（{profile.name} #{profile.id}）
            {profile.prompt_apply_inherited ? " · 继承时也生效" : ""}
          </summary>
          <div className="mt-2 grid gap-2">
            <textarea
              className="min-h-24 w-full resize-y rounded-md border bg-background px-2 py-1.5 font-mono text-xs leading-relaxed focus-visible:ring-2 focus-visible:ring-foreground"
              value={prompt}
              placeholder="纯文本，不做模板变量渲染"
              onChange={(e) => {
                setPrompt(e.target.value);
                onDirtyChange(true);
              }}
            />
            <div className="flex flex-wrap items-center gap-3 text-muted-foreground text-xs">
              <select
                className="h-7 rounded-md border bg-background px-1 text-foreground text-xs focus-visible:ring-2 focus-visible:ring-foreground"
                value={mode}
                onChange={(e) => {
                  setMode(e.target.value);
                  onDirtyChange(true);
                }}
              >
                <option value="" disabled={prompt.trim().length > 0}>
                  不注入{prompt.trim().length > 0 ? "（需先清空正文）" : ""}
                </option>
                <option value="prepend">prepend（正文最前）</option>
                <option value="append">append（硬尾之前）</option>
                <option value="replace">replace（替换正文）</option>
              </select>
              <label className="inline-flex min-h-6 items-center gap-1.5">
                <input
                  type="checkbox"
                  className="size-4 accent-foreground"
                  checked={inherit}
                  onChange={(e) => {
                    setInherit(e.target.checked);
                    onDirtyChange(true);
                  }}
                />
                继承时也生效
              </label>
              <span className="ml-auto font-mono tabular-nums">
                {Array.from(prompt).length}
                {chain.prompt_max_runes > 0 ? ` / ${chain.prompt_max_runes}` : ""} 字符
              </span>
            </div>
            <button
              type="button"
              className="justify-self-start rounded-md bg-foreground px-2.5 py-1 font-medium text-background text-xs disabled:opacity-50 focus-visible:ring-2 focus-visible:ring-foreground"
              disabled={busy}
              onClick={() => void savePrompt()}
            >
              保存提示词
            </button>
          </div>
        </details>
      )}
    </div>
  );
}
