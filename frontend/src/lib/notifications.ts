/**
 * 全局通知的纯逻辑：类型、显示时长、可见数量与记录上限。
 * 通知只保存在前端内存中（不落库、不持久化），页面切换与
 * 弹窗关闭都不影响记录；浮动提示与右侧通知台共用同一份
 * 记录，内容完全一致。
 */

export type NotificationKind = "success" | "info" | "warning" | "error";

/** 逐项结果（批量操作、同步等）：名称 + 结果 + 实际原因。 */
export interface NotificationItem {
  name: string;
  status: "success" | "failed" | "skipped";
  reason?: string;
}

export interface AppNotification {
  id: number;
  kind: NotificationKind;
  /** 一句话说明发生了什么。 */
  summary: string;
  /** 可选的补充说明（较长内容只在通知台完整展示）。 */
  detail?: string;
  /** 可选的逐项结果，在通知台展开查看。 */
  items?: NotificationItem[];
  /** 关联的业务 operation_id（业务已有时才带）。 */
  operationId?: string;
  /** 发生时间（毫秒时间戳）。 */
  createdAt: number;
}

export interface NotificationInput {
  kind: NotificationKind;
  summary: string;
  detail?: string;
  items?: NotificationItem[];
  operationId?: string;
}

/** 浮动提示的统一显示时长（毫秒）：不分类型，全部 3 秒。 */
export const TOAST_DURATION_MS = 3000;

/** 浮动提示同时显示的上限。 */
export const MAX_VISIBLE_TOASTS = 3;

/** 通知台保留的记录上限，超出后最旧的先淘汰。 */
export const MAX_NOTIFICATIONS = 50;

/** 追加一条记录并按上限淘汰最旧记录，返回新列表（不修改入参）。 */
export function pushNotification(
  list: readonly AppNotification[],
  input: NotificationInput,
  id: number,
  now: number,
): AppNotification[] {
  const next = [
    ...list,
    {
      id,
      kind: input.kind,
      summary: input.summary,
      detail: input.detail,
      items: input.items,
      operationId: input.operationId,
      createdAt: now,
    },
  ];
  return next.length > MAX_NOTIFICATIONS ? next.slice(next.length - MAX_NOTIFICATIONS) : next;
}

/** 一条记录的浮动提示是否仍在显示期内（从显示时起 3 秒）。 */
export function toastAlive(record: AppNotification, now: number): boolean {
  return now - record.createdAt < TOAST_DURATION_MS;
}

/**
 * 浮窗窗口（先进先出）：按到达顺序维护最多 MAX_VISIBLE_TOASTS
 * 条——新通知到来且窗口已满时，最早的一条立即被顶掉，不等
 * 其 3 秒结束；被顶掉、被手动关闭或已过期的通知不会因为
 * 其他通知消失而重新出现。窗口只决定浮窗展示，记录本身
 * 仍保留在通知台列表中。
 */
function toastWindow(list: readonly AppNotification[]): AppNotification[] {
  const window: AppNotification[] = [];
  for (const record of list) {
    window.push(record);
    if (window.length > MAX_VISIBLE_TOASTS) window.shift();
  }
  return window;
}

/** 当前应显示的浮动提示：窗口内未过期、未手动关闭的记录，最新在前。 */
export function visibleToasts(
  list: readonly AppNotification[],
  closedToastIds: ReadonlySet<number>,
  now: number,
): AppNotification[] {
  return toastWindow(list)
    .filter((r) => !closedToastIds.has(r.id) && toastAlive(r, now))
    .reverse();
}

/**
 * 下一条浮动提示的到期时间（毫秒时间戳）：取当前浮窗窗口
 * 内未关闭、未过期记录中最早的到期时刻；没有这样的记录时
 * 返回 null（此时不应保留任何定时器）。
 */
export function nextToastExpiry(
  list: readonly AppNotification[],
  closedToastIds: ReadonlySet<number>,
  now: number,
): number | null {
  let next: number | null = null;
  for (const record of toastWindow(list)) {
    if (closedToastIds.has(record.id) || !toastAlive(record, now)) continue;
    const expiry = record.createdAt + TOAST_DURATION_MS;
    if (next === null || expiry < next) next = expiry;
  }
  return next;
}

/** 逐项结果的汇总计数（供浮动提示与通知台共用文案）。 */
export function itemCounts(items: readonly NotificationItem[]): {
  success: number;
  failed: number;
  skipped: number;
} {
  let success = 0;
  let failed = 0;
  let skipped = 0;
  for (const item of items) {
    if (item.status === "success") success++;
    else if (item.status === "failed") failed++;
    else skipped++;
  }
  return { success, failed, skipped };
}
