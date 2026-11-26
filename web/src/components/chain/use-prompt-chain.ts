"use client";

import * as React from "react";

import { toast } from "sonner";

import { api } from "@/lib/api";
import { buildChainModel, type ChainItem, type ChainModel } from "@/lib/chain-model";
import type { AgentPromptChain as AgentPromptChainData } from "@/lib/types";

// Go 的 nil slice 序列化成 null；mock 模式未知路由返回 {}。取数后统一补形，
// 下游的 .map/.length/filter 才不用各自判空。
function normalizeChain(data: AgentPromptChainData): AgentPromptChainData {
  const tools = data.tools ?? {
    builtin: [],
    custom: [],
    traffic: [],
    shell: [],
    deferred_names: [],
    excluded: [],
    total: 0,
  };
  const llm = data.llm ?? {
    profile_id: null,
    name: "",
    model: "",
    format: "",
    source: "none",
    source_label: "未解析",
    prompt_mode: "",
    prompt_chars: 0,
    prompt_apply_inherited: false,
    prompt_active: false,
  };
  return {
    ...data,
    agent_key: data.agent_key ?? "",
    agent_kind: data.agent_kind ?? "builtin",
    llm,
    totals: data.totals ?? { segments: 0, chars: 0, est_tokens: 0 },
    segments: data.segments ?? [],
    mcp: (data.mcp ?? []).map((m) => ({ ...m, tools: m.tools ?? [] })),
    skills: (data.skills ?? []).map((s) => ({
      ...s,
      mcp_gated: s.mcp_gated ?? [],
      index_state: s.index_state ?? (s.in_index ? "described" : "absent"),
    })),
    warnings: data.warnings ?? [],
    static_only: data.static_only ?? true,
    prompt_max_runes: data.prompt_max_runes ?? 0,
    tools: {
      ...tools,
      builtin: tools.builtin ?? [],
      custom: tools.custom ?? [],
      traffic: tools.traffic ?? [],
      shell: tools.shell ?? [],
      deferred_names: tools.deferred_names ?? [],
      excluded: tools.excluded ?? [],
      total: tools.total ?? 0,
    },
  };
}

export interface PromptChainController {
  chain: AgentPromptChainData | null;
  model: ChainModel | null;
  loading: boolean;
  error: string;
  /** 已有数据时刷新失败：旧数据还在，但可能过期。 */
  stale: boolean;
  sel: string | null;
  select: (id: string | null) => void;
  expanded: ReadonlySet<string>;
  toggle: (id: string) => void;
  focusId: string | null;
  setFocusId: (id: string) => void;
  item: (id: string | null) => ChainItem | undefined;
  parentOf: (id: string) => ChainItem | undefined;
  /** 成员属于某个分组时给出分组 id；否则就是它自己。 */
  topId: (id: string) => string;
  related: (id: string) => ChainItem[];
  /** 选中并展开分组，必要时把舞台滚到它。 */
  reveal: (id: string) => void;
  reload: () => void;
  copyText: (text: string, what: string) => Promise<void>;
  copyStatic: () => Promise<void>;
  stageRef: React.RefObject<HTMLDivElement | null>;
  setDirty: (v: boolean) => void;
  /** 有未保存草稿时先确认；返回是否继续。 */
  confirmDiscard: () => boolean;
}

export function usePromptChain({
  agentKey,
  profileId,
  active,
  reloadKey,
  sel,
  onSelect,
}: {
  agentKey: string;
  profileId?: string;
  active: boolean;
  reloadKey?: number;
  sel: string | null;
  onSelect: (id: string | null) => void;
}): PromptChainController {
  const [chain, setChain] = React.useState<AgentPromptChainData | null>(null);
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState("");
  const [stale, setStale] = React.useState(false);
  const [expanded, setExpanded] = React.useState<ReadonlySet<string>>(() => new Set());
  const [focusId, setFocusIdState] = React.useState<string | null>(null);
  const stageRef = React.useRef<HTMLDivElement>(null);
  const reqRef = React.useRef(0);
  const dirtyRef = React.useRef(false);
  // 用户此刻真正在看的那组 key。load() 读这个 ref，而不是闭包里的 agentKey/profileId：
  // 保存回调可能被尚未卸载的编辑区持有，切换 Agent 后若还用旧 key 重载，就会以最新
  // 序号通过校验、把上一个 Agent 的链写进当前页面（且不会自愈）。
  const keyRef = React.useRef({ agentKey, profileId });
  keyRef.current = { agentKey, profileId };

  const model = React.useMemo(() => (chain ? buildChainModel(chain) : null), [chain]);

  // 声明在 loader 之前：挂载时先跑它（序号 1），load 取序号 2；切换 key 时它先让
  // 在途的旧请求失效，再开始新请求。
  React.useEffect(() => {
    reqRef.current++;
    setChain(null);
    setExpanded(new Set());
    setFocusIdState(null);
    setStale(false);
    setLoading(false);
  }, [agentKey, profileId]);

  const load = React.useCallback(async (keepData = false) => {
    const started = keyRef.current;
    const seq = ++reqRef.current;
    setLoading(true);
    setError("");
    try {
      const data = await api.agentPromptChain(started.agentKey, started.profileId);
      if (reqRef.current !== seq) return;
      const now = keyRef.current;
      if (now.agentKey !== started.agentKey || now.profileId !== started.profileId) return;
      // 纵深防御：任何指名了别的 Agent 的载荷都不渲染。
      if (data.agent_key && data.agent_key !== started.agentKey) return;
      setChain(normalizeChain(data));
      setStale(false);
    } catch (e) {
      if (reqRef.current !== seq) return;
      if (keepData) {
        setStale(true);
        toast.error(`刷新装配链失败：${(e as Error).message}`);
      } else {
        setChain(null);
        setError((e as Error).message);
      }
    } finally {
      if (reqRef.current === seq) setLoading(false);
    }
  }, []);

  React.useEffect(() => {
    if (active) void load();
  }, [active, agentKey, profileId, reloadKey, load]);

  const item = React.useCallback(
    (id: string | null): ChainItem | undefined => {
      if (!model || !id) return undefined;
      for (const it of model.items) {
        if (it.id === id) return it;
        const kid = it.children?.find((c) => c.id === id);
        if (kid) return kid;
      }
      return undefined;
    },
    [model],
  );

  const parentOf = React.useCallback(
    (id: string): ChainItem | undefined => model?.items.find((it) => it.children?.some((c) => c.id === id)),
    [model],
  );

  const topId = React.useCallback(
    (id: string): string => parentOf(id)?.id ?? id,
    [parentOf],
  );

  const related = React.useCallback(
    (id: string): ChainItem[] => {
      if (!model) return [];
      const it = item(id);
      if (!it) return [];
      const ids = new Set<string>();
      if (it.feeds) ids.add(it.feeds);
      for (const other of model.items) {
        if (other.feeds === it.id) ids.add(other.id);
      }
      ids.delete(it.id);
      return [...ids].map((x) => item(x)).filter((x): x is ChainItem => !!x);
    },
    [model, item],
  );

  const confirmDiscard = React.useCallback((): boolean => {
    if (!dirtyRef.current) return true;
    const go = window.confirm("独立提示词有未保存的修改，离开会丢失。继续？");
    if (go) dirtyRef.current = false;
    return go;
  }, []);

  const select = React.useCallback(
    (id: string | null) => {
      // 换一个对象（含回到总览）先拦未保存草稿；再点已选中的对象不拦也不变。
      if (dirtyRef.current && id !== sel && !confirmDiscard()) return;
      if (id) {
        const parent = parentOf(id);
        if (parent) {
          setExpanded((prev) => (prev.has(parent.id) ? prev : new Set(prev).add(parent.id)));
        }
        setFocusIdState(id);
      }
      onSelect(id);
    },
    [onSelect, parentOf, sel, confirmDiscard],
  );

  // 刷新后选中的对象可能不存在了：清空选中。成员被选中时展开它所属的分组。
  React.useEffect(() => {
    if (!model || !sel) return;
    if (!item(sel)) {
      onSelect(null);
      return;
    }
    const parent = parentOf(sel);
    if (parent) {
      setExpanded((prev) => (prev.has(parent.id) ? prev : new Set(prev).add(parent.id)));
    }
  }, [model, sel, item, parentOf, onSelect]);

  // focusId 指向已消失的对象（解绑 Skill、段被替换）时清空：否则清单与关系图都
  // 没有 tabIndex=0 的落点，Tab 键直接跳过整块内容。
  React.useEffect(() => {
    if (focusId && !item(focusId)) setFocusIdState(null);
  }, [model, focusId, item]);

  const toggle = React.useCallback((id: string) => {
    setExpanded((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const reveal = React.useCallback(
    (id: string) => {
      select(id);
      requestAnimationFrame(() => {
        stageRef.current
          ?.querySelector<HTMLElement>(`[data-id="${CSS.escape(id)}"]`)
          ?.scrollIntoView({ block: "nearest" });
      });
    },
    [select],
  );

  const copyText = React.useCallback(async (text: string, what: string) => {
    if (!text) {
      toast.error("没有可复制的内容");
      return;
    }
    try {
      await navigator.clipboard.writeText(text);
      toast.success(`已复制${what}`);
    } catch {
      toast.error("复制失败：浏览器拒绝了剪贴板访问");
    }
  }, []);

  const copyStatic = React.useCallback(async () => {
    if (!chain) return;
    const text = chain.segments
      .filter((s) => s.present && s.available && s.text)
      .map((s) => s.text)
      .join("");
    await copyText(text, "静态装配提示词（不含运行时段）");
  }, [chain, copyText]);

  const setDirty = React.useCallback((v: boolean) => {
    dirtyRef.current = v;
  }, []);

  React.useEffect(() => {
    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      if (!dirtyRef.current) return;
      e.preventDefault();
      e.returnValue = "";
    };
    window.addEventListener("beforeunload", onBeforeUnload);
    return () => window.removeEventListener("beforeunload", onBeforeUnload);
  }, []);

  const setFocusId = React.useCallback((id: string) => setFocusIdState(id), []);

  return {
    chain,
    model,
    loading,
    error,
    stale,
    sel,
    select,
    expanded,
    toggle,
    focusId,
    setFocusId,
    item,
    parentOf,
    topId,
    related,
    reveal,
    reload: () => void load(true),
    copyText,
    copyStatic,
    stageRef,
    setDirty,
    confirmDiscard,
  };
}
