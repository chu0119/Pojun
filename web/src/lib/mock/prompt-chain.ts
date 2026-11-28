// 装配链只读投影的 mock（NEXT_PUBLIC_MOCK=1 的 webui 演示用）。
// 形状与 server/prompt_chain.go 的输出逐字段对应：segments 的 kind/门控/来源、
// 工具分组、MCP 健康度、Skill 索引状态，便于在无后端时复现真实页面。
//
// 独立提示词按真实的两层三态解析：
//   配置默认（llm_profiles.prompt，受 inherit 开关约束）
//   (Agent × 配置) 覆盖（无行=继承；空正文=本 Agent 关闭；非空=覆盖，永远生效）
// 演示数据里 planner 显式绑定 #2，其余 Agent 未绑定、走全局 #1，与线上默认部署一致。

import type { AgentPromptChain, PromptChainSegment } from "../types";

// 演示模式下的可变状态：让「右侧配置面板」的保存/清除在 mock 里也看得到效果。
const effortOverrides = new Map<string, string>();
const profileDefaults = new Map<number, { prompt: string; mode: string; inherit: boolean }>();
// key = `${agentKey}:${profileId}`。与真实表一致：没有键 = 继承；值里的空正文 = 关闭。
const promptOverrides = new Map<string, { prompt: string; mode: string }>();

export function setMockEffortOverride(agentKey: string, profileId: number, effort: string) {
  // 空串 = 显式清除（保留一个哨兵，避免回落到演示默认的 “high”）。
  effortOverrides.set(`${agentKey}:${profileId}`, effort);
}

export function getMockEffortOverride(agentKey: string, profileId: number): string | undefined {
  return effortOverrides.get(`${agentKey}:${profileId}`);
}

export function setMockProfilePrompt(
  profileId: number,
  prompt: string,
  mode: string,
  inherit: boolean,
) {
  profileDefaults.set(profileId, { prompt, mode, inherit });
}

export function getMockProfilePrompt(profileId: number) {
  return profileDefaults.get(profileId);
}

export function setMockPromptOverride(agentKey: string, profileId: number, prompt: string, mode: string) {
  promptOverrides.set(`${agentKey}:${profileId}`, { prompt, mode });
}

export function deleteMockPromptOverride(agentKey: string, profileId: number) {
  promptOverrides.delete(`${agentKey}:${profileId}`);
}

export function listMockPromptOverrides(agentKey: string) {
  const out: { profile_id: number; prompt: string; prompt_mode: string }[] = [];
  for (const [key, value] of promptOverrides) {
    const [agent, pid] = key.split(":");
    if (agent === agentKey) out.push({ profile_id: Number(pid), prompt: value.prompt, prompt_mode: value.mode });
  }
  return out.sort((a, b) => a.profile_id - b.profile_id);
}

/** 全量反查（LLM 配置页的「被哪些 Agent 覆盖」）。 */
export function allMockPromptOverrides() {
  const out: { agent_key: string; profile_id: number; prompt: string; prompt_mode: string }[] = [];
  for (const [key, value] of promptOverrides) {
    const [agent, pid] = key.split(":");
    out.push({ agent_key: agent, profile_id: Number(pid), prompt: value.prompt, prompt_mode: value.mode });
  }
  return out.sort((a, b) => a.profile_id - b.profile_id || a.agent_key.localeCompare(b.agent_key));
}

const seg = (
  key: string,
  title: string,
  kind: string,
  over: Partial<PromptChainSegment> = {},
): PromptChainSegment => ({
  key,
  title,
  kind,
  source: kind === "code_tail" ? "code" : kind,
  source_label:
    kind === "body"
      ? "DB 模板"
      : kind === "profile_prompt"
        ? "LLM 配置"
        : kind === "skill_index"
          ? "宿主注入"
          : kind === "host"
            ? "宿主注入"
            : kind === "code_tail"
              ? "代码固定"
              : "运行时",
  editable: kind === "body" || kind === "profile_prompt",
  present: true,
  available: true,
  order: 1,
  chars: 0,
  text: "",
  ...over,
});

const MOCK_PROFILES: {
  id: number;
  name: string;
  model: string;
  format: string;
  effort: string;
  prompt: string;
  promptMode: string;
  inherit: boolean;
}[] = [
  {
    id: 1,
    name: "Claude Opus 4.8",
    model: "claude-opus-4-8",
    format: "anthropic",
    effort: "high",
    prompt: "优先用最小改动验证假设；每次工具调用后复述当前结论与下一步。",
    promptMode: "append",
    inherit: true,
  },
  {
    id: 2,
    name: "DeepSeek V4",
    model: "deepseek-v4-flash",
    format: "openai",
    effort: "",
    prompt: "",
    promptMode: "",
    inherit: false,
  },
];

// 演示绑定：planner 显式绑定 #2；其余 Agent 未绑定，运行时走全局 #1。
const MOCK_AGENT_BINDINGS: Record<string, number> = { planner: 2 };
const MOCK_ACTIVE_PROFILE = 1;

const profileDefault = (pid: number) => {
  const base = MOCK_PROFILES.find((p) => p.id === pid) ?? MOCK_PROFILES[0];
  return profileDefaults.get(pid) ?? { prompt: base.prompt, mode: base.promptMode, inherit: base.inherit };
};

export function mockPromptChain(agentKey: string, profileId?: string | number): AgentPromptChain {
  const preview = Number(profileId) || 0;
  const bound = preview ? 0 : (MOCK_AGENT_BINDINGS[agentKey] ?? 0);
  const pid = preview || bound || MOCK_ACTIVE_PROFILE;
  const prof = MOCK_PROFILES.find((p) => p.id === pid) ?? MOCK_PROFILES[0];
  const source = preview ? "preview_override" : bound ? "agent_binding" : "global_profile";
  const def = profileDefault(prof.id);
  const ov = promptOverrides.get(`${agentKey}:${prof.id}`);

  let scope: "agent_override" | "agent_disabled" | "profile_default" | "profile_default_inactive" | "none";
  let effective = { text: "", mode: "" };
  let promptActive = false;
  let reason = "";
  if (ov) {
    effective = { text: ov.prompt, mode: ov.mode };
    if (!ov.prompt.trim()) {
      scope = "agent_disabled";
      reason = "本 Agent 已关闭该配置的独立提示词（不注入）；可在本页恢复";
    } else {
      scope = "agent_override";
      promptActive = !!ov.mode;
      if (!promptActive) reason = "本 Agent 的覆盖模式无效，未注入";
    }
  } else {
    effective = { text: def.prompt, mode: def.mode };
    const applies = source !== "global_profile" || def.inherit;
    promptActive = applies && !!def.prompt.trim() && !!def.mode;
    if (promptActive) {
      scope = "profile_default";
    } else if (def.prompt.trim()) {
      scope = "profile_default_inactive";
      reason = !applies
        ? "全局激活配置设置了独立提示词，但未开启「继承时也生效」"
        : "独立提示词未生效";
    } else {
      scope = "none";
      reason = "该 LLM 配置未设置独立提示词";
    }
  }

  const codeTail = (n: number, present = true, gate = "") =>
    seg(`tail-${n}`, `硬尾巴 #${n}`, "code_tail", {
      present,
      gate: gate || undefined,
      chars: 240 + n * 30,
      source_label: "代码固定",
      reason: present ? undefined : "该工具集未启用",
    });

  const promptLabel =
    scope === "agent_override"
      ? "本 Agent 覆盖"
      : scope === "agent_disabled"
        ? "本 Agent 已关闭"
        : scope === "profile_default"
          ? preview
            ? "LLM 配置 · 预览指定"
            : bound
              ? "LLM 配置 · 显式绑定"
              : "LLM 配置 · 继承生效"
          : "LLM 配置 · 未生效";

  const segments: PromptChainSegment[] = [
    seg("body", "提示词正文", "body", {
      chars: 3260,
      text: "你是 {{agent}}，负责在授权范围内完成目标……（DB 模板正文，可在配置中编辑）",
    }),
    seg("profile_prompt", "独立提示词", "profile_prompt", {
      chars: Array.from(effective.text).length,
      text: effective.text,
      present: promptActive,
      source: "llm_profile",
      source_label: promptLabel,
      reason: promptActive ? undefined : reason,
    }),
    seg("skill_index", "技能速查", "skill_index", { chars: 1580 }),
    seg("host", "本机可用工具", "host", { chars: 640 }),
    codeTail(1),
    codeTail(2),
    codeTail(3),
    codeTail(4, false, "traffic 工具集未开启"),
    seg("deferred_tools", "延迟工具块", "deferred_tools", {
      chars: 0,
      present: false,
      available: false,
      source_label: "第二 system（按需）",
    }),
    seg("runtime-step", "运行时注入", "runtime", {
      chars: 0,
      present: false,
      available: false,
      source_label: "运行时",
      reason: "任务执行时生成",
    }),
  ].map((s, i) => ({ ...s, order: i + 1 }));

  // 与后端解析顺序一致：显式覆盖 > 配置自身档位 > 未设置。
  // 空串是「已删除覆盖」的哨兵，应回落到配置自身，而不是当成覆盖值。
  const override = getMockEffortOverride(agentKey, pid);
  const overrideEffort = override && override !== "" ? override : "";
  const effort = overrideEffort || prof.effort;
  const effortSource = overrideEffort ? "override" : prof.effort ? "profile" : "unset";
  // 与 server/prompt_chain.go 同口径：只累计 present 且 available 的段，token 估算 (chars+1)/2。
  const presentChars = segments
    .filter((s) => s.present && s.available)
    .reduce((n, s) => n + s.chars, 0);

  return {
    agent_key: agentKey,
    agent_kind: agentKey === "worker" || agentKey === "planner" ? "builtin" : "custom",
    llm: {
      profile_id: prof.id,
      name: prof.name,
      model: prof.model,
      format: prof.format,
      source,
      source_label: preview
        ? "预览指定配置"
        : bound
          ? "Agent 显式绑定"
          : def.inherit
            ? "全局激活配置 · 继承生效"
            : "全局激活配置 · 未生效",
      prompt_scope: scope,
      prompt_mode: effective.mode,
      prompt_chars: Array.from(effective.text).length,
      prompt_text: effective.text,
      prompt_apply_inherited: def.inherit,
      prompt_active: promptActive,
      default_prompt_mode: def.mode,
      default_prompt_chars: Array.from(def.prompt).length,
      effort,
      effort_source: effortSource,
    },
    segments,
    tools: {
      builtin: Array.from({ length: 12 }, (_, i) => ({
        key: `builtin_${i + 1}`,
        description: "内置工具",
        deferred: false,
      })),
      custom: Array.from({ length: 8 }, (_, i) => ({
        key: `custom_${i + 1}`,
        description: "自定义工具",
        deferred: false,
      })),
      traffic: Array.from({ length: 3 }, (_, i) => ({
        key: `traffic_${i + 1}`,
        description: "流量工具",
        deferred: false,
      })),
      shell: Array.from({ length: 4 }, (_, i) => ({
        key: `shell_${i + 1}`,
        description: "Shell 工具",
        deferred: false,
      })),
      deferred_names: ["browser_run", "http_request", "session_login", "proxy_rotate", "dns_lookup", "port_scan"],
      excluded: [
        { key: "kb_search", reason: "该 Agent 未绑定知识库" },
        { key: "browser_run", reason: "工具集未启用" },
      ],
      total: 27,
    },
    mcp: [
      {
        id: 1,
        name: "filesystem",
        transport: "stdio",
        enabled: true,
        mode: "global",
        skill: "",
        health: "ok",
        last_check_at: "2026-11-25T09:12:00Z",
        tool_count: 9,
        cache_stale: false,
        tools: Array.from({ length: 9 }, (_, i) => ({ name: `fs_${i + 1}`, description: "文件操作" })),
      },
      {
        id: 2,
        name: "playwright",
        transport: "http",
        enabled: true,
        mode: "skill_gated",
        skill: "web-recon",
        health: "failed",
        last_check_at: "2026-11-25T08:40:00Z",
        last_error: "connect timeout after 10s",
        tool_count: 14,
        cache_stale: true,
        tools: Array.from({ length: 6 }, (_, i) => ({ name: `pw_${i + 1}`, description: "浏览器操作" })),
      },
      {
        id: 3,
        name: "nuclei",
        transport: "stdio",
        enabled: false,
        mode: "excluded",
        skill: "",
        health: "never_checked",
        tool_count: 0,
        cache_stale: false,
        tools: [],
      },
    ],
    skills: [
      { name: "web-recon", description: "站点侦察", in_index: true, mcp_gated: ["playwright"], index_state: "described" },
      { name: "web-exploit", description: "Web 漏洞验证", in_index: true, mcp_gated: [], index_state: "described" },
      { name: "mini-program", description: "小程序逆向", in_index: true, mcp_gated: [], index_state: "described" },
      { name: "traffic-replay", description: "流量重放", in_index: false, mcp_gated: [], index_state: "absent" },
      { name: "report-writing", description: "报告撰写", in_index: true, mcp_gated: [], index_state: "name_only" },
    ],
    totals: {
      // 与 server/prompt_chain.go 同口径：只累计 present 段，token 估算 (chars+1)/2。
      segments: segments.length,
      chars: presentChars,
      est_tokens: Math.floor((presentChars + 1) / 2),
    },
    warnings: [
      // 与 server/prompt_chain.go 的告警原文一致；MCP 健康问题由前端按 health 派生，不在这里重复。
      "静态视图中的出口代理地址可能已脱敏或为运行时占位；实际出口以运行为准",
      "流量捕获未开启：traffic_* 工具不会出现在运行时",
    ],
    static_only: true,
    prompt_max_runes: 65535,
  };
}
