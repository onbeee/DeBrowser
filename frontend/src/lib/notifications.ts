// 全局通知的纯逻辑部分：一套通知数据，两种展示层级。
//
// - 通知台：全面显示需要用户知道的操作结果与状态通知，
//   只保留最近 80 条，仅存在于应用运行期间的内存中，
//   不写入数据库或文件，不做持久化。
// - 通知浮窗：重点提醒。业务调用点发布通知时通过
//   important 明确标记重点，重点通知在进入通知台的同时
//   触发浮窗；普通通知只进入通知台。每条浮窗的 3 秒
//   展示计时由浮窗组件自身管理，本模块不维护任何全局
//   时间状态。
//
// 通知与日志是两套独立机制：日志（recordOperationFeedback
// + operation_id）负责持久记录与导出，本模块只负责面向
// 用户的即时展示，两者不共享存储。

/** 通知类型：与「是否重点」是两个独立维度。 */
export type NotificationKind = "info" | "success" | "warning" | "error";

/** 通知台最多显示的最近通知条数。 */
export const MAX_NOTIFICATIONS = 80;

/** 浮窗同时显示的最大条数（先进先出）。 */
export const MAX_VISIBLE_TOASTS = 3;

/** 每条浮窗的统一显示时长（毫秒），由浮窗组件自行计时。 */
export const TOAST_DURATION_MS = 3000;

/** 批量操作中的逐项结果（成功 / 失败 / 跳过及原因）。 */
export interface NotificationItem {
  name: string;
  status: "success" | "failed" | "skipped";
  reason?: string;
}

/** 一条通知实体：通知台与浮窗共用同一实体，不重复创建。 */
export interface AppNotification {
  id: number;
  kind: NotificationKind;
  summary: string;
  detail?: string;
  items?: NotificationItem[];
  operationId?: string;
  /** 是否重点提醒：由业务调用点明确决定，重点通知同时触发浮窗。 */
  important: boolean;
  createdAt: number;
}

/** 业务调用点发布通知时提供的内容。 */
export interface NotifyInput {
  kind: NotificationKind;
  summary: string;
  detail?: string;
  items?: NotificationItem[];
  operationId?: string;
  /** 缺省为普通通知（只进入通知台）；重点通知须显式标记。 */
  important?: boolean;
}

/**
 * 把一条新通知追加到通知列表末尾（最新在末尾，通知台
 * 展示时倒序），超过上限时移除最早的一条。不修改入参。
 */
export function pushNotification(
  list: readonly AppNotification[],
  input: NotifyInput,
  id: number,
  now: number,
): AppNotification[] {
  const record: AppNotification = {
    id,
    kind: input.kind,
    summary: input.summary,
    detail: input.detail,
    items: input.items,
    operationId: input.operationId,
    important: input.important === true,
    createdAt: now,
  };
  const next = [...list, record];
  return next.length > MAX_NOTIFICATIONS ? next.slice(next.length - MAX_NOTIFICATIONS) : next;
}

/**
 * 重点通知进入浮窗展示列表：同时最多 MAX_VISIBLE_TOASTS 条，
 * 已满时移除最早显示的一条（先进先出），被移除的浮窗不会
 * 再回来。列表中保存的是与通知台相同的通知实体引用。
 */
export function pushToast(
  toasts: readonly AppNotification[],
  record: AppNotification,
): AppNotification[] {
  const next = [...toasts, record];
  return next.length > MAX_VISIBLE_TOASTS ? next.slice(next.length - MAX_VISIBLE_TOASTS) : next;
}

/** 逐项结果的汇总计数（供浮窗与通知台共用文案）。 */
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
