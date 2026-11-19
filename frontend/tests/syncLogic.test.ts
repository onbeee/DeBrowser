import { test } from "node:test";
import assert from "node:assert/strict";
// @ts-ignore plain TS module import for node --test
import { refreshSelection, canStart, phaseControls } from "../src/pages/syncLogic.ts";

const cands = [
  { instanceId: "a", cdpAvailable: true },
  { instanceId: "b", cdpAvailable: true },
  { instanceId: "c", cdpAvailable: false },
];

test("selection survives repeated polls unchanged", () => {
  let sel = { masterId: "a", followerIds: ["b"] };
  for (let i = 0; i < 10; i++) {
    sel = refreshSelection(sel, cands, false, "");
  }
  assert.deepEqual(sel, { masterId: "a", followerIds: ["b"] });
});

test("incapable or vanished entries are dropped", () => {
  const sel = refreshSelection(
    { masterId: "c", followerIds: ["b", "gone"] },
    cands,
    false,
    "",
  );
  assert.deepEqual(sel, { masterId: "", followerIds: ["b"] });
});

test("master is never its own follower", () => {
  const sel = refreshSelection(
    { masterId: "a", followerIds: ["a", "b"] },
    cands,
    false,
    "",
  );
  assert.deepEqual(sel.followerIds, ["b"]);
});

test("active session selection is pinned to the session master", () => {
  const sel = refreshSelection(
    { masterId: "", followerIds: ["a", "b"] },
    cands,
    true,
    "a",
  );
  assert.equal(sel.masterId, "a");
  assert.deepEqual(sel.followerIds, ["b"]);
});

test("canStart requires master plus a follower", () => {
  assert.equal(canStart({ masterId: "a", followerIds: ["b"] }), true);
  assert.equal(canStart({ masterId: "a", followerIds: [] }), false);
  assert.equal(canStart({ masterId: "", followerIds: ["b"] }), false);
});

test("phase controls follow the session state", () => {
  assert.deepEqual(phaseControls("同步中"), { start: false, pause: true, resume: false, stop: true });
  assert.deepEqual(phaseControls("已暂停"), { start: false, pause: false, resume: true, stop: true });
  assert.deepEqual(phaseControls("已停止"), { start: true, pause: false, resume: false, stop: false });
  assert.deepEqual(phaseControls("启动中"), { start: false, pause: false, resume: false, stop: false });
});
