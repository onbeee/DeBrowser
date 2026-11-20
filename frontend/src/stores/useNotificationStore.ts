import { create } from "zustand";
import {
  pushNotification,
  type AppNotification,
  type NotificationInput,
  type NotificationKind,
} from "../lib/notifications";

/**
 * 全局通知状态：全软件唯一通知入口。记录保存在内存中，
 * 最多 50 条，不落库、不持久化；页面切换、弹窗关闭都不
 * 丢失。浮动提示与通知台读取同一份记录。
 */
interface NotificationState {
  records: AppNotification[];
  closedToastIds: ReadonlySet<number>;
  centerOpen: boolean;
  notify: (input: NotificationInput) => void;
  closeToast: (id: number) => void;
  setCenterOpen: (open: boolean) => void;
  clearAll: () => void;
}

let nextId = 1;

export const useNotificationStore = create<NotificationState>((set) => ({
  records: [],
  closedToastIds: new Set<number>(),
  centerOpen: false,
  notify: (input) =>
    set((state) => ({
      records: pushNotification(state.records, input, nextId++, Date.now()),
    })),
  closeToast: (id) =>
    set((state) => {
      const next = new Set(state.closedToastIds);
      next.add(id);
      return { closedToastIds: next };
    }),
  setCenterOpen: (open) => set({ centerOpen: open }),
  clearAll: () => set({ records: [], closedToastIds: new Set<number>() }),
}));

/** 页面与业务代码的统一调用入口。 */
export function notify(kind: NotificationKind, summary: string, extra?: Partial<NotificationInput>): void {
  useNotificationStore.getState().notify({ kind, summary, ...extra });
}

export function notifySuccess(summary: string, extra?: Partial<NotificationInput>): void {
  notify("success", summary, extra);
}

export function notifyInfo(summary: string, extra?: Partial<NotificationInput>): void {
  notify("info", summary, extra);
}

export function notifyWarning(summary: string, extra?: Partial<NotificationInput>): void {
  notify("warning", summary, extra);
}

export function notifyError(summary: string, extra?: Partial<NotificationInput>): void {
  notify("error", summary, extra);
}
