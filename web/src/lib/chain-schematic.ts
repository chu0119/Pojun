// 装配关系图几何：舞台宽度 + 模型结构 → 节点矩形、组框、标题位置、连线折点。
//
// 纯函数，不依赖 DOM。布局只依赖舞台宽度与模型结构，与选中状态无关——所以
// 选中不会让图重排，位置在两次渲染之间稳定。
//
// 设计规则（见方案 §4.4）：
//   - 三列模式（宽 ≥ 672）：来源在左、主干居中、随请求下发在右。
//   - 两列模式（520–671）：随请求下发移到左列下方。
//   - < 520 不提供关系图（mode = "one"，节点为空）。
//   - 来源与它喂给的主干节点同一行，连线是一条水平直线。
//   - 随请求下发收进一个组框，整组只有一条连线进入模型请求。
//   - 模型请求是汇聚结果，用双线边框（由渲染层处理）。

import type { ChainModel } from "./chain-model";

export const SCHEMATIC_G = {
  NODE_H: 44,
  GAP: 12,
  PAD: 16,
  GUT: 40,
  BOUND: 24,
  CAP: 24,
  BOX: 8,
  SIDE_MIN: 160,
  SIDE_MAX: 200,
  TRUNK_MAX: 440,
} as const;

export type NodeCol = "left" | "trunk" | "right" | "out";

export interface SchematicNode {
  id: string;
  col: NodeCol;
  x: number;
  y: number;
  w: number;
  h: number;
  box?: string;
}

export interface SchematicEdge {
  from: string;
  to: string;
  members: string[];
  pts: [number, number][];
}

export interface SchematicBox {
  id: string;
  x: number;
  y: number;
  w: number;
  h: number;
  label: string;
}

export interface SchematicCap {
  x: number;
  y: number;
  text: string;
}

export interface SchematicTick {
  x: number;
  y1: number;
  y2: number;
}

export interface SchematicBoundary {
  x: number;
  w: number;
  y: number;
}

export interface SchematicLayout {
  mode: "three" | "two" | "one";
  w: number;
  h: number;
  nodes: SchematicNode[];
  edges: SchematicEdge[];
  caps: SchematicCap[];
  boxes: SchematicBox[];
  boundary: SchematicBoundary | null;
  ticks: SchematicTick[];
  side: number;
  trunkW: number;
}

export interface SchematicInput {
  width: number;
  trunk: { id: string; boundaryBefore?: boolean }[];
  sources: { id: string; feeds?: string }[];
  sidecar: string[];
  hasExcluded: boolean;
}

/** 从模型推出几何输入。关系图与清单读同一份模型，顺序天然一致。 */
export function schematicInputFromModel(model: ChainModel, width: number): SchematicInput {
  const system = model.items.filter((i) => i.group === "system");
  return {
    width,
    trunk: [...system.map((i) => ({ id: i.id, boundaryBefore: i.boundaryBefore })), { id: "model" }],
    sources: model.items.filter((i) => i.group === "source").map((i) => ({ id: i.id, feeds: i.feeds })),
    sidecar: model.items.filter((i) => i.group === "sidecar").map((i) => i.id),
    hasExcluded: model.items.some((i) => i.group === "excluded"),
  };
}

export function layoutSchematic({ width, trunk, sources, sidecar, hasExcluded }: SchematicInput): SchematicLayout {
  const G = SCHEMATIC_G;
  const W = Math.round(width);
  const mode: SchematicLayout["mode"] = W >= 672 ? "three" : W >= 520 ? "two" : "one";
  if (mode === "one") {
    return { mode, w: W, h: 0, nodes: [], edges: [], caps: [], boxes: [], boundary: null, ticks: [], side: 0, trunkW: 0 };
  }
  const side = mode === "three" ? Math.max(G.SIDE_MIN, Math.min(G.SIDE_MAX, Math.round(W * 0.2))) : G.SIDE_MIN;
  const fixed = mode === "three" ? 2 * side + 2 * G.GUT : side + G.GUT;
  const trunkW = Math.min(G.TRUNK_MAX, W - 2 * G.PAD - fixed);
  const group = fixed + trunkW;
  const x0 = Math.round((W - group) / 2);
  const leftX = x0;
  const trunkX = x0 + side + G.GUT;
  const rightX = trunkX + trunkW + G.GUT;
  const n = trunk.length;
  const yRow: number[] = [];
  let y = G.PAD + G.CAP;
  trunk.forEach((t, i) => {
    if (i > 0) y += G.GAP + (t.boundaryBefore ? G.BOUND : 0);
    yRow[i] = y;
    y += G.NODE_H;
  });
  const slotY = (k: number) => (k < n ? yRow[k] : yRow[n - 1] + (k - n + 1) * (G.NODE_H + G.GAP));
  const mid = (yy: number) => yy + G.NODE_H / 2;
  const sinkY = mid(yRow[n - 1]);
  const nodes: SchematicNode[] = [];
  const edges: SchematicEdge[] = [];
  const ticks: SchematicTick[] = [];
  const boxes: SchematicBox[] = [];
  const caps: SchematicCap[] = [{ x: trunkX, y: yRow[0] - 20, text: "system 提示词 · 自上而下拼接" }];
  trunk.forEach((t, i) => {
    nodes.push({ id: t.id, col: "trunk", x: trunkX, y: yRow[i], w: trunkW, h: G.NODE_H });
    if (i > 0) ticks.push({ x: trunkX + 18, y1: yRow[i - 1] + G.NODE_H, y2: yRow[i] });
  });
  const bi = trunk.findIndex((t) => t.boundaryBefore);
  const boundary: SchematicBoundary | null =
    bi > 0 ? { x: trunkX + 36, w: trunkW - 36, y: yRow[bi] - (G.GAP + G.BOUND) / 2 - 8 } : null;
  // 来源：与它喂给的主干节点同一行，连线是一条水平直线；喂给的段不存在时放到第一个空行，不画线。
  const used = new Set<number>();
  sources.forEach((s) => {
    let k = trunk.findIndex((t) => t.id === s.feeds);
    const linked = k >= 0 && !used.has(k);
    if (!linked) {
      k = 0;
      while (used.has(k)) k++;
    }
    used.add(k);
    const yy = slotY(k);
    nodes.push({ id: s.id, col: "left", x: leftX, y: yy, w: side, h: G.NODE_H });
    if (linked) {
      edges.push({ from: s.id, to: s.feeds as string, members: [s.id], pts: [[leftX + side, mid(yy)], [trunkX, mid(yy)]] });
    }
  });
  if (used.size) caps.push({ x: leftX, y: slotY(Math.min(...used)) - 20, text: "来源" });
  // 随请求下发：收进一个组框，整组只有一条连线进入模型请求。
  const m = sidecar.length;
  if (m) {
    const right = mode === "three";
    const colX = right ? rightX : leftX;
    const r0 = right ? Math.max(0, n - m) : Math.max((used.size ? Math.max(...used) : -1) + 2, n - m, 0);
    const ys = sidecar.map((_, k) => slotY(r0 + k));
    sidecar.forEach((id, k) => nodes.push({ id, col: right ? "right" : "left", x: colX, y: ys[k], w: side, h: G.NODE_H, box: "sidecar" }));
    const box: SchematicBox = {
      id: "sidecar",
      x: colX - G.BOX,
      y: ys[0] - G.CAP,
      w: side + 2 * G.BOX,
      h: ys[m - 1] + G.NODE_H + G.BOX - (ys[0] - G.CAP),
      label: "随请求下发",
    };
    boxes.push(box);
    const facing = right ? box.x : box.x + box.w;
    const tx = right ? trunkX + trunkW : trunkX;
    const busX = Math.round((facing + tx) / 2);
    const yOut = Math.max(box.y + 12, Math.min(box.y + box.h - 12, sinkY));
    edges.push({
      from: "sidecar",
      to: trunk[n - 1].id,
      members: sidecar.slice(),
      pts: yOut === sinkY ? [[facing, sinkY], [tx, sinkY]] : [[facing, yOut], [busX, yOut], [busX, sinkY], [tx, sinkY]],
    });
  }
  let bottom = Math.max(...nodes.map((b) => b.y + b.h), ...boxes.map((b) => b.y + b.h));
  if (hasExcluded) {
    nodes.push({ id: "excluded", col: "out", x: leftX, y: bottom + G.GAP, w: group, h: G.NODE_H });
    bottom += G.GAP + G.NODE_H;
  }
  return { mode, w: W, h: bottom + G.PAD, nodes, edges, caps, boxes, boundary, ticks, side, trunkW };
}

export interface GeometryCheck {
  overlaps: string[];
  outOfBounds: string[];
  crossings: number;
  bends: number;
  edgesThroughNodes: string[];
  boxOverlaps: string[];
  labelOverlaps: string[];
}

/** 自检：节点重叠、越界、连线交叉、连线穿过节点或组框、组框压到别的节点、标题压到节点。 */
export function checkGeometry(g: SchematicLayout): GeometryCheck {
  const hit = (a: { x: number; y: number; w: number; h: number }, b: { x: number; y: number; w: number; h: number }) =>
    a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h;
  const inside = (a: { x: number; y: number; w: number; h: number }) =>
    a.x >= 0 && a.y >= 0 && a.x + a.w <= g.w + 0.5 && a.y + a.h <= g.h + 0.5;
  const ov: string[] = [];
  const out: string[] = [];
  const boxOv: string[] = [];
  const capOv: string[] = [];
  g.nodes.forEach((a, i) => {
    if (!inside(a)) out.push(a.id);
    g.nodes.slice(i + 1).forEach((b) => {
      if (hit(a, b)) ov.push(a.id + "×" + b.id);
    });
  });
  g.boxes.forEach((bx) => {
    if (!inside(bx)) out.push("box:" + bx.id);
    g.nodes.forEach((nd) => {
      if (nd.box !== bx.id && hit(bx, nd)) boxOv.push(bx.id + "×" + nd.id);
    });
  });
  // 文字宽度按每字 12px 的上界估算。
  const labels: { id: string; x: number; y: number; w: number; h: number }[] = g.caps.map((c) => ({
    id: "cap:" + c.text,
    x: c.x,
    y: c.y,
    w: c.text.length * 12,
    h: 16,
  }));
  if (g.boundary) labels.push({ id: "boundary", x: g.boundary.x, y: g.boundary.y, w: g.boundary.w, h: 16 });
  labels.forEach((lb) => {
    if (lb.x < 0 || lb.y < 0 || lb.y + lb.h > g.h + 0.5) out.push(lb.id);
    g.nodes.forEach((nd) => {
      if (hit(lb, nd)) capOv.push(lb.id + "×" + nd.id);
    });
    g.boxes.forEach((bx) => {
      if (hit(lb, bx)) capOv.push(lb.id + "×box");
    });
  });
  const segs = g.edges.flatMap((e, ei) => e.pts.slice(1).map((p, i) => ({ ei, a: e.pts[i], b: p })));
  let cross = 0;
  let bends = 0;
  g.edges.forEach((e) => (bends += e.pts.length - 2));
  segs.forEach((s, i) =>
    segs.slice(i + 1).forEach((t) => {
      if (s.ei === t.ei) return;
      const sh = s.a[1] === s.b[1];
      const th = t.a[1] === t.b[1];
      if (sh === th) return;
      const h = sh ? s : t;
      const v = sh ? t : s;
      const hx1 = Math.min(h.a[0], h.b[0]);
      const hx2 = Math.max(h.a[0], h.b[0]);
      const vy1 = Math.min(v.a[1], v.b[1]);
      const vy2 = Math.max(v.a[1], v.b[1]);
      if (v.a[0] > hx1 && v.a[0] < hx2 && h.a[1] > vy1 && h.a[1] < vy2) cross++;
    }),
  );
  const through: string[] = [];
  segs.forEach((s) =>
    [...g.nodes, ...g.boxes.map((b) => ({ ...b, id: "box:" + b.id }))].forEach((nd) => {
      const x1 = Math.min(s.a[0], s.b[0]);
      const x2 = Math.max(s.a[0], s.b[0]);
      const y1 = Math.min(s.a[1], s.b[1]);
      const y2 = Math.max(s.a[1], s.b[1]);
      if (x1 < nd.x + nd.w && x2 > nd.x && y1 < nd.y + nd.h && y2 > nd.y) through.push(nd.id);
    }),
  );
  return { overlaps: ov, outOfBounds: out, crossings: cross, bends, edgesThroughNodes: through, boxOverlaps: boxOv, labelOverlaps: capOv };
}
