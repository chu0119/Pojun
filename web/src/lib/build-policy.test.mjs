import {
  AUTO_RELOAD_MAX_AGE_MS,
  BUILD_POLL_INTERVAL_MS,
  isChainPath,
  isChunkLoadFailure,
  isNextAssetUrl,
  isValidBuildId,
  normalizeBuildId,
  shouldAutoReload,
} from "./build-policy.ts";
import { RELOAD_COOLDOWN_MS, reloadOnce, resetReloadGuardForTest } from "./reload-guard.ts";
import assert from "node:assert/strict";
import test from "node:test";

test("build ids are exactly 16 lowercase hex chars", () => {
  assert.equal(isValidBuildId("0123456789abcdef"), true);
  assert.equal(isValidBuildId("0123456789ABCDEF"), false);
  assert.equal(isValidBuildId("0123456789abcde"), false);
  assert.equal(isValidBuildId("0123456789abcdef0"), false);
  assert.equal(isValidBuildId(""), false);
  assert.equal(isValidBuildId(null), false);
});

test("a stale server answering HTML is not mistaken for a build id", () => {
  // 旧 handler 会把 /_pojun_build.txt 回成整个 index.html：不能当新代次。
  assert.equal(normalizeBuildId("<!DOCTYPE html><html></html>"), null);
  assert.equal(normalizeBuildId("  0123456789abcdef\n"), "0123456789abcdef");
  assert.equal(normalizeBuildId(undefined), null);
});

test("chunk failures are recognised across bundlers", () => {
  assert.equal(isChunkLoadFailure("ChunkLoadError: Loading chunk 812 failed."), true);
  assert.equal(isChunkLoadFailure("", "ChunkLoadError"), true);
  assert.equal(isChunkLoadFailure("Failed to fetch dynamically imported module: /_next/x.js"), true);
  assert.equal(isChunkLoadFailure("error loading dynamically imported module"), true);
  assert.equal(isChunkLoadFailure("Importing a module script failed."), true);
  assert.equal(isChunkLoadFailure("Unable to preload CSS for /_next/a.css"), true);
  assert.equal(isChunkLoadFailure("TypeError: x is not a function"), false);
  assert.equal(isChunkLoadFailure(""), false);
  assert.equal(isChunkLoadFailure(null), false);
});

test("only Next's own assets count as asset failures", () => {
  assert.equal(isNextAssetUrl("http://h/_next/static/chunks/a.js"), true);
  assert.equal(isNextAssetUrl("/_next/static/css/a.css"), true);
  assert.equal(isNextAssetUrl("/uploads/shot.png"), false);
  assert.equal(isNextAssetUrl(""), false);
  assert.equal(isNextAssetUrl(null), false);
});

test("chain path matching tolerates the trailing slash of static export", () => {
  assert.equal(isChainPath("/system/agents/chain"), true);
  assert.equal(isChainPath("/system/agents/chain/"), true);
  assert.equal(isChainPath("/system/agents"), false);
  assert.equal(isChainPath("/system/agents/chainx"), false);
  assert.equal(isChainPath(""), false);
  assert.equal(isChainPath(null), false);
});

test("auto reload is limited to fresh, clean chain pages", () => {
  const base = { pathname: "/system/agents/chain/", dirty: false, pageAgeMs: 1_000 };
  assert.equal(shouldAutoReload(base), true);
  assert.equal(shouldAutoReload({ ...base, dirty: true }), false);
  assert.equal(shouldAutoReload({ ...base, pathname: "/function/tasks/" }), false);
  assert.equal(shouldAutoReload({ ...base, pageAgeMs: AUTO_RELOAD_MAX_AGE_MS }), false);
  assert.equal(shouldAutoReload({ ...base, pageAgeMs: Number.NaN }), false);
});

test("poll interval is a minute and cooldown is longer than the auto window", () => {
  assert.equal(BUILD_POLL_INTERVAL_MS, 60_000);
  assert.equal(RELOAD_COOLDOWN_MS, 30_000);
  assert.ok(RELOAD_COOLDOWN_MS > AUTO_RELOAD_MAX_AGE_MS);
});

function fakeWindow({ storageFails = false } = {}) {
  const store = new Map();
  const timers = [];
  const state = { reloads: 0 };
  const sessionStorage = {
    getItem(key) {
      if (storageFails) throw new Error("blocked");
      return store.has(key) ? store.get(key) : null;
    },
    setItem(key, value) {
      if (storageFails) throw new Error("blocked");
      store.set(key, String(value));
    },
  };
  globalThis.window = {
    sessionStorage,
    setTimeout(fn) {
      timers.push(fn);
      return timers.length;
    },
    location: {
      reload() {
        state.reloads += 1;
      },
    },
  };
  const runTimers = () => {
    for (const fn of timers.splice(0)) fn();
  };
  return { state, timers, runTimers };
}

test("reloadOnce reloads at most once per cooldown window", () => {
  resetReloadGuardForTest();
  const { state, runTimers } = fakeWindow();
  assert.equal(reloadOnce(), true);
  // 冷却内第二次调用被拒：避免换装后的重载惊群。
  assert.equal(reloadOnce(), false);
  runTimers();
  assert.equal(state.reloads, 1);
  assert.equal(reloadOnce(), false);
});

test("reloadOnce degrades to no auto-reload when sessionStorage is blocked", () => {
  resetReloadGuardForTest();
  const { state, runTimers } = fakeWindow({ storageFails: true });
  // 存储不可用就没有跨载入冷却：自动重载会把"每次加载都失败"变成无限重载，
  // 所以这里必须返回 false，让调用方退化为只出横幅。
  assert.equal(reloadOnce(), false);
  runTimers();
  assert.equal(state.reloads, 0);
});
