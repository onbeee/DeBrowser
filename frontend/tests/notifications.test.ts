import { test } from "node:test";
import assert from "node:assert/strict";
// @ts-ignore plain TS module import for node --test
import {
  MAX_NOTIFICATIONS,
  MAX_VISIBLE_TOASTS,
  TOAST_DURATION_MS,
  itemCounts,
  nextToastExpiry,
  pushNotification,
  toastAlive,
  visibleToasts,
  type AppNotification,
} from "../src/lib/notifications.ts";

function rec(id: number, kind: AppNotification["kind"], createdAt: number): AppNotification {
  return { id, kind, summary: `n${id}`, createdAt };
}

test("toast duration is a unified 3 seconds for every kind", () => {
  assert.equal(TOAST_DURATION_MS, 3000);
  const t0 = 1_000_000;
  for (const kind of ["info", "success", "warning", "error"] as const) {
    assert.equal(toastAlive(rec(1, kind, t0), t0 + 2999), true);
    assert.equal(toastAlive(rec(1, kind, t0), t0 + 3000), false);
  }
});

test("push appends and evicts the oldest beyond the record cap", () => {
  let list: AppNotification[] = [];
  for (let i = 1; i <= MAX_NOTIFICATIONS + 5; i++) {
    list = pushNotification(list, { kind: "info", summary: `n${i}` }, i, i * 1000);
  }
  assert.equal(list.length, MAX_NOTIFICATIONS);
  assert.equal(list[0].id, 6);
  assert.equal(list[list.length - 1].id, MAX_NOTIFICATIONS + 5);
});

test("push does not mutate the input list", () => {
  const before = [rec(1, "info", 0)];
  const after = pushNotification(before, { kind: "success", summary: "x" }, 2, 1000);
  assert.equal(before.length, 1);
  assert.equal(after.length, 2);
});

test("two toasts keep independent lifetimes", () => {
  const t0 = 1_000_000;
  const list = [rec(1, "success", t0), rec(2, "success", t0 + 1000)];
  // At t0+3000 the first has expired, the second still has 1s left.
  assert.deepEqual(
    visibleToasts(list, new Set(), t0 + 3000).map((r) => r.id),
    [2],
  );
  assert.deepEqual(
    visibleToasts(list, new Set(), t0 + 3999).map((r) => r.id),
    [2],
  );
  assert.deepEqual(visibleToasts(list, new Set(), t0 + 4000), []);
});

test("FIFO: the 4th toast immediately displaces the 1st, the 5th the 2nd", () => {
  // Spec timeline (seconds): A@0, B@1, C@2, D@2.5 -> B,C,D; E@3.5 -> C,D,E.
  const t0 = 1_000_000;
  const mk = (id: number, atSec: number) => rec(id, "info", t0 + atSec * 1000);
  const abc = [mk(1, 0), mk(2, 1), mk(3, 2)];
  assert.deepEqual(
    visibleToasts(abc, new Set(), t0 + 2000).map((r) => r.id),
    [3, 2, 1],
  );
  const abcd = [...abc, mk(4, 2.5)];
  // A is displaced at once even though it has only been shown 2.5s.
  assert.deepEqual(
    visibleToasts(abcd, new Set(), t0 + 2500).map((r) => r.id),
    [4, 3, 2],
  );
  const abcde = [...abcd, mk(5, 3.5)];
  assert.deepEqual(
    visibleToasts(abcde, new Set(), t0 + 3500).map((r) => r.id),
    [5, 4, 3],
  );
});

test("a displaced toast never comes back when a newer one is closed", () => {
  const t0 = 1_000_000;
  const list = [rec(1, "info", t0), rec(2, "info", t0), rec(3, "info", t0), rec(4, "info", t0)];
  // 4th arrival displaced #1; closing #4 must not resurrect #1.
  assert.deepEqual(
    visibleToasts(list, new Set([4]), t0 + 100).map((r) => r.id),
    [3, 2],
  );
  // Manual close removes only that toast; the records stay in the list.
  assert.deepEqual(
    visibleToasts(list, new Set([3]), t0 + 100).map((r) => r.id),
    [4, 2],
  );
  assert.equal(list.length, 4);
});

test("visible toasts are capped at MAX_VISIBLE_TOASTS, newest first", () => {
  const t0 = 1_000_000;
  const list = [1, 2, 3, 4, 5].map((i) => rec(i, "error", t0 + i));
  const vis = visibleToasts(list, new Set(), t0 + 100);
  assert.equal(vis.length, MAX_VISIBLE_TOASTS);
  assert.deepEqual(
    vis.map((r) => r.id),
    [5, 4, 3],
  );
});

test("nextToastExpiry returns the earliest live expiry or null", () => {
  const t0 = 1_000_000;
  const list = [
    rec(1, "error", t0), // expires t0+3000
    rec(2, "info", t0 + 1000), // expires t0+4000
    rec(3, "success", t0), // closed below
  ];
  assert.equal(nextToastExpiry(list, new Set([3]), t0 + 500), t0 + 3000);
  // Everything closed -> no timer should be kept.
  assert.equal(nextToastExpiry(list, new Set([1, 2, 3]), t0 + 500), null);
  // Everything expired -> null.
  assert.equal(nextToastExpiry(list, new Set(), t0 + 9000), null);
  // Empty list -> null.
  assert.equal(nextToastExpiry([], new Set(), t0), null);
});

test("itemCounts tallies per-item results", () => {
  assert.deepEqual(
    itemCounts([
      { name: "a", status: "success" },
      { name: "b", status: "failed", reason: "x" },
      { name: "c", status: "skipped", reason: "y" },
      { name: "d", status: "success" },
    ]),
    { success: 2, failed: 1, skipped: 1 },
  );
  assert.deepEqual(itemCounts([]), { success: 0, failed: 0, skipped: 0 });
});
