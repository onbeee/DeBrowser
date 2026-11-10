import { RefreshCw, TerminalSquare } from "lucide-react";
import { NAV_ITEMS } from "./navItems";
import { useAppStore } from "../../stores/useAppStore";

interface TopBarProps {
  onRefreshStatus: () => void;
}

export default function TopBar({ onRefreshStatus }: TopBarProps) {
  const page = useAppStore((state) => state.page);
  const consoleOpen = useAppStore((state) => state.consoleOpen);
  const toggleConsole = useAppStore((state) => state.toggleConsole);

  const current = NAV_ITEMS.find((item) => item.key === page);

  return (
    <header className="flex h-14 shrink-0 items-center gap-3 border-b border-(--border) bg-(--chrome-bg) px-5">
      <h1 className="text-sm font-semibold">{current?.label ?? "DeBrowser"}</h1>
      <span className="text-xs text-(--text-faint)">{current?.stage}</span>
      <div className="flex-1" />
      <button
        onClick={onRefreshStatus}
        className="flex items-center gap-2 rounded-md border border-(--border-strong) px-3 py-1.5 text-xs text-(--text-dim) transition-colors hover:bg-(--hover-bg) hover:text-(--text-main)"
        title="刷新后端状态"
      >
        <RefreshCw size={14} />
        刷新状态
      </button>
      <button
        onClick={toggleConsole}
        className={`flex items-center gap-2 rounded-md border px-3 py-1.5 text-xs transition-colors ${
          consoleOpen
            ? "border-(--accent) bg-(--accent-soft) text-(--text-main)"
            : "border-(--border-strong) text-(--text-dim) hover:bg-(--hover-bg) hover:text-(--text-main)"
        }`}
        title="开关控制台"
      >
        <TerminalSquare size={14} />
        控制台
      </button>
    </header>
  );
}
