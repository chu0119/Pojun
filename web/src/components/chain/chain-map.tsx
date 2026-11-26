"use client";

import * as React from "react";

import { layoutSchematic, schematicInputFromModel } from "@/lib/chain-schematic";
import { cn } from "@/lib/utils";

import { StatusWord } from "./chain-status";
import type { PromptChainController } from "./use-prompt-chain";

const fmt = (n: number) => n.toLocaleString("en-US");

// 节点轮廓的两个取值写成局部 CSS 变量，只影响这张图，不动 globals.css。
const MAP_CSS = `
.chain-map { --cn-border: oklch(0.66 0 0); }
.dark .chain-map { --cn-border: oklch(0.52 0 0); }
`;

export function ChainMap({ pc }: { pc: PromptChainController }) {
  const wrapRef = React.useRef<HTMLDivElement>(null);
  const [width, setWidth] = React.useState(0);

  React.useEffect(() => {
    const el = wrapRef.current;
    if (!el) return;
    const ro = new ResizeObserver((entries) => {
      const w = Math.round(entries[0]?.contentRect.width ?? 0);
      setWidth((prev) => (prev === w ? prev : w));
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  const model = pc.model;
  const g = React.useMemo(
    () => (model && width > 0 ? layoutSchematic(schematicInputFromModel(model, width)) : null),
    [model, width],
  );

  const onKeyDown = React.useCallback(
    (e: React.KeyboardEvent<HTMLDivElement>) => {
      if (!g) return;
      const cur = (e.target as HTMLElement).closest<HTMLElement>("[data-id]");
      const id = cur?.dataset.id;
      if (!cur || !id) return;
      const me = g.nodes.find((n) => n.id === id);
      if (!me) return;
      const cy = (n: { y: number; h: number }) => n.y + n.h / 2;
      const cols: string[] = g.mode === "three" ? ["left", "trunk", "right"] : ["left", "trunk"];
      const inCol = (c: string) => g.nodes.filter((n) => n.col === c);
      const near = (list: typeof g.nodes) =>
        list.slice().sort((a, b) => Math.abs(cy(a) - cy(me)) - Math.abs(cy(b) - cy(me)))[0];
      const out = g.nodes.find((n) => n.col === "out");
      let to: { id: string } | null = null;
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        const d = e.key === "ArrowDown" ? 1 : -1;
        if (me.col === "out") to = d < 0 ? (inCol("trunk").slice(-1)[0] ?? null) : null;
        else to = near(inCol(me.col).filter((n) => (cy(n) - cy(me)) * d > 0)) ?? (d > 0 ? out : null) ?? null;
      } else if (e.key === "ArrowLeft" || e.key === "ArrowRight") {
        const k = cols.indexOf(me.col) + (e.key === "ArrowRight" ? 1 : -1);
        if (me.col !== "out" && k >= 0 && cols[k]) to = near(inCol(cols[k])) ?? null;
      } else if (e.key === "Home") {
        to = g.nodes.find((n) => n.id === pc.model?.items[0]?.id) ?? null;
      } else if (e.key === "End") {
        const last = pc.model?.items[pc.model.items.length - 1];
        to = last ? (g.nodes.find((n) => n.id === last.id) ?? null) : null;
      }
      if (!to) return;
      e.preventDefault();
      const nid = to.id;
      pc.setFocusId(nid);
      const tree = e.currentTarget;
      requestAnimationFrame(() => {
        tree.querySelector<HTMLElement>(`[data-id="${CSS.escape(nid)}"]`)?.focus();
      });
    },
    [g, pc],
  );

  if (!model) return null;
  if (!g) return <div className="h-64" />;
  if (g.mode === "one") {
    return (
      <p className="p-3 text-muted-foreground text-sm">
        舞台太窄（小于 520px），关系图不提供，请使用清单。
      </p>
    );
  }

  const selTop = pc.sel ? pc.topId(pc.sel) : null;
  const related = new Set(selTop ? pc.related(selTop).map((r) => r.id) : []);
  const hotEdge = (e: { from: string; to: string; members: string[] }) =>
    !!selTop && (e.from === selTop || e.to === selTop || e.members.includes(selTop));
  const path = (pts: [number, number][]) => "M" + pts.map((p) => p.join(",")).join(" L");

  return (
    <div className="chain-map" ref={wrapRef}>
      <style>{MAP_CSS}</style>
      <div
        role="group"
        aria-label="装配关系图"
        className="relative"
        style={{ height: g.h, width: g.w }}
        onKeyDown={onKeyDown}
      >
        <svg
          className="pointer-events-none absolute inset-0"
          width={g.w}
          height={g.h}
          viewBox={`0 0 ${g.w} ${g.h}`}
          aria-hidden="true"
        >
          {g.ticks.map((t, i) => (
            <path
              key={`t${i}`}
              d={`M${t.x},${t.y1} L${t.x},${t.y2}`}
              className="fill-none stroke-muted-foreground"
              strokeWidth={1.5}
            />
          ))}
          {g.edges.map((e, i) => {
            const hot = hotEdge(e);
            const px = e.pts[e.pts.length - 2][0];
            const [tx, ty] = e.pts[e.pts.length - 1];
            const d = tx > px ? 1 : -1;
            const line = [...e.pts.slice(0, -1), [tx - 6 * d, ty]] as [number, number][];
            return (
              <g key={`e${i}`}>
                <path
                  d={path(line)}
                  className={cn("fill-none", hot ? "stroke-foreground" : "stroke-muted-foreground")}
                  strokeWidth={hot ? 2 : 1.5}
                />
                <path
                  d={`M${tx},${ty} L${tx - 8 * d},${ty - 4} L${tx - 8 * d},${ty + 4}Z`}
                  className={cn(hot ? "fill-foreground" : "fill-muted-foreground")}
                />
              </g>
            );
          })}
        </svg>
        {g.boxes.map((b) => (
          <div
            key={b.id}
            className="pointer-events-none absolute rounded-[10px] border"
            style={{ left: b.x, top: b.y, width: b.w, height: b.h }}
          >
            <span className="absolute top-1 left-2 text-muted-foreground text-xs">{b.label}</span>
          </div>
        ))}
        {g.caps.map((c) => (
          <span
            key={c.text}
            className="absolute whitespace-nowrap text-muted-foreground text-xs"
            style={{ left: c.x, top: c.y }}
          >
            {c.text}
          </span>
        ))}
        {g.boundary && (
          <div
            className="pointer-events-none absolute flex h-4 items-center gap-2 text-muted-foreground text-xs"
            style={{ left: g.boundary.x, top: g.boundary.y, width: g.boundary.w }}
          >
            <span className="h-px flex-1 bg-border" />
            <span className="whitespace-nowrap">以下由代码固定</span>
            <span className="h-px flex-1 bg-border" />
          </div>
        )}
        {model.items.map((it) => {
          const n = g.nodes.find((x) => x.id === it.id);
          if (!n) return null;
          const side = n.col === "left" || n.col === "right";
          const selected = selTop === n.id;
          const rel = related.has(n.id);
          const focusable = (pc.focusId ?? model.items[0]?.id) === n.id;
          const borderWidth = n.id === "model" ? "border-[3px]" : selected ? "border-2" : "border";
          const borderStyle = n.id === "model" ? "border-double" : n.col === "out" ? "border-dashed" : "";
          const borderTone = selected || rel ? "border-foreground" : "border-[var(--cn-border)]";
          return (
            <button
              key={n.id}
              type="button"
              data-id={n.id}
              data-chain-item
              data-selected={selected}
              aria-current={selected ? "true" : undefined}
              tabIndex={focusable ? 0 : -1}
              onClick={() => pc.select(n.id)}
              onFocus={() => pc.setFocusId(n.id)}
              style={{ left: n.x, top: n.y, width: n.w, height: n.h }}
              className={cn(
                "group absolute grid grid-cols-[minmax(0,1fr)_auto] content-center gap-x-2 rounded-md bg-card px-2 py-0.5 text-left outline-none",
                "focus-visible:ring-2 focus-visible:ring-foreground focus-visible:ring-offset-2",
                borderWidth,
                borderStyle,
                borderTone,
                selected ? "bg-accent" : "hover:bg-accent/60",
                n.col === "out" && !selected && "bg-transparent",
              )}
            >
              <span
                className={cn(
                  "min-w-0 truncate text-sm leading-[18px]",
                  side && "col-span-2",
                  selected && "font-medium",
                )}
              >
                {it.order != null && (
                  <span className="mr-1.5 font-mono text-muted-foreground text-xs group-hover:text-foreground">
                    {it.order}
                  </span>
                )}
                {it.title}
              </span>
              {!side && (
                <span className="text-right font-mono text-xs leading-[18px] tabular-nums">
                  {it.chars != null ? fmt(it.chars) : ""}
                </span>
              )}
              <span className="min-w-0 truncate text-muted-foreground text-xs leading-4 group-hover:text-foreground group-data-[selected=true]:text-foreground">
                {it.note}
              </span>
              <StatusWord
                state={it.state}
                word={it.stateWord}
                highlightOnHover
                className="justify-self-end leading-4"
              />
            </button>
          );
        })}
      </div>
    </div>
  );
}
