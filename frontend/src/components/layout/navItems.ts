import {
  AppWindow,
  Archive,
  Bookmark,
  Cpu,
  LayoutDashboard,
  Network,
  RefreshCw,
  Settings,
  type LucideIcon,
} from "lucide-react";
import type { PageKey } from "../../stores/useAppStore";

export interface NavItem {
  key: PageKey;
  label: string;
  icon: LucideIcon;
  /** Development stage that delivers this module (see docs/ROADMAP.md). */
  stage: string;
}

export const NAV_ITEMS: NavItem[] = [
  { key: "overview", label: "Overview", icon: LayoutDashboard, stage: "Stage 1" },
  { key: "instances", label: "Browser Instances", icon: AppWindow, stage: "Stage 3" },
  { key: "proxies", label: "Proxies", icon: Network, stage: "Stage 4" },
  { key: "kernels", label: "Browser Kernels", icon: Cpu, stage: "Stage 5" },
  { key: "bookmarks", label: "Bookmarks", icon: Bookmark, stage: "Stage 5" },
  { key: "sync", label: "Synchronization", icon: RefreshCw, stage: "Stage 6" },
  { key: "backup", label: "Backup & Recovery", icon: Archive, stage: "Stage 7" },
  { key: "settings", label: "Settings", icon: Settings, stage: "Later stages" },
];
