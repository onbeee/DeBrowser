// Pure, framework-free helpers for the sync page (Stage 10 v3):
// master/follower selection rules and the session-state wording.
// Keeping them free of React and of the generated bindings makes
// them directly unit-testable (see frontend/tests/syncLogic.test.ts).

/** Clicking the selected master again clears the selection. */
export function toggleMaster(current: string, id: string): string {
  return current === id ? "" : id;
}

/**
 * Toggle one follower in the selection. The master can never be
 * its own follower: selecting it as a follower is a no-op.
 */
export function toggleFollower(selected: readonly string[], id: string, masterId: string): string[] {
  if (id === masterId) return [...selected];
  return selected.includes(id)
    ? selected.filter((x) => x !== id)
    : [...selected, id];
}

/** Drop selections that no longer exist or clash with the master. */
export function pruneSelection(
  masterId: string,
  followerIds: readonly string[],
  existingIds: ReadonlySet<string>,
): { masterId: string; followerIds: string[] } {
  const master = existingIds.has(masterId) ? masterId : "";
  const followers = followerIds.filter((id) => existingIds.has(id) && id !== master);
  return { masterId: master, followerIds: followers };
}

/**
 * One polling refresh applied to the current selection: prune
 * only IDs that no longer exist, keep every still-valid choice,
 * and — while a session is live — follow the session's master.
 * Pure and side-effect free, so repeated polls are idempotent:
 * applying it any number of times never erodes the selection.
 */
export function refreshSelection(
  masterId: string,
  followerIds: readonly string[],
  existingIds: ReadonlySet<string>,
  sessionMasterId: string,
): { masterId: string; followerIds: string[] } {
  const pruned = pruneSelection(masterId, followerIds, existingIds);
  const master = sessionMasterId || pruned.masterId;
  return {
    masterId: master,
    followerIds: pruned.followerIds.filter((id) => id !== master),
  };
}

/** A start is possible only with a master and at least one follower, and no live session. */
export function canStart(phase: string, masterId: string, followerCount: number): boolean {
  return phase === "idle" && masterId !== "" && followerCount > 0;
}

export interface FollowerStateLike {
  instanceId: string;
  connected: boolean;
  pageReady: boolean;
  state: string;
  reason: string;
}

export interface SyncStateLike {
  phase: string;
  masterId: string;
  masterConnected: boolean;
  masterPageReady: boolean;
  followers: FollowerStateLike[];
}

/** One-line wording for a follower's state in the instance table. */
export function followerStateText(f: FollowerStateLike): string {
  if (f.state === "failed") return f.reason ? `已失败：${f.reason}` : "已失败";
  if (f.state === "unconfirmed") return f.reason ? `页面未确认：${f.reason}` : "页面未确认";
  if (!f.connected) return "未连接";
  return f.pageReady ? "已连接 · 活动页面已确认" : "已连接 · 等待活动页面";
}

/** Compact session health summary for the toolbar. */
export function sessionSummary(state: SyncStateLike): string {
  if (state.phase === "idle") return "未开始同步";
  const total = state.followers.length;
  const ready = state.followers.filter((f) => f.state === "ready").length;
  const master = state.masterConnected
    ? state.masterPageReady
      ? "主实例已连接 · 活动页面已确认"
      : "主实例已连接 · 活动页面未确认"
    : "主实例未连接";
  const phase = state.phase === "paused" ? "已暂停" : "同步中";
  return `${phase} · ${master} · 跟随 ${ready}/${total} 就绪`;
}
