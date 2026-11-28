// chain-schematic 几何测试：布局规则 + 自检函数本身。
import { checkGeometry, layoutSchematic, schematicInputFromModel, SCHEMATIC_G } from "./chain-schematic.ts";
import { buildChainModel } from "./chain-model.ts";
import { strict as assert } from "node:assert";
import test from "node:test";

const trunk = [
  { id: "seg:body" },
  { id: "seg:skill_index" },
  { id: "seg:profile_prompt" },
  { id: "code", boundaryBefore: true },
  { id: "seg:host_tools" },
  { id: "model" },
];
const sources = [
  { id: "skills", feeds: "seg:skill_index" },
  { id: "llm", feeds: "seg:profile_prompt" },
];
const sidecar = ["tools", "mcp", "seg:deferred_tools"];

const at = (g, id) => g.nodes.find((n) => n.id === id);

test("三列模式：来源在左、主干居中、随请求下发在右", () => {
  const g = layoutSchematic({ width: 900, trunk, sources, sidecar, hasExcluded: true });
  assert.equal(g.mode, "three");
  assert.equal(at(g, "seg:body").col, "trunk");
  assert.equal(at(g, "skills").col, "left");
  assert.equal(at(g, "tools").col, "right");
  assert.equal(at(g, "excluded").col, "out");
  // 左中右的 x 递增
  assert.ok(at(g, "skills").x < at(g, "seg:body").x);
  assert.ok(at(g, "seg:body").x + at(g, "seg:body").w < at(g, "tools").x);
});

test("两列模式：随请求下发移到左列下方，仍只有一个组框", () => {
  const g = layoutSchematic({ width: 600, trunk, sources, sidecar, hasExcluded: false });
  assert.equal(g.mode, "two");
  assert.equal(at(g, "tools").col, "left");
  assert.equal(g.boxes.length, 1);
  assert.equal(g.boxes[0].id, "sidecar");
  // 组框在来源下方，不重叠
  assert.ok(g.boxes[0].y >= at(g, "skills").y + SCHEMATIC_G.NODE_H);
  assert.deepEqual(checkGeometry(g).overlaps, []);
});

test("宽度不足 520：不提供关系图，返回空节点", () => {
  const g = layoutSchematic({ width: 480, trunk, sources, sidecar, hasExcluded: true });
  assert.equal(g.mode, "one");
  assert.equal(g.nodes.length, 0);
  assert.equal(g.h, 0);
});

test("来源与它喂的段同一行，连线是一条水平直线", () => {
  const g = layoutSchematic({ width: 900, trunk, sources, sidecar, hasExcluded: false });
  for (const s of sources) {
    assert.equal(at(g, s.id).y, at(g, s.feeds).y, s.id);
    const e = g.edges.find((x) => x.from === s.id);
    assert.equal(e.pts.length, 2, "直线只有两个端点");
    assert.equal(e.pts[0][1], e.pts[1][1], "两端 y 相同");
  }
});

test("随请求下发整组只有一条连线，进入模型请求", () => {
  const g = layoutSchematic({ width: 900, trunk, sources, sidecar, hasExcluded: false });
  const boxEdges = g.edges.filter((e) => e.from === "sidecar");
  assert.equal(boxEdges.length, 1);
  assert.equal(boxEdges[0].to, "model");
  assert.deepEqual(boxEdges[0].members, sidecar);
  // 三列模式在右侧，连线从框左边缘出发
  assert.equal(boxEdges[0].pts[0][0], g.boxes[0].x);
});

test("边界：代码固定段之前画出分隔，且只在 boundaryBefore 时出现", () => {
  const g = layoutSchematic({ width: 900, trunk, sources, sidecar, hasExcluded: false });
  assert.ok(g.boundary);
  const noBoundary = layoutSchematic({
    width: 900,
    trunk: trunk.map((t) => ({ id: t.id })),
    sources,
    sidecar,
    hasExcluded: false,
  });
  assert.equal(noBoundary.boundary, null);
});

test("几何自检：三列、两列、带 excluded 都没有重叠/越界/穿节点", () => {
  for (const [width, excluded] of [
    [900, true],
    [900, false],
    [672, false],
    [600, true],
    [520, false],
  ]) {
    const g = layoutSchematic({ width, trunk, sources, sidecar, hasExcluded: excluded });
    const c = checkGeometry(g);
    assert.deepEqual(c.overlaps, [], `overlaps @${width}`);
    assert.deepEqual(c.outOfBounds, [], `outOfBounds @${width}`);
    assert.deepEqual(c.edgesThroughNodes, [], `through @${width}`);
    assert.deepEqual(c.boxOverlaps, [], `boxOverlaps @${width}`);
    assert.deepEqual(c.labelOverlaps, [], `labelOverlaps @${width}`);
    assert.equal(c.crossings, 0, `crossings @${width}`);
  }
});

test("连线弯折数：来源 0 弯折，随请求下发最多 2 处", () => {
  const g = layoutSchematic({ width: 900, trunk, sources, sidecar, hasExcluded: false });
  assert.equal(checkGeometry(g).bends <= 2, true);
  const side = g.edges.find((e) => e.from === "sidecar");
  assert.equal(side.pts.length - 2 <= 2, true);
});

test("schematicInputFromModel：模型顺序原样进入主干，代码固定段带边界", () => {
  const chain = {
    agent_key: "worker",
    agent_kind: "builtin",
    llm: {
      profile_id: 1,
      name: "Opus",
      model: "m",
      format: "anthropic",
      source: "agent_binding",
      source_label: "绑定",
      prompt_mode: "append",
      prompt_chars: 10,
      prompt_apply_inherited: false,
      prompt_active: true,
      effort: "",
      effort_source: "",
    },
    segments: [
      { key: "body", title: "正文", kind: "body", source: "db", source_label: "DB", editable: false, present: true, available: true, order: 0, chars: 100, text: "" },
      { key: "skill_index", title: "速查", kind: "skill_index", source: "host", source_label: "宿主", editable: false, present: true, available: true, order: 0, chars: 50, text: "" },
      { key: "profile_prompt", title: "独立", kind: "profile_prompt", source: "profile", source_label: "配置", editable: false, present: true, available: true, order: 0, chars: 10, text: "" },
      { key: "spec", title: "规约", kind: "code_tail", source: "code", source_label: "代码", editable: false, present: true, available: true, order: 0, chars: 20, text: "" },
    ],
    tools: { builtin: [], custom: [], traffic: [], shell: [], deferred_names: [], excluded: [], total: 3 },
    mcp: [],
    skills: [{ name: "s", description: "d", in_index: true, index_state: "described", mcp_gated: [] }],
    totals: { segments: 4, chars: 180, est_tokens: 90 },
    warnings: [],
    static_only: true,
    prompt_max_runes: 65535,
  };
  const model = buildChainModel(chain);
  const input = schematicInputFromModel(model, 900);
  assert.deepEqual(
    input.trunk.map((t) => t.id),
    ["seg:body", "seg:skill_index", "seg:profile_prompt", "code", "model"],
  );
  assert.equal(input.trunk.find((t) => t.id === "code").boundaryBefore, true);
  assert.deepEqual(input.sources.map((s) => s.id), ["skills", "llm"]);
  assert.deepEqual(input.sidecar, ["tools"]);
  assert.equal(input.hasExcluded, false);
  const g = layoutSchematic(input);
  assert.deepEqual(checkGeometry(g).overlaps, []);
});
