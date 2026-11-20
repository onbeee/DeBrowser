import { useState } from "react";
import { ChevronDown, ChevronRight, Trash2, X } from "lucide-react";
import type { AppNotification } from "../../lib/notifications";
import { useNotificationStore } from "../../stores/useNotificationStore";
import { NotificationKindIcon } from "./NotificationToasts";

const ITEM_STATUS_LABEL: Record<string, string> = {
  success: "成功",
  failed: "失败",
  skipped: "跳过",
};

function RecordRow({ record }: { record: AppNotification }) {
  const [expanded, setExpanded] = useState(false);
  const hasItems = !!record.items && record.items.length > 0;
  return (
    <div className="border-b border-(--border) px-4 py-3">
      <div className="flex items-start gap-2">
        <span className="mt-0.5">
          <NotificationKindIcon kind={record.kind} size={15} />
        </span>
        <div className="min-w-0 flex-1">
          <p className="text-sm leading-snug text-(--text-main)">{record.summary}</p>
          <p className="mt-0.5 text-xs text-(--text-faint)">
            {new Date(record.createdAt).toLocaleTimeString("zh-CN")}
          </p>
          {record.detail && (
            <p className="mt-1 whitespace-pre-wrap break-words text-xs leading-relaxed text-(--text-dim)">
              {record.detail}
            </p>
          )}
          {hasItems && (
            <button
              type="button"
              className="mt-1.5 flex items-center gap-1 text-xs text-(--accent)"
              onClick={() => setExpanded((v) => !v)}
            >
              {expanded ? <ChevronDown size={13} /> : <ChevronRight size={13} />}
              {expanded ? "收起逐项结果" : `查看逐项结果（${record.items!.length} 项）`}
            </button>
          )}
        </div>
      </div>
      {hasItems && expanded && (
        <ul className="mt-2 space-y-1 pl-6">
          {record.items!.map((item, idx) => (
            <li key={idx} className="text-xs leading-relaxed text-(--text-dim)">
              <span className="text-(--text-main)">{item.name}</span>
              {"："}
              <span
                className={
                  item.status === "failed"
                    ? "text-(--danger)"
                    : item.status === "skipped"
                      ? "text-(--text-faint)"
                      : "text-(--ok)"
                }
              >
                {ITEM_STATUS_LABEL[item.status]}
              </span>
              {item.reason && <span> — {item.reason}</span>}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

/**
 * 通知台：从窗口右侧滑出的覆盖式面板，不改变页面布局与
 * 滚动位置，关闭后页面状态不变。展示最近 50 条通知记录，
 * 与浮动提示内容一致，可展开逐项结果，可一键清空。
 */
export default function NotificationCenter() {
  const open = useNotificationStore((s) => s.centerOpen);
  const setCenterOpen = useNotificationStore((s) => s.setCenterOpen);
  const records = useNotificationStore((s) => s.records);
  const clearAll = useNotificationStore((s) => s.clearAll);

  if (!open) return null;
  const ordered = [...records].reverse();

  return (
    <div className="fixed inset-0 z-50">
      <div className="absolute inset-0 bg-black/30" onClick={() => setCenterOpen(false)} />
      <aside className="notification-drawer absolute right-0 top-0 flex h-full w-[380px] max-w-[92vw] flex-col border-l border-(--border) bg-(--panel-bg) shadow-xl">
        <div className="flex shrink-0 items-center gap-2 border-b border-(--border) px-4 py-3">
          <h2 className="text-[15px] font-semibold">通知台</h2>
          <div className="ml-auto flex items-center gap-1">
            <button
              type="button"
              disabled={records.length === 0}
              onClick={clearAll}
              className="flex items-center gap-1 rounded-md px-2 py-1.5 text-xs text-(--text-dim) hover:bg-(--hover-bg) disabled:opacity-40"
            >
              <Trash2 size={13} /> 清空
            </button>
            <button
              type="button"
              aria-label="关闭通知台"
              onClick={() => setCenterOpen(false)}
              className="rounded-md p-1.5 text-(--text-dim) hover:bg-(--hover-bg)"
            >
              <X size={16} />
            </button>
          </div>
        </div>
        <div className="min-h-0 flex-1 overflow-y-auto">
          {ordered.length === 0 ? (
            <p className="px-4 py-10 text-center text-sm text-(--text-faint)">暂无通知</p>
          ) : (
            ordered.map((record) => <RecordRow key={record.id} record={record} />)
          )}
        </div>
      </aside>
    </div>
  );
}
