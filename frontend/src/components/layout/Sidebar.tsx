import { Globe } from "lucide-react";
import { NAV_ITEMS } from "./navItems";
import { useAppStore } from "../../stores/useAppStore";

export default function Sidebar() {
  const page = useAppStore((state) => state.page);
  const setPage = useAppStore((state) => state.setPage);

  return (
    <aside className="flex w-60 shrink-0 flex-col border-r border-(--border) bg-(--chrome-bg)">
      <div className="flex items-center gap-2.5 px-5 pb-5 pt-6">
        <span className="flex h-9 w-9 items-center justify-center rounded-lg bg-(--accent) text-(--accent-contrast)">
          <Globe size={20} strokeWidth={2.2} />
        </span>
        <div className="leading-tight">
          <div className="text-[15px] font-semibold tracking-wide">DeBrowser</div>
          <div className="text-[11px] text-(--text-faint)">多配置文件浏览器管理</div>
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
                  ? "bg-(--accent-soft) text-(--text-main)"
                  : "text-(--text-dim) hover:bg-(--hover-bg) hover:text-(--text-main)"
              }`}
            >
              <Icon size={17} strokeWidth={2} />
              <span className="flex-1">{item.label}</span>
              {item.key !== "overview" && item.key !== "settings" && (
                <span className="rounded bg-(--badge-bg) px-1.5 py-0.5 text-[10px] text-(--text-faint)">
                  {item.stage}
                </span>
              )}
            </button>
          );
        })}
      </nav>

      <div className="px-5 py-4 text-[11px] leading-relaxed text-(--text-faint)">
        本地优先：你的配置文件与数据只保存在本机应用目录中。
      </div>
    </aside>
  );
}
