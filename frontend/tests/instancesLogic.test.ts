// Unit tests for the instances page's pure helpers
// (src/pages/instancesLogic.ts). Run with: npm test
// (Node's built-in test runner; no extra dependencies).
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  compareInstances,
  sortInstancesByName,
  planBatch,
  pruneSelection,
  planBatchCreate,
  kernelOptionLabels,
  MAX_BATCH_CREATE,
} from "../src/pages/instancesLogic.ts";

const key = (name: string, createdAt = "2026-01-01T00:00:00Z", id = name) => ({
  name,
  createdAt,
  id,
});

test("natural sort: plain numbers order by value", () => {
  const sorted = sortInstancesByName(["1", "10", "11", "2", "3"].map((n) => key(n)));
  assert.deepEqual(
    sorted.map((i) => i.name),
    ["1", "2", "3", "10", "11"],
  );
});

test("natural sort: P-N names order by value", () => {
  const sorted = sortInstancesByName(["P-1", "P-10", "P-2", "P-3"].map((n) => key(n)));
  assert.deepEqual(
    sorted.map((i) => i.name),
    ["P-1", "P-2", "P-3", "P-10"],
  );
});

test("natural sort: Chinese prefix with numbers", () => {
  const sorted = sortInstancesByName(
    ["测试浏览器1", "测试浏览器10", "测试浏览器2"].map((n) => key(n)),
  );
  assert.deepEqual(
    sorted.map((i) => i.name),
    ["测试浏览器1", "测试浏览器2", "测试浏览器10"],
  );
});

test("natural sort: identical names fall back to createdAt, then id", () => {
  const a = key("同名", "2026-01-02T00:00:00Z", "id-b");
  const b = key("同名", "2026-01-01T00:00:00Z", "id-z");
  const c = key("同名", "2026-01-01T00:00:00Z", "id-a");
  const sorted = sortInstancesByName([a, b, c]);
  assert.deepEqual(
    sorted.map((i) => i.id),
    ["id-a", "id-z", "id-b"],
  );
  assert.equal(compareInstances(a, a), 0);
});

test("natural sort: input array is not mutated", () => {
  const input = [key("2"), key("10"), key("1")];
  sortInstancesByName(input);
  assert.deepEqual(
    input.map((i) => i.name),
    ["2", "10", "1"],
  );
});

const cand = (id: string, status: string) => ({ id, name: `实例${id}`, status });

test("planBatch start: running skipped, unknown blocked, stopped/error run", () => {
  const plan = planBatch("start", [
    cand("1", "running"),
    cand("2", "stopped"),
    cand("3", "error"),
    cand("4", "unknown"),
    cand("5", "starting"),
  ]);
  assert.deepEqual(
    plan.map((p) => [p.id, p.run]),
    [
      ["1", false],
      ["2", true],
      ["3", true],
      ["4", false],
      ["5", false],
    ],
  );
  assert.match(plan[0].reason, /已在运行/);
  assert.match(plan[3].reason, /安全限制/);
});

test("planBatch stop: only running runs; stopped/error skipped as not running", () => {
  const plan = planBatch("stop", [
    cand("1", "running"),
    cand("2", "stopped"),
    cand("3", "error"),
    cand("4", "unknown"),
    cand("5", "stopping"),
  ]);
  assert.deepEqual(
    plan.map((p) => p.run),
    [true, false, false, false, false],
  );
  assert.match(plan[3].reason, /安全限制/);
});

test("planBatch delete: running and unknown protected, stopped/error run", () => {
  const plan = planBatch("delete", [
    cand("1", "running"),
    cand("2", "stopped"),
    cand("3", "error"),
    cand("4", "unknown"),
  ]);
  assert.deepEqual(
    plan.map((p) => p.run),
    [false, true, true, false],
  );
  assert.match(plan[0].reason, /不能删除/);
  assert.match(plan[3].reason, /安全限制/);
});

test("pruneSelection keeps only visible ids", () => {
  const next = pruneSelection(new Set(["a", "b", "c"]), ["a", "c", "d"]);
  assert.deepEqual([...next].sort(), ["a", "c"]);
});

test("planBatchCreate: expands prefix with sequential numbers", () => {
  const plan = planBatchCreate("测试浏览器", 3, new Set());
  assert.deepEqual(plan, {
    ok: true,
    names: ["测试浏览器1", "测试浏览器2", "测试浏览器3"],
  });
});

test("planBatchCreate: count boundaries", () => {
  assert.equal(planBatchCreate("x", 1, new Set()).ok, true);
  assert.equal(planBatchCreate("x", MAX_BATCH_CREATE, new Set()).ok, true);
  assert.equal(planBatchCreate("x", 0, new Set()).ok, false);
  assert.equal(planBatchCreate("x", -2, new Set()).ok, false);
  assert.equal(planBatchCreate("x", 2.5, new Set()).ok, false);
  assert.equal(planBatchCreate("x", MAX_BATCH_CREATE + 1, new Set()).ok, false);
  assert.equal(planBatchCreate("  ", 3, new Set()).ok, false);
});

test("planBatchCreate: name conflicts are reported, not hidden", () => {
  const plan = planBatchCreate("测试浏览器", 3, new Set(["测试浏览器2"]));
  assert.equal(plan.ok, false);
  if (!plan.ok) assert.match(plan.error, /测试浏览器2/);
});

test("kernelOptionLabels: names only, duplicates get a numeric suffix", () => {
  const labels = kernelOptionLabels([
    { id: "a", name: "Google Chrome" },
    { id: "b", name: "Microsoft Edge" },
    { id: "c", name: "Google Chrome" },
    { id: "d", name: "Google Chrome" },
  ]);
  assert.equal(labels.get("a"), "Google Chrome");
  assert.equal(labels.get("b"), "Microsoft Edge");
  assert.equal(labels.get("c"), "Google Chrome（2）");
  assert.equal(labels.get("d"), "Google Chrome（3）");
  for (const label of labels.values()) {
    assert.ok(!label.includes(":\\"), "label must not contain a path");
    assert.ok(!label.includes(".exe"), "label must not contain an executable name");
  }
});
