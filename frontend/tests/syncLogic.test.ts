// Unit tests for the sync page's pure helpers
// (src/pages/syncLogic.ts). Run with: npm test
// (Node's built-in test runner; no extra dependencies).
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  canStart,
  followerStateText,
  pruneSelection,
  sessionSummary,
  toggleFollower,
  toggleMaster,
} from "../src/pages/syncLogic.ts";

test("toggleMaster selects, switches and clears on re-click", () => {
  assert.equal(toggleMaster("", "a"), "a");
  assert.equal(toggleMaster("a", "b"), "b");
  assert.equal(toggleMaster("a", "a"), "");
});

test("toggleFollower adds and removes, never the master itself", () => {
  assert.deepEqual(toggleFollower([], "f1", "m"), ["f1"]);
  assert.deepEqual(toggleFollower(["f1"], "f2", "m"), ["f1", "f2"]);
  assert.deepEqual(toggleFollower(["f1", "f2"], "f1", "m"), ["f2"]);
  assert.deepEqual(toggleFollower(["f1"], "m", "m"), ["f1"]);
});

test("pruneSelection drops vanished instances and master clashes", () => {
  const ids = new Set(["a", "b"]);
  assert.deepEqual(pruneSelection("gone", ["a", "gone", "b"], ids), {
    masterId: "",
    followerIds: ["a", "b"],
  });
  assert.deepEqual(pruneSelection("a", ["a", "b"], ids), {
    masterId: "a",
    followerIds: ["b"],
  });
});

test("canStart requires idle phase, a master and at least one follower", () => {
  assert.equal(canStart("idle", "m", 1), true);
  assert.equal(canStart("idle", "", 1), false);
  assert.equal(canStart("idle", "m", 0), false);
  assert.equal(canStart("running", "m", 1), false);
  assert.equal(canStart("paused", "m", 1), false);
});

test("followerStateText surfaces the per-follower failure reason", () => {
  const base = { instanceId: "f", connected: true, pageReady: true, reason: "" };
  assert.equal(followerStateText({ ...base, state: "ready" }), "已连接 · 活动页面已确认");
  assert.equal(
    followerStateText({ ...base, state: "failed", reason: "连接失败" }),
    "已失败：连接失败",
  );
  assert.equal(
    followerStateText({ ...base, state: "unconfirmed", reason: "无法确认活动页面" }),
    "页面未确认：无法确认活动页面",
  );
  assert.equal(
    followerStateText({ ...base, state: "ready", connected: false }),
    "未连接",
  );
});

test("sessionSummary reports phase, master readiness and follower counts", () => {
  assert.equal(
    sessionSummary({ phase: "idle", masterId: "", masterConnected: false, masterPageReady: false, followers: [] }),
    "未开始同步",
  );
  const text = sessionSummary({
    phase: "running",
    masterId: "m",
    masterConnected: true,
    masterPageReady: true,
    followers: [
      { instanceId: "a", connected: true, pageReady: true, state: "ready", reason: "" },
      { instanceId: "b", connected: false, pageReady: false, state: "failed", reason: "x" },
    ],
  });
  assert.ok(text.includes("同步中"));
  assert.ok(text.includes("主实例已连接 · 活动页面已确认"));
  assert.ok(text.includes("跟随 1/2 就绪"));
});
