/**
 * 带冷却与抖动的整页重载。
 *
 * 换装后所有旧标签页会同时发现代次不一致：没有冷却会形成重载惊群，没有抖动
 * 会让它们在同一毫秒一起打新进程。
 *
 * 本模块刻意不 import 其它模块：`npm test` 用 node 直接跑 .ts，Node 的 ESM
 * 解析要求显式扩展名，而项目内约定是省略扩展名（webpack/tsc 解析）。保持零
 * 依赖就能同时被两边使用。
 */

/** 同一标签页两次自动重载的最小间隔。 */
export const RELOAD_COOLDOWN_MS = 30_000;

const STORAGE_KEY = "pojun:last-reload-at";

/** 本次页面生命周期内已排过一次重载。 */
let scheduled = false;

/**
 * 上次重载时刻；**null 表示存储不可用**。
 *
 * 存储不可用就没有跨载入冷却：`location.reload()` 会重置模块状态，若仍自动重载，
 * 一个"每次加载都重现"的失败（例如按需 chunk 每次都 404）会变成无限重载，用户连
 * 横幅都看不清。这种情况一律退化为"只提示，由用户点"。
 */
function readLastReloadAt(): number | null {
  try {
    const raw = window.sessionStorage.getItem(STORAGE_KEY);
    const n = raw ? Number(raw) : 0;
    return Number.isFinite(n) ? n : 0;
  } catch {
    return null;
  }
}

function writeLastReloadAt(at: number): void {
  try {
    window.sessionStorage.setItem(STORAGE_KEY, String(at));
  } catch {
    // 写入失败不影响本次重载：scheduled 已经置位，本页不会再排第二次。
  }
}

/**
 * 自动重载一次；冷却内、已排过、或存储不可用时返回 false（调用方应退化为只提示）。
 */
export function reloadOnce(): boolean {
  if (typeof window === "undefined") return false;
  if (scheduled) return false;
  const last = readLastReloadAt();
  if (last === null) return false;
  if (last > 0 && Date.now() - last < RELOAD_COOLDOWN_MS) return false;
  scheduled = true;
  writeLastReloadAt(Date.now());
  const jitter = Math.floor(Math.random() * 2000);
  window.setTimeout(() => window.location.reload(), jitter);
  return true;
}

/** 用户主动点击"重新加载"：无条件执行，不受冷却限制。 */
export function reloadNow(): void {
  if (typeof window === "undefined") return;
  window.location.reload();
}

/** 测试用：重置模块状态。 */
export function resetReloadGuardForTest(): void {
  scheduled = false;
}
