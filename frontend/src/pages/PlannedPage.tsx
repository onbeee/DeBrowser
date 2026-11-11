import { Construction } from "lucide-react";
import { NAV_ITEMS } from "../components/layout/navItems";
import type { PageKey } from "../stores/useAppStore";

interface PlannedPageProps {
  page: Exclude<PageKey, "overview" | "settings" | "instances">;
}

/**
 * 后续阶段模块的诚实占位页：不展示假数据，也不提供无效控件。
 */
export default function PlannedPage({ page }: PlannedPageProps) {
  const item = NAV_ITEMS.find((nav) => nav.key === page);
  const Icon = item?.icon ?? Construction;

  return (
    <div className="flex h-full flex-col items-center justify-center px-8 text-center">
      <span className="flex h-14 w-14 items-center justify-center rounded-2xl bg-(--hover-bg) text-(--text-faint)">
        <Icon size={26} />
      </span>
      <h2 className="mt-4 text-lg font-semibold">{item?.label}</h2>
      <p className="mt-1.5 max-w-md text-sm leading-relaxed text-(--text-dim)">
        该模块计划在{item?.stage}交付（见 docs/ROADMAP.md），尚未实现。当前版本只提供应用基础架构，这里有意不放置任何数据或功能。
      </p>
    </div>
  );
}
