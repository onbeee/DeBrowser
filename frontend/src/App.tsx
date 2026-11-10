import { useCallback, useEffect } from "react";
import { Events } from "@wailsio/runtime";
import { AppService } from "../bindings/github.com/onbeee/DeBrowser/internal/service";
import { AppStatus } from "../bindings/events";
import Sidebar from "./components/layout/Sidebar";
import TopBar from "./components/layout/TopBar";
import StatusBar from "./components/layout/StatusBar";
import ConsolePanel from "./components/ConsolePanel";
import OverviewPage from "./pages/OverviewPage";
import PlannedPage from "./pages/PlannedPage";
import { useAppStore } from "./stores/useAppStore";

export default function App() {
  const page = useAppStore((state) => state.page);
  const consoleOpen = useAppStore((state) => state.consoleOpen);
  const setStatus = useAppStore((state) => state.setStatus);
  const setStatusError = useAppStore((state) => state.setStatusError);
  const log = useAppStore((state) => state.log);

  const refreshStatus = useCallback(() => {
    AppService.GetStatus()
      .then((status) => {
        setStatus(status);
        log("info", `GetStatus → ${status.name} v${status.version}, database ${status.database}`);
      })
      .catch((err: unknown) => {
        const message = err instanceof Error ? err.message : String(err);
        setStatusError(message);
        log("error", `GetStatus failed: ${message}`);
      });
  }, [log, setStatus, setStatusError]);

  useEffect(() => {
    refreshStatus();
    // Live updates pushed by the backend on the typed "app:status" event.
    const cancel = Events.On(AppStatus, (event) => {
      if (event.data) {
        setStatus(event.data);
      }
    });
    return () => {
      if (typeof cancel === "function") cancel();
    };
  }, [refreshStatus, setStatus]);

  return (
    <div className="flex h-full">
      <Sidebar />
      <div className="flex min-w-0 flex-1 flex-col">
        <TopBar onRefreshStatus={refreshStatus} />
        <main className="min-h-0 flex-1 overflow-y-auto bg-[#0f1115]">
          {page === "overview" ? <OverviewPage /> : <PlannedPage page={page} />}
        </main>
        {consoleOpen && <ConsolePanel />}
        <StatusBar />
      </div>
    </div>
  );
}
