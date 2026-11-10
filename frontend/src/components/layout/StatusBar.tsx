import { useAppStore } from "../../stores/useAppStore";

export default function StatusBar() {
  const status = useAppStore((state) => state.status);
  const statusError = useAppStore((state) => state.statusError);

  const dotClass = statusError
    ? "bg-[#e5484d]"
    : status
      ? status.database === "ok"
        ? "bg-[#46a758]"
        : "bg-[#e5484d]"
      : "bg-[#646c7e]";
  const label = statusError
    ? "Backend unreachable"
    : status
      ? status.database === "ok"
        ? "Backend connected · Database OK"
        : "Backend connected · Database error"
      : "Connecting to backend…";

  return (
    <footer className="flex h-8 shrink-0 items-center gap-4 border-t border-[#262b36] bg-[#13161d] px-5 text-[11px] text-[#8b93a5]">
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
            Data: {status.dataDir}
          </span>
          <span className="ml-auto">Checked {new Date(status.checkedAt).toLocaleTimeString()}</span>
        </>
      )}
      {statusError && <span className="ml-auto text-[#e5484d]">{statusError}</span>}
    </footer>
  );
}
