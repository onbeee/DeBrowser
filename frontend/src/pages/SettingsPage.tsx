import { useState } from "react";
import { AppService } from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
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
