import {
  AppWindow,
  Bookmark,
  Cpu,
  Database,
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

// 侧边栏一级入口由产品负责人固定为以下八项，名称与顺序不得增减或调整。
export const NAV_ITEMS: NavItem[] = [
  { key: "overview", label: "总览", icon: LayoutDashboard, stage: "阶段 1" },
  { key: "instances", label: "浏览器实例", icon: AppWindow, stage: "阶段 3" },
  { key: "proxyconfig", label: "代理配置", icon: Network, stage: "暂未开放" },
  { key: "kernels", label: "浏览器内核", icon: Cpu, stage: "阶段 9" },
  { key: "bookmarks", label: "书签管理", icon: Bookmark, stage: "阶段 9" },
  { key: "sync", label: "多开同步", icon: RefreshCw, stage: "阶段 10" },
  { key: "datamanagement", label: "数据管理", icon: Database, stage: "阶段 5B" },
  { key: "settings", label: "系统设置", icon: Settings, stage: "基础设置" },
];
