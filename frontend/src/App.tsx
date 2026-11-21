import { useCallback, useEffect, useRef } from "react";
import { Events } from "@wailsio/runtime";
import { AppService } from "../bindings/github.com/onbeee/DeBrowser/internal/service";
import { AppStatus } from "../bindings/events";
import Sidebar from "./components/layout/Sidebar";
import TopBar from "./components/layout/TopBar";
import NotificationCenter from "./components/notifications/NotificationCenter";
import NotificationToasts from "./components/notifications/NotificationToasts";
import DataManagementPage from "./pages/DataManagementPage";
import InstancesPage from "./pages/InstancesPage";
import KernelsPage from "./pages/KernelsPage";
import PlannedPage from "./pages/PlannedPage";
import SyncPage from "./pages/SyncPage";
import BookmarksPage from "./pages/BookmarksPage";
import ProxyConfigPage from "./pages/ProxyConfigPage";
import SettingsPage from "./pages/SettingsPage";
import { useAppStore, type Theme } from "./stores/useAppStore";
import { notifyError } from "./stores/useNotificationStore";
import { recordPageNavigation } from "./lib/operations";

export default function App() {
  const page = useAppStore((state) => state.page);
  const setStatus = useAppStore((state) => state.setStatus);
  const setTheme = useAppStore((state) => state.setTheme);

  // 加载一次后端状态（供系统设置与数据管理页面显示）。
  // 只在挂载时调用一次；读取失败通过全局通知显示真实原因，
  // 不会重复弹出（后续状态更新走 app:status 事件推送）。
  const refreshStatus = useCallback(() => {
    AppService.GetStatus()
      .then((status) => {
        setStatus(status);
      })
      .catch((err: unknown) => {
        const message = err instanceof Error ? err.message : String(err);
        notifyError(`读取应用状态失败：${message}`);
      });
  }, [setStatus]);

  useEffect(() => {
    refreshStatus();
    // 加载已保存的设置（主题），启动后立即应用。
    AppService.GetSettings()
      .then((settings) => setTheme(settings.theme as Theme))
      .catch((err: unknown) => {
        const message = err instanceof Error ? err.message : String(err);
        notifyError(`加载设置失败：${message}`);
      });
    // 后端通过类型化事件 app:status 推送的实时更新。
    const cancel = Events.On(AppStatus, (event) => {
      if (event.data) {
        setStatus(event.data);
      }
    });
    return () => {
      if (typeof cancel === "function") cancel();
    };
  }, [refreshStatus, setStatus, setTheme]);

  // 页面访问记录（统一记录系统）：打开/离开页面写入运行记录。
  const prevPage = useRef<string | null>(null);
  useEffect(() => {
    if (prevPage.current && prevPage.current !== page) {
      recordPageNavigation("离开页面", prevPage.current);
    }
    recordPageNavigation("打开页面", page);
    prevPage.current = page;
  }, [page]);

  return (
    <div className="flex h-full">
      <Sidebar />
      <div className="flex min-w-0 flex-1 flex-col">
        <TopBar />
        <main className="min-h-0 flex-1 overflow-y-auto bg-(--app-bg)">
          {page === "settings" ? (
            <SettingsPage />
          ) : page === "instances" ? (
            <InstancesPage />
          ) : page === "kernels" ? (
            <KernelsPage />
          ) : page === "proxyconfig" ? (
            <ProxyConfigPage />
          ) : page === "datamanagement" ? (
            <DataManagementPage />
          ) : page === "bookmarks" ? (
            <BookmarksPage />
          ) : page === "sync" ? (
            <SyncPage />
          ) : (
            <PlannedPage page={page} />
          )}
        </main>
      </div>
      <NotificationToasts />
      <NotificationCenter />
    </div>
  );
}
