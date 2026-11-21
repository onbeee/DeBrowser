import { create } from "zustand";
import {
  pushNotification,
  pushToast,
  type AppNotification,
  type NotificationKind,
  type NotifyInput,
} from "../lib/notifications.ts";

// 全局通知状态：一套通知数据，两种展示层级。
//
// - notifications 是唯一的通知列表，通知台据此全面显示
//   最近 80 条（仅内存，不持久化）。
// - toasts 是当前浮窗展示列表，保存重点通知的同一实体
//   引用，最多 3 条、先进先出；浮窗的关闭（自动或手动）
//   只从 toasts 移除，不影响 notifications。
// 业务页面只调用 notify 发布一次，不分别维护两份数据。

interface NotificationState {
  notifications: AppNotification[];
  toasts: AppNotification[];
  centerOpen: boolean;
  notify: (input: NotifyInput) => void;
  dismissToast: (id: number) => void;
  clearAll: () => void;
  setCenterOpen: (open: boolean) => void;
}

let nextId = 1;

export const useNotificationStore = create<NotificationState>()((set) => ({
  notifications: [],
  toasts: [],
  centerOpen: false,

  notify: (input) =>
    set((state) => {
      const notifications = pushNotification(state.notifications, input, nextId++, Date.now());
      const created = notifications[notifications.length - 1];
      // 重点通知在进入通知台的同时触发浮窗（同一实体）。
      const toasts = created.important ? pushToast(state.toasts, created) : state.toasts;
      return { notifications, toasts };
    }),

  // 只移除浮窗展示，不删除通知台中的通知。
  dismissToast: (id) =>
    set((state) => ({ toasts: state.toasts.filter((t) => t.id !== id) })),

  clearAll: () => set({ notifications: [], toasts: [] }),

  setCenterOpen: (open) => set({ centerOpen: open }),
}));

interface NotifyOptions {
  detail?: string;
  items?: NotifyInput["items"];
  operationId?: string;
  /** 重点通知须在调用点显式标记；缺省只进入通知台。 */
  important?: boolean;
}

/** 业务调用点发布通知的统一入口（只发布一次）。 */
export function notify(kind: NotificationKind, summary: string, opts?: NotifyOptions): void {
  useNotificationStore.getState().notify({
    kind,
    summary,
    detail: opts?.detail,
    items: opts?.items,
    operationId: opts?.operationId,
    important: opts?.important,
  });
}

function publish(kind: NotificationKind, summary: string, opts?: NotifyOptions): void {
  notify(kind, summary, opts);
}

export function notifySuccess(summary: string, opts?: NotifyOptions): void {
  publish("success", summary, opts);
}

export function notifyInfo(summary: string, opts?: NotifyOptions): void {
  publish("info", summary, opts);
}

export function notifyWarning(summary: string, opts?: NotifyOptions): void {
  publish("warning", summary, opts);
}

export function notifyError(summary: string, opts?: NotifyOptions): void {
  publish("error", summary, opts);
}
