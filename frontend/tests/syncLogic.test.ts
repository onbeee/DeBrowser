// Unit tests for the sync page's pure helpers
// (src/pages/syncLogic.ts). Run with: npm test
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  startBlockReason,
  followerIds,
  roleText,
  sessionStateText,
  type SyncInstanceLike,
} from "../src/pages/syncLogic.ts";

const running = (id: string): SyncInstanceLike => ({
  id,
  status: "running",
  cdpAvailable: true,
  cdpReason: "",
});

test("startBlockReason requires two participants and a master", () => {
  const list = [running("a"), running("b")];
  assert.equal(
    startBlockReason(new Set(["a"]), "a", list),
    "至少需要选择两个实例才能开始同步",
  );
  assert.equal(
    startBlockReason(new Set(["a", "b"]), "", list),
    "请从参与实例中指定一个主实例",
  );
  assert.equal(startBlockReason(new Set(["a", "b"]), "a", list), null);
});

test("startBlockReason rejects non-running and CDP-unavailable picks", () => {
  const list = [
    running("a"),
    { id: "b", status: "stopped", cdpAvailable: false, cdpReason: "实例未在运行" },
    {
      id: "c",
      status: "running",
      cdpAvailable: false,
      cdpReason: "该实例由旧版本启动，暂不支持同步，请关闭后重新启动",
    },
  ];
  assert.equal(
    startBlockReason(new Set(["a", "b"]), "a", list),
    "所有参与实例必须处于已确认运行状态",
  );
  assert.equal(
    startBlockReason(new Set(["a", "c"]), "a", list),
    "该实例由旧版本启动，暂不支持同步，请关闭后重新启动",
  );
});

test("followerIds excludes the master", () => {
  assert.deepEqual(followerIds(new Set(["a", "b", "c"]), "b"), ["a", "c"]);
});

test("role and session labels", () => {
  assert.equal(roleText("", "idle"), "未参与");
  assert.equal(roleText("master", "active"), "主实例");
  assert.equal(roleText("follower", "active"), "跟随中");
  assert.equal(roleText("follower", "paused"), "已暂停");
  assert.equal(sessionStateText("active"), "同步中");
  assert.equal(sessionStateText("paused"), "已暂停");
  assert.equal(sessionStateText("idle"), "未开始");
});
