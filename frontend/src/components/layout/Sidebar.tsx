import { Globe } from "lucide-react";
import { NAV_ITEMS } from "./navItems";
import { useAppStore } from "../../stores/useAppStore";

export default function Sidebar() {
  const page = useAppStore((state) => state.page);
  const setPage = useAppStore((state) => state.setPage);

  return (
    <aside className="flex w-60 shrink-0 flex-col border-r border-[#262b36] bg-[#13161d]">
      <div className="flex items-center gap-2.5 px-5 pb-5 pt-6">
        <span className="flex h-9 w-9 items-center justify-center rounded-lg bg-[#2f6fed] text-white">
          <Globe size={20} strokeWidth={2.2} />
        </span>
        <div className="leading-tight">
          <div className="text-[15px] font-semibold tracking-wide">DeBrowser</div>
          <div className="text-[11px] text-[#8b93a5]">Multi-profile browser manager</div>
        </div>
      </div>

      <nav className="flex-1 space-y-1 overflow-y-auto px-3">
        {NAV_ITEMS.map((item) => {
          const Icon = item.icon;
          const active = page === item.key;
          return (
            <button
              key={item.key}
              onClick={() => setPage(item.key)}
              className={`flex w-full items-center gap-3 rounded-md px-3 py-2 text-left text-[13px] transition-colors ${
                active
                  ? "bg-[#1d2739] text-white"
                  : "text-[#a7aebd] hover:bg-[#1a1e27] hover:text-white"
              }`}
            >
              <Icon size={17} strokeWidth={2} />
              <span className="flex-1">{item.label}</span>
              {item.key !== "overview" && (
                <span className="rounded bg-[#222836] px-1.5 py-0.5 text-[10px] text-[#8b93a5]">
                  {item.stage}
                </span>
              )}
            </button>
          );
        })}
      </nav>

      <div className="px-5 py-4 text-[11px] leading-relaxed text-[#646c7e]">
        Local-first. Your profiles and data stay on this machine.
      </div>
    </aside>
  );
}
