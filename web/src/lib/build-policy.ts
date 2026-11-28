/**
 * 构建身份与资源失败的纯判定逻辑。
 *
 * 除 `readEmbeddedBuildId()` 外不碰 DOM / fetch，方便用
 * `node --experimental-strip-types --test` 直接测；组件只负责把浏览器事件喂进来。
 */

/** 服务端注入的 `<meta name="pojun-build">` 与 `/_pojun_build.txt` 都是这个形状。 */
export const BUILD_ID_RE = /^[0-9a-f]{16}$/;
/** 存活页面自检的间隔（仅在页面可见时计时）。 */
export const BUILD_POLL_INTERVAL_MS = 60_000;

/** 只有刚打开的页面才自动重载：用户还没开始操作的窗口。 */
export const AUTO_RELOAD_MAX_AGE_MS = 15_000;

export const CHAIN_PATH = "/system/agents/chain";

export function isValidBuildId(value: unknown): value is string {
  return typeof value === "string" && BUILD_ID_RE.test(value);
}

/**
 * 把服务端给的原始文本/属性收敛成合法身份。
 *
 * 必须严格校验：旧服务端会把任意缺失路径（包括 `/_pojun_build.txt`）回成 HTML，
 * 如果只做 trim 就会被当成"新代次"，让每个标签页永久顶一条过期横幅。
 */
export function normalizeBuildId(raw: string | null | undefined): string | null {
  if (typeof raw !== "string") return null;
  const value = raw.trim();
  return isValidBuildId(value) ? value : null;
}

const CHUNK_FAILURE_RE =
  /ChunkLoadError|Loading chunk|error loading dynamically imported module|Failed to fetch dynamically imported module|Importing a module script failed|Unable to preload CSS/i;

/** Webpack 的 ChunkLoadError 与 Vite/ESM 的动态 import 失败都算"资源代次失配"族。 */
export function isChunkLoadFailure(message: string | null | undefined, name?: string | null): boolean {
  if (name === "ChunkLoadError") return true;
  return typeof message === "string" && message !== "" && CHUNK_FAILURE_RE.test(message);
}

/** 只关心 Next 自己的产物；图片、用户上传等失败不该触发重载。 */
export function isNextAssetUrl(url: string | null | undefined): boolean {
  return typeof url === "string" && url.includes("/_next/");
}

export function isChainPath(pathname: string | null | undefined): boolean {
  if (typeof pathname !== "string") return false;
  const normalized = pathname.replace(/\/+$/, "");
  return normalized === CHAIN_PATH || normalized.startsWith(`${CHAIN_PATH}/`);
}

/**
 * 本页自己加载的那一代前端身份。页面由服务端注入 meta；`next dev` 或老版本
 * 服务端没有它，返回 null 表示"无法判定"（所有基于代次的自动行为都应停用）。
 * 这是本模块唯一的 DOM 访问，其余函数都是纯函数。
 */
export function readEmbeddedBuildId(): string | null {
  if (typeof document === "undefined") return null;
  const meta = document.querySelector<HTMLMetaElement>('meta[name="pojun-build"]');
  return normalizeBuildId(meta?.content);
}

/**
 * 是否允许自动重载（而不是只提示）。
 *
 * 约束来自"自动重载不能丢用户输入"：只对链页、且没有未保存草稿、且页面刚打开
 * （用户还没来得及动手）时自动重载；其余一律只出横幅，由用户点。
 */
export function shouldAutoReload({
  pathname,
  dirty,
  pageAgeMs,
}: {
  pathname: string | null | undefined;
  dirty: boolean;
  pageAgeMs: number;
}): boolean {
  if (dirty) return false;
  if (!isChainPath(pathname)) return false;
  return Number.isFinite(pageAgeMs) && pageAgeMs >= 0 && pageAgeMs < AUTO_RELOAD_MAX_AGE_MS;
}
