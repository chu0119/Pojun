// 全链路「门控信号流」几何核心（纯函数，无 React/DOM）。
//
// 设计基线沿用 PR #62 的视觉语法：左侧 LLM 配置解析 + Skill 绑定，中部单路径
// 装配主干（Agent → 独立提示词 → 正文 → 技能 → 硬尾 → 门控 → 运行时 → 延迟 →
// 模型请求），右侧工具 schema 与 MCP 服务器，底部未进入主干区。
//
// #62 唯一的缺陷是几何：所有坐标硬编码在 990 宽的 viewBox 里再整体缩放，
// 容器一窄就挤在一起、文案按固定字符数截断。这里把几何全部改成由容器宽度
// 推导，并把字号固定为 CSS px（1 单位 = 1px，不再整体缩放）：
//
//   宽屏  ≥1040：三列（左轨 · 主干 · 右轨），列宽随容器增长
//   中屏  720–1039：两列，左轨在左、主干在右，工具/MCP 落到主干下方
//   窄屏  <720：纵向单列，主干保持在首位，侧栏依次堆叠
//
// 文本一律按真实可用宽度裁切（measure 可注入；默认按 CJK=1em / ASCII≈0.55em
// 估算），不再出现「18 个字」这类与容器无关的魔法数。

export const BREAKPOINTS = { wide: 1040, two: 720 } as const;
export const MIN_ROW_W = 240;
export const MARGIN = 20;
export const GAP = 24;

export type ChainMode = "wide" | "two" | "one";

export type Rect = { x: number; y: number; w: number; h: number };

export type RowSpec = {
  id: string;
  h: number;
  gapBefore?: number;
  title: string;
  sub: string;
  tag: string;
  chars: number | null;
};

export type RowGeom = Rect & { id: string; gapBefore: number };

export type SignalFlowInput = {
  width: number;
  rows: RowSpec[];
  skills: number;
  mcpCount: number;
  hasTray: boolean;
  quarantineH: number;
};

export type SignalFlowGeometry = {
  mode: ChainMode;
  w: number;
  h: number;
  center: { x: number; w: number };
  trunkX: number;
  rows: RowGeom[];
  byId: Map<string, RowGeom>;
  llm: Rect;
  skills: Rect;
  tools: Rect;
  mcp: Rect;
  tray: Rect | null;
  quarantine: Rect | null;
  boundary: { y: number; x1: number; x2: number } | null;
  notesY: number;
  rowTitleMaxW: number;
};

export type TextStyle = { px: number; mono?: boolean };
export type MeasureText = (text: string, style: TextStyle) => number;

const CJK =
  /[⺀-⻿　-〿぀-ゟ゠-ヿ㐀-䶿一-鿿豈-﫿＀-￯]/;

// 无 DOM 时的确定性估算：CJK 全角按 1em，ASCII 按字宽表。
export function estimateTextWidth(text: string, style: TextStyle): number {
  let w = 0;
  for (const ch of text) {
    if (CJK.test(ch)) w += style.px;
    else if (ch === " ") w += style.px * 0.28;
    else if ("il.,:;'|!".includes(ch)) w += style.px * 0.3;
    else if (ch >= "0" && ch <= "9") w += style.px * (style.mono ? 0.6 : 0.56);
    else w += style.px * (style.mono ? 0.6 : 0.53);
  }
  return w;
}

export function fitText(
  text: string,
  maxW: number,
  style: TextStyle,
  measure: MeasureText = estimateTextWidth,
): string {
  if (maxW <= 0) return "";
  if (measure(text, style) <= maxW) return text;
  const ell = "…";
  const ellW = measure(ell, style);
  const chars = Array.from(text);
  let out = "";
  let w = 0;
  for (const ch of chars) {
    const cw = measure(ch, style);
    if (w + cw + ellW > maxW) break;
    out += ch;
    w += cw;
  }
  return out ? out + ell : ell;
}

// 两行折行（CJK 逐字、ASCII 按词优先），供侧栏标题使用。
export function wrapText(
  text: string,
  maxW: number,
  style: TextStyle,
  maxLines = 2,
  measure: MeasureText = estimateTextWidth,
): string[] {
  const lines: string[] = [];
  let rest = text.trim();
  while (rest && lines.length < maxLines) {
    if (measure(rest, style) <= maxW) {
      lines.push(rest);
      rest = "";
      break;
    }
    const chars = Array.from(rest);
    let line = "";
    let w = 0;
    for (const ch of chars) {
      const cw = measure(ch, style);
      if (w + cw > maxW) break;
      line += ch;
      w += cw;
    }
    if (!line) break;
    // ASCII 单词尽量不在中间断开。
    if (/[A-Za-z0-9_-]$/.test(line) && !CJK.test(line)) {
      const sp = line.lastIndexOf(" ");
      if (sp > line.length * 0.6) line = line.slice(0, sp);
    }
    rest = rest.slice(line.length).trimStart();
    lines.push(line);
  }
  if (rest && lines.length > 0) {
    const last = lines.length - 1;
    lines[last] = fitText(lines[last] + "…" + rest.slice(0, 1), maxW, style, measure);
  }
  return lines;
}

const clamp = (v: number, lo: number, hi: number) => Math.max(lo, Math.min(hi, v));

function stackY(rows: RowSpec[], startY: number): RowGeom[] {
  const out: RowGeom[] = [];
  let y = startY;
  rows.forEach((r, i) => {
    const gap = i === 0 ? 0 : (r.gapBefore ?? GAP);
    y += gap;
    out.push({ id: r.id, x: 0, y, w: 0, h: r.h, gapBefore: gap });
    y += r.h;
  });
  return out;
}

function rowsBottom(rows: RowGeom[]): number {
  const last = rows[rows.length - 1];
  return last ? last.y + last.h : 0;
}

export function layoutSignalFlow(input: SignalFlowInput): SignalFlowGeometry {
  const W = Math.max(320, Math.round(input.width));
  const mode: ChainMode = W >= BREAKPOINTS.wide ? "wide" : W >= BREAKPOINTS.two ? "two" : "one";

  const llmH = 146;
  const skillsH = 40 + Math.min(input.skills, 6) * 15;
  const toolsH = 104;
  const mcpH = input.mcpCount > 0 ? 16 + input.mcpCount * 38 : 40;
  const trayH = 64;

  const llm: Rect = { x: 0, y: 0, w: 0, h: llmH };
  const skills: Rect = { x: 0, y: 0, w: 0, h: skillsH };
  const tools: Rect = { x: 0, y: 0, w: 0, h: toolsH };
  const mcp: Rect = { x: 0, y: 0, w: 0, h: mcpH };
  let tray: Rect | null = null;
  let quarantine: Rect | null = null;
  let centerX = MARGIN;
  let centerW = W - MARGIN * 2;
  let trunkX = MARGIN;
  let rowsStart = 24;
  let h = 0;
  let notesY = 0;

  if (mode === "wide") {
    const leftW = clamp(Math.round(W * 0.23), 280, 340);
    const rightW = clamp(Math.round(W * 0.23), 280, 340);
    centerX = MARGIN + leftW + 80;
    const rightX = W - MARGIN - rightW;
    centerW = clamp(rightX - 60 - centerX, MIN_ROW_W, 460);
    trunkX = centerX - 20;

    llm.x = MARGIN;
    llm.y = 96;
    llm.w = leftW;
    skills.x = MARGIN;
    skills.y = llm.y + llm.h + 14;
    skills.w = leftW;
    if (input.hasTray) {
      tray = { x: MARGIN, y: skills.y + skills.h + 14, w: leftW, h: trayH };
    }

    tools.x = rightX;
    tools.y = 96;
    tools.w = rightW;
    mcp.x = rightX;
    mcp.y = tools.y + tools.h + 14;
    mcp.w = rightW;

    rowsStart = 100;
  } else if (mode === "two") {
    const leftW = clamp(Math.round(W * 0.36), 240, 280);
    centerX = MARGIN + leftW + 20;
    centerW = W - centerX - MARGIN;
    trunkX = centerX - 20;

    llm.x = MARGIN;
    llm.y = 96;
    llm.w = leftW;
    skills.x = MARGIN;
    skills.y = llm.y + llm.h + 14;
    skills.w = leftW;
    if (input.hasTray) {
      tray = { x: MARGIN, y: skills.y + skills.h + 14, w: leftW, h: trayH };
    }
    tools.w = centerW;
    mcp.w = centerW;
    rowsStart = 96;
  } else {
    centerX = 16;
    centerW = W - 32;
    trunkX = centerX - 16;
    llm.w = centerW;
    skills.w = centerW;
    tools.w = centerW;
    mcp.w = centerW;
    rowsStart = 24;
  }

  const rows = stackY(input.rows, rowsStart).map((r) => ({ ...r, x: centerX, w: centerW }));
  const byId = new Map(rows.map((r) => [r.id, r]));
  const bottom = rowsBottom(rows);

  if (mode === "wide") {
    const leftBottom = tray ? tray.y + tray.h : skills.y + skills.h;
    const rightBottom = mcp.y + mcp.h;
    const qY = Math.max(leftBottom, rightBottom, bottom) + 24;
    if (input.quarantineH > 0) {
      quarantine = { x: MARGIN, y: qY, w: centerX + centerW + 24 - MARGIN, h: input.quarantineH };
    }
    h = Math.max(760, (quarantine ? quarantine.y + quarantine.h : qY) + 40);
    notesY = h - 18;
  } else if (mode === "two") {
    const leftBottom = tray ? tray.y + tray.h : skills.y + skills.h;
    const q0 = Math.max(leftBottom, bottom) + 24;
    tools.x = centerX;
    tools.y = q0;
    mcp.x = centerX;
    mcp.y = tools.y + tools.h + 14;
    const qY = Math.max(mcp.y + mcp.h, leftBottom) + 24;
    if (input.quarantineH > 0) {
      quarantine = { x: MARGIN, y: qY, w: W - MARGIN * 2, h: input.quarantineH };
    }
    h = Math.max(560, (quarantine ? quarantine.y + quarantine.h : qY) + 40);
    notesY = h - 18;
  } else {
    let y = bottom + 20;
    llm.x = 16;
    llm.y = y;
    y += llm.h + 12;
    skills.x = 16;
    skills.y = y;
    y += skills.h + 12;
    tools.x = 16;
    tools.y = y;
    y += tools.h + 12;
    mcp.x = 16;
    mcp.y = y;
    y += mcp.h + 12;
    if (input.hasTray) {
      tray = { x: 16, y, w: centerW, h: trayH };
      y += trayH + 12;
    }
    if (input.quarantineH > 0) {
      quarantine = { x: 16, y, w: centerW, h: input.quarantineH };
      y += input.quarantineH + 12;
    }
    h = y + 24;
    notesY = h - 14;
  }

  const code = byId.get("code");
  const boundary = code
    ? {
        y: code.y - 20,
        x1: Math.max(0, centerX - 60),
        x2: Math.min(W, centerX + centerW + 25),
      }
    : null;

  return {
    mode,
    w: W,
    h: Math.round(h),
    center: { x: centerX, w: centerW },
    trunkX,
    rows,
    byId,
    llm,
    skills,
    tools,
    mcp,
    tray,
    quarantine,
    boundary,
    notesY,
    rowTitleMaxW: centerW - 36,
  };
}
