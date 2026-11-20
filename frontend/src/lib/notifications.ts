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

/** 各类型浮动提示的默认显示时长（毫秒），全局统一，页面不得自设。 */
export const NOTIFICATION_DURATIONS: Record<NotificationKind, number> = {
  info: 3000,
  success: 4000,
  warning: 6000,
  error: 8000,
};

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

/** 一条记录的浮动提示是否仍在显示期内。 */
export function toastAlive(record: AppNotification, now: number): boolean {
  return now - record.createdAt < NOTIFICATION_DURATIONS[record.kind];
}

/**
 * 当前应显示的浮动提示：未过期、未被手动关闭的记录中，
 * 取最新 MAX_VISIBLE_TOASTS 条，最新在前。超出的记录不
 * 丢失，可在通知台查看；较早的提示过期后，较新的仍在
 * 显示期内的记录按此规则继续占据可见位置。
 */
export function visibleToasts(
  list: readonly AppNotification[],
  closedToastIds: ReadonlySet<number>,
  now: number,
): AppNotification[] {
  const alive = list.filter((r) => !closedToastIds.has(r.id) && toastAlive(r, now));
  return alive.slice(-MAX_VISIBLE_TOASTS).reverse();
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
