import { Trash2, X } from "lucide-react";
import { useAppStore } from "../stores/useAppStore";

/**
 * Console panel: a live view of this session's frontend/backend
 * communication (binding calls and status events). It shows real runtime
 * messages only — it is not a log file viewer yet.
 */
export default function ConsolePanel() {
  const logs = useAppStore((state) => state.logs);
  const clearLogs = useAppStore((state) => state.clearLogs);
  const setConsoleOpen = useAppStore((state) => state.setConsoleOpen);

  return (
    <section className="flex h-52 shrink-0 flex-col border-t border-[#262b36] bg-[#0b0d11]">
      <div className="flex items-center gap-2 border-b border-[#1d212b] px-4 py-1.5 text-[11px] text-[#8b93a5]">
        <span className="font-semibold uppercase tracking-wider">Console</span>
        <span>· binding calls &amp; status events from this session</span>
        <div className="flex-1" />
        <button
          onClick={clearLogs}
          className="rounded p-1 hover:bg-[#1a1e27] hover:text-white"
          title="Clear console"
        >
          <Trash2 size={13} />
        </button>
        <button
          onClick={() => setConsoleOpen(false)}
          className="rounded p-1 hover:bg-[#1a1e27] hover:text-white"
          title="Close console"
        >
          <X size={13} />
        </button>
      </div>
      <div className="flex-1 overflow-y-auto px-4 py-2 font-mono text-[11.5px] leading-relaxed">
        {logs.length === 0 && (
          <div className="text-[#646c7e]">No messages yet. Runtime events will appear here.</div>
        )}
        {logs.map((line) => (
          <div key={line.id} className={line.level === "error" ? "text-[#ff8589]" : "text-[#c6ccda]"}>
            <span className="text-[#646c7e]">{line.time}</span> {line.message}
          </div>
        ))}
      </div>
    </section>
  );
}
