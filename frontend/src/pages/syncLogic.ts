// Pure selection/state logic for the sync page, kept apart
// from the component so it is unit-testable.

export interface SyncCandidateLike {
  instanceId: string;
  cdpAvailable: boolean;
}

export interface SyncSelection {
  masterId: string;
  followerIds: string[];
}

// refreshSelection reconciles the user's selection with the
// latest candidate list after a poll: entries whose instance
// vanished or lost sync capability are dropped, the master is
// never its own follower, and a running session's selection
// is left untouched (the session owns it).
export function refreshSelection(
  prev: SyncSelection,
  candidates: SyncCandidateLike[],
  sessionActive: boolean,
  sessionMasterId: string,
): SyncSelection {
  if (sessionActive) {
    return {
      masterId: sessionMasterId,
      followerIds: prev.followerIds.filter((id) => id !== sessionMasterId),
    };
  }
  const capable = new Set(
    candidates.filter((c) => c.cdpAvailable).map((c) => c.instanceId),
  );
  const masterId = capable.has(prev.masterId) ? prev.masterId : "";
  const followerIds = prev.followerIds.filter(
    (id) => capable.has(id) && id !== masterId,
  );
  return { masterId, followerIds };
}

export type SyncEntryAction =
  | { type: "open" }
  | { type: "unavailable"; count: number };

// canStart reports whether the current selection can start a
// session: one master, at least one follower, all capable.
export function canStart(sel: SyncSelection): boolean {
  return sel.masterId !== "" && sel.followerIds.length > 0;
}

// phaseControls maps the session phase label to which control
// buttons are enabled.
export function phaseControls(phase: string): {
  start: boolean;
  pause: boolean;
  resume: boolean;
  stop: boolean;
} {
  switch (phase) {
    case "同步中":
      return { start: false, pause: true, resume: false, stop: true };
    case "已暂停":
      return { start: false, pause: false, resume: true, stop: true };
    case "启动中":
    case "停止中":
      return { start: false, pause: false, resume: false, stop: false };
    default: // 已停止
      return { start: true, pause: false, resume: false, stop: false };
  }
}
