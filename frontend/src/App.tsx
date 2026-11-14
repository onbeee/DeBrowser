import { useCallback, useEffect } from "react";
import { Events } from "@wailsio/runtime";
import { AppService } from "../bindings/github.com/onbeee/DeBrowser/internal/service";
import { AppStatus } from "../bindings/events";
import Sidebar from "./components/layout/Sidebar";
import TopBar from "./components/layout/TopBar";
import StatusBar from "./components/layout/StatusBar";
import DiagnosticsPage from "./pages/DiagnosticsPage";
import InstancesPage from "./pages/InstancesPage";
import OverviewPage from "./pages/OverviewPage";
import PlannedPage from "./pages/PlannedPage";
import ProxyConfigPage from "./pages/ProxyConfigPage";
import SettingsPage from "./pages/SettingsPage";
import { useAppStore, type Theme } from "./stores/useAppStore";

export default function App() {
  const page = useAppStore((state) => state.page);
  const setStatus = useAppStore((state) => state.setStatus);
  const setStatusError = useAppStore((state) => state.setStatusError);
  const setTheme = useAppStore((state) => state.setTheme);
  const log = useAppStore((state) => state.log);

  // 静默加载一次后端状态（供底部状态栏显示连接状态）；不再提
  // 供右上角常驻刷新入口，也不把状态刷新写进控制台（§9）。
  const refreshStatus = useCallback(() => {
    AppService.GetStatus()
      .then((status) => {
        setStatus(status);
      })
      .catch((err: unknown) => {
        const message = err instanceof Error ? err.message : String(err);
        setStatusError(message);
      });
  }, [setStatus, setStatusError]);

  useEffect(() => {
    refreshStatus();
    // 加载已保存的设置（主题），启动后立即应用。
    AppService.GetSettings()
      .then((settings) => setTheme(settings.theme as Theme))
      .catch((err: unknown) => {
        const message = err instanceof Error ? err.message : String(err);
        log("error", `加载设置失败：${message}`);
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
  }, [refreshStatus, setStatus, setTheme, log]);

  return (
    <div className="flex h-full">
      <Sidebar />
      <div className="flex min-w-0 flex-1 flex-col">
        <TopBar />
        <main className="min-h-0 flex-1 overflow-y-auto bg-(--app-bg)">
          {page === "overview" ? (
            <OverviewPage />
          ) : page === "settings" ? (
            <SettingsPage />
          ) : page === "instances" ? (
            <InstancesPage />
          ) : page === "proxyconfig" ? (
            <ProxyConfigPage />
          ) : page === "diagnostics" ? (
            <DiagnosticsPage />
          ) : (
            <PlannedPage page={page} />
          )}
        </main>
        <StatusBar />
      </div>
    </div>
  );
}
