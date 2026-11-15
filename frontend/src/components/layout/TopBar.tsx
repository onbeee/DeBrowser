import { NAV_ITEMS } from "./navItems";
import { useAppStore } from "../../stores/useAppStore";

// 顶栏是页面标题的唯一所有者：只显示当前一级页面的名称。
// 页面内容中不再重复主标题或阶段名称；没有底部固定状态栏，
// 连接与数据库异常由各页面在真正发生时就地提示。
export default function TopBar() {
  const page = useAppStore((state) => state.page);
  const current = NAV_ITEMS.find((item) => item.key === page);

  return (
    <header className="flex h-14 shrink-0 items-center gap-3 border-b border-(--border) bg-(--topbar-bg) px-6">
      <h1 className="text-xl font-semibold">{current?.label ?? "DeBrowser"}</h1>
    </header>
  );
}
