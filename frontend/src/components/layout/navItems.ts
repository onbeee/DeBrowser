import {
  Activity,
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
  /** 交付该模块的开发阶段（见 docs/ROADMAP.md）。 */
  stage: string;
}

export const NAV_ITEMS: NavItem[] = [
  { key: "overview", label: "总览", icon: LayoutDashboard, stage: "阶段 1" },
  { key: "instances", label: "浏览器实例", icon: AppWindow, stage: "阶段 3" },
  { key: "proxies", label: "代理", icon: Network, stage: "阶段 4" },
  { key: "kernels", label: "浏览器内核", icon: Cpu, stage: "阶段 5" },
  { key: "bookmarks", label: "书签", icon: Bookmark, stage: "阶段 5" },
  { key: "sync", label: "同步", icon: RefreshCw, stage: "阶段 6" },
  { key: "backup", label: "备份与恢复", icon: Archive, stage: "阶段 7" },
  { key: "diagnostics", label: "诊断", icon: Activity, stage: "阶段 5B" },
  { key: "settings", label: "系统设置", icon: Settings, stage: "基础设置" },
];
