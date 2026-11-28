/**
 * 页面级"未保存草稿"标记。
 *
 * 单独成模块是因为 BuildGuard（挂在根布局）要知道链页有没有草稿，而草稿状态
 * 藏在 usePromptChain 的 ref 里。两边都写这个模块级标记，就不必把状态提升到
 * 全局 store。
 *
 * 默认 false 只在链页被置为 true；其它页面不写草稿。BuildGuard 另外用
 * `isChainPath()` 兜底：不在链页时本来就不自动重载。
 */
let dirty = false;

export function setAppDirty(value: boolean): void {
  dirty = value;
}

export function isAppDirty(): boolean {
  return dirty;
}
