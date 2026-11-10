import { CheckCircle2, Database, FolderCog, MonitorCog, Timer } from "lucide-react";
import { useAppStore } from "../stores/useAppStore";

function formatUptime(totalSeconds: number): string {
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  if (minutes <= 0) return `${seconds}s`;
  return `${minutes}m ${seconds}s`;
}

export default function OverviewPage() {
  const status = useAppStore((state) => state.status);
  const statusError = useAppStore((state) => state.statusError);

  return (
    <div className="mx-auto max-w-4xl space-y-6 p-8">
      <div>
        <h2 className="text-xl font-semibold">Project foundation is running</h2>
        <p className="mt-1.5 max-w-2xl text-sm leading-relaxed text-[#a7aebd]">
          Stage 1 delivers the application shell: configuration, structured logging, the SQLite
          foundation and a live connection between this interface and the Go backend. Business
          modules (browser instances, proxies, kernels, bookmarks) arrive in later stages — no
          placeholder data is shown for them.
        </p>
      </div>

      {statusError && (
        <div className="rounded-lg border border-[#5a2b2e] bg-[#2a1517] px-4 py-3 text-sm text-[#ff8589]">
          Could not reach the Go backend: {statusError}
        </div>
      )}

      <div className="grid grid-cols-2 gap-4">
        <div className="rounded-lg border border-[#262b36] bg-[#15181f] p-4">
          <div className="flex items-center gap-2 text-xs uppercase tracking-wider text-[#8b93a5]">
            <CheckCircle2 size={14} /> Application
          </div>
          <div className="mt-2 text-lg font-semibold">
            {status ? `${status.name} v${status.version}` : "Loading…"}
          </div>
          <div className="mt-0.5 text-xs text-[#8b93a5]">Wails v3 · Go backend · React frontend</div>
        </div>

        <div className="rounded-lg border border-[#262b36] bg-[#15181f] p-4">
          <div className="flex items-center gap-2 text-xs uppercase tracking-wider text-[#8b93a5]">
            <MonitorCog size={14} /> Platform
          </div>
          <div className="mt-2 text-lg font-semibold">
            {status ? `${status.platform} / ${status.arch}` : "Loading…"}
          </div>
          <div className="mt-0.5 text-xs text-[#8b93a5]">Target: Windows 11 x64</div>
        </div>

        <div className="rounded-lg border border-[#262b36] bg-[#15181f] p-4">
          <div className="flex items-center gap-2 text-xs uppercase tracking-wider text-[#8b93a5]">
            <Database size={14} /> Database
          </div>
          <div className="mt-2 text-lg font-semibold">
            {status ? (status.database === "ok" ? "SQLite · healthy" : "SQLite · error") : "Loading…"}
          </div>
          <div className="mt-0.5 text-xs text-[#8b93a5]">WAL mode · foreign keys on · migrations applied</div>
        </div>

        <div className="rounded-lg border border-[#262b36] bg-[#15181f] p-4">
          <div className="flex items-center gap-2 text-xs uppercase tracking-wider text-[#8b93a5]">
            <Timer size={14} /> Session
          </div>
          <div className="mt-2 text-lg font-semibold">
            {status ? `Up ${formatUptime(status.uptimeSeconds)}` : "Loading…"}
          </div>
          <div className="mt-0.5 flex items-center gap-1.5 text-xs text-[#8b93a5]">
            <FolderCog size={12} />
            <span className="truncate" title={status?.dataDir}>
              {status ? status.dataDir : "Resolving data directory…"}
            </span>
          </div>
        </div>
      </div>

      <div className="rounded-lg border border-[#262b36] bg-[#15181f] p-4">
        <h3 className="text-sm font-semibold">What Stage 1 includes</h3>
        <ul className="mt-2 list-inside list-disc space-y-1 text-sm leading-relaxed text-[#a7aebd]">
          <li>Modular monolith layout: Go backend under <code className="text-[#e6e9ef]">internal/</code>, React frontend under <code className="text-[#e6e9ef]">frontend/</code></li>
          <li>Configuration and data-directory management, structured JSON logging</li>
          <li>SQLite infrastructure with migrations (business schema arrives per stage)</li>
          <li>Typed Wails bindings and the <code className="text-[#e6e9ef]">app:status</code> event channel</li>
          <li>CI on Windows x64: format, tests, vet, type check, frontend and desktop builds</li>
        </ul>
      </div>
    </div>
  );
}
