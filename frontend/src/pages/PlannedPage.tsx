import { Construction } from "lucide-react";
import { NAV_ITEMS } from "../components/layout/navItems";
import type { PageKey } from "../stores/useAppStore";

interface PlannedPageProps {
  page: Exclude<PageKey, "overview">;
}

/**
 * Honest placeholder for a module that a later stage will deliver. It shows
 * no fake data and offers no non-working controls.
 */
export default function PlannedPage({ page }: PlannedPageProps) {
  const item = NAV_ITEMS.find((nav) => nav.key === page);
  const Icon = item?.icon ?? Construction;

  return (
    <div className="flex h-full flex-col items-center justify-center px-8 text-center">
      <span className="flex h-14 w-14 items-center justify-center rounded-2xl bg-[#1a1e27] text-[#8b93a5]">
        <Icon size={26} />
      </span>
      <h2 className="mt-4 text-lg font-semibold">{item?.label}</h2>
      <p className="mt-1.5 max-w-md text-sm leading-relaxed text-[#a7aebd]">
        This module is scheduled for {item?.stage} (see docs/ROADMAP.md) and is not implemented
        yet. Stage 1 only provides the application foundation, so there is intentionally no data
        or functionality here.
      </p>
    </div>
  );
}
