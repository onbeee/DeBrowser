// Unit tests for the sync page's pure helpers
// (src/pages/syncLogic.ts). Run with: npm test
// (Node's built-in test runner; no extra dependencies).
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  canStart,
  followerStateText,
  pruneSelection,
  refreshSelection,
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

test("repeated polling refreshes never erode a still-valid selection", () => {
  // Regression test for the stale-closure bug: the 2s poll once
  // rebuilt the selection from the initial (empty) follower list
  // captured by the first refresh closure, wiping the user's
  // choices. Applying a refresh must be idempotent for any
  // number of polls while the instances still exist.
  const ids = new Set(["m", "f1", "f2", "other"]);
  let sel = { masterId: "m", followerIds: ["f1", "f2"] };
  for (let poll = 0; poll < 10; poll++) {
    sel = refreshSelection(sel.masterId, sel.followerIds, ids, "");
    assert.deepEqual(sel, { masterId: "m", followerIds: ["f1", "f2"] });
  }
  // Only genuinely vanished instances are pruned.
  const shrunk = new Set(["m", "f2"]);
  assert.deepEqual(refreshSelection("m", ["f1", "f2"], shrunk, ""), {
    masterId: "m",
    followerIds: ["f2"],
  });
  assert.deepEqual(refreshSelection("m", ["f2"], shrunk, ""), {
    masterId: "m",
    followerIds: ["f2"],
  });
  // A vanished master clears, and the master is never its own
  // follower after a refresh.
  assert.deepEqual(refreshSelection("gone", ["m", "f2"], shrunk, ""), {
    masterId: "",
    followerIds: ["m", "f2"],
  });
  assert.deepEqual(refreshSelection("m", ["m", "f2"], ids, ""), {
    masterId: "m",
    followerIds: ["f2"],
  });
  // While a session is live the refresh follows the session's
  // master without resurrecting stale followers.
  assert.deepEqual(refreshSelection("stale", ["f1"], ids, "m"), {
    masterId: "m",
    followerIds: ["f1"],
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
