// 装配链模型：把接口数据变成一组稳定对象。
//
// 三条规则（见方案 §4.2）：
//   1. 每个事物只有一个对象。同一个 Skill / 同一台 MCP 不会出现在两个分组里。
//   2. 对象之间只有一种关系 feeds（它喂给谁）。连线、相关高亮、检视器的「相关」
//      都从这一个字段推出。
//   3. 顺序由模型决定。清单、关系图、键盘、读屏都按同一个顺序。
//
// 本文件是纯函数，不依赖 React 与 DOM；node --experimental-strip-types 可直接测试。

import type { AgentPromptChain, PromptChainMCP, PromptChainSegment, PromptChainSkill } from "./types";

export type ItemState = "active" | "inactive" | "runtime" | "attention" | "error";
export type ItemGroup = "source" | "system" | "sidecar" | "result" | "excluded";

// 目标种类。前 10 种来自旧页面，后 3 种是本次新增（单个 Skill、MCP 组、单个被排除工具）。
export type Target =
  | { kind: "segment"; key: string }
  | { kind: "code" }
  | { kind: "runtime" }
  | { kind: "deferred" }
  | { kind: "llm" }
  | { kind: "skills" }
  | { kind: "skill"; name: string }
  | { kind: "tools" }
  | { kind: "mcp"; id: number }
  | { kind: "mcp-group" }
  | { kind: "model" }
  | { kind: "excluded" }
  | { kind: "excluded-tool"; key: string };

export interface ChainItem {
  id: string;
  group: ItemGroup;
  /** 模型自己编的序号；只有会进入本次 system 文本的对象（生效的、或运行时才有的）有。 */
  order?: number;
  /** 代码固定段之前的分隔。 */
  boundaryBefore?: boolean;
  title: string;
  note: string;
  state: ItemState;
  stateWord: string;
  chars: number | null;
  /** 它喂给哪个对象；唯一的关系字段。 */
  feeds?: string;
  target: Target;
  children?: ChainItem[];
}

export interface Attention {
  text: string;
  /** 能对应到对象时给出对象 id，界面据此提供「查看」。 */
  itemId?: string;
}

export interface ChainSummary {
  model: string;
  profile: string;
  sourceLabel: string;
  prompt: string;
  effort: string;
  effortSource: string;
}

export interface ChainModel {
  agentKey: string;
  agentKind: "builtin" | "custom";
  items: ChainItem[];
  /** 占比条的共用分母。 */
  maxChars: number;
  attention: Attention[];
  /** 后端的提示性说明，不计入判决。 */
  notes: string[];
  summary: ChainSummary;
}

export const GROUP_LABEL: Record<ItemGroup, string> = {
  source: "来源 · 配置与绑定",
  system: "system 提示词 · 自上而下拼接",
  sidecar: "随请求下发 · 不在上面这段 system 文本里",
  result: "结果",
  excluded: "未进入本次请求",
};

/** 描述的第一个语义单元：到第一个句号、分号或换行。不按字符切。 */
export function firstSemanticUnit(text: string): string {
  const t = (text || "").trim();
  if (!t) return "";
  const m = t.search(/[。；;\n]/);
  return m > 0 ? t.slice(0, m).trim() : t;
}

export function modeLabel(mode: string): string {
  switch (mode) {
    case "prepend":
      return "正文最前";
    case "append":
      return "正文之后";
    case "replace":
      return "替换正文";
    default:
      return "未配置";
  }
}

export function effortSourceLabel(source?: string): string {
  switch (source) {
    case "override":
      return "覆盖（本 Agent）";
    case "profile":
      return "配置自身";
    default:
      return "未设置";
  }
}

function healthWord(m: PromptChainMCP): string {
  switch (m.health) {
    case "ok":
      return "健康";
    case "failed":
      return "上次失败";
    default:
      return "未检查";
  }
}

function healthState(m: PromptChainMCP): ItemState {
  return m.health === "ok" ? "active" : "attention";
}

const NOTE_PREFIXES = ["静态视图中的出口代理地址", "该 Agent 未绑定 LLM 配置", "故障转移兜底已开启", "流量捕获未开启"];

function attentionForWarning(text: string): Attention {
  if (/^MCP「.+」尚未检查/.test(text)) {
    // 已由 MCP 派生项计数，不重复。
    return { text: "" };
  }
  if (text.startsWith("该 LLM 配置未设置 API Key") || text.startsWith("预览指定的 LLM 配置不存在") || text.startsWith("Agent 绑定的 LLM 配置不存在") || text.startsWith("读取 LLM 配置")) {
    return { text, itemId: "llm" };
  }
  return { text };
}

export function buildChainModel(chain: AgentPromptChain): ChainModel {
  const seg = (kind: string) => chain.segments.find((s) => s.kind === kind);
  const profileSeg = seg("profile_prompt");
  const bodySeg = seg("body");
  const skillIndexSeg = seg("skill_index");
  const hostSeg = seg("host");
  const deferredSeg = seg("deferred_tools");
  const codeSegs = chain.segments.filter((s) => s.kind === "code_tail");
  const runtimeSegs = chain.segments.filter((s) => !s.available && s.kind !== "deferred_tools");
  const append = chain.llm.prompt_mode === "append";

  const items: ChainItem[] = [];

  // ── 来源 ────────────────────────────────────────────────────────────
  const sourceItems: ChainItem[] = [];
  if (chain.skills.length > 0) {
    const inIndex = chain.skills.filter((s) => s.index_state !== "absent").length;
    sourceItems.push({
      id: "skills",
      group: "source",
      title: "Skill 绑定",
      note: `${chain.skills.length} 个 · ${inIndex} 个进速查`,
      state: "active",
      stateWord: "已绑定",
      chars: null,
      feeds: skillIndexSeg ? "seg:skill_index" : undefined,
      target: { kind: "skills" },
      children: chain.skills.map(skillItem),
    });
  }
  sourceItems.push({
    id: "llm",
    group: "source",
    title: "LLM 配置",
    note: chain.llm.name ? `${chain.llm.name} · ${chain.llm.source_label}` : chain.llm.source_label,
    state: chain.llm.profile_id ? "active" : "inactive",
    stateWord: chain.llm.profile_id ? "已解析" : "未绑定",
    chars: null,
    feeds: profileSeg ? "seg:profile_prompt" : undefined,
    target: { kind: "llm" },
  });

  // ── system 提示词（顺序跟随注入模式）────────────────────────────────
  const system: ChainItem[] = [];
  const promptItem: ChainItem | null = profileSeg
    ? {
        id: "seg:profile_prompt",
        group: "system",
        title: "独立提示词",
        note: profileSeg.source_label,
        state: profileSeg.present ? "active" : "inactive",
        stateWord: profileSeg.present ? "生效" : "未生效",
        chars: profileSeg.chars,
        target: { kind: "segment", key: profileSeg.key },
      }
    : null;

  if (promptItem && !append) system.push(promptItem);
  if (bodySeg) {
    const replaced = !bodySeg.present && chain.llm.prompt_mode === "replace";
    system.push({
      id: "seg:body",
      group: "system",
      title: "提示词正文",
      note: bodySeg.source_label,
      state: bodySeg.present ? "active" : "inactive",
      stateWord: bodySeg.present ? "生效" : replaced ? "已被替换" : "未生效",
      chars: bodySeg.chars,
      target: { kind: "segment", key: bodySeg.key },
    });
  }
  if (skillIndexSeg) {
    system.push({
      id: "seg:skill_index",
      group: "system",
      title: "技能速查",
      note: "宿主注入 · 来自 Skill 绑定",
      state: skillIndexSeg.present ? "active" : "inactive",
      stateWord: skillIndexSeg.present ? "生效" : "未生效",
      chars: skillIndexSeg.chars,
      target: { kind: "segment", key: skillIndexSeg.key },
    });
  }
  if (promptItem && append) system.push(promptItem);
  if (codeSegs.length > 0) {
    const active = codeSegs.filter((s) => s.present && s.available);
    const inactive = codeSegs.length - active.length;
    system.push({
      id: "code",
      group: "system",
      boundaryBefore: true,
      title: "代码固定段",
      note: inactive > 0 ? `${active.length} 段生效 · ${inactive} 段未生效` : `${active.length} 段生效`,
      state: active.length > 0 ? "active" : "inactive",
      stateWord: active.length > 0 ? "生效" : "未生效",
      chars: active.reduce((n, s) => n + s.chars, 0),
      target: { kind: "code" },
      children: codeSegs.map((s) => segmentChild(s, "代码固定")),
    });
  }
  if (hostSeg) {
    system.push({
      id: "seg:host_tools",
      group: "system",
      title: "本机工具",
      note: "宿主注入",
      state: hostSeg.present ? "active" : "inactive",
      stateWord: hostSeg.present ? "生效" : "未生效",
      chars: hostSeg.chars,
      target: { kind: "segment", key: hostSeg.key },
    });
  }
  if (runtimeSegs.length > 0) {
    system.push({
      id: "runtime",
      group: "system",
      title: "运行时注入点",
      note: `${runtimeSegs.length} 处 · 任务运行时才有`,
      state: "runtime",
      stateWord: "未采集",
      chars: null,
      target: { kind: "runtime" },
      children: runtimeSegs.map((s) => ({
        id: `seg:${s.key}`,
        group: "system",
        title: s.title,
        note: s.gate || "按任务当时事实注入",
        state: "runtime",
        stateWord: "未采集",
        chars: null,
        target: { kind: "segment", key: s.key },
      })),
    });
  }

  // 序号：只编给会进入本次 system 文本的对象（生效的、或运行时才有的）。
  let order = 0;
  for (const it of system) {
    if (it.state === "active" || it.state === "runtime") it.order = ++order;
  }

  // 来源按它喂给的段在 system 里的先后排；喂给的对象不存在时排在后面。
  const posOf = (id?: string) => (id ? system.findIndex((s) => s.id === id) : -1);
  sourceItems.sort((a, b) => {
    const pa = posOf(a.feeds);
    const pb = posOf(b.feeds);
    if (pa < 0 && pb < 0) return 0;
    if (pa < 0) return 1;
    if (pb < 0) return -1;
    return pa - pb;
  });

  items.push(...sourceItems, ...system);

  // ── 随请求下发 ──────────────────────────────────────────────────────
  items.push({
    id: "tools",
    group: "sidecar",
    title: "工具 schema",
    note: `${chain.tools.total} 个 · 随请求单独下发`,
    state: "active",
    stateWord: "下发",
    chars: null,
    feeds: "model",
    target: { kind: "tools" },
  });
  const visibleMCP = chain.mcp.filter((m) => m.mode !== "excluded");
  if (visibleMCP.length > 0) {
    const bad = visibleMCP.filter((m) => m.health !== "ok").length;
    items.push({
      id: "mcp",
      group: "sidecar",
      title: "MCP 服务器",
      note: `${visibleMCP.length} 台进入本 Agent`,
      state: bad > 0 ? "attention" : "active",
      stateWord: bad > 0 ? `${bad} 台异常` : "健康",
      chars: null,
      feeds: "model",
      target: { kind: "mcp-group" },
      children: visibleMCP.map((m) => ({
        id: `mcp:${m.id}`,
        group: "sidecar",
        title: m.name,
        note: `${m.transport} · ${m.mode === "skill_gated" && m.skill ? `由 Skill ${m.skill} 解锁` : "全局可见"}`,
        state: healthState(m),
        stateWord: healthWord(m),
        chars: null,
        feeds: "model",
        target: { kind: "mcp", id: m.id },
      })),
    });
  }
  if (deferredSeg || chain.tools.deferred_names.length > 0) {
    items.push({
      id: "seg:deferred_tools",
      group: "sidecar",
      title: "延迟工具块",
      note: `第二段 system · ${chain.tools.deferred_names.length} 个延迟工具`,
      state: "runtime",
      stateWord: "按需",
      chars: null,
      feeds: "model",
      target: { kind: "deferred" },
    });
  }

  // ── 结果 ────────────────────────────────────────────────────────────
  const presentCount = chain.segments.filter((s) => s.present && s.available).length;
  items.push({
    id: "model",
    group: "result",
    title: "模型请求",
    note: `合计 ${presentCount} 段生效 · ≈${chain.totals.est_tokens.toLocaleString("en-US")} tok`,
    state: "active",
    stateWord: "",
    chars: chain.totals.chars,
    target: { kind: "model" },
  });

  // ── 未进入本次请求 ──────────────────────────────────────────────────
  const excludedMCP = chain.mcp.filter((m) => m.mode === "excluded");
  const excludedTools = chain.tools.excluded;
  if (excludedTools.length > 0 || excludedMCP.length > 0) {
    const parts: string[] = [];
    if (excludedTools.length > 0) parts.push(`工具 ${excludedTools.length}`);
    if (excludedMCP.length > 0) parts.push(`MCP ${excludedMCP.length}`);
    items.push({
      id: "excluded",
      group: "excluded",
      title: "已排除",
      note: parts.join(" · "),
      state: "inactive",
      stateWord: "未进入",
      chars: null,
      target: { kind: "excluded" },
      children: [
        ...excludedTools.map((t) => ({
          id: `xtool:${t.key}`,
          group: "excluded" as ItemGroup,
          title: t.key,
          note: `工具 · ${t.reason}`,
          state: "inactive" as ItemState,
          stateWord: "未进入",
          chars: null,
          target: { kind: "excluded-tool", key: t.key } as Target,
        })),
        ...excludedMCP.map((m) => ({
          id: `mcp:${m.id}`,
          group: "excluded" as ItemGroup,
          title: m.name,
          note: "MCP · 未对本 Agent 开放",
          state: "inactive" as ItemState,
          stateWord: "未进入",
          chars: null,
          target: { kind: "mcp", id: m.id } as Target,
        })),
      ],
    });
  }

  // ── 判决：只数异常，同一对象只算一条 ────────────────────────────────
  const attention: Attention[] = [];
  if (chain.llm.prompt_chars > 0 && !chain.llm.prompt_active) {
    attention.push({ text: "独立提示词已配置但未生效，本次装配未注入", itemId: "seg:profile_prompt" });
  }
  for (const m of visibleMCP) {
    if (m.health === "failed") {
      attention.push({
        text: `MCP「${m.name}」上次检查失败${m.cache_stale ? "，工具缓存可能已过期" : ""}`,
        itemId: `mcp:${m.id}`,
      });
    } else if (m.health === "never_checked") {
      attention.push({ text: `MCP「${m.name}」尚未检查：工具清单来自缓存，运行时可能为空`, itemId: `mcp:${m.id}` });
    }
  }
  const notes: string[] = [];
  for (const w of chain.warnings) {
    if (NOTE_PREFIXES.some((p) => w.startsWith(p))) {
      notes.push(w);
      continue;
    }
    const a = attentionForWarning(w);
    if (a.text) attention.push(a);
  }

  const maxChars = Math.max(0, ...items.filter((i) => i.chars != null).map((i) => i.chars as number));

  return {
    agentKey: chain.agent_key,
    agentKind: chain.agent_kind,
    items,
    maxChars,
    attention,
    notes,
    summary: {
      model: chain.llm.name ? `${chain.llm.format} · ${chain.llm.model}` : "未解析",
      profile: chain.llm.name || "未绑定",
      sourceLabel: chain.llm.source_label,
      prompt: `${modeLabel(chain.llm.prompt_mode)} · ${chain.llm.prompt_active ? "生效" : "未生效"} · ${chain.llm.prompt_chars} 字符`,
      effort: chain.llm.effort || "未设置",
      effortSource: effortSourceLabel(chain.llm.effort_source),
    },
  };
}

function skillItem(s: PromptChainSkill): ChainItem {
  let state: ItemState = "active";
  let word = "进速查";
  if (s.index_state === "name_only") {
    state = "inactive";
    word = "仅名称";
  } else if (s.index_state === "absent") {
    state = "inactive";
    word = "索引外";
  }
  const purpose = firstSemanticUnit(s.description);
  return {
    id: `skill:${s.name}`,
    group: "source",
    title: s.name,
    note: purpose || (s.mcp_gated.length > 0 ? `解锁 MCP ${s.mcp_gated.join("、")}` : "（无描述）"),
    state,
    stateWord: word,
    chars: null,
    target: { kind: "skill", name: s.name },
  };
}

function segmentChild(s: PromptChainSegment, note: string): ChainItem {
  return {
    id: `seg:${s.key}`,
    group: "system",
    title: s.title,
    note: s.present ? note : s.reason || note,
    state: s.present ? "active" : "inactive",
    stateWord: s.present ? "生效" : "未生效",
    chars: s.present ? s.chars : null,
    target: { kind: "segment", key: s.key },
  };
}
