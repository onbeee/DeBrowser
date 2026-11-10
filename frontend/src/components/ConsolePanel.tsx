import { Trash2, X } from "lucide-react";
import { useAppStore } from "../stores/useAppStore";

/**
 * 控制台面板：实时显示本次会话中前端与后端的通信
 *（绑定调用与状态事件），只呈现真实运行消息。
 */
export default function ConsolePanel() {
  const logs = useAppStore((state) => state.logs);
  const clearLogs = useAppStore((state) => state.clearLogs);
  const setConsoleOpen = useAppStore((state) => state.setConsoleOpen);

  return (
    <section className="flex h-52 shrink-0 flex-col border-t border-(--border) bg-(--console-bg)">
      <div className="flex items-center gap-2 border-b border-(--border) px-4 py-1.5 text-[11px] text-(--text-dim)">
        <span className="font-semibold tracking-wider">控制台</span>
        <span>· 本次会话的绑定调用与状态事件</span>
        <div className="flex-1" />
        <button
          onClick={clearLogs}
          className="rounded p-1 hover:bg-(--hover-bg) hover:text-(--text-main)"
          title="清空控制台"
        >
          <Trash2 size={13} />
        </button>
        <button
          onClick={() => setConsoleOpen(false)}
          className="rounded p-1 hover:bg-(--hover-bg) hover:text-(--text-main)"
          title="关闭控制台"
        >
          <X size={13} />
        </button>
      </div>
      <div className="flex-1 overflow-y-auto px-4 py-2 font-mono text-[11.5px] leading-relaxed">
        {logs.length === 0 && (
          <div className="text-(--text-faint)">暂无消息，运行事件将显示在这里。</div>
        )}
        {logs.map((line) => (
          <div key={line.id} className={line.level === "error" ? "text-(--danger)" : "text-(--text-dim)"}>
            <span className="text-(--text-faint)">{line.time}</span> {line.message}
          </div>
        ))}
      </div>
    </section>
  );
}
