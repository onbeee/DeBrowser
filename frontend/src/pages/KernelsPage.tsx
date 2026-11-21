import { useCallback, useEffect, useRef, useState } from "react";
import { AppWindow, FolderOpen, Loader2, Plus, Trash2 } from "lucide-react";
import {
  BrowserService,
  type BrowserView,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import { notifyError, notifySuccess, notifyWarning } from "../stores/useNotificationStore";
import { newOperationId, recordOperationFeedback } from "../lib/operations";

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// 用户在系统文件选择器中取消选择时，后端以「未选择文件」
// 返回。这不是错误：静默结束，不显示任何提示。
const PICKER_CANCELLED = "未选择文件";

// 浏览器内核页面：布局与「浏览器实例」页面一致（顶部说明 +
// 主按钮、下方统一列表）。登记的内核供创建浏览器实例时选择。
// DeBrowser 不会自动扫描、验证或修改电脑中已有的浏览器；
// 用户手动选择的程序只登记路径（纯路径登记），不做任何文件
// 检查，也不启动所选程序。DeBrowser 独立内核的下载与管理
// 功能已按产品决定取消，本页面没有任何下载、安装或验证入口。
export default function KernelsPage() {
  const [browsers, setBrowsers] = useState<BrowserView[]>([]);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState<string | null>(null);
  const [confirmRemove, setConfirmRemove] = useState<string | null>(null);
  // 读取失败与真实空列表严格区分：从未成功加载且失败时，
  // 页面显示加载失败与重试入口，不显示「尚未登记浏览器」。
  const [loadError, setLoadError] = useState<string | null>(null);
  const [refreshError, setRefreshError] = useState<string | null>(null);
  const loadedRef = useRef(false);

  // 返回错误信息（成功返回 null）。失败保留已有列表。
  const reload = useCallback(async (): Promise<string | null> => {
    try {
      const list = await BrowserService.ListBrowsers();
      setBrowsers(list ?? []);
      loadedRef.current = true;
      setLoadError(null);
      setRefreshError(null);
      return null;
    } catch (e) {
      const message = errText(e);
      if (!loadedRef.current) setLoadError(message);
      else setRefreshError(message);
      return message;
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void reload().then((message) => {
      if (message) notifyError(`加载内核列表失败：${message}`);
    });
  }, [reload]);

  const retryLoad = () => {
    void reload().then((message) => {
      if (message) notifyError(`加载内核列表失败：${message}`);
    });
  };

  // run executes one user operation: it generates the operation
  // ID the backend records under, passes it to the operation, and
  // afterwards records the exact feedback shown to the user under
  // the same ID. Results go to the global notification system.
  const run = async (
    key: string,
    action: string,
    fn: (opId: string) => Promise<string | void>,
  ) => {
    setBusy(key);
    const opId = newOperationId();
    try {
      const feedback = await fn(opId);
      if (feedback) {
        notifySuccess(feedback, { operationId: opId, important: true });
        recordOperationFeedback(opId, true, feedback, "浏览器内核", action);
        // 操作结果已确定；随后的列表刷新失败单独提示
        // （仅通知台），不覆盖、不改写操作结果。
        const refreshError = await reload();
        if (refreshError) notifyWarning("列表刷新失败，请重新加载。");
      }
    } catch (e) {
      const message = errText(e);
      if (message !== PICKER_CANCELLED) {
        notifyError(message, { operationId: opId, important: true });
        recordOperationFeedback(opId, false, message, "浏览器内核", action);
      }
    } finally {
      setBusy(null);
    }
  };

  const addManual = () =>
    run("add-manual", "browser.add_manual", async (opId) => {
      const added = await BrowserService.AddBrowserManually(opId);
      return `已添加浏览器：${added.name}`;
    });

  const reselectPath = (b: BrowserView) =>
    run(`reselect-${b.id}`, "browser.update_path", async (opId) => {
      const v = await BrowserService.UpdateBrowserPath(opId, b.id);
      return `已更新路径：${v.name}`;
    });

  const removeRegistration = (b: BrowserView) =>
    run(`remove-${b.id}`, "browser.remove_registration", async (opId) => {
      await BrowserService.RemoveBrowserRegistration(opId, b.id);
      setConfirmRemove(null);
      return `已移除登记：${b.name}（浏览器本身未被删除或修改）`;
    });

  const btnSecondary =
    "inline-flex h-9 items-center gap-1.5 rounded-md border border-(--border-strong) bg-(--input-bg) px-3 text-sm text-(--text-main) transition hover:bg-(--hover-bg) disabled:opacity-50";
  const btnDanger =
    "inline-flex h-9 items-center gap-1.5 rounded-md bg-(--danger) px-3 text-sm font-medium text-(--danger-contrast) transition hover:opacity-90 disabled:opacity-50";

  return (
    <div className="mx-auto max-w-5xl px-6 py-6">
      <div className="flex items-center justify-between">
        <p className="text-sm text-(--text-dim)">登记本机浏览器程序，供创建浏览器实例时选择。</p>
        <button
          type="button"
          onClick={addManual}
          disabled={busy != null}
          className="flex h-9 shrink-0 items-center gap-1.5 rounded-md bg-(--accent) px-3 text-sm font-medium text-(--accent-contrast) hover:opacity-90 disabled:opacity-50"
        >
          {busy === "add-manual" ? <Loader2 size={14} className="animate-spin" /> : <Plus size={14} />}
          添加内核
        </button>
      </div>

      {refreshError && browsers.length > 0 && (
        <div className="mt-4 flex items-center gap-2 rounded-lg border border-(--border) bg-(--panel-bg) px-3 py-2 text-sm text-(--text-dim)">
          <span className="flex-1">列表刷新失败：{refreshError}</span>
          <button
            type="button"
            onClick={retryLoad}
            className="shrink-0 font-medium text-(--accent) underline underline-offset-2"
          >
            重新加载
          </button>
        </div>
      )}

      <div className="mt-5 overflow-hidden rounded-lg border border-(--border) bg-(--panel-bg)">
        {loading ? (
          <p className="px-4 py-10 text-center text-sm text-(--text-faint)">加载中…</p>
        ) : loadError && browsers.length === 0 ? (
          <div className="flex flex-col items-center px-4 py-12 text-center">
            <p className="text-sm text-(--danger)">内核列表加载失败：{loadError}</p>
            <button
              type="button"
              onClick={retryLoad}
              className="mt-3 rounded-md border border-(--border-strong) bg-(--input-bg) px-3 py-1.5 text-sm text-(--text-main) hover:bg-(--hover-bg)"
            >
              重新加载
            </button>
          </div>
        ) : browsers.length === 0 ? (
          <div className="flex flex-col items-center px-4 py-12 text-center">
            <span className="flex h-12 w-12 items-center justify-center rounded-2xl bg-(--hover-bg) text-(--text-faint)">
              <AppWindow size={22} />
            </span>
            <p className="mt-3 text-sm text-(--text-dim)">尚未登记浏览器内核</p>
            <p className="mt-1 text-xs text-(--text-faint)">点击右上角「添加内核」，手动选择电脑中的浏览器程序。</p>
          </div>
        ) : (
          <ul className="divide-y divide-(--border)">
            {browsers.map((b) => (
              <li key={b.id} className="flex flex-col gap-2 px-4 py-3 hover:bg-(--hover-bg)">
                <span className="text-sm font-medium">{b.name}</span>
                <span className="break-all font-mono text-xs text-(--text-faint)" title={b.executablePath}>
                  {b.executablePath}
                </span>
                <div className="flex flex-wrap items-center gap-2">
                  <button
                    type="button"
                    onClick={() => reselectPath(b)}
                    disabled={busy != null}
                    className={btnSecondary}
                  >
                    {busy === `reselect-${b.id}` ? (
                      <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
                    ) : (
                      <FolderOpen className="h-4 w-4" aria-hidden />
                    )}
                    重新选择路径
                  </button>
                  {confirmRemove === b.id ? (
                    <>
                      <button
                        type="button"
                        onClick={() => removeRegistration(b)}
                        disabled={busy != null}
                        className={btnDanger}
                      >
                        {busy === `remove-${b.id}` ? (
                          <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
                        ) : (
                          <Trash2 className="h-4 w-4" aria-hidden />
                        )}
                        确认移除登记
                      </button>
                      <button
                        type="button"
                        onClick={() => setConfirmRemove(null)}
                        className={btnSecondary}
                      >
                        取消
                      </button>
                      <span className="self-center text-xs text-(--text-faint)">
                        只删除 DeBrowser 中的登记记录，不会删除或修改浏览器本身。
                      </span>
                    </>
                  ) : (
                    <button
                      type="button"
                      onClick={() => setConfirmRemove(b.id)}
                      disabled={busy != null}
                      className={btnSecondary}
                    >
                      <Trash2 className="h-4 w-4" aria-hidden />
                      移除登记
                    </button>
                  )}
                </div>
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}
