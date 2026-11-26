"use client";

import { CircleDashedIcon, CircleIcon, CircleXIcon, TriangleAlertIcon } from "lucide-react";

import { cn } from "@/lib/utils";

import type { ItemState } from "@/lib/chain-model";

// 五种状态：生效不配图标；未生效空心圆；运行时虚线圆；需注意三角；错误圆叉。
const ICON = {
  active: null,
  inactive: CircleIcon,
  runtime: CircleDashedIcon,
  attention: TriangleAlertIcon,
  error: CircleXIcon,
} as const;

export function StatusWord({
  state,
  word,
  highlightOnHover,
  className,
}: {
  state: ItemState;
  word: string;
  /** 悬停与选中时，中性的次要文字升为主文字色；琥珀与红保持不变。 */
  highlightOnHover?: boolean;
  className?: string;
}) {
  if (!word) return null;
  const Icon = ICON[state];
  const tone =
    state === "attention"
      ? "text-amber-700 dark:text-amber-400"
      : state === "error"
        ? "text-red-700 dark:text-red-400"
        : "text-muted-foreground";
  const neutral = state !== "attention" && state !== "error";
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1 whitespace-nowrap text-xs",
        tone,
        neutral && highlightOnHover && "group-hover:text-foreground group-data-[selected=true]:text-foreground",
        className,
      )}
    >
      {Icon && <Icon className="size-3.5 shrink-0" aria-hidden="true" />}
      {word}
    </span>
  );
}
