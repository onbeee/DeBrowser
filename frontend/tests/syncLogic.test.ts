import assert from "node:assert/strict";
import { describe, test } from "node:test";
import {
  canStart,
  capsSummary,
  eligibleInstances,
  followerSummary,
  sessionFollower,
  toggleFollower,
  toggleMaster,
  unsupportedInstances,
} from "../src/pages/syncLogic.ts";

const options = [
  { id: "a", name: "A", running: true, cdpSupported: true },
  { id: "b", name: "B", running: true, cdpSupported: false },
  { id: "c", name: "C", running: false, cdpSupported: false },
  { id: "d", name: "D", running: true, cdpSupported: true },
];

describe("syncLogic", () => {
  test("eligible instances are running with CDP support", () => {
    assert.deepEqual(eligibleInstances(options).map((o) => o.id), ["a", "d"]);
  });

  test("unsupported instances run without CDP support", () => {
    assert.deepEqual(unsupportedInstances(options).map((o) => o.id), ["b"]);
  });

  test("canStart requires a master and a distinct follower", () => {
    assert.equal(canStart("a", ["d"]), true);
    assert.equal(canStart("", ["d"]), false);
    assert.equal(canStart("a", []), false);
    assert.equal(canStart("a", ["a"]), false);
  });

  test("toggleFollower adds, removes and never adds the master", () => {
    assert.deepEqual(toggleFollower([], "d", "a"), ["d"]);
    assert.deepEqual(toggleFollower(["d"], "d", "a"), []);
    assert.deepEqual(toggleFollower([], "a", "a"), []);
    assert.deepEqual(toggleFollower(["d"], "a", "d"), ["d", "a"]);
  });

  test("followerSummary reports health counts", () => {
    assert.equal(followerSummary([]), "");
    assert.equal(
      followerSummary([{ id: "1", status: "active" }, { id: "2", status: "active" }]),
      "全部 2 个跟随实例正常",
    );
    assert.equal(
      followerSummary([{ id: "1", status: "active" }, { id: "2", status: "failed", reason: "x" }]),
      "1 个正常 · 1 个失败",
    );
    assert.equal(
      followerSummary([
        { id: "1", status: "active" },
        { id: "2", status: "suspended", reason: "未建立当前标签的同步映射" },
        { id: "3", status: "failed", reason: "x" },
      ]),
      "1 个正常 · 1 个等待标签映射 · 1 个失败",
    );
  });

  test("toggleMaster selects, replaces and clears on re-click", () => {
    assert.equal(toggleMaster("", "a"), "a");
    assert.equal(toggleMaster("a", "d"), "d");
    assert.equal(toggleMaster("a", "a"), "");
  });

  test("sessionFollower finds only participants", () => {
    const followers = [
      { id: "1", status: "active" },
      { id: "2", status: "failed", reason: "x" },
    ];
    assert.equal(sessionFollower(followers, "2")?.status, "failed");
    assert.equal(sessionFollower(followers, "3"), undefined);
  });

  test("capsSummary reflects probe results", () => {
    assert.equal(
      capsSummary({ available: false, addressBar: false, tabStrip: false, menuButton: false }),
      "界面控件检测不可用",
    );
    assert.match(
      capsSummary({ available: true, addressBar: true, tabStrip: true, menuButton: false }),
      /标签栏可检测/,
    );
  });
});
