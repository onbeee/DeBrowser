import { useCallback, useEffect, useState } from "react";
import { Activity, Database, Timer } from "lucide-react";
import {
  DiagnosticsService,
  type DiagnosticsView,
  type ExportResult,
  type LogExportInfo,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import { useAppStore } from "../stores/useAppStore";
import { notifyError, notifySuccess } from "../stores/useNotificationStore";
import { newOperationId, recordOperationFeedback } from "../lib/operations";

type RecordCategory = "all" | "operation" | "error" | "runtime";

const RECORD_ROWS: { key: RecordCategory; label: string }[] = [
  { key: "all", label: "全部记录" },
  { key: "operation", label: "操作记录" },
  { key: "error", label: "错误记录" },
  { key: "runtime", label: "运行记录" },
];

const CAPACITY_PRESETS: { label: string; bytes: number }[] = [
  { label: "30 MB", bytes: 30 * 1024 * 1024 },
  { label: "50 MB", bytes: 50 * 1024 * 1024 },
  { label: "100 MB", bytes: 100 * 1024 * 1024 },
  { label: "200 MB", bytes: 200 * 1024 * 1024 },
  { label: "500 MB", bytes: 500 * 1024 * 1024 },
  { label: "1 GB", bytes: 1024 * 1024 * 1024 },
];

/** 容量显示：整数 MB/GB 时按示例格式（如「30 MB」），否则用通用格式。 */
function formatCapacity(bytes: number): string {
  if (bytes % (1024 * 1024 * 1024) === 0) return `${bytes / (1024 * 1024 * 1024)} GB`;
  if (bytes % (1024 * 1024) === 0) return `${bytes / (1024 * 1024)} MB`;
  return formatBytes(bytes);
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(2)} MB`;
}

function formatUptime(totalSeconds: number): string {
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  if (minutes <= 0) return `${seconds} 秒`;
  return `${minutes} 分 ${seconds} 秒`;
}

/**
 * 数据管理：数据概览、统一日志（全部 / 操作 / 错误 / 运行
 * 记录与总容量管理）。日志是唯一的记录系统：没有独立的
 * 操作审计或诊断导出区域。
 */
export default function DataManagementPage() {
  const status = useAppStore((state) => state.status);
  const [view, setView] = useState<DiagnosticsView | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [exporting, setExporting] = useState(false);
  // 记录导出对话框（类别 + 动态日期范围）。
  const [exportDialog, setExportDialog] = useState<RecordCategory | null>(null);
  const [exportInfo, setExportInfo] = useState<LogExportInfo | null>(null);
  const [exportStart, setExportStart] = useState("");
  const [exportEnd, setExportEnd] = useState("");
  const [dialogError, setDialogError] = useState<string | null>(null);
  // 容量设置对话框。
  const [capacityDialog, setCapacityDialog] = useState(false);
  const [capChoice, setCapChoice] = useState<number | "custom">(100 * 1024 * 1024);
  const [capCustomMB, setCapCustomMB] = useState("");
  const [capError, setCapError] = useState<string | null>(null);
  const [capSaving, setCapSaving] = useState(false);

  const refresh = useCallback(() => {
    DiagnosticsService.GetDiagnostics()
      .then((v) => {
        setView(v);
        setLoadError(null);
      })
      .catch((err: unknown) => {
        const message = err instanceof Error ? err.message : String(err);
        // 首次读取失败保留原因供页面显示失败状态（不一直
        // 显示加载中）；已有数据时保留旧数据不清空。
        setLoadError((prev) => prev ?? message);
        notifyError(`无法读取数据管理信息：${message}`);
      });
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  // 打开某类记录的导出设置：日期范围每次都从后端按当前实际
  // 保留的记录重新读取（旧记录被自动清理后范围会随之变化）。
  const openExportDialog = useCallback((category: RecordCategory) => {
    setExportDialog(category);
    setExportInfo(null);
    setExportStart("");
    setExportEnd("");
    setDialogError(null);
    DiagnosticsService.GetLogExportInfo(category)
      .then((info) => {
        setExportInfo(info);
        if (info.hasRecords) {
          setExportStart(info.earliestDate);
          setExportEnd(info.latestDate);
        }
      })
      .catch((err: unknown) => {
        setDialogError(err instanceof Error ? err.message : String(err));
      });
  }, []);

  const confirmRecordsExport = useCallback(() => {
    if (!exportDialog) return;
    const category = exportDialog;
    const label = RECORD_ROWS.find((r) => r.key === category)?.label ?? category;
    setExporting(true);
    setDialogError(null);
    const opId = newOperationId();
    DiagnosticsService.ExportRecords(opId, category, exportStart, exportEnd)
      .then((res: ExportResult) => {
        if (res.cancelled) {
          // 用户主动取消保存对话框：静默关闭，不通知、
          // 不记录成功或失败的操作反馈。
          setExportDialog(null);
        } else {
          const msg = `${label}导出完成：${res.path}`;
          notifySuccess(msg, { operationId: opId, important: true });
          setExportDialog(null);
          recordOperationFeedback(opId, true, msg, "数据管理", "logs.export");
          refresh();
        }
      })
      .catch((err: unknown) => {
        const message = err instanceof Error ? err.message : String(err);
        notifyError(`导出失败：${message}`, { operationId: opId, important: true });
        recordOperationFeedback(opId, false, `导出失败：${message}`, "数据管理", "logs.export");
      })
      .finally(() => setExporting(false));
  }, [exportDialog, exportStart, exportEnd, refresh]);

  const openCapacityDialog = useCallback(() => {
    setCapError(null);
    const limit = view?.logCapacityLimitBytes ?? 100 * 1024 * 1024;
    const preset = CAPACITY_PRESETS.find((p) => p.bytes === limit);
    if (preset) {
      setCapChoice(preset.bytes);
      setCapCustomMB("");
    } else {
      setCapChoice("custom");
      setCapCustomMB(String(Math.round((limit / (1024 * 1024)) * 10) / 10));
    }
    setCapacityDialog(true);
  }, [view]);

  const confirmCapacity = useCallback(() => {
    let bytes: number;
    if (capChoice === "custom") {
      const mb = Number(capCustomMB);
      if (!Number.isFinite(mb) || mb < 1) {
        setCapError("自定义容量至少为 1 MB。");
        return;
      }
      bytes = Math.round(mb * 1024 * 1024);
    } else {
      bytes = capChoice;
    }
    setCapSaving(true);
    setCapError(null);
    const opId = newOperationId();
    DiagnosticsService.SetLogCapacity(opId, bytes)
      .then(() => {
        const msg = `日志容量已设置为 ${formatCapacity(bytes)}。`;
        notifySuccess(msg, { operationId: opId, important: true });
        recordOperationFeedback(opId, true, msg, "数据管理", "logs.set_capacity");
        setCapacityDialog(false);
        refresh();
      })
      .catch((err: unknown) => {
        const message = err instanceof Error ? err.message : String(err);
        notifyError(`设置失败：${message}`, { operationId: opId, important: true });
        recordOperationFeedback(opId, false, `设置失败：${message}`, "数据管理", "logs.set_capacity");
      })
      .finally(() => setCapSaving(false));
  }, [capChoice, capCustomMB, refresh]);

  const totalLogBytes = (view?.categories ?? []).reduce((sum, c) => sum + c.bytes, 0);
  const totalLogFiles = (view?.categories ?? []).reduce((sum, c) => sum + c.files, 0);

  // 实际日志文件名（从后端返回的当前磁盘文件清单动态生成，
  // 不写死、不假设每类只有一个文件）：全部记录汇总三类文件。
  const fileNamesFor = (key: RecordCategory): string[] => {
    const cats = view?.categories ?? [];
    if (key === "all") return cats.flatMap((c) => c.fileNames ?? []);
    const cat = cats.find((c) => c.category === (key === "runtime" ? "run" : key));
    return cat?.fileNames ?? [];
  };
  const fileNamesSummary = (key: RecordCategory): string => {
    const names = fileNamesFor(key);
    if (names.length === 0) return "暂无文件";
    if (names.length === 1) return names[0];
    return `${names[0]}，另有 ${names.length - 1} 个文件`;
  };
  const fileNamesTitle = (key: RecordCategory): string => {
    const names = fileNamesFor(key);
    return names.length > 0 ? names.join("\n") : "暂无文件";
  };

  if (view === null && loadError) {
    return (
      <div className="mx-auto max-w-4xl p-8">
        <div className="rounded-lg border border-(--border) bg-(--panel-bg) p-6 text-center">
          <p className="text-sm text-(--danger)">无法读取数据管理信息：{loadError}</p>
          <button
            type="button"
            onClick={refresh}
            className="mt-3 rounded-md border border-(--border-strong) bg-(--input-bg) px-3 py-1.5 text-sm text-(--text-main) hover:bg-(--hover-bg)"
          >
            重新加载
          </button>
        </div>
      </div>
    );
  }

  return (
    <div className="mx-auto max-w-4xl space-y-6 p-8">

      <section className="rounded-lg border border-(--border) bg-(--panel-bg) p-4">
        <h3 className="text-[15px] font-semibold">数据概览</h3>
        <div className="mt-3 grid grid-cols-2 gap-4">
          <div>
            <div className="flex items-center gap-2 text-xs tracking-wider text-(--text-faint)">
              <Database size={14} /> 数据库
            </div>
            <div className="mt-2 text-lg font-semibold">
              {view
                ? view.databaseStatus === "ok"
                  ? `结构版本 ${view.schemaVersion} · 健康`
                  : `结构版本 ${view.schemaVersion} · 异常`
                : "加载中…"}
            </div>
            <div className="mt-0.5 text-xs text-(--text-faint)">
              {view ? `数据库文件占用 ${formatBytes(view.databaseBytes)}` : "正在检查…"}
            </div>
          </div>
          <div>
            <div className="flex items-center gap-2 text-xs tracking-wider text-(--text-faint)">
              <Activity size={14} /> 应用与实例
            </div>
            <div className="mt-2 text-lg font-semibold">
              {view ? `${view.appName} v${view.appVersion}` : "加载中…"}
            </div>
            <div className="mt-0.5 flex items-center gap-1.5 text-xs text-(--text-faint)">
              <Timer size={12} />
              {view
                ? `浏览器实例 ${view.instanceCount} 个 · 本次会话已运行 ${formatUptime(view.uptimeSeconds)}`
                : "正在检查…"}
            </div>
          </div>
        </div>
        <div className="mt-4 space-y-1 text-xs leading-relaxed text-(--text-faint)">
          <div className="truncate" title={view?.dataDir ?? status?.dataDir}>
            数据目录：{view ? view.dataDir : (status?.dataDir ?? "正在解析…")}
          </div>
          <div className="truncate" title={view?.logDir}>
            日志目录：{view ? view.logDir : "正在解析…"}（共 {totalLogFiles} 个文件，占用{" "}
            {formatBytes(totalLogBytes)}）
          </div>
        </div>
      </section>

      <section className="rounded-lg border border-(--border) bg-(--panel-bg) p-4">
        <h3 className="text-[15px] font-semibold">日志</h3>
        <p className="mt-1 text-xs leading-relaxed text-(--text-faint)">
          包含操作、错误与运行三类记录，超出容量时自动清理最早记录。
        </p>
        <div className="mt-2 divide-y divide-(--border) text-sm">
          {RECORD_ROWS.map((row) => (
            <div key={row.key} className="flex items-center gap-3 py-2.5">
              <span className="w-20 shrink-0">{row.label}</span>
              <span
                className="w-56 min-w-0 shrink-0 truncate font-mono text-xs text-(--text-dim)"
                title={fileNamesTitle(row.key)}
              >
                {view ? fileNamesSummary(row.key) : "加载中…"}
              </span>
              <span className="min-w-0 flex-1 truncate text-(--text-faint)" title={view?.logDir}>
                {view ? view.logDir : "加载中…"}
              </span>
              <button
                onClick={() => openExportDialog(row.key)}
                disabled={!view || exporting}
                className="shrink-0 rounded-md border border-(--border) bg-(--panel-bg) px-3 py-1.5 text-[13px] text-(--text-dim) transition-colors hover:text-(--text-main) disabled:opacity-50"
              >
                导出
              </button>
            </div>
          ))}
          <div className="flex items-center gap-3 py-2.5">
            <span className="w-20 shrink-0">存储容量</span>
            <span className="min-w-0 flex-1">
              {view
                ? `${formatCapacity(view.logCapacityLimitBytes)} / ${formatCapacity(view.logRemainingBytes)}`
                : "加载中…"}
            </span>
            <button
              onClick={openCapacityDialog}
              disabled={!view}
              className="shrink-0 rounded-md border border-(--border) bg-(--panel-bg) px-3 py-1.5 text-[13px] text-(--text-dim) transition-colors hover:text-(--text-main) disabled:opacity-50"
            >
              修改
            </button>
          </div>
        </div>
        {view && view.logWriteErrors > 0 && (
          <div className="mt-2 text-xs text-(--danger)">
            有 {view.logWriteErrors} 条记录写入失败，日志系统已降级运行，业务操作不受影响。
          </div>
        )}
        {(view?.categories ?? []).some((c) => c.cleanupErrors > 0) && (
          <div className="mt-2 text-xs text-(--danger)">部分日志清理曾失败，详情见运行日志。</div>
        )}
      </section>

      {exportDialog && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="w-full max-w-md rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">
              导出{RECORD_ROWS.find((r) => r.key === exportDialog)?.label}
            </h3>
            {exportInfo === null && !dialogError && (
              <p className="mt-3 text-sm text-(--text-dim)">正在读取当前记录范围…</p>
            )}
            {exportInfo && !exportInfo.hasRecords && (
              <p className="mt-3 text-sm text-(--text-dim)">暂无记录。</p>
            )}
            {exportInfo && exportInfo.hasRecords && (
              <div className="mt-4 space-y-3">
                <p className="text-sm text-(--text-dim)">
                  最早记录：{exportInfo.earliestDate} · 最新记录：{exportInfo.latestDate}（共{" "}
                  {exportInfo.recordCount} 条）
                </p>
                <div className="grid grid-cols-2 gap-3">
                  <div>
                    <label className="mb-1 block text-xs text-(--text-dim)">开始日期</label>
                    <input
                      type="date"
                      className="w-full rounded-md border border-(--border) bg-(--input-bg) px-2.5 py-1.5 text-sm text-(--text-main) outline-none"
                      value={exportStart}
                      min={exportInfo.earliestDate}
                      max={exportInfo.latestDate}
                      onChange={(e) => setExportStart(e.target.value)}
                    />
                  </div>
                  <div>
                    <label className="mb-1 block text-xs text-(--text-dim)">结束日期</label>
                    <input
                      type="date"
                      className="w-full rounded-md border border-(--border) bg-(--input-bg) px-2.5 py-1.5 text-sm text-(--text-main) outline-none"
                      value={exportEnd}
                      min={exportInfo.earliestDate}
                      max={exportInfo.latestDate}
                      onChange={(e) => setExportEnd(e.target.value)}
                    />
                  </div>
                </div>
                <p className="text-xs leading-relaxed text-(--text-faint)">
                  导出为 ZIP，包含所选日期范围内的记录、清单（含 SHA-256）与说明文件；错误记录导出时会附带其所属操作的关联上下文。
                </p>
              </div>
            )}
            {dialogError && <div className="mt-3 text-sm text-(--danger)">{dialogError}</div>}
            <div className="mt-5 flex justify-end gap-2">
              <button
                onClick={() => setExportDialog(null)}
                className="rounded-md border border-(--border) px-3.5 py-1.5 text-sm text-(--text-dim) transition-colors hover:text-(--text-main)"
              >
                取消
              </button>
              <button
                onClick={confirmRecordsExport}
                disabled={exporting || !exportInfo?.hasRecords}
                className="rounded-md bg-(--accent) px-3.5 py-1.5 text-sm font-medium text-(--accent-contrast) transition-opacity disabled:opacity-50"
              >
                {exporting ? "正在导出…" : "导出"}
              </button>
            </div>
          </div>
        </div>
      )}

      {capacityDialog && view && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="w-full max-w-md rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">日志存储容量</h3>
            <div className="mt-3 space-y-1 text-sm text-(--text-dim)">
              <div>容量上限：{formatCapacity(view.logCapacityLimitBytes)}</div>
              <div>已使用容量：{formatBytes(view.logUsedBytes)}</div>
              <div>剩余容量：{formatBytes(view.logRemainingBytes)}</div>
            </div>
            <div className="mt-2 h-2 overflow-hidden rounded-full bg-(--hover-bg)">
              <div
                className="h-full bg-(--accent)"
                style={{
                  width: `${
                    view.logCapacityLimitBytes > 0
                      ? Math.min(100, Math.round((view.logUsedBytes / view.logCapacityLimitBytes) * 100))
                      : 0
                  }%`,
                }}
              />
            </div>
            <div className="mt-4 space-y-2">
              {CAPACITY_PRESETS.map((preset) => (
                <label key={preset.bytes} className="flex items-center gap-2 text-sm text-(--text-main)">
                  <input
                    type="radio"
                    name="log-capacity"
                    checked={capChoice === preset.bytes}
                    onChange={() => setCapChoice(preset.bytes)}
                  />
                  {preset.label}
                </label>
              ))}
              <label className="flex items-center gap-2 text-sm text-(--text-main)">
                <input
                  type="radio"
                  name="log-capacity"
                  checked={capChoice === "custom"}
                  onChange={() => setCapChoice("custom")}
                />
                自定义
                <input
                  type="number"
                  min={1}
                  step={1}
                  className="w-24 rounded-md border border-(--border) bg-(--input-bg) px-2 py-1 text-sm text-(--text-main) outline-none"
                  value={capCustomMB}
                  onChange={(e) => {
                    setCapCustomMB(e.target.value);
                    setCapChoice("custom");
                  }}
                  placeholder="容量"
                />
                MB
              </label>
            </div>
            <p className="mt-3 text-xs leading-relaxed text-(--text-faint)">
              容量针对全部日志的总占用。修改后立即生效：若当前占用超过新上限，系统会自动清理最早的记录。
            </p>
            {capError && <div className="mt-3 text-sm text-(--danger)">{capError}</div>}
            <div className="mt-5 flex justify-end gap-2">
              <button
                onClick={() => setCapacityDialog(false)}
                className="rounded-md border border-(--border) px-3.5 py-1.5 text-sm text-(--text-dim) transition-colors hover:text-(--text-main)"
              >
                取消
              </button>
              <button
                onClick={confirmCapacity}
                disabled={capSaving}
                className="rounded-md bg-(--accent) px-3.5 py-1.5 text-sm font-medium text-(--accent-contrast) transition-opacity disabled:opacity-50"
              >
                {capSaving ? "正在保存…" : "保存"}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
