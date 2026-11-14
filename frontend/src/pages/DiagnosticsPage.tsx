import { useCallback, useEffect, useState } from "react";
import { Activity, Database, Download, FileArchive, RefreshCw, Timer } from "lucide-react";
import {
  DiagnosticsService,
  type DiagnosticsView,
  type ExportResult,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import { useAppStore } from "../stores/useAppStore";

const CATEGORY_LABELS: Record<string, string> = {
  run: "运行日志",
  operation: "操作日志",
  error: "错误日志",
};

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

export default function DiagnosticsPage() {
  const log = useAppStore((state) => state.log);
  const [view, setView] = useState<DiagnosticsView | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [exporting, setExporting] = useState(false);
  const [exportNote, setExportNote] = useState<string | null>(null);
  const [exportError, setExportError] = useState<string | null>(null);

  const refresh = useCallback(() => {
    DiagnosticsService.GetDiagnostics()
      .then((v) => {
        setView(v);
        setError(null);
      })
      .catch((err: unknown) => {
        setError(err instanceof Error ? err.message : String(err));
      });
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const doExport = useCallback(() => {
    setExporting(true);
    setExportNote(null);
    setExportError(null);
    DiagnosticsService.ExportDiagnostics()
      .then((res: ExportResult) => {
        if (res.cancelled) {
          setExportNote("已取消导出。");
        } else {
          setExportNote(`导出完成：${res.path}（包含 ${res.files} 个日志文件）`);
          log("info", `诊断信息已导出到 ${res.path}`);
          refresh();
        }
      })
      .catch((err: unknown) => {
        const message = err instanceof Error ? err.message : String(err);
        setExportError(`导出失败：${message}`);
        log("error", `诊断导出失败：${message}`);
      })
      .finally(() => setExporting(false));
  }, [log, refresh]);

  const doFaultReport = useCallback(() => {
    setExporting(true);
    setExportNote(null);
    setExportError(null);
    DiagnosticsService.ExportFaultReport()
      .then((res: ExportResult) => {
        if (res.cancelled) {
          setExportNote("已取消导出。");
        } else {
          setExportNote(
            `故障报告已导出：${res.path}。可将该 ZIP 直接发送给技术支持或 ChatGPT 用于定位故障（已自动脱敏，不含订阅链接、密码与凭据）。`,
          );
          log("info", `故障报告已导出到 ${res.path}`);
          refresh();
        }
      })
      .catch((err: unknown) => {
        const message = err instanceof Error ? err.message : String(err);
        setExportError(`导出失败：${message}`);
        log("error", `故障报告导出失败：${message}`);
      })
      .finally(() => setExporting(false));
  }, [log, refresh]);

  return (
    <div className="mx-auto max-w-4xl space-y-6 p-8">
      <div className="flex items-start justify-between">
        <div>
          <h2 className="text-xl font-semibold">日志与诊断</h2>
          <p className="mt-1.5 max-w-2xl text-sm leading-relaxed text-(--text-dim)">
            查看应用与日志系统的当前状态，并可一键导出诊断信息。导出包只包含三类日志与诊断元数据，不包含数据库、凭据
            Vault、浏览器配置文件或任何业务数据。
          </p>
        </div>
        <button
          onClick={refresh}
          className="flex shrink-0 items-center gap-1.5 rounded-md border border-(--border) bg-(--panel-bg) px-3 py-1.5 text-[13px] text-(--text-dim) transition-colors hover:text-(--text-main)"
        >
          <RefreshCw size={14} /> 刷新
        </button>
      </div>

      {error && (
        <div className="rounded-lg border border-(--danger-border) bg-(--danger-bg) px-4 py-3 text-sm text-(--danger)">
          无法读取诊断信息：{error}
        </div>
      )}

      <div className="grid grid-cols-2 gap-4">
        <div className="rounded-lg border border-(--border) bg-(--panel-bg) p-4">
          <div className="flex items-center gap-2 text-xs tracking-wider text-(--text-faint)">
            <Activity size={14} /> 应用版本
          </div>
          <div className="mt-2 text-lg font-semibold">
            {view ? `${view.appName} v${view.appVersion}` : "加载中…"}
          </div>
          <div className="mt-0.5 text-xs text-(--text-faint)">当前运行状态见右侧卡片</div>
        </div>

        <div className="rounded-lg border border-(--border) bg-(--panel-bg) p-4">
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
          <div className="mt-0.5 flex items-center gap-1.5 text-xs text-(--text-faint)">
            <Timer size={12} />
            {view ? `本次会话已运行 ${formatUptime(view.uptimeSeconds)}` : "正在检查…"}
          </div>
        </div>
      </div>

      <div className="rounded-lg border border-(--border) bg-(--panel-bg) p-4">
        <h3 className="text-sm font-semibold">日志目录</h3>
        <div className="mt-1 truncate text-xs text-(--text-faint)" title={view?.logDir}>
          {view ? view.logDir : "正在解析…"}
        </div>
        <table className="mt-3 w-full text-sm">
          <thead>
            <tr className="text-left text-xs text-(--text-faint)">
              <th className="pb-2 font-medium">类别</th>
              <th className="pb-2 font-medium">文件数量</th>
              <th className="pb-2 font-medium">占用空间</th>
              <th className="pb-2 font-medium">保留策略</th>
              <th className="pb-2 font-medium">容量上限</th>
            </tr>
          </thead>
          <tbody className="text-(--text-dim)">
            {(view?.categories ?? []).map((cat) => (
              <tr key={cat.category} className="border-t border-(--border)">
                <td className="py-2">{CATEGORY_LABELS[cat.category] ?? cat.category}</td>
                <td className="py-2">{cat.files} 个</td>
                <td className="py-2">{formatBytes(cat.bytes)}</td>
                <td className="py-2">{cat.retentionDays} 天</td>
                <td className="py-2">{formatBytes(cat.capBytes)}</td>
              </tr>
            ))}
            {!view && (
              <tr className="border-t border-(--border)">
                <td className="py-2" colSpan={5}>
                  加载中…
                </td>
              </tr>
            )}
          </tbody>
        </table>
        {(view?.categories ?? []).some((c) => c.cleanupErrors > 0) && (
          <div className="mt-2 text-xs text-(--danger)">
            部分日志清理曾失败，详情见运行日志。
          </div>
        )}
      </div>

      <div className="rounded-lg border border-(--border) bg-(--panel-bg) p-4">
        <h3 className="text-sm font-semibold">诊断导出</h3>
        <p className="mt-1.5 text-sm leading-relaxed text-(--text-dim)">
          {view?.lastExport
            ? `最近一次导出：${view.lastExport.at} · ${view.lastExport.path}（${view.lastExport.files} 个日志文件）`
            : "本次运行尚未导出过诊断信息。"}
        </p>
        <button
          onClick={doExport}
          disabled={exporting}
          className="mt-3 flex items-center gap-2 rounded-md bg-(--accent) px-4 py-2 text-sm font-medium text-(--accent-contrast) transition-opacity disabled:opacity-50"
        >
          {exporting ? <FileArchive size={16} /> : <Download size={16} />}
          {exporting ? "正在导出…" : "一键导出诊断信息"}
        </button>
        <button
          onClick={doFaultReport}
          disabled={exporting}
          className="mt-3 ml-3 inline-flex items-center gap-2 rounded-md border border-(--border-strong) px-4 py-2 text-sm text-(--text-main) transition-colors hover:bg-(--hover-bg) disabled:opacity-50"
        >
          <FileArchive size={16} />
          导出故障报告
        </button>
        <p className="mt-2 text-xs leading-relaxed text-(--text-faint)">
          故障报告在诊断信息之外附带导入/刷新阶段记录与 Mihomo 核心状态，全部经后端脱敏，可直接交给技术支持或
          ChatGPT 分析。
        </p>
        {exportNote && <div className="mt-2 text-sm text-(--text-dim)">{exportNote}</div>}
        {exportError && <div className="mt-2 text-sm text-(--danger)">{exportError}</div>}
      </div>
    </div>
  );
}
