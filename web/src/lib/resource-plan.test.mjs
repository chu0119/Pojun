import {
  effectiveWorkersLabel,
  formatResourceBytes,
  isResourcePlanActive,
  normalizeResourceManual,
  validateResourceThresholds,
} from "./resource-plan.ts";
import assert from "node:assert/strict";
import test from "node:test";

test("formatResourceBytes 区分未知与真实容量", () => {
  assert.equal(formatResourceBytes(0), "—");
  assert.equal(formatResourceBytes(undefined), "—");
  assert.equal(formatResourceBytes(512 * 1024 ** 2), "512M");
  assert.equal(formatResourceBytes(1.5 * 1024 ** 3), "1.5G");
  assert.equal(formatResourceBytes(32 * 1024 ** 3), "32G");
});

test("isResourcePlanActive 只在 enabled=true 时生效", () => {
  assert.equal(isResourcePlanActive(null), false);
  assert.equal(isResourcePlanActive({ enabled: false }), false);
  assert.equal(isResourcePlanActive({ enabled: true }), true);
});

test("validateResourceThresholds 覆盖边界与大小关系", () => {
  assert.equal(validateResourceThresholds(85, 95), null);
  assert.match(validateResourceThresholds(49, 95) ?? "", /50–95/);
  assert.match(validateResourceThresholds(85, 100) ?? "", /60–99/);
  assert.match(validateResourceThresholds(90, 90) ?? "", /大于/);
  assert.match(validateResourceThresholds(90, 88) ?? "", /大于/);
});

test("normalizeResourceManual 留空表示跟随自动计划", () => {
  assert.deepEqual(normalizeResourceManual({}).manual, {});
  assert.deepEqual(normalizeResourceManual({ workers: "", max_jobs: "  " }).manual, {});
});

test("normalizeResourceManual 归一有效覆盖项", () => {
  const ok = normalizeResourceManual({
    workers: "4",
    max_jobs: "6",
    scanner_scale: "0.5",
    gomemlimit_mb: "2048",
  });
  assert.equal(ok.error, undefined);
  assert.deepEqual(ok.manual, { workers: 4, max_jobs: 6, scanner_scale: 0.5, gomemlimit_mb: 2048 });
});

test("normalizeResourceManual 拒绝非法/越界值且不返回半成品", () => {
  const cases = [
    { input: { workers: "abc" }, pattern: /worker 覆盖/ },
    { input: { workers: "0" }, pattern: /worker 覆盖/ },
    { input: { max_jobs: "129" }, pattern: /作业上限覆盖/ },
    { input: { scanner_scale: "1.5" }, pattern: /扫描缩放覆盖/ },
    { input: { scanner_scale: "0" }, pattern: /扫描缩放覆盖/ },
    { input: { gomemlimit_mb: "128" }, pattern: /GOMEMLIMIT/ },
  ];
  for (const c of cases) {
    const out = normalizeResourceManual(c.input);
    assert.ok(out.error, `expected error for ${JSON.stringify(c.input)}`);
    assert.match(out.error, c.pattern);
    assert.deepEqual(out.manual, {});
  }
});

test("effectiveWorkersLabel 显式生效值优先于计划推荐值", () => {
  assert.equal(effectiveWorkersLabel({ workers: 4 }, 3), "3");
  assert.equal(effectiveWorkersLabel({ workers: 4 }, 0), "4");
  assert.equal(effectiveWorkersLabel({ workers: 0 }, 0), "3");
  assert.equal(effectiveWorkersLabel(null, undefined), "3");
});
