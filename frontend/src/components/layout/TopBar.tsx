import { NAV_ITEMS } from "./navItems";
import { useAppStore } from "../../stores/useAppStore";

// 顶栏只显示当前页面标题。右上角不再常驻「刷新状态」与「控制台」
// 入口（Stage 6C 重做 §9）：状态通过底部状态栏与「日志与诊断」
// 页面查看，错误在各页面内就地提示。
export default function TopBar() {
  const page = useAppStore((state) => state.page);
  const current = NAV_ITEMS.find((item) => item.key === page);

  return (
    <header className="flex h-14 shrink-0 items-center gap-3 border-b border-(--border) bg-(--chrome-bg) px-5">
      <h1 className="text-sm font-semibold">{current?.label ?? "DeBrowser"}</h1>
      <span className="text-xs text-(--text-faint)">{current?.stage}</span>
    </header>
  );
}
