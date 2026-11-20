import { useEffect, useState } from "react";
import { AlertTriangle, CheckCircle2, Info, X, XCircle } from "lucide-react";
import {
  itemCounts,
  nextToastExpiry,
  visibleToasts,
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
 * 全局浮动提示层：固定在窗口右上角，不参与页面布局、不被
 * 弹窗遮挡（z 序高于全部弹窗）。同时最多 3 条，按类型时长
 * 自动消失，每条都可手动关闭；点击提示打开通知台看详情。
 */
export default function NotificationToasts() {
  const records = useNotificationStore((s) => s.records);
  const closedToastIds = useNotificationStore((s) => s.closedToastIds);
  const closeToast = useNotificationStore((s) => s.closeToast);
  const setCenterOpen = useNotificationStore((s) => s.setCenterOpen);
  const [now, setNow] = useState(() => Date.now());

  // 只按下一条提示的到期时刻安排一次性定时器：没有未过期
  // 的提示时不保留定时器；新通知进入、手动关闭或记录清空
  // 都会通过依赖变化重新计算。
  useEffect(() => {
    const expiry = nextToastExpiry(records, closedToastIds, now);
    if (expiry === null) return;
    const t = setTimeout(() => setNow(Date.now()), Math.max(0, expiry - now) + 20);
    return () => clearTimeout(t);
  }, [records, closedToastIds, now]);

  const toasts = visibleToasts(records, closedToastIds, now);
  if (toasts.length === 0) return null;

  return (
    <div className="pointer-events-none fixed right-4 top-16 z-50 flex w-[360px] max-w-[calc(100vw-2rem)] flex-col gap-2">
      {toasts.map((record) => {
        const counts = countsLine(record);
        return (
          <div
            key={record.id}
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
                closeToast(record.id);
              }}
            >
              <X size={14} />
            </button>
          </div>
        );
      })}
    </div>
  );
}
