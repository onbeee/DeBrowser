import { useCallback, useEffect } from "react";
import { Events } from "@wailsio/runtime";
import { AppService } from "../bindings/github.com/onbeee/DeBrowser/internal/service";
import { AppStatus } from "../bindings/events";
import Sidebar from "./components/layout/Sidebar";
import TopBar from "./components/layout/TopBar";
import StatusBar from "./components/layout/StatusBar";
import ConsolePanel from "./components/ConsolePanel";
import DiagnosticsPage from "./pages/DiagnosticsPage";
import InstancesPage from "./pages/InstancesPage";
import OverviewPage from "./pages/OverviewPage";
import PlannedPage from "./pages/PlannedPage";
import ProxiesPage from "./pages/ProxiesPage";
import SettingsPage from "./pages/SettingsPage";
import { useAppStore, type Theme } from "./stores/useAppStore";

export default function App() {
  const page = useAppStore((state) => state.page);
  const consoleOpen = useAppStore((state) => state.consoleOpen);
  const setStatus = useAppStore((state) => state.setStatus);
  const setStatusError = useAppStore((state) => state.setStatusError);
  const setTheme = useAppStore((state) => state.setTheme);
  const log = useAppStore((state) => state.log);

  const refreshStatus = useCallback(() => {
    AppService.GetStatus()
      .then((status) => {
        setStatus(status);
        log(
          "info",
          `状态已更新：${status.name} v${status.version}，数据库${status.database === "ok" ? "正常" : "异常"}`,
        );
      })
      .catch((err: unknown) => {
        const message = err instanceof Error ? err.message : String(err);
        setStatusError(message);
        log("error", `获取状态失败：${message}`);
      });
  }, [log, setStatus, setStatusError]);

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
        <TopBar onRefreshStatus={refreshStatus} />
        <main className="min-h-0 flex-1 overflow-y-auto bg-(--app-bg)">
          {page === "overview" ? (
            <OverviewPage />
          ) : page === "settings" ? (
            <SettingsPage />
          ) : page === "instances" ? (
            <InstancesPage />
          ) : page === "proxies" ? (
            <ProxiesPage />
          ) : page === "diagnostics" ? (
            <DiagnosticsPage />
          ) : (
            <PlannedPage page={page} />
          )}
        </main>
        {consoleOpen && <ConsolePanel />}
        <StatusBar />
      </div>
    </div>
  );
}
