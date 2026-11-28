"use client";

import * as React from "react";

import { ChevronRightIcon } from "lucide-react";

import { GROUP_LABEL, type ChainItem } from "@/lib/chain-model";
import { cn } from "@/lib/utils";

import { StatusWord } from "./chain-status";
import type { PromptChainController } from "./use-prompt-chain";

const fmt = (n: number) => n.toLocaleString("en-US");

// 列宽：展开与序号 40、标题 152、说明弹性、状态 76、字符 64。
const ROW_GRID = "grid-cols-[40px_152px_minmax(0,1fr)_76px_64px]";

function Row({ pc, item, level }: { pc: PromptChainController; item: ChainItem; level: 1 | 2 }) {
  const kids = level === 1 ? item.children : undefined;
  const hasKids = !!kids && kids.length > 0;
  const open = hasKids ? pc.expanded.has(item.id) : false;
  const selected = pc.sel === item.id;
  const focused = (pc.focusId ?? pc.model?.items[0]?.id) === item.id;
  const [ringVisible, setRingVisible] = React.useState(false);
  const maxChars = Math.max(1, pc.model?.maxChars ?? 1);
  const bar = item.chars != null && item.id !== "model" ? Math.max(5, Math.round((item.chars / maxChars) * 100)) : 0;
  const label = [
    item.order ? `第 ${item.order} 位` : "",
    item.title,
    item.note,
    item.stateWord,
    item.chars != null ? `${item.chars} 字符` : "",
  ]
    .filter(Boolean)
    .join("，");

  return (
    <div
      role="treeitem"
      aria-level={level}
      aria-selected={selected}
      aria-expanded={hasKids ? open : undefined}
      aria-label={label}
      data-id={item.id}
      data-chain-item
      data-selected={selected}
      tabIndex={focused ? 0 : -1}
      className="outline-none"
      onClick={(e) => {
        // 成员行在分组 treeitem 内部：只处理落在本行上的点击，别让冒泡改成选中分组。
        const own = (e.target as HTMLElement).closest('[role="treeitem"]');
        if (own !== e.currentTarget) return;
        pc.select(item.id);
      }}
      onFocus={(e) => {
        pc.setFocusId(item.id);
        setRingVisible(e.currentTarget.matches(":focus-visible"));
      }}
      onBlur={() => setRingVisible(false)}
    >
      <div
        data-selected={selected}
        className={cn(
          "group relative grid items-center gap-x-2 rounded-md pr-3 pl-1.5 hover:bg-accent/60",
          ROW_GRID,
          level === 1 ? "h-9" : "h-8",
          item.id === "model" && "my-1 rounded-none border-y",
          selected && "bg-accent",
          selected &&
            "after:absolute after:inset-y-1.5 after:left-0 after:w-0.5 after:rounded after:bg-foreground after:content-['']",
          ringVisible && "ring-2 ring-foreground ring-inset",
        )}
      >
        <span className="flex items-center">
          <span
            aria-hidden="true"
            className={cn(
              "flex size-6 items-center justify-center rounded text-muted-foreground",
              hasKids ? "hover:bg-accent hover:text-foreground" : "invisible",
            )}
            onClick={(e) => {
              if (!hasKids) return;
              e.stopPropagation();
              pc.toggle(item.id);
            }}
          >
            <ChevronRightIcon className={cn("size-3.5 transition-transform", open && "rotate-90")} />
          </span>
          <span className="w-4 text-center font-mono text-muted-foreground text-xs tabular-nums group-hover:text-foreground group-data-[selected=true]:text-foreground">
            {item.order ?? (item.id === "model" ? "=" : "")}
          </span>
        </span>
        <span
          className={cn(
            "truncate text-sm",
            level === 2 && "pl-3.5",
            selected && "font-medium",
          )}
          title={item.title}
        >
          {item.title}
        </span>
        <span className="truncate text-muted-foreground text-xs group-hover:text-foreground group-data-[selected=true]:text-foreground">
          {item.note}
        </span>
        <StatusWord state={item.state} word={item.stateWord} highlightOnHover />
        <span className="relative text-right font-mono text-xs tabular-nums">
          {item.chars != null ? fmt(item.chars) : "—"}
          {bar > 0 && (
            <span
              aria-hidden="true"
              className="absolute right-0 -bottom-1.5 h-[3px] rounded-full bg-muted-foreground"
              style={{ width: `${bar}%` }}
            />
          )}
        </span>
      </div>
      {open && (
        <div role="group">
          {kids!.map((c) => (
            <Row key={c.id} pc={pc} item={c} level={2} />
          ))}
        </div>
      )}
    </div>
  );
}

function GroupHeader({ label, showChars }: { label: string; showChars?: boolean }) {
  return (
    <div
      aria-hidden="true"
      className={cn("grid h-6 items-center gap-x-2 pr-3 pl-1.5 text-muted-foreground text-xs", ROW_GRID)}
    >
      <span className="col-span-4 pl-1.5">{label}</span>
      {showChars && <span className="text-right">字符</span>}
    </div>
  );
}

export function ChainList({ pc }: { pc: PromptChainController }) {
  const model = pc.model;

  const onKeyDown = React.useCallback(
    (e: React.KeyboardEvent<HTMLDivElement>) => {
      const tree = e.currentTarget;
      const cur = (e.target as HTMLElement).closest<HTMLElement>('[role="treeitem"]');
      const id = cur?.dataset.id;
      if (!cur || !id) return;
      const items = Array.from(tree.querySelectorAll<HTMLElement>('[role="treeitem"]'));
      const i = items.indexOf(cur);
      const it = pc.item(id);
      if (!it) return;
      const focus = (nid: string | undefined) => {
        if (!nid) return;
        e.preventDefault();
        pc.setFocusId(nid);
        requestAnimationFrame(() => {
          tree
            .querySelector<HTMLElement>(`[role="treeitem"][data-id="${CSS.escape(nid)}"]`)
            ?.focus();
        });
      };
      const kids = it.children?.length ? it.children : undefined;
      if (e.key === "ArrowDown") focus(items[i + 1]?.dataset.id ?? id);
      else if (e.key === "ArrowUp") focus(items[i - 1]?.dataset.id ?? id);
      else if (e.key === "Home") focus(items[0]?.dataset.id);
      else if (e.key === "End") focus(items[items.length - 1]?.dataset.id);
      else if (e.key === "ArrowRight") {
        if (!kids) return;
        if (!pc.expanded.has(id)) {
          e.preventDefault();
          pc.toggle(id);
        } else focus(kids[0].id);
      } else if (e.key === "ArrowLeft") {
        if (kids && pc.expanded.has(id)) {
          e.preventDefault();
          pc.toggle(id);
        } else focus(pc.parentOf(id)?.id);
      } else if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        pc.select(id);
      }
    },
    [pc],
  );

  if (!model) return null;
  const byGroup = (g: ChainItem["group"]) => model.items.filter((i) => i.group === g);
  const sys = byGroup("system");
  const bi = sys.findIndex((i) => i.boundaryBefore);
  const sysA = bi > 0 ? sys.slice(0, bi) : sys;
  const sysB = bi > 0 ? sys.slice(bi) : [];

  const section = (key: string, label: string, items: ChainItem[], showChars?: boolean) =>
    items.length > 0 ? (
      <div key={key} role="group" aria-label={label}>
        <GroupHeader label={label} showChars={showChars} />
        {items.map((it) => (
          <Row key={it.id} pc={pc} item={it} level={1} />
        ))}
      </div>
    ) : null;

  return (
    <div role="tree" aria-label="装配清单" className="pb-4" onKeyDown={onKeyDown}>
      {section("source", GROUP_LABEL.source, byGroup("source"))}
      {section("system", GROUP_LABEL.system, sysA, true)}
      {sysB.length > 0 && (
        <div role="group" aria-label="system 提示词 · 以下由代码固定">
          <div
            aria-hidden="true"
            className="flex h-4 items-center gap-2 pr-3 pl-[54px] text-muted-foreground text-xs"
          >
            <span className="h-px flex-1 bg-border" />
            <span className="whitespace-nowrap">以下由代码固定，配置改不了</span>
            <span className="h-px flex-1 bg-border" />
          </div>
          {sysB.map((it) => (
            <Row key={it.id} pc={pc} item={it} level={1} />
          ))}
        </div>
      )}
      {section("sidecar", GROUP_LABEL.sidecar, byGroup("sidecar"))}
      <div role="group" aria-label={GROUP_LABEL.result}>
        {byGroup("result").map((it) => (
          <Row key={it.id} pc={pc} item={it} level={1} />
        ))}
      </div>
      {section("excluded", GROUP_LABEL.excluded, byGroup("excluded"))}
    </div>
  );
}
