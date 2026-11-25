"use client";

import * as React from "react";

import { toast } from "sonner";
import { AlertTriangleIcon, RefreshCwIcon } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { api } from "@/lib/api";
import { cn } from "@/lib/utils";
import type { AgentLLMRoute, LLMRouteLayer, LLMRouteProfile } from "@/lib/types";

// Read-only LLM routing panel: the static precedence chain + provider pool and
// the task-scoped runtime transition history. It never writes anything and never
// shows credentials. Absent records are stated explicitly, never rendered blank.

// 界面只用中文标签；未知值回退原值，避免把内部枚举直接甩给用户。
const LAYER_LABEL: Record<string, string> = {
  pin: "任务/压缩器 pin",
  agent_binding: "Agent 绑定",
  task_chain: "任务配置链",
  global_profile: "全局激活配置",
  environment: "环境变量/内置",
};
const SOURCE_LABEL: Record<string, string> = {
  agent_binding: "Agent 绑定",
  task_chain: "任务配置链",
  global_profile: "全局激活",
  environment: "环境变量",
  preview_override: "预览覆盖",
  none: "未解析",
};
const CHAIN_STATUS_LABEL: Record<string, string> = {
  ready: "就绪",
  exhausted: "已耗尽",
  skipped: "已跳过",
  invalid: "配置无效",
};
const POOL_STATE_LABEL: Record<string, string> = { ok: "正常", degraded: "降级", tripped: "熔断" };
const MODE_LABEL: Record<string, string> = { automatic: "额度耗尽", manual: "手动切换", exhausted: "链耗尽" };
const WORKER_LABEL: Record<string, string> = { planner: "规划器", worker: "执行", mainagent: "主助手", goals: "目标拆解", chat: "对话" };
const FAILOVER_LABEL: Record<string, string> = { default: "未触发", ready: "链就绪", chain_exhausted: "链耗尽" };
const label = (m: Record<string, string>, v?: string) => (v ? (m[v] ?? v) : "");

function availabilityBadge(p?: LLMRouteProfile) {
  if (!p) return null;
  if (p.availability === "ok") return null;
  return (
    <Badge variant="outline" className="border-destructive/40 px-1.5 py-0 text-[10px] text-destructive">
      {p.availability === "missing_key" ? "缺少 Key" : "配置无效"}
    </Badge>
  );
}

function effortBadge(p?: LLMRouteProfile) {
  if (!p || !p.effort) return null;
  return (
    <Badge variant={p.effort_source === "override" ? "default" : "secondary"} className="px-1.5 py-0 text-[10px]">
      {p.effort}
      {p.effort_source === "override" ? " · 覆盖" : ""}
    </Badge>
  );
}

// 后端 precedence 自上而下即优先级（server/llm_route.go: "Precedence, top to bottom"），
// 运行时取第一个可用层，后面的层只作回退：Agent 绑定 → 任务配置链 → 全局激活 → 环境变量/内置。
// 两个例外：
//  - pin 层只钉住上下文压缩器（reason 自述“不影响该 Agent 的主调用”），不算主调用生效；
//  - 任务一旦有配置链，运行时就停在链上（游标耗尽/不可用即失败，见 server/task_resolution.go），
//    此时全局激活即使可用也不能标成“实际生效”。
function effectiveLayer(route: AgentLLMRoute): LLMRouteLayer | undefined {
  const pick = (layer: LLMRouteLayer["layer"]) =>
    route.precedence.find((l) => l.layer === layer && l.hit);
  const binding = pick("agent_binding");
  if (binding) return binding;
  if (route.scope === "task" && route.chain.length > 0) return pick("task_chain");
  return pick("task_chain") ?? pick("global_profile") ?? pick("environment");
}

function LayerRow({ layer, effective = false }: { layer: LLMRouteLayer; effective?: boolean }) {
  const p = layer.profile;
  return (
    <div
      className={cn(
        "flex flex-wrap items-center gap-2 rounded border px-2 py-1.5",
        effective && "border-emerald-500/50 bg-emerald-500/5",
      )}
    >
      <span
        className={cn(
          "size-2 shrink-0 rounded-full",
          layer.hit ? "bg-emerald-500" : "border border-muted-foreground/50 bg-transparent",
        )}
        aria-hidden
      />
      <span className="w-28 shrink-0 text-[11px]">{label(LAYER_LABEL, layer.layer)}</span>
      {effective && <Badge className="px-1.5 py-0 text-[10px]">实际生效</Badge>}
      {layer.applies_to && (
        <Badge variant="outline" className="px-1 py-0 text-[10px]">
          {layer.applies_to}
        </Badge>
      )}
      {p ? (
        <>
          <span className="min-w-0 max-w-[200px] truncate text-xs" title={p.name}>
            {p.name}
          </span>
          <span className="text-muted-foreground font-mono text-[11px]">
            {p.format} · {p.model}
          </span>
          {availabilityBadge(p)}
          {effortBadge(p)}
        </>
      ) : (
        <span className="text-muted-foreground text-xs">{layer.reason || "未命中"}</span>
      )}
      {p && layer.reason && <span className="text-muted-foreground text-[11px]">{layer.reason}</span>}
    </div>
  );
}

export function AgentLLMRoutePanel({ agentKey, active }: { agentKey: string; active: boolean }) {
  const [route, setRoute] = React.useState<AgentLLMRoute | null>(null);
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState("");
  const [taskId, setTaskId] = React.useState("");
  const reqRef = React.useRef(0);

  // autoPick 只在“进入页面/切换 Agent/从错误恢复”时为 true：用户显式选择
  // “只看静态层”后必须停在静态视图，不能被自动挑任务覆盖。
  const load = React.useCallback(
    async (tid: string, autoPick = false) => {
      const seq = ++reqRef.current;
      setLoading(true);
      setError("");
      try {
        const data = await api.agentLLMRoute(agentKey, tid ? { taskId: tid } : undefined);
        if (reqRef.current !== seq) return;
        setRoute(data);
        if (!tid && autoPick && data.tasks.length > 0) {
          const newest = data.tasks[0].id;
          setTaskId(newest);
          const withTask = await api.agentLLMRoute(agentKey, { taskId: newest });
          if (reqRef.current === seq) setRoute(withTask);
        }
      } catch (e) {
        if (reqRef.current === seq) {
          // 失败的任务可能已被归档：清掉选择，重试回到可用的静态/最新任务视图。
          setTaskId("");
          setRoute(null);
          setError((e as Error).message);
        }
      } finally {
        if (reqRef.current === seq) setLoading(false);
      }
    },
    [agentKey],
  );

  React.useEffect(() => {
    if (!active) return;
    setTaskId("");
    setRoute(null);
    void load("", true);
    // Agent 变化时 taskId 属于上一个 Agent，必须重置后再加载。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [active, agentKey, load]);

  if (!active) return null;

  const effective = route ? effectiveLayer(route) : undefined;

  if (error) {
    return (
      <div className="flex flex-1 items-center justify-center rounded-lg border border-destructive/40 p-6">
        <div className="grid gap-2 text-center">
          <p className="text-destructive text-sm">LLM 路由加载失败</p>
          <p className="text-muted-foreground text-xs">{error}</p>
          <Button size="sm" variant="outline" className="mx-auto w-fit" onClick={() => void load("", true)}>
            <RefreshCwIcon /> 重试
          </Button>
        </div>
      </div>
    );
  }

  return (
    <div className="min-h-0 flex-1 overflow-y-auto rounded-lg border p-3">
      <div className="mb-3 flex flex-wrap items-center gap-2">
        <span className="font-medium text-sm">LLM 路由与回退</span>
        <Badge variant="outline" className="px-1.5 py-0 text-[10px]">只读</Badge>
        {route && (
          <>
            <Badge variant="secondary" className="px-1.5 py-0 text-[10px]">
              {route.scope === "task" ? "任务视图" : "静态视图"}
            </Badge>
            <span className="text-muted-foreground text-[11px]">
              快照 {route.observed_at} · 回退状态：{label(FAILOVER_LABEL, route.failover_state) || "未触发"}
            </span>
          </>
        )}
        <div className="ml-auto flex items-center gap-2">
          <Select
            value={taskId || "__none__"}
            onValueChange={(v) => {
              const next = v === "__none__" ? "" : v;
              setTaskId(next);
              void load(next);
            }}
          >
            <SelectTrigger className="h-7 w-64 text-xs">
              <SelectValue placeholder="选择任务（运行时历史）" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="__none__">只看静态层（不选任务）</SelectItem>
              {(route?.tasks ?? []).map((t) => (
                <SelectItem key={t.id} value={t.id}>
                  #{t.id} {t.name || "未命名"} {t.status ? `· ${t.status}` : ""}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Button size="sm" variant="ghost" onClick={() => void load(taskId)} disabled={loading}>
            <RefreshCwIcon className={cn(loading && "animate-spin")} /> 刷新
          </Button>
        </div>
      </div>

      {!route ? (
        <p className="text-muted-foreground text-sm">{loading ? "加载中…" : "（无数据）"}</p>
      ) : (
        <div className="grid gap-4">
          {route.warnings.length > 0 && (
            <div className="rounded border border-amber-500/40 bg-amber-500/5 p-2 text-[11px] text-amber-700 dark:text-amber-400">
              {route.warnings.map((w) => (
                <p key={w}>· {w}</p>
              ))}
            </div>
          )}

          <section className="grid gap-1.5">
            <h3 className="font-medium text-xs">来源优先级（上 → 下）</h3>
            {effective ? (
              <p className="text-[11px]">
                实际生效：
                <span className="font-medium">{label(LAYER_LABEL, effective.layer)}</span>
                {effective.profile?.name && (
                  <span className="text-muted-foreground">{" · "}{effective.profile.name}</span>
                )}
              </p>
            ) : (
              <p className="text-destructive text-[11px]">无可用配置（运行时会失败）</p>
            )}
            {route.precedence.map((l, i) => (
              <LayerRow key={`${l.layer}-${i}`} layer={l} effective={l === effective} />
            ))}
          </section>

          <section className="grid gap-1.5">
            <h3 className="font-medium text-xs">各角色当前解析（静态）</h3>
            <div className="grid gap-1 sm:grid-cols-2">
              {Object.entries(route.roles).map(([role, r]) => (
                <div key={role} className="flex flex-wrap items-center gap-2 rounded border px-2 py-1.5">
                  <span className="w-20 shrink-0 font-mono text-[11px]">{role}</span>
                  <span className="text-xs">{r.name || "—"}</span>
                  <Badge variant="outline" className="px-1 py-0 text-[10px]">{label(SOURCE_LABEL, r.source)}</Badge>
                  {!r.available && (
                    <Badge
                      variant="outline"
                      className="border-destructive/40 px-1 py-0 text-[10px] text-destructive"
                      title={r.reason || undefined}
                    >
                      {r.availability === "missing_key" ? "缺少 Key" : "不可用"}
                    </Badge>
                  )}
                  {(r.format || r.model) && (
                    <span className="text-muted-foreground basis-full font-mono text-[10px]" title={r.reason || undefined}>
                      {[r.format, r.model].filter(Boolean).join(" · ")}
                    </span>
                  )}
                  {r.effort && (
                    <Badge variant={r.effort_source === "override" ? "default" : "secondary"} className="px-1 py-0 text-[10px]">
                      effort {r.effort}
                    </Badge>
                  )}
                </div>
              ))}
            </div>
          </section>

          <section className="grid gap-1.5">
            <h3 className="font-medium text-xs">任务配置链（用户顺序；只有额度耗尽才推进）</h3>
            {route.chain.length === 0 ? (
              <p className="text-muted-foreground text-xs">该任务没有显式配置链，运行时按绑定/全局解析。</p>
            ) : (
              route.chain.map((c) => (
                <div key={`${c.profile_id}-${c.position}`} className="flex flex-wrap items-center gap-2 rounded border px-2 py-1.5">
                  <span className="font-mono text-[11px]">#{c.position}</span>
                  <span className="min-w-0 max-w-[200px] truncate text-xs">{c.profile?.name || `配置 #${c.profile_id}`}</span>
                  <span className="text-muted-foreground font-mono text-[11px]">
                    {c.profile?.format} · {c.profile?.model}
                  </span>
                  <Badge variant={c.status === "ready" ? "secondary" : "outline"} className="px-1 py-0 text-[10px]">
                    {label(CHAIN_STATUS_LABEL, c.status)}
                  </Badge>
                  {c.is_cursor && (
                    <Badge className="px-1 py-0 text-[10px]">当前游标</Badge>
                  )}
                  {availabilityBadge(c.profile)}
                  {effortBadge(c.profile)}
                  {c.last_error && (
                    <span className="text-destructive/80 text-[10px]" title={c.last_error}>
                      {c.last_error.slice(0, 60)}
                    </span>
                  )}
                </div>
              ))
            )}
          </section>

          <section className="grid gap-1.5">
            <h3 className="font-medium text-xs">
              Provider 池（{route.pool.enabled ? "已启用" : "未启用"} · 包裹 {route.pool.wraps}）
            </h3>
            <p className="text-muted-foreground text-[11px]">{route.pool.order}；{route.pool.note}</p>
            <div className="grid gap-1">
              {route.pool.members.map((m) => (
                <div key={m.profile_id} className="flex flex-wrap items-center gap-2 rounded border px-2 py-1">
                  <span className="min-w-0 max-w-[180px] truncate text-xs">{m.name}</span>
                  <span className="text-muted-foreground font-mono text-[11px]">{m.format} · {m.model}</span>
                  <span className="text-muted-foreground text-[11px]">priority {m.priority}</span>
                  {m.active && <Badge variant="secondary" className="px-1 py-0 text-[10px]">激活</Badge>}
                  {m.excluded && <Badge variant="outline" className="px-1 py-0 text-[10px]">排除</Badge>}
                  {m.has_key === false && (
                    <Badge variant="outline" className="border-destructive/40 px-1 py-0 text-[10px] text-destructive">缺少 Key</Badge>
                  )}
                  {m.in_chain === false && <Badge variant="outline" className="px-1 py-0 text-[10px]">不在链上</Badge>}
                  <Badge
                    variant="outline"
                    className={cn(
                      "px-1 py-0 text-[10px]",
                      m.state === "tripped" && "border-destructive/50 text-destructive",
                      m.state === "degraded" && "border-amber-500/50 text-amber-600",
                    )}
                  >
                    {label(POOL_STATE_LABEL, m.state)}{m.cooldown_secs > 0 ? ` ${m.cooldown_secs}s` : ""}
                  </Badge>
                </div>
              ))}
            </div>
          </section>

          <section className="grid gap-1">
            <h3 className="font-medium text-xs">额度转跳规则</h3>
            <p className="text-muted-foreground text-[11px]">触发：{route.failover_policy.trigger}</p>
            <p className="text-muted-foreground text-[11px]">不触发：{route.failover_policy.does_not_trigger}</p>
            <p className="text-muted-foreground text-[11px]">推进：{route.failover_policy.advance_rule}</p>
            <p className="text-muted-foreground text-[11px]">重试/降级：{route.failover_policy.same_provider_retry}</p>
            <p className="text-muted-foreground text-[11px]">池选路：{route.failover_policy.pool_selection}</p>
          </section>

          <section className="grid gap-1.5">
            <div className="flex flex-wrap items-center gap-2">
              <h3 className="font-medium text-xs">最近一次实际使用</h3>
              <Badge variant="outline" className="px-1 py-0 text-[10px]">来自记账</Badge>
              <span className="text-muted-foreground text-[11px]">不代表当前游标</span>
            </div>
            {route.last_used ? (
              <div className="flex flex-wrap items-center gap-2 rounded border px-2 py-1.5">
                <span className="max-w-[220px] truncate text-xs" title={route.last_used.profile_name}>
                  {route.last_used.profile_name || "未记录名称"}
                </span>
                <span className="text-muted-foreground font-mono text-[11px]">{route.last_used.model || "—"}</span>
                <Badge variant="secondary" className="px-1 py-0 text-[10px]">{label(WORKER_LABEL, route.last_used.worker)}</Badge>
                {route.last_used.status && (
                  <Badge variant="outline" className="px-1 py-0 text-[10px]">{route.last_used.status}</Badge>
                )}
                <span className="text-muted-foreground text-[10px]">{route.last_used.at}</span>
              </div>
            ) : (
              <p className="text-muted-foreground text-xs">
                无记账记录（该任务尚未发生模型调用，或该角色不在 planner / worker / mainagent 记账泳道内）。
              </p>
            )}
            <p className="text-muted-foreground text-[10px]">
              {route.history_note || "记账不含 effort：同一配置的不同思考强度共用同一个 profile_name，故 effort 无法从记账恢复。"}
            </p>
          </section>

          <section className="grid gap-1.5">
            <div className="flex flex-wrap items-center gap-2">
              <h3 className="font-medium text-xs">运行期转跳历史</h3>
              <Badge variant="outline" className="px-1 py-0 text-[10px]">
                {route.history.scope === "task" ? "该任务" : "跨任务近似"}
              </Badge>
              <span className="text-muted-foreground text-[11px]">{route.history.retention_note}</span>
            </div>
            {route.history.state === "error" ? (
              <div className="rounded border border-destructive/40 p-2 text-xs text-destructive">
                读取审计失败：{route.history.error || "未知错误"}
              </div>
            ) : route.history.state === "no_records" ? (
              <div className="rounded border p-2 text-muted-foreground text-xs">
                没有额度转跳记录；同 provider 重试、thinking 降级与池内转移不写入审计。
              </div>
            ) : (
              <div className="grid gap-1">
                {route.history.items.map((t) => (
                  <div key={`${t.task_id}-${t.id}`} className="flex flex-wrap items-center gap-2 rounded border px-2 py-1.5">
                    <span className="font-mono text-[10px] text-muted-foreground">{t.created_at}</span>
                    <Badge
                      variant="outline"
                      className={cn(
                        "px-1 py-0 text-[10px]",
                        t.mode === "exhausted" && "border-destructive/50 text-destructive",
                        t.mode === "manual" && "border-sky-500/50 text-sky-600",
                      )}
                    >
                      {label(MODE_LABEL, t.mode) || "未记录"}
                    </Badge>
                    {t.previous || t.next || t.mode ? (
                      <span className="text-xs">
                        {t.previous?.name || "默认"} →{" "}
                        {t.next?.name || (t.mode === "exhausted" ? "（链耗尽）" : "（目标未记录）")}
                      </span>
                    ) : (
                      <span className="text-muted-foreground text-[11px]">
                        该记录缺少转跳元数据（旧版本或外部写入），无法还原前后配置
                      </span>
                    )}
                    {t.previous?.effort && (
                      <span className="text-muted-foreground text-[10px]">
                        旧 effort {t.previous.effort}{t.previous.effort_source === "override" ? "（覆盖）" : ""}
                      </span>
                    )}
                    {t.next?.effort && (
                      <span className="text-muted-foreground text-[10px]">
                        新 effort {t.next.effort}{t.next.effort_source === "override" ? "（覆盖）" : ""}
                      </span>
                    )}
                    {!t.previous?.effort && !t.next?.effort && (
                      <span className="text-muted-foreground text-[10px]">effort 未记录</span>
                    )}
                    {t.reason && (
                      <span className="min-w-0 max-w-full truncate text-[10px] text-muted-foreground" title={t.reason}>
                        {t.reason}
                      </span>
                    )}
                  </div>
                ))}
              </div>
            )}
            {route.history.has_more && (
              <Button
                size="sm"
                variant="ghost"
                className="w-fit text-xs"
                onClick={async () => {
                  try {
                    const more = await api.agentLLMRoute(agentKey, {
                      taskId: taskId || undefined,
                      before: route.history.earliest_cursor,
                    });
                    setRoute({ ...route, history: { ...more.history, items: [...more.history.items, ...route.history.items] } });
                  } catch (e) {
                    toast.error(`加载更早记录失败：${(e as Error).message}`);
                  }
                }}
              >
                加载更早
              </Button>
            )}
          </section>

          <p className="text-muted-foreground text-[10px]">
            不可观测项：池内部转移与同 provider 重试不产生审计事件；静态可用性只表示配置完整，未验证连通性；最近一次实际使用来自记账，不代表当前游标。
          </p>
        </div>
      )}
    </div>
  );
}
