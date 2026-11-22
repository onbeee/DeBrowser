import {
  AppWindow,
  Bookmark,
  Cpu,
  Database,
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
}

// 侧边栏一级入口由产品负责人固定为以下七项，名称与顺序不得增减或调整。
export const NAV_ITEMS: NavItem[] = [
  { key: "instances", label: "浏览器实例", icon: AppWindow },
  { key: "proxyconfig", label: "代理配置", icon: Network },
  { key: "kernels", label: "浏览器内核", icon: Cpu },
  { key: "bookmarks", label: "书签管理", icon: Bookmark },
  { key: "sync", label: "多开同步", icon: RefreshCw },
  { key: "datamanagement", label: "数据管理", icon: Database },
  { key: "settings", label: "系统设置", icon: Settings },
];
