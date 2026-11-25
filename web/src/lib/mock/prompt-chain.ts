// 装配链只读投影的 mock（NEXT_PUBLIC_MOCK=1 的 webui 演示用）。
// 形状与 server/prompt_chain.go 的输出逐字段对应：segments 的 kind/门控/来源、
// 工具分组、MCP 健康度、Skill 索引状态，便于在无后端时复现真实页面。

import type { AgentPromptChain, PromptChainSegment } from "../types";

// 演示模式下的可变状态：让「右侧配置面板」的保存/清除在 mock 里也看得到效果。
const effortOverrides = new Map<string, string>();
const promptEdits = new Map<number, { prompt: string; mode: string; inherit: boolean }>();

export function setMockEffortOverride(agentKey: string, profileId: number, effort: string) {
  const key = `${agentKey}:${profileId}`;
  if (effort) effortOverrides.set(key, effort);
  else effortOverrides.delete(key);
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
  promptEdits.set(profileId, { prompt, mode, inherit });
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

export function mockPromptChain(agentKey: string): AgentPromptChain {
  const baseProfileId = 3;
  const codeTail = (n: number, present = true, gate = "") =>
    seg(`tail-${n}`, `硬尾巴 #${n}`, "code_tail", {
      present,
      gate: gate || undefined,
      chars: 240 + n * 30,
      source_label: "代码固定",
      reason: present ? undefined : "该工具集未启用",
    });

  const segments: PromptChainSegment[] = [
    seg("body", "提示词正文", "body", {
      chars: 3260,
      text: "你是 {{agent}}，负责在授权范围内完成目标……（DB 模板正文，可在配置中编辑）",
    }),
    seg("profile_prompt", "独立提示词", "profile_prompt", {
      chars: 412,
      text: "优先用最小改动验证假设；每次工具调用后复述当前结论与下一步。",
      source: "llm_profile",
      source_label: "LLM 配置 · append",
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

  const edit = promptEdits.get(baseProfileId);
  if (edit) {
    const seg = segments.find((x) => x.kind === "profile_prompt");
    if (seg) {
      seg.text = edit.prompt;
      seg.chars = edit.prompt.length;
      seg.present = edit.prompt.length > 0 && !!edit.mode;
      seg.source_label = edit.mode ? `LLM 配置 · ${edit.mode}` : "未配置";
    }
  }
  const override = getMockEffortOverride(agentKey, baseProfileId);

  return {
    agent_key: agentKey,
    agent_kind: agentKey === "worker" || agentKey === "planner" ? "builtin" : "custom",
    llm: {
      profile_id: 3,
      name: "主模型 · 长上下文",
      model: "gpt-5.2-codex",
      format: "openai-responses",
      source: "agent_binding",
      source_label: "显式绑定",
      prompt_mode: edit?.mode || "append",
      prompt_chars: edit ? edit.prompt.length : 412,
      prompt_apply_inherited: edit ? edit.inherit : false,
      prompt_active: edit ? !!edit.mode && edit.prompt.length > 0 : true,
      effort: override ?? "high",
      effort_source: override === undefined ? "override" : override ? "override" : "unset",
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
      { name: "web-recon", description: "站点侦察", in_index: true, mcp_gated: ["playwright"] },
      { name: "web-exploit", description: "Web 漏洞验证", in_index: true, mcp_gated: [] },
      { name: "mini-program", description: "小程序逆向", in_index: true, mcp_gated: [] },
      { name: "traffic-replay", description: "流量重放", in_index: false, mcp_gated: [] },
      { name: "report-writing", description: "报告撰写", in_index: true, mcp_gated: [] },
    ],
    totals: {
      segments: segments.length,
      chars: 3260 + 412 + 1580 + 640 + 810,
      est_tokens: 1850,
    },
    warnings: [
      "运行时注入在任务执行时生成；本视图只给位置与条件，不展开正文、不连接 MCP、不含凭据。",
      "MCP「playwright」上次健康检查失败，缓存可能过期。",
    ],
    static_only: true,
  };
}
