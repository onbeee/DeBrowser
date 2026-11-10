import { useAppStore } from "../../stores/useAppStore";

export default function StatusBar() {
  const status = useAppStore((state) => state.status);
  const statusError = useAppStore((state) => state.statusError);

  const dotClass = statusError
    ? "bg-(--danger)"
    : status
      ? status.database === "ok"
        ? "bg-(--ok)"
        : "bg-(--danger)"
      : "bg-(--text-faint)";
  const label = statusError
    ? "后端不可达"
    : status
      ? status.database === "ok"
        ? "后端已连接 · 数据库正常"
        : "后端已连接 · 数据库异常"
      : "正在连接后端…";

  return (
    <footer className="flex h-8 shrink-0 items-center gap-4 border-t border-(--border) bg-(--chrome-bg) px-5 text-[11px] text-(--text-dim)">
      <span className="flex items-center gap-2">
        <span className={`h-2 w-2 rounded-full ${dotClass}`} />
        {label}
      </span>
      {status && (
        <>
          <span>
            {status.name} v{status.version}
          </span>
          <span>
            {status.platform}/{status.arch}
          </span>
          <span className="max-w-[380px] truncate" title={status.dataDir}>
            数据目录：{status.dataDir}
          </span>
          <span className="ml-auto">
            更新于 {new Date(status.checkedAt).toLocaleTimeString("zh-CN")}
          </span>
        </>
      )}
      {statusError && <span className="ml-auto text-(--danger)">{statusError}</span>}
    </footer>
  );
}
