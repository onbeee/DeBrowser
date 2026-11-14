import { useCallback, useEffect, useState } from "react";
import {
  AppService,
  CoreService,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import type {
  CoreStatusView,
  CoreUpdateView,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service/models";
import { useAppStore, type Theme } from "../stores/useAppStore";

const THEME_OPTIONS: { value: Theme; label: string }[] = [
  { value: "dark", label: "深色" },
  { value: "light", label: "浅色" },
  { value: "warm", label: "暖色" },
];

function PathRow({ label, value }: { label: string; value?: string }) {
  return (
    <div className="flex items-baseline gap-4 py-2">
      <span className="w-36 shrink-0 text-xs text-(--text-faint)">{label}</span>
      <span className="min-w-0 break-all font-mono text-xs text-(--text-dim)" title={value}>
        {value ?? "加载中…"}
      </span>
    </div>
  );
}

/**
 * 系统设置：界面语言（固定简体中文）、外观主题（即时生效并持久化），
 * 以及当前便携式目录的只读展示。自定义数据路径将在后续阶段提供，
 * 路径架构已预留接口。
 */
export default function SettingsPage() {
  const status = useAppStore((state) => state.status);
  const theme = useAppStore((state) => state.theme);
  const setTheme = useAppStore((state) => state.setTheme);
  const log = useAppStore((state) => state.log);
  const [saveError, setSaveError] = useState<string | null>(null);

  const changeTheme = (next: Theme) => {
    setTheme(next); // 即时生效
    AppService.SaveTheme(next)
      .then(() => {
        setSaveError(null);
        log("info", `主题已切换并保存：${next}`);
      })
      .catch((err: unknown) => {
        const message = err instanceof Error ? err.message : String(err);
        setSaveError(message);
        log("error", `保存主题失败：${message}`);
      });
  };

  return (
    <div className="mx-auto max-w-4xl space-y-6 p-8">
      <div>
        <h2 className="text-xl font-semibold">系统设置</h2>
        <p className="mt-1.5 max-w-2xl text-sm leading-relaxed text-(--text-dim)">
          语言与外观偏好保存在应用目录的 config/settings.json 中，随应用文件夹一起迁移，重启后保持不变。
        </p>
      </div>

      {saveError && (
        <div className="rounded-lg border border-(--danger-border) bg-(--danger-bg) px-4 py-3 text-sm text-(--danger)">
          保存设置失败：{saveError}
        </div>
      )}

      <div className="rounded-lg border border-(--border) bg-(--panel-bg) p-5">
        <h3 className="text-sm font-semibold">外观与语言</h3>
        <div className="mt-4 space-y-4">
          <label className="flex items-center gap-4">
            <span className="w-36 shrink-0 text-xs text-(--text-faint)">应用语言</span>
            <select
              value="zh-CN"
              disabled
              className="w-56 cursor-not-allowed rounded-md border border-(--border-strong) bg-(--panel-bg) px-3 py-1.5 text-sm text-(--text-dim)"
              title="当前版本仅提供简体中文界面"
            >
              <option value="zh-CN">简体中文</option>
            </select>
            <span className="text-xs text-(--text-faint)">当前版本仅提供简体中文界面</span>
          </label>

          <label className="flex items-center gap-4">
            <span className="w-36 shrink-0 text-xs text-(--text-faint)">外观主题</span>
            <select
              value={theme}
              onChange={(e) => changeTheme(e.target.value as Theme)}
              className="w-56 rounded-md border border-(--border-strong) bg-(--panel-bg) px-3 py-1.5 text-sm text-(--text-main)"
              title="切换界面主题"
            >
              {THEME_OPTIONS.map((option) => (
                <option key={option.value} value={option.value}>
                  {option.label}
                </option>
              ))}
            </select>
            <span className="text-xs text-(--text-faint)">切换后即时生效并自动保存</span>
          </label>
        </div>
      </div>

      <MihomoCoreSection />

      <div className="rounded-lg border border-(--border) bg-(--panel-bg) p-5">
        <h3 className="text-sm font-semibold">目录（便携模式）</h3>
        <p className="mt-1 text-xs leading-relaxed text-(--text-faint)">
          全部数据都位于可执行文件所在目录下，移动整个应用文件夹即可完成迁移。本阶段暂不支持自定义数据路径。
        </p>
        <div className="mt-2 divide-y divide-(--border)">
          <PathRow label="应用根目录" value={status?.rootDir} />
          <PathRow label="数据目录" value={status?.dataDir} />
          <PathRow label="数据库文件" value={status?.databasePath} />
          <PathRow label="WebView2 数据目录" value={status?.webView2Dir} />
          <PathRow label="日志文件" value={status?.logFile} />
          <PathRow label="设置文件" value={status?.settingsFile} />
        </div>
      </div>
    </div>
  );
}

// Mihomo 内核（系统设置）：展示实际安装核心的版本（由运行核心
// 本体查询得到，不写死），以及手动检查/更新入口。检查只在用户
// 点击时进行，不自动检查、不自动更新；更新包来自 MetaCubeX
// 官方 GitHub Releases，安装前强制校验官方 SHA-256。
function MihomoCoreSection() {
  const [core, setCore] = useState<CoreStatusView | null>(null);
  const [update, setUpdate] = useState<CoreUpdateView | null>(null);
  const [busy, setBusy] = useState<"" | "check" | "update" | "power">("");
  const [error, setError] = useState("");
  const [coreLog, setCoreLog] = useState<string[] | null>(null);

  const load = useCallback(async () => {
    try {
      const [st, up] = await Promise.all([
        CoreService.GetCoreStatus(),
        CoreService.GetCoreUpdate(),
      ]);
      setCore(st);
      setUpdate(up);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const check = async () => {
    setBusy("check");
    setError("");
    try {
      setUpdate(await CoreService.CheckCoreUpdate());
      setCore(await CoreService.GetCoreStatus());
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setUpdate(await CoreService.GetCoreUpdate().catch(() => null));
    } finally {
      setBusy("");
    }
  };

  const doUpdate = async () => {
    setBusy("update");
    setError("");
    try {
      setUpdate(await CoreService.UpdateCore());
      setCore(await CoreService.GetCoreStatus());
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setUpdate(await CoreService.GetCoreUpdate().catch(() => null));
    } finally {
      setBusy("");
    }
  };

  const togglePower = async () => {
    setBusy("power");
    setError("");
    try {
      if (core?.running) {
        await CoreService.StopCore();
      } else {
        await CoreService.StartCore();
      }
      setCore(await CoreService.GetCoreStatus());
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy("");
    }
  };

  const toggleLog = async () => {
    if (coreLog !== null) {
      setCoreLog(null);
      return;
    }
    try {
      setCoreLog(await CoreService.GetCoreLog(100));
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const statusText =
    update?.status === "up-to-date"
      ? "已是最新版本"
      : update?.status === "update-available"
        ? "发现新版本"
        : update?.status === "check-failed"
          ? "检查失败"
          : busy === "check"
            ? "检查中"
            : "尚未检查";

  return (
    <div className="rounded-lg border border-(--border) bg-(--panel-bg) p-5">
      <h3 className="text-sm font-semibold">Mihomo 内核</h3>
      <p className="mt-1 text-xs leading-relaxed text-(--text-faint)">
        使用 MetaCubeX 官方原始发布二进制，不做任何修改。更新检查仅在点击时进行，不会自动更新。
      </p>
      {error && (
        <div className="mt-3 rounded-lg border border-(--danger-border) bg-(--danger-bg) px-3 py-2 text-sm text-(--danger)">
          {error}
        </div>
      )}
      <div className="mt-4 space-y-2.5 text-sm">
        <div className="flex items-baseline gap-4">
          <span className="w-36 shrink-0 text-xs text-(--text-faint)">当前版本</span>
          <span>
            {core === null
              ? "加载中…"
              : core.installed
                ? core.version || "版本读取失败"
                : "核心未安装"}
          </span>
        </div>
        <div className="flex items-baseline gap-4">
          <span className="w-36 shrink-0 text-xs text-(--text-faint)">最新版本</span>
          <span>{update?.latestVersion || "—"}</span>
        </div>
        <div className="flex items-baseline gap-4">
          <span className="w-36 shrink-0 text-xs text-(--text-faint)">更新状态</span>
          <span>{statusText}</span>
        </div>
        <div className="flex items-baseline gap-4">
          <span className="w-36 shrink-0 text-xs text-(--text-faint)">最近检查</span>
          <span>{update?.checkedAt || "—"}</span>
        </div>
        <div className="flex items-baseline gap-4">
          <span className="w-36 shrink-0 text-xs text-(--text-faint)">运行状态</span>
          <span>
            {core?.running
              ? `运行中（进程 ${core.pid}，启动于 ${core.startedAt}）`
              : core?.lastExit
                ? `未运行 · ${core.lastExit}`
                : "未运行"}
          </span>
        </div>
      </div>
      <div className="mt-4 flex flex-wrap items-center gap-3">
        <button
          onClick={() => void check()}
          disabled={busy !== ""}
          className="rounded-md border border-(--border-strong) px-4 py-1.5 text-sm text-(--text-main) transition-colors hover:bg-(--hover-bg) disabled:opacity-50"
        >
          {busy === "check" ? "检查中…" : "检查更新"}
        </button>
        {update?.status === "update-available" && (
          <button
            onClick={() => void doUpdate()}
            disabled={busy !== ""}
            className="rounded-md bg-(--accent) px-4 py-1.5 text-sm font-medium text-white transition-opacity hover:opacity-90 disabled:opacity-50"
          >
            {busy === "update" ? "更新中…" : `更新内核到 ${update.latestVersion}`}
          </button>
        )}
        {core?.installed && (
          <button
            onClick={() => void togglePower()}
            disabled={busy !== ""}
            className="rounded-md border border-(--border-strong) px-4 py-1.5 text-sm text-(--text-main) transition-colors hover:bg-(--hover-bg) disabled:opacity-50"
          >
            {busy === "power" ? "处理中…" : core.running ? "停止核心" : "启动核心"}
          </button>
        )}
        <button
          onClick={() => void toggleLog()}
          className="rounded-md border border-(--border-strong) px-4 py-1.5 text-sm text-(--text-dim) transition-colors hover:bg-(--hover-bg)"
        >
          {coreLog !== null ? "收起日志" : "查看核心日志"}
        </button>
      </div>
      {update?.error && update.status === "check-failed" && (
        <p className="mt-2 text-xs text-(--danger)">{update.error}</p>
      )}
      {coreLog !== null && (
        <pre className="mt-3 max-h-64 overflow-y-auto rounded-md border border-(--border) bg-(--app-bg) p-3 font-mono text-[11px] leading-relaxed text-(--text-dim)">
          {coreLog.length > 0 ? coreLog.join("\n") : "（暂无核心日志）"}
        </pre>
      )}
    </div>
  );
}
