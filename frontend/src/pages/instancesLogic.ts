// Pure, framework-free helpers for the instances page: natural
// name ordering, batch-operation planning, batch-create planning
// and kernel option labels. Keeping them free of React and of the
// generated bindings makes them directly unit-testable (see
// frontend/tests/instancesLogic.test.ts); the page component only
// executes the plans produced here through the existing
// single-instance service calls.

// Natural ordering: digit runs compare by value, not by character
// ("P-2" < "P-10", "测试浏览器2" < "测试浏览器10"). Intl.Collator
// with numeric collation is built into the WebView2 runtime, so
// no dependency is needed.
const nameCollator = new Intl.Collator("zh-CN", {
  numeric: true,
  sensitivity: "base",
});

export interface InstanceOrderKey {
  name: string;
  createdAt: string;
  id: string;
}

// compareInstances orders by name (natural), then — for identical
// names — by creation time and finally by id, so the order is
// total and stable across refreshes.
export function compareInstances(a: InstanceOrderKey, b: InstanceOrderKey): number {
  const byName = nameCollator.compare(a.name, b.name);
  if (byName !== 0) return byName;
  if (a.createdAt !== b.createdAt) return a.createdAt < b.createdAt ? -1 : 1;
  if (a.id !== b.id) return a.id < b.id ? -1 : 1;
  return 0;
}

export function sortInstancesByName<T extends InstanceOrderKey>(list: readonly T[]): T[] {
  return [...list].sort(compareInstances);
}

// --- Batch operations ------------------------------------------------

export type BatchAction = "start" | "stop" | "delete";

export interface BatchCandidate {
  id: string;
  name: string;
  status: string;
}

export interface BatchPlanItem {
  id: string;
  name: string;
  status: string;
  // run=false items are never executed; reason explains the skip
  // in user-facing terms (already in the target state, or a
  // safety gate the single-instance operations also enforce).
  run: boolean;
  reason: string;
}

// planBatch decides, per selected instance, whether the batch
// action may run. It mirrors the single-instance gates exactly:
// batch operations never widen what a single operation allows.
export function planBatch(action: BatchAction, selected: readonly BatchCandidate[]): BatchPlanItem[] {
  return selected.map((inst) => {
    const item = (run: boolean, reason = ""): BatchPlanItem => ({
      id: inst.id,
      name: inst.name,
      status: inst.status,
      run,
      reason,
    });
    switch (inst.status) {
      case "unknown":
        // The backend refuses start/stop/delete while the run
        // state is unconfirmed; the batch must not bypass that.
        return item(false, "状态无法确认，按安全限制跳过");
      case "starting":
      case "stopping":
        return item(false, "状态正在变化中，跳过");
      case "running":
        if (action === "start") return item(false, "已在运行");
        if (action === "delete") return item(false, "正在运行，不能删除");
        return item(true);
      case "stopped":
        if (action === "stop") return item(false, "未在运行");
        return item(true);
      case "error":
        if (action === "stop") return item(false, "未在运行");
        return item(true);
      default:
        return item(false, "状态未知，跳过");
    }
  });
}

// pruneSelection drops selected ids that are no longer displayed
// (deleted, or the list reloaded), so the selection count always
// reflects visible instances only.
export function pruneSelection(selected: ReadonlySet<string>, visibleIds: readonly string[]): Set<string> {
  const visible = new Set(visibleIds);
  const next = new Set<string>();
  for (const id of selected) {
    if (visible.has(id)) next.add(id);
  }
  return next;
}

// --- Batch create ------------------------------------------------------

export const MAX_BATCH_CREATE = 50;

export type BatchCreatePlan =
  | { ok: true; names: string[] }
  | { ok: false; error: string };

// planBatchCreate validates the batch-create form and expands it
// to the concrete instance names (prefix + 1..N). Name conflicts
// against the existing instances are reported, never silently
// overwritten; the backend re-checks each name at creation time.
export function planBatchCreate(
  prefix: string,
  count: number,
  existingNames: ReadonlySet<string>,
): BatchCreatePlan {
  const trimmed = prefix.trim();
  if (trimmed === "") {
    return { ok: false, error: "请输入实例名称前缀。" };
  }
  if (!Number.isInteger(count) || count < 1) {
    return { ok: false, error: "创建数量必须为正整数。" };
  }
  if (count > MAX_BATCH_CREATE) {
    return { ok: false, error: `一次最多创建 ${MAX_BATCH_CREATE} 个实例。` };
  }
  const names: string[] = [];
  for (let i = 1; i <= count; i++) {
    names.push(`${trimmed}${i}`);
  }
  const conflicts = names.filter((n) => existingNames.has(n));
  if (conflicts.length > 0) {
    return {
      ok: false,
      error: `以下实例名称已存在，无法创建：${conflicts.join("、")}。请修改前缀后重试。`,
    };
  }
  return { ok: true, names };
}

// --- Kernel option labels ----------------------------------------------

// kernelOptionLabels maps each registered browser's id to the
// label shown in the instance form's kernel select: the browser
// name only — never the executable path or the internal id. When
// several registrations share one name, later ones get a concise
// numeric suffix （名称（2）、名称（3）) so they stay
// distinguishable without exposing paths.
export function kernelOptionLabels(
  browsers: readonly { id: string; name: string }[],
): Map<string, string> {
  const labels = new Map<string, string>();
  const seen = new Map<string, number>();
  for (const b of browsers) {
    const n = (seen.get(b.name) ?? 0) + 1;
    seen.set(b.name, n);
    labels.set(b.id, n === 1 ? b.name : `${b.name}（${n}）`);
  }
  return labels;
}
