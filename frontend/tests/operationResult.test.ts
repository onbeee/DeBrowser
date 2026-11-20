import { test } from "node:test";
import assert from "node:assert/strict";
// @ts-ignore plain TS module import for node --test
import {
  classifyApplyResult,
  classifyCounts,
  classifySyncResult,
} from "../src/lib/operationResult.ts";

// ---------- 批量操作（实例批量等） ----------

test("batch: all succeeded -> success", () => {
  assert.equal(classifyCounts(3, 0, 0), "success");
  assert.equal(classifyCounts(2, 0, 1), "success");
});

test("batch: partial failure -> warning", () => {
  assert.equal(classifyCounts(2, 1, 0), "warning");
  assert.equal(classifyCounts(1, 1, 1), "warning");
  assert.equal(classifyCounts(0, 1, 1), "warning");
});

test("batch: all failed -> error", () => {
  assert.equal(classifyCounts(0, 3, 0), "error");
});

test("batch: all skipped -> info, never success", () => {
  assert.equal(classifyCounts(0, 0, 4), "info");
});

test("batch: no items -> null (no notification)", () => {
  assert.equal(classifyCounts(0, 0, 0), null);
});

// ---------- 书签同步（实例 + 书签双口径） ----------

const syncBase = {
  successInstances: 0,
  failedInstances: 0,
  skippedInstances: 0,
  addedBookmarks: 0,
  skippedBookmarks: 0,
  failedBookmarks: 0,
};

test("sync: clean completion -> success", () => {
  assert.equal(
    classifySyncResult({ ...syncBase, successInstances: 2, addedBookmarks: 10, skippedBookmarks: 3 }),
    "success",
  );
});

test("sync: instance ok but bookmark writes failed -> warning, not success", () => {
  assert.equal(
    classifySyncResult({ ...syncBase, successInstances: 1, failedBookmarks: 4 }),
    "warning",
  );
});

test("sync: some instances failed -> warning", () => {
  assert.equal(
    classifySyncResult({ ...syncBase, successInstances: 1, failedInstances: 1, addedBookmarks: 5 }),
    "warning",
  );
});

test("sync: everything failed -> error", () => {
  assert.equal(
    classifySyncResult({ ...syncBase, failedInstances: 2, failedBookmarks: 6 }),
    "error",
  );
});

test("sync: all skipped, nothing executed -> info", () => {
  assert.equal(classifySyncResult({ ...syncBase, skippedInstances: 3 }), "info");
});

// ---------- 书签聚合应用（编辑 / 删除） ----------

const applyBase = {
  updatedInstances: 0,
  failedInstances: 0,
  skippedInstances: 0,
  affectedNodes: 0,
  failedNodes: 0,
};

test("apply: all succeeded -> success", () => {
  assert.equal(
    classifyApplyResult({ ...applyBase, updatedInstances: 2, affectedNodes: 5 }),
    "success",
  );
});

test("apply: partial failure -> warning", () => {
  assert.equal(
    classifyApplyResult({ ...applyBase, updatedInstances: 1, failedInstances: 1, affectedNodes: 3, failedNodes: 1 }),
    "warning",
  );
  assert.equal(
    classifyApplyResult({ ...applyBase, updatedInstances: 1, affectedNodes: 2, failedNodes: 2 }),
    "warning",
  );
});

test("apply: all failed -> error", () => {
  assert.equal(
    classifyApplyResult({ ...applyBase, failedInstances: 2, failedNodes: 4 }),
    "error",
  );
});

test("apply: all skipped -> info", () => {
  assert.equal(classifyApplyResult({ ...applyBase, skippedInstances: 3 }), "info");
});

test("apply: nothing executed -> null (no notification)", () => {
  assert.equal(classifyApplyResult({ ...applyBase }), null);
});
