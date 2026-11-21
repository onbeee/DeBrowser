import { useEffect } from "react";
import { AlertTriangle, CheckCircle2, Info, X, XCircle } from "lucide-react";
import {
  TOAST_DURATION_MS,
  itemCounts,
  type AppNotification,
  type NotificationKind,
} from "../../lib/notifications";
import { useNotificationStore } from "../../stores/useNotificationStore";

export function NotificationKindIcon({ kind, size = 16 }: { kind: NotificationKind; size?: number }) {
  if (kind === "success") return <CheckCircle2 size={size} className="shrink-0 text-(--ok)" />;
  if (kind === "warning") return <AlertTriangle size={size} className="shrink-0 text-(--warn)" />;
  if (kind === "error") return <XCircle size={size} className="shrink-0 text-(--danger)" />;
  return <Info size={size} className="shrink-0 text-(--accent)" />;
}

function countsLine(record: AppNotification): string | null {
  if (!record.items || record.items.length === 0) return null;
  const c = itemCounts(record.items);
  const parts = [`成功 ${c.success} 个`];
  if (c.failed > 0) parts.push(`失败 ${c.failed} 个`);
  if (c.skipped > 0) parts.push(`跳过 ${c.skipped} 个`);
  return parts.join(" · ");
}

/**
 * 单条浮窗：组件实际挂载显示时启动自己的一次性 3000ms
 * 定时器，时间到后按通知 ID 关闭自己；手动关闭也只移除
 * 自己；卸载时清理定时器。每条浮窗独立计时，互不影响，
 * 不存在全局时间状态或全局到期计算。关闭浮窗只移除浮窗
 * 展示，通知台中的通知不受影响。
 */
function ToastItem({ record }: { record: AppNotification }) {
  const dismissToast = useNotificationStore((s) => s.dismissToast);
  const setCenterOpen = useNotificationStore((s) => s.setCenterOpen);

  useEffect(() => {
    const t = setTimeout(() => dismissToast(record.id), TOAST_DURATION_MS);
    return () => clearTimeout(t);
  }, [record.id, dismissToast]);

  const counts = countsLine(record);
  return (
    <div
      className="notification-toast pointer-events-auto flex cursor-pointer items-start gap-2 rounded-lg border border-(--border) bg-(--panel-bg) px-3 py-2.5 shadow-lg"
      onClick={() => setCenterOpen(true)}
      title="点击查看通知台"
    >
      <span className="mt-0.5">
        <NotificationKindIcon kind={record.kind} />
      </span>
      <div className="min-w-0 flex-1">
        <p className="text-sm leading-snug text-(--text-main)">{record.summary}</p>
        {counts && <p className="mt-0.5 text-xs text-(--text-dim)">{counts}</p>}
        {record.detail && (
          <p className="mt-0.5 line-clamp-2 text-xs leading-relaxed text-(--text-dim)">
            {record.detail}
          </p>
        )}
      </div>
      <button
        type="button"
        aria-label="关闭通知"
        className="shrink-0 rounded p-0.5 text-(--text-faint) hover:bg-(--hover-bg)"
        onClick={(e) => {
          e.stopPropagation();
          dismissToast(record.id);
        }}
      >
        <X size={14} />
      </button>
    </div>
  );
}

/**
 * 全局浮窗层：固定在窗口右上角，只展示重点通知，最多
 * 同时 3 条（最新在上）。数据来自通知 store 的浮窗展示
 * 列表，与通知台共用同一套通知实体。
 */
export default function NotificationToasts() {
  const toasts = useNotificationStore((s) => s.toasts);
  if (toasts.length === 0) return null;

  return (
    <div className="pointer-events-none fixed right-4 top-16 z-50 flex w-[360px] max-w-[calc(100vw-2rem)] flex-col gap-2">
      {[...toasts].reverse().map((record) => (
        <ToastItem key={record.id} record={record} />
      ))}
    </div>
  );
}
