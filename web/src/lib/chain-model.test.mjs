// chain-model 纯函数测试：node --experimental-strip-types --test src/lib/*.test.mjs
import {
  buildChainModel,
  firstSemanticUnit,
  modeLabel,
  GROUP_LABEL,
} from "./chain-model.ts";
import { strict as assert } from "node:assert";
import test from "node:test";

const seg = (key, kind, present, extra = {}) => ({
  key,
  title: extra.title || key,
  kind,
  source: extra.source || kind,
  source_label: extra.source_label || kind,
  editable: false,
  gate: extra.gate,
  present,
  available: extra.available !== undefined ? extra.available : true,
  reason: extra.reason,
  order: extra.order || 0,
  chars: extra.chars || 0,
  text: extra.text || "",
});

function fixture(over = {}) {
  const base = {
    agent_key: "worker",
    agent_kind: "builtin",
    llm: {
      profile_id: 1,
      name: "Opus",
      model: "claude-opus-4-8",
      format: "anthropic",
      source: "agent_binding",
      source_label: "Agent 显式绑定",
      prompt_mode: "append",
      prompt_chars: 412,
      prompt_apply_inherited: false,
      prompt_active: true,
      effort: "high",
      effort_source: "profile",
    },
    segments: [
      seg("body", "body", true, { source_label: "DB 模板正文 v3", chars: 3260 }),
      seg("skill_index", "skill_index", true, { source_label: "宿主注入", chars: 1580 }),
      seg("profile_prompt", "profile_prompt", true, { source_label: "LLM 配置 · 置于正文之后", chars: 412 }),
      seg("traffic_tools", "code_tail", false, { title: "流量工具说明", reason: "需开启流量捕获" }),
      seg("artifact_spec", "code_tail", true, { title: "中间产物输出规约", chars: 420 }),
      seg("host_tools", "host", true, { chars: 640 }),
      seg("runtime_intent", "runtime_intent", false, { available: false, title: "意图与资产快照", gate: "每次 run 注入" }),
      seg("finding_guidance", "finding_guidance", false, { available: false, title: "证据纪律" }),
      seg("deferred_tools", "deferred_tools", false, { available: false, title: "延迟工具块" }),
    ],
    tools: {
      builtin: [],
      custom: [],
      traffic: [],
      shell: [],
      deferred_names: ["custom_tool"],
      excluded: [{ key: "kb_search", reason: "该 Agent 未绑定" }],
      total: 27,
    },
    mcp: [
      { id: 1, name: "filesystem", transport: "stdio", enabled: true, mode: "global", skill: "", health: "ok", tool_count: 9, cache_stale: false, tools: [] },
      { id: 2, name: "playwright", transport: "http", enabled: true, mode: "skill_gated", skill: "web-recon", health: "failed", tool_count: 5, cache_stale: true, tools: [] },
      { id: 3, name: "nuclei", transport: "stdio", enabled: true, mode: "excluded", skill: "", health: "never_checked", tool_count: 0, cache_stale: false, tools: [] },
    ],
    skills: [
      { name: "web-recon", description: "站点侦察。扫描子域与端口。", in_index: true, index_state: "described", mcp_gated: ["playwright"] },
      { name: "report-writing", description: "报告撰写", in_index: true, index_state: "name_only", mcp_gated: [] },
      { name: "traffic-replay", description: "流量重放", in_index: false, index_state: "absent", mcp_gated: [] },
    ],
    totals: { segments: 9, chars: 8762, est_tokens: 4381 },
    warnings: [
      "流量捕获未开启：traffic_* 工具不会出现在运行时",
      "MCP「playwright」尚未检查：工具清单来自缓存，运行时可能为空",
      "某个没见过的告警",
      "该 LLM 配置未设置 API Key，运行时可能回落到其他配置",
    ],
    static_only: true,
    prompt_max_runes: 65535,
    ...over,
  };
  return base;
}

const byId = (m, id) => m.items.find((i) => i.id === id);

test("来源按喂给段的先后排序：append 模式下 Skill 绑定在 LLM 配置之前", () => {
  const m = buildChainModel(fixture());
  const ids = m.items.filter((i) => i.group === "source").map((i) => i.id);
  assert.deepEqual(ids, ["skills", "llm"]);
});

test("prepend 模式下 LLM 配置排在 Skill 绑定之前", () => {
  const c = fixture();
  c.llm.prompt_mode = "prepend";
  const m = buildChainModel(c);
  const ids = m.items.filter((i) => i.group === "source").map((i) => i.id);
  assert.deepEqual(ids, ["llm", "skills"]);
  // 独立提示词排在正文之前
  const sys = m.items.filter((i) => i.group === "system").map((i) => i.id);
  assert.ok(sys.indexOf("seg:profile_prompt") < sys.indexOf("seg:body"));
});

test("序号只编给生效或运行时才有的对象；未生效段不编号", () => {
  const m = buildChainModel(fixture());
  assert.equal(byId(m, "seg:body").order, 1);
  assert.equal(byId(m, "seg:skill_index").order, 2);
  assert.equal(byId(m, "seg:profile_prompt").order, 3);
  assert.equal(byId(m, "code").order, 4);
  assert.equal(byId(m, "runtime").order, 6);
  // code 里未生效的成员不编号
  const traffic = byId(m, "code").children.find((c) => c.id === "seg:traffic_tools");
  assert.equal(traffic.order, undefined);
});

test("replace 生效时正文仍列出，标已被替换且不编号", () => {
  const c = fixture();
  c.llm.prompt_mode = "replace";
  c.segments = c.segments.map((s) => (s.kind === "body" ? { ...s, present: false, chars: 3260, reason: "已被独立提示词替换" } : s));
  const m = buildChainModel(c);
  const body = byId(m, "seg:body");
  assert.equal(body.state, "inactive");
  assert.equal(body.stateWord, "已被替换");
  assert.equal(body.order, undefined);
  assert.equal(body.chars, 3260);
});

test("技能三态：有描述 / 仅名称 / 索引外", () => {
  const m = buildChainModel(fixture());
  const kids = byId(m, "skills").children;
  assert.equal(kids.find((k) => k.id === "skill:web-recon").stateWord, "进速查");
  assert.equal(kids.find((k) => k.id === "skill:report-writing").stateWord, "仅名称");
  assert.equal(kids.find((k) => k.id === "skill:traffic-replay").stateWord, "索引外");
  // 成员 note 是首个语义单元，不是切到字符数
  assert.equal(kids.find((k) => k.id === "skill:web-recon").note, "站点侦察");
});

test("判决只数异常：失败的 MCP 一条；warning 里的未检查不重复计数", () => {
  const m = buildChainModel(fixture());
  const mcpAttention = m.attention.filter((a) => a.itemId === "mcp:2");
  assert.equal(mcpAttention.length, 1);
  assert.ok(mcpAttention[0].text.includes("工具缓存可能已过期"));
  // excluded 的 MCP 不计入
  assert.equal(m.attention.filter((a) => a.itemId === "mcp:3").length, 0);
  // warning 里的「尚未检查」被并入派生项，没有第二条
  assert.equal(m.attention.filter((a) => a.text.includes("尚未检查")).length, 0);
});

test("提示性说明不进判决；认不出的告警计为需注意", () => {
  const m = buildChainModel(fixture());
  assert.ok(m.notes.some((n) => n.startsWith("流量捕获未开启")));
  assert.ok(!m.attention.some((a) => a.text.startsWith("流量捕获未开启")));
  assert.ok(m.attention.some((a) => a.text === "某个没见过的告警"));
  assert.ok(m.attention.some((a) => a.itemId === "llm" && a.text.includes("API Key")));
});

test("同一事物只有一个对象：被排除的 MCP 只在已排除里", () => {
  const m = buildChainModel(fixture());
  const inMCP = (byId(m, "mcp").children || []).some((c) => c.id === "mcp:3");
  const inExcluded = (byId(m, "excluded").children || []).some((c) => c.id === "mcp:3");
  assert.equal(inMCP, false);
  assert.equal(inExcluded, true);
});

test("代码固定段：字符数是生效成员之和，带硬边界标记", () => {
  const m = buildChainModel(fixture());
  const code = byId(m, "code");
  assert.equal(code.chars, 420);
  assert.equal(code.boundaryBefore, true);
  assert.equal(code.note, "1 段生效 · 1 段未生效");
});

test("未配置独立提示词时不产生该对象，也不产生来源连线", () => {
  const c = fixture();
  c.llm.prompt_mode = "";
  c.llm.prompt_chars = 0;
  c.llm.prompt_active = false;
  c.segments = c.segments.filter((s) => s.kind !== "profile_prompt");
  const m = buildChainModel(c);
  assert.equal(byId(m, "seg:profile_prompt"), undefined);
  assert.equal(byId(m, "llm").feeds, undefined);
  assert.ok(!m.attention.some((a) => a.text.includes("独立提示词已配置但未生效")));
});

test("独立提示词配置了但未生效：计入判决并指向该对象", () => {
  const c = fixture();
  c.llm.prompt_active = false;
  c.segments = c.segments.map((s) => (s.kind === "profile_prompt" ? { ...s, present: false } : s));
  const m = buildChainModel(c);
  assert.ok(m.attention.some((a) => a.itemId === "seg:profile_prompt"));
});

test("firstSemanticUnit 在句号、分号、换行处截断且不补省略号", () => {
  assert.equal(firstSemanticUnit("站点侦察。扫描子域"), "站点侦察");
  assert.equal(firstSemanticUnit("第一句；第二句"), "第一句");
  assert.equal(firstSemanticUnit("单行描述"), "单行描述");
  assert.equal(firstSemanticUnit(""), "");
  assert.equal(firstSemanticUnit("无标点但很长".repeat(30)).endsWith("…"), false);
});

test("分组标题齐全", () => {
  assert.equal(GROUP_LABEL.source, "来源 · 配置与绑定");
  assert.equal(modeLabel("append"), "正文之后");
});
