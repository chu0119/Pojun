import {
  BREAKPOINTS,
  estimateTextWidth,
  fitText,
  layoutSignalFlow,
  MIN_ROW_W,
  wrapText,
} from "./chain-layout.ts";
import { strict as assert } from "node:assert";
import test from "node:test";

// ---------------------------------------------------------------------------
// 门控信号流几何：三档宽度下不重叠、不越界、文本按可用宽度裁切。
// 设计基线是 PR #62 的三列信号流；这里只校验几何契约（纯函数，无 DOM）。
// ---------------------------------------------------------------------------

const row = (i, h = 36, gapBefore) => ({
  id: `r${i}`,
  h,
  gapBefore,
  title: `第 ${i} 段提示词`,
  sub: "宿主注入",
  tag: "正文",
  chars: 100 + i,
});

const rows = (n = 7) =>
  Array.from({ length: n }, (_, i) => row(i, i === 3 ? 44 : i === n - 1 ? 48 : 36, i === 0 ? undefined : i % 3 === 0 ? 36 : 24));

const lay = (width, over = {}) =>
  layoutSignalFlow({
    width,
    rows: over.rows ?? rows(),
    skills: over.skills ?? 3,
    mcpCount: over.mcpCount ?? 2,
    hasTray: over.hasTray ?? false,
    quarantineH: over.quarantineH ?? 0,
  });

const overlap = (a, b) =>
  a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h;

const allRects = (g) =>
  [...g.rows, g.llm, g.skills, ...(g.tray ? [g.tray] : []), g.tools, g.mcp, ...(g.quarantine ? [g.quarantine] : [])];

test("断点：≥1040 三列、720–1039 两列、<720 单列", () => {
  assert.equal(lay(BREAKPOINTS.wide).mode, "wide");
  assert.equal(lay(1920).mode, "wide");
  assert.equal(lay(BREAKPOINTS.two).mode, "two");
  assert.equal(lay(900).mode, "two");
  assert.equal(lay(BREAKPOINTS.two - 1).mode, "one");
  assert.equal(lay(375).mode, "one");
});

test("三档宽度下任何两个块都不重叠", () => {
  for (const w of [320, 375, 480, 719, 720, 900, 1039, 1040, 1280, 1440, 1920, 2560]) {
    const g = lay(w, { hasTray: true, quarantineH: 76 });
    const rects = allRects(g);
    for (let i = 0; i < rects.length; i++) {
      for (let j = i + 1; j < rects.length; j++) {
        assert.ok(!overlap(rects[i], rects[j]), `${w}px: 块 ${i} 与 ${j} 重叠`);
      }
    }
  }
});

test("主干行宽不因容器变宽而无意义膨胀，也不低于可读下限", () => {
  for (const w of [1040, 1280, 1440, 1920, 2560]) {
    const g = lay(w);
    assert.ok(g.center.w >= MIN_ROW_W, `${w}px 主干过窄`);
    assert.ok(g.center.w <= 560, `${w}px 主干过宽`);
    for (const r of g.rows) assert.equal(r.w, g.center.w);
  }
});

test("所有内容都在画布内（不产生横向滚动）", () => {
  for (const w of [320, 375, 600, 719, 720, 900, 1040, 1280, 1440, 1920]) {
    const g = lay(w, { hasTray: true, quarantineH: 76 });
    for (const r of allRects(g)) {
      assert.ok(r.x >= 0, `${w}px: x<0`);
      assert.ok(r.x + r.w <= g.w + 0.5, `${w}px: 右越界 ${r.x + r.w} > ${g.w}`);
      assert.ok(r.y >= 0, `${w}px: y<0`);
      assert.ok(r.y + r.h <= g.h + 0.5, `${w}px: 下越界`);
    }
  }
});

test("窄屏主干保持在首位，侧栏依次堆叠在其下方", () => {
  const g = lay(600, { hasTray: true });
  const lastRow = g.rows[g.rows.length - 1];
  assert.ok(g.llm.y > lastRow.y + lastRow.h, "LLM 应在主干下方");
  assert.ok(g.skills.y > g.llm.y + g.llm.h, "Skill 应在 LLM 下方");
  assert.ok(g.tools.y > g.skills.y + g.skills.h, "工具应在 Skill 下方");
  assert.ok(g.mcp.y > g.tools.y + g.tools.h, "MCP 应在工具下方");
});

test("两列模式：工具与 MCP 落到主干下方，不与左轨并排挤压", () => {
  const g = lay(900);
  const lastRow = g.rows[g.rows.length - 1];
  assert.ok(g.tools.y > lastRow.y + lastRow.h);
  assert.equal(g.tools.x, g.center.x);
});

test("硬边界落在代码硬尾所在行之前，并横跨主干区", () => {
  // 默认行里没有 "code"，必须显式构造，否则断言会被整体跳过而空转。
  const g = lay(1280, {
    rows: [
      { id: "body", h: 44, title: "提示词正文", sub: "DB 模板", tag: "正文", chars: 3000 },
      { id: "code", h: 52, gapBefore: 40, title: "代码硬尾", sub: "代码固定", tag: "硬尾", chars: 900 },
      { id: "host", h: 36, gapBefore: 24, title: "本机工具", sub: "宿主注入", tag: "工具", chars: 640 },
    ],
  });
  const code = g.byId.get("code");
  assert.ok(code, "测试需要 code 行，否则该用例没有意义");
  assert.ok(g.boundary, "存在 code 行时必须产出 boundary");
  assert.equal(g.boundary.y, code.y - 20);
  assert.ok(g.boundary.x1 < g.center.x, "boundary 应从主干左侧起");
  assert.ok(g.boundary.x2 > g.center.x + g.center.w, "boundary 应越过主干右侧");
  assert.ok(g.boundary.x1 >= 0 && g.boundary.x2 <= g.w, "boundary 不得越出画布");
  // 无 code 行时不应产出边界。
  assert.equal(lay(1280).boundary, null);
});

test("画布高度覆盖所有块", () => {
  for (const w of [375, 900, 1280, 1920]) {
    const g = lay(w, { hasTray: true, quarantineH: 76 });
    const bottom = Math.max(...allRects(g).map((r) => r.y + r.h));
    assert.ok(g.h >= bottom, `${w}px: 高度 ${g.h} < 内容底 ${bottom}`);
  }
});

test("fitText：未超宽时原样返回，超宽时以省略号收尾且不超宽", () => {
  const style = { px: 11 };
  assert.equal(fitText("短标题", 200, style), "短标题");
  const long = "这是一个很长很长的提示词段落标题需要被裁切";
  const cut = fitText(long, 110, style);
  assert.ok(cut.endsWith("…"));
  assert.ok(cut.length < long.length);
  assert.ok(estimateTextWidth(cut, style) <= 110 + 0.01, "裁切后仍超宽");
});

test("fitText 按真实测量函数工作", () => {
  const measure = (t) => t.length * 10; // 每字符 10px 的确定性测量
  assert.equal(fitText("abcdef", 60, { px: 10 }, measure), "abcdef");
  const cut = fitText("abcdefghij", 55, { px: 10 }, measure);
  assert.ok(cut.endsWith("…"));
  assert.ok(cut.length <= 6);
});

test("wrapText：不超过行数上限，且逐行不超宽", () => {
  const style = { px: 10 };
  const lines = wrapText("多行标题需要在窄容器里折行显示并且不能溢出容器边界", 100, style, 2);
  assert.ok(lines.length <= 2);
  for (const l of lines) assert.ok(estimateTextWidth(l, style) <= 110, `行超宽: ${l}`);
});

test("随机宽度属性测试：500 组不重叠且不越界", () => {
  let seed = 20261125;
  const rnd = () => {
    seed = (seed * 1103515245 + 12345) % 2147483648;
    return seed / 2147483648;
  };
  for (let i = 0; i < 500; i++) {
    const w = 320 + Math.floor(rnd() * 2240);
    const n = 1 + Math.floor(rnd() * 9);
    const g = lay(w, {
      rows: rows(n),
      skills: Math.floor(rnd() * 10),
      mcpCount: Math.floor(rnd() * 6),
      hasTray: rnd() > 0.5,
      quarantineH: rnd() > 0.5 ? 76 : 0,
    });
    const rects = allRects(g);
    for (let a = 0; a < rects.length; a++) {
      for (let b = a + 1; b < rects.length; b++) {
        assert.ok(!overlap(rects[a], rects[b]), `w=${w} n=${n}: 块 ${a}/${b} 重叠`);
      }
      assert.ok(rects[a].x + rects[a].w <= g.w + 0.5, `w=${w}: 右越界`);
    }
  }
});
