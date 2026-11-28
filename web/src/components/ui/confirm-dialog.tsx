"use client";

import * as React from "react";

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";

/**
 * 应用内确认框（替代 window.confirm）。
 *
 * 为什么不用 window.confirm：
 *   - 原生对话框用的是浏览器的外观与语言，和产品界面完全脱节；
 *   - Chrome 的「阻止此页面创建更多对话框」会让 confirm() 直接返回 false，
 *     调用方看起来就是"点了没反应"（关系图/LLM 路由回归里踩过）；
 *   - 原生对话框在移动端会被部分浏览器改写，文案与按钮顺序不可控。
 *
 * 用法：
 *   const confirm = useConfirm();
 *   if (!(await confirm({ title: "删除？", destructive: true }))) return;
 *
 * 需要 ConfirmProvider 在树上（已挂在根布局）。
 */
export interface ConfirmOptions {
  title: string;
  /** 支持 \n 换行（会按 pre-line 渲染）。需要富文本时传 ReactNode。 */
  description?: React.ReactNode;
  confirmText?: string;
  cancelText?: string;
  /** 危险操作：确认按钮变红、默认焦点落在"取消"上。 */
  destructive?: boolean;
  /** 描述较长（如点名一串受影响的 Agent）时放宽到 max-w-lg，避免挤成窄条。 */
  wide?: boolean;
}

type ConfirmFn = (options: ConfirmOptions) => Promise<boolean>;

const ConfirmContext = React.createContext<ConfirmFn | null>(null);

export function ConfirmProvider({ children }: { children: React.ReactNode }) {
  const [options, setOptions] = React.useState<ConfirmOptions | null>(null);
  const resolverRef = React.useRef<((ok: boolean) => void) | null>(null);
  const cancelRef = React.useRef<HTMLButtonElement>(null);

  const confirm = React.useCallback<ConfirmFn>((next) => {
    // 上一个还没结论就被新调用覆盖：先把旧的按"取消"结束，避免调用方永远等待。
    resolverRef.current?.(false);
    setOptions(next);
    return new Promise<boolean>((resolve) => {
      resolverRef.current = resolve;
    });
  }, []);

  const settle = React.useCallback((ok: boolean) => {
    const resolve = resolverRef.current;
    resolverRef.current = null;
    setOptions(null);
    resolve?.(ok);
  }, []);

  // 卸载时把还挂着的调用方按"取消"结束，否则它的 await 永远不会返回。
  React.useEffect(
    () => () => {
      resolverRef.current?.(false);
      resolverRef.current = null;
    },
    [],
  );

  // 关闭动画期间 options 已经是 null，但内容还要再渲染几十毫秒；
  // 用最后一次的选项渲染，避免标题/描述在退场时闪成空白。
  const lastRef = React.useRef<ConfirmOptions | null>(null);
  if (options) lastRef.current = options;
  const view = options ?? lastRef.current;

  const destructive = view?.destructive === true;
  const hasDescription = view?.description !== undefined && view?.description !== null && view?.description !== "";

  return (
    <ConfirmContext.Provider value={confirm}>
      {children}
      <AlertDialog
        open={options !== null}
        onOpenChange={(open) => {
          if (!open) settle(false);
        }}
      >
        <AlertDialogContent
          // 默认尺寸的 max-w 写在 data-[size=default] 变体里，特异性高于裸 max-w-*，
          // 所以放宽宽度也必须带同一前缀，否则不生效。
          className={view?.wide ? "data-[size=default]:max-w-md data-[size=default]:sm:max-w-lg" : undefined}
          // 没有描述时不生成 aria-describedby（Radix 对缺失描述会告警）。
          {...(hasDescription ? {} : { "aria-describedby": undefined })}
          onOpenAutoFocus={(event) => {
            // 危险操作默认焦点给"取消"：误按回车不该删东西。
            if (!destructive) return;
            event.preventDefault();
            cancelRef.current?.focus();
          }}
        >
          <AlertDialogHeader>
            <AlertDialogTitle>{view?.title ?? ""}</AlertDialogTitle>
            {hasDescription ? (
              <AlertDialogDescription className="whitespace-pre-line">{view?.description}</AlertDialogDescription>
            ) : null}
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel ref={cancelRef} onClick={() => settle(false)}>
              {view?.cancelText ?? "取消"}
            </AlertDialogCancel>
            <AlertDialogAction
              variant={destructive ? "destructive-solid" : "default"}
              onClick={() => settle(true)}
            >
              {view?.confirmText ?? "确定"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </ConfirmContext.Provider>
  );
}

export function useConfirm(): ConfirmFn {
  const ctx = React.useContext(ConfirmContext);
  if (!ctx) throw new Error("useConfirm 必须在 ConfirmProvider 内使用（根布局已挂载）");
  return ctx;
}
