import { test } from "node:test";
import assert from "node:assert/strict";
// @ts-ignore plain TS module imports for node --test
import {
  MAX_NOTIFICATIONS,
  MAX_VISIBLE_TOASTS,
  TOAST_DURATION_MS,
  itemCounts,
  pushNotification,
  pushToast,
  type AppNotification,
} from "../src/lib/notifications.ts";
// @ts-ignore plain TS module import for node --test
import { useNotificationStore } from "../src/stores/useNotificationStore.ts";

function rec(id: number, summary = `n${id}`): AppNotification {
  return { id, kind: "info", summary, important: false, createdAt: id * 1000 };
}

function state() {
  return useNotificationStore.getState();
}

function reset() {
  state().clearAll();
}

// ---------- 通知台（lib 层） ----------

test("center list caps at 80 and evicts the oldest on the 81st", () => {
  assert.equal(MAX_NOTIFICATIONS, 80);
  let list: AppNotification[] = [];
  for (let i = 1; i <= MAX_NOTIFICATIONS + 1; i++) {
    list = pushNotification(list, { kind: "info", summary: `n${i}` }, i, i * 1000);
  }
  assert.equal(list.length, MAX_NOTIFICATIONS);
  assert.equal(list[0].id, 2);
  assert.equal(list[list.length - 1].id, MAX_NOTIFICATIONS + 1);
});

test("push appends in time order (newest last) and does not mutate input", () => {
  const before = [rec(1)];
  const after = pushNotification(before, { kind: "success", summary: "x" }, 2, 2000);
  assert.equal(before.length, 1);
  assert.deepEqual(
    after.map((r) => r.id),
    [1, 2],
  );
});

test("important flag defaults to false and is preserved when set", () => {
  const plain = pushNotification([], { kind: "info", summary: "a" }, 1, 0);
  assert.equal(plain[0].important, false);
  const marked = pushNotification([], { kind: "info", summary: "b", important: true }, 2, 0);
  assert.equal(marked[0].important, true);
});

// ---------- 浮窗展示列表（lib 层） ----------

test("toast list is FIFO capped at 3: the 4th displaces the 1st", () => {
  assert.equal(MAX_VISIBLE_TOASTS, 3);
  let toasts: AppNotification[] = [];
  for (let i = 1; i <= 4; i++) toasts = pushToast(toasts, rec(i));
  assert.deepEqual(
    toasts.map((t) => t.id),
    [2, 3, 4],
  );
  toasts = pushToast(toasts, rec(5));
  assert.deepEqual(
    toasts.map((t) => t.id),
    [3, 4, 5],
  );
});

test("toast duration constant is a unified 3 seconds", () => {
  assert.equal(TOAST_DURATION_MS, 3000);
});

// ---------- 统一发布（store 层） ----------

test("a plain notification enters the center only, never the toasts", () => {
  reset();
  state().notify({ kind: "info", summary: "普通通知" });
  assert.equal(state().notifications.length, 1);
  assert.equal(state().toasts.length, 0);
});

test("an important notification is ONE entity shown in both center and toasts", () => {
  reset();
  state().notify({ kind: "success", summary: "重点通知", important: true });
  assert.equal(state().notifications.length, 1);
  assert.equal(state().toasts.length, 1);
  // Same object reference: not a second, duplicated entity.
  assert.equal(state().toasts[0], state().notifications[0]);
  assert.equal(state().toasts[0].id, state().notifications[0].id);
});

test("the newest notification is last in the list (center renders it first)", () => {
  reset();
  state().notify({ kind: "info", summary: "第一条" });
  state().notify({ kind: "info", summary: "第二条" });
  const list = state().notifications;
  assert.equal(list[list.length - 1].summary, "第二条");
});

test("store keeps at most 80 notifications, evicting the earliest", () => {
  reset();
  for (let i = 0; i < MAX_NOTIFICATIONS + 1; i++) {
    state().notify({ kind: "info", summary: `批量 ${i}` });
  }
  const list = state().notifications;
  assert.equal(list.length, MAX_NOTIFICATIONS);
  assert.equal(list[0].summary, "批量 1");
});

test("dismissing a toast never deletes the center notification", () => {
  reset();
  state().notify({ kind: "error", summary: "重点失败", important: true });
  const id = state().notifications[0].id;
  state().dismissToast(id);
  assert.equal(state().toasts.length, 0);
  assert.equal(state().notifications.length, 1);
  assert.equal(state().notifications[0].id, id);
});

test("manual dismiss removes only that toast; same-text toasts are unaffected", () => {
  reset();
  state().notify({ kind: "success", summary: "相同文本", important: true });
  state().notify({ kind: "success", summary: "相同文本", important: true });
  const [first, second] = state().toasts;
  state().dismissToast(first.id);
  assert.deepEqual(
    state().toasts.map((t) => t.id),
    [second.id],
  );
  assert.equal(state().notifications.length, 2);
});

test("a displaced toast never comes back when a newer toast is dismissed", () => {
  reset();
  for (let i = 0; i < 4; i++) {
    state().notify({ kind: "info", summary: `重点 ${i}`, important: true });
  }
  // 4 important toasts: the 1st was displaced by the 4th.
  assert.equal(state().toasts.length, 3);
  const newest = state().toasts[state().toasts.length - 1];
  state().dismissToast(newest.id);
  assert.equal(state().toasts.length, 2);
  assert.equal(state().notifications.length, 4);
});

test("clearAll empties both the center list and the toasts", () => {
  reset();
  state().notify({ kind: "info", summary: "x", important: true });
  state().clearAll();
  assert.equal(state().notifications.length, 0);
  assert.equal(state().toasts.length, 0);
});

// ---------- 逐项计数 ----------

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
