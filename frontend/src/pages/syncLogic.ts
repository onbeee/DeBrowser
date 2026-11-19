// Pure helpers for the Stage 10 v2 sync page. Kept free of React
// and bindings so they can be unit-tested directly.

export interface SyncOptionLike {
  id: string;
  running: boolean;
  cdpSupported: boolean;
}

export interface FollowerStateLike {
  id: string;
  status: string; // "active" | "failed"
  reason?: string;
}

/** Instances that can take part in a sync session. */
export function eligibleInstances<T extends SyncOptionLike>(options: T[]): T[] {
  return options.filter((o) => o.running && o.cdpSupported);
}

/** Instances that run but cannot take part (old launch, no CDP). */
export function unsupportedInstances<T extends SyncOptionLike>(options: T[]): T[] {
  return options.filter((o) => o.running && !o.cdpSupported);
}

/** Start is possible with a master and at least one follower. */
export function canStart(masterId: string, followerIds: string[]): boolean {
  return masterId !== "" && followerIds.length > 0 && !followerIds.includes(masterId);
}

/** Toggle one follower in the selection, keeping pick order. */
export function toggleFollower(selected: string[], id: string, masterId: string): string[] {
  if (id === masterId) return selected;
  return selected.includes(id) ? selected.filter((x) => x !== id) : [...selected, id];
}

/** Toggle the master selection: choosing the current master
 * again clears it, choosing another row replaces it. */
export function toggleMaster(current: string, id: string): string {
  return current === id ? "" : id;
}

/** The session state entry for one follower id, if it takes part. */
export function sessionFollower<T extends { id: string }>(followers: T[], id: string): T | undefined {
  return followers.find((f) => f.id === id);
}

/** One-line summary of follower health for the session header. */
export function followerSummary(followers: FollowerStateLike[]): string {
  if (followers.length === 0) return "";
  const failed = followers.filter((f) => f.status === "failed").length;
  const suspended = followers.filter((f) => f.status === "suspended").length;
  const active = followers.length - failed - suspended;
  if (failed === 0 && suspended === 0) return `全部 ${followers.length} 个跟随实例正常`;
  const parts = [`${active} 个正常`];
  if (suspended > 0) parts.push(`${suspended} 个等待标签映射`);
  if (failed > 0) parts.push(`${failed} 个失败`);
  return parts.join(" · ");
}

/** Capability summary line for one instance's UIA probe. */
export function capsSummary(caps: {
  available: boolean;
  addressBar: boolean;
  tabStrip: boolean;
  menuButton: boolean;
}): string {
  if (!caps.available) return "界面控件检测不可用";
  const parts: string[] = [];
  parts.push(caps.tabStrip ? "标签栏可检测" : "标签栏不可检测");
  parts.push(caps.addressBar ? "地址栏可识别" : "地址栏不可识别");
  parts.push(caps.menuButton ? "菜单按钮可识别" : "菜单按钮不可识别");
  return parts.join(" · ");
}
