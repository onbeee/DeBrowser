import { Bell } from "lucide-react";
import { NAV_ITEMS } from "./navItems";
import { useAppStore } from "../../stores/useAppStore";
import { useNotificationStore } from "../../stores/useNotificationStore";

// 顶栏是页面标题的唯一所有者：只显示当前一级页面的名称，
// 右侧是全局「通知台」入口。页面内容中不再重复主标题；
// 操作结果与异常通过全局通知系统统一反馈（通知台全面显示，重点结果同时浮窗提醒）。
export default function TopBar() {
  const page = useAppStore((state) => state.page);
  const setCenterOpen = useNotificationStore((state) => state.setCenterOpen);
  const current = NAV_ITEMS.find((item) => item.key === page);

  return (
    <header className="flex h-14 shrink-0 items-center gap-3 border-b border-(--border) bg-(--topbar-bg) px-6">
      <h1 className="text-xl font-semibold">{current?.label ?? "DeBrowser"}</h1>
      <button
        type="button"
        onClick={() => setCenterOpen(true)}
        className="ml-auto flex items-center gap-1.5 rounded-md border border-(--border-strong) px-3 py-1.5 text-sm text-(--text-dim) transition-colors hover:bg-(--hover-bg) hover:text-(--text-main)"
      >
        <Bell size={15} />
        通知台
      </button>
    </header>
  );
}
