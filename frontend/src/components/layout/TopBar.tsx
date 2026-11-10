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
    <header className="flex h-14 shrink-0 items-center gap-3 border-b border-[#262b36] bg-[#13161d] px-5">
      <h1 className="text-sm font-semibold">{current?.label ?? "DeBrowser"}</h1>
      <span className="text-xs text-[#646c7e]">{current?.stage}</span>
      <div className="flex-1" />
      <button
        onClick={onRefreshStatus}
        className="flex items-center gap-2 rounded-md border border-[#2b3140] px-3 py-1.5 text-xs text-[#c6ccda] transition-colors hover:bg-[#1a1e27]"
        title="Refresh backend status"
      >
        <RefreshCw size={14} />
        Refresh status
      </button>
      <button
        onClick={toggleConsole}
        className={`flex items-center gap-2 rounded-md border px-3 py-1.5 text-xs transition-colors ${
          consoleOpen
            ? "border-[#2f6fed] bg-[#1d2739] text-white"
            : "border-[#2b3140] text-[#c6ccda] hover:bg-[#1a1e27]"
        }`}
        title="Toggle console"
      >
        <TerminalSquare size={14} />
        Console
      </button>
    </header>
  );
}
