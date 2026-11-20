import { test } from "node:test";
import assert from "node:assert/strict";
// @ts-ignore plain TS module import for node --test
import {
  MAX_NOTIFICATIONS,
  MAX_VISIBLE_TOASTS,
  NOTIFICATION_DURATIONS,
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

test("durations are unified per kind", () => {
  assert.equal(NOTIFICATION_DURATIONS.info, 3000);
  assert.equal(NOTIFICATION_DURATIONS.success, 4000);
  assert.equal(NOTIFICATION_DURATIONS.warning, 6000);
  assert.equal(NOTIFICATION_DURATIONS.error, 8000);
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

test("toast lifetime follows the per-kind duration", () => {
  const t0 = 1_000_000;
  assert.equal(toastAlive(rec(1, "info", t0), t0 + 2999), true);
  assert.equal(toastAlive(rec(1, "info", t0), t0 + 3000), false);
  assert.equal(toastAlive(rec(2, "success", t0), t0 + 3999), true);
  assert.equal(toastAlive(rec(2, "success", t0), t0 + 4000), false);
  assert.equal(toastAlive(rec(3, "warning", t0), t0 + 5999), true);
  assert.equal(toastAlive(rec(4, "error", t0), t0 + 7999), true);
  assert.equal(toastAlive(rec(4, "error", t0), t0 + 8000), false);
});

test("visible toasts are capped at 3, newest first", () => {
  const t0 = 1_000_000;
  const list = [1, 2, 3, 4, 5].map((i) => rec(i, "error", t0 + i));
  const vis = visibleToasts(list, new Set(), t0 + 100);
  assert.equal(vis.length, MAX_VISIBLE_TOASTS);
  assert.deepEqual(
    vis.map((r) => r.id),
    [5, 4, 3],
  );
});

test("closed and expired toasts are excluded; overflow stays in records only", () => {
  const t0 = 1_000_000;
  const list = [
    rec(1, "info", t0), // expired by t0+5000 (info = 3s)
    rec(2, "error", t0),
    rec(3, "error", t0),
    rec(4, "error", t0),
    rec(5, "error", t0),
  ];
  const vis = visibleToasts(list, new Set([5]), t0 + 5000);
  assert.deepEqual(
    vis.map((r) => r.id),
    [4, 3, 2],
  );
  // The overflowed/expired records are NOT deleted from the list.
  assert.equal(list.length, 5);
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

test("nextToastExpiry returns the earliest live expiry or null", () => {
  const t0 = 1_000_000;
  const list = [
    rec(1, "error", t0), // expires t0+8000
    rec(2, "info", t0 + 1000), // expires t0+4000
    rec(3, "success", t0), // closed below
  ];
  assert.equal(nextToastExpiry(list, new Set([3]), t0 + 500), t0 + 4000);
  // Everything closed -> no timer should be kept.
  assert.equal(nextToastExpiry(list, new Set([1, 2, 3]), t0 + 500), null);
  // Everything expired -> null.
  assert.equal(nextToastExpiry(list, new Set(), t0 + 9000), null);
  // Empty list -> null.
  assert.equal(nextToastExpiry([], new Set(), t0), null);
});
