// 思考强度（reasoning_effort）的共享常量与哨兵转换。
// 存库空字符串 = 不发送；Radix Select 不接受空 value，UI 用 "none" 哨兵表示
// 不发送/继承，存取时与 "" 互转。LLM 配置页与 Agent 覆盖区块共用这一份，
// 避免两处档位或哨兵口径漂移。

export const NONE = "none";

export const fromStore = (v?: string) => (v ? v : NONE);
export const toStore = (v: string) => (v === NONE ? "" : v);

export const EFFORT_VALUES = ["low", "medium", "high", "xhigh", "max"] as const;

export const EFFORT_LEVELS: { value: string; label: string }[] = [
  { value: NONE, label: "继承配置（默认）" },
  ...EFFORT_VALUES.map((v) => ({ value: v, label: v })),
];

export function effortLabel(v?: string): string {
  if (!v) return "未设置";
  return EFFORT_LEVELS.find((e) => e.value === v)?.label ?? v;
}

export type EffortSource = "override" | "profile" | "unset";
