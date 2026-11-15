import { useCallback, useEffect, useState } from "react";
import { AlertTriangle, CheckCircle2, FolderOpen, Loader2, Trash2 } from "lucide-react";
import {
  BrowserService,
  type BrowserView,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import { newOperationId, recordOperationFeedback } from "../lib/operations";

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// 浏览器内核页面：只有一个「自定内核」区域。DeBrowser 不会
// 自动扫描、验证或修改电脑中已有的浏览器；用户手动选择的
// 程序只登记路径（纯路径登记），不做任何文件检查，也不启动
// 所选程序。DeBrowser 独立内核的下载与管理功能已按产品决定
// 取消，本页面没有任何下载、安装或验证入口。
export default function KernelsPage() {
  const [browsers, setBrowsers] = useState<BrowserView[]>([]);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [confirmRemove, setConfirmRemove] = useState<string | null>(null);

  const reload = useCallback(async () => {
    try {
      const list = await BrowserService.ListBrowsers();
      setBrowsers(list ?? []);
      setError(null);
    } catch (e) {
      setError(errText(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void reload();
  }, [reload]);

  // run executes one user operation: it generates the operation
  // ID the backend records under, passes it to the operation, and
  // afterwards records the exact feedback shown to the user under
  // the same ID.
  const run = async (
    key: string,
    action: string,
    fn: (opId: string) => Promise<string | void>,
  ) => {
    setBusy(key);
    setError(null);
    setNotice(null);
    const opId = newOperationId();
    try {
      const feedback = await fn(opId);
      if (action && feedback) {
        recordOperationFeedback(opId, true, feedback, "浏览器内核", action);
      }
    } catch (e) {
      const message = errText(e);
      setError(message);
      if (action) {
        recordOperationFeedback(opId, false, message, "浏览器内核", action);
      }
    } finally {
      setBusy(null);
    }
  };

  const addManual = () =>
    run("add-manual", "browser.add_manual", async (opId) => {
      const added = await BrowserService.AddBrowserManually(opId);
      const msg = `已添加浏览器：${added.name}`;
      setNotice(msg);
      await reload();
      return msg;
    });

  const reselectPath = (b: BrowserView) =>
    run(`reselect-${b.id}`, "browser.update_path", async (opId) => {
      const v = await BrowserService.UpdateBrowserPath(opId, b.id);
      const msg = `已更新路径：${v.name}`;
      setNotice(msg);
      await reload();
      return msg;
    });

  const removeRegistration = (b: BrowserView) =>
    run(`remove-${b.id}`, "browser.remove_registration", async (opId) => {
      await BrowserService.RemoveBrowserRegistration(opId, b.id);
      setConfirmRemove(null);
      const msg = `已移除登记：${b.name}（浏览器本身未被删除或修改）`;
      setNotice(msg);
      await reload();
      return msg;
    });

  const btnSecondary =
    "inline-flex h-9 items-center gap-1.5 rounded-md border border-(--border-strong) bg-(--input-bg) px-3 text-sm text-(--text-main) transition hover:bg-(--hover-bg) disabled:opacity-50";
  const btnPrimary =
    "inline-flex h-9 items-center gap-1.5 rounded-md bg-(--accent) px-3 text-sm font-medium text-(--accent-contrast) transition hover:opacity-90 disabled:opacity-50";
  const btnDanger =
    "inline-flex h-9 items-center gap-1.5 rounded-md bg-(--danger) px-3 text-sm font-medium text-(--danger-contrast) transition hover:opacity-90 disabled:opacity-50";

  return (
    <div className="mx-auto max-w-4xl space-y-6 p-8">
      {error && (
        <div
          role="alert"
          className="flex items-start gap-2 rounded-lg border border-(--danger-border) bg-(--danger-bg) px-4 py-3 text-sm text-(--danger)"
        >
          <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
          <span>{error}</span>
        </div>
      )}
      {notice && (
        <div className="flex items-start gap-2 rounded-lg border border-(--border) bg-(--accent-soft) px-4 py-3 text-sm text-(--text-main)">
          <CheckCircle2 className="mt-0.5 h-4 w-4 shrink-0 text-(--ok)" aria-hidden />
          <span>{notice}</span>
        </div>
      )}

      <section className="rounded-lg border border-(--border) bg-(--panel-bg) p-5">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <h2 className="text-[15px] font-semibold">自定内核</h2>
          <button type="button" onClick={addManual} disabled={busy != null} className={btnPrimary}>
            {busy === "add-manual" ? (
              <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
            ) : (
              <FolderOpen className="h-4 w-4" aria-hidden />
            )}
            手动添加浏览器
          </button>
        </div>

        <p className="mt-1.5 text-sm leading-relaxed text-(--text-dim)">
          手动选择电脑中的浏览器程序，将其路径添加到 DeBrowser。DeBrowser
          不会自动扫描、验证或修改电脑中已有的浏览器。
        </p>

        {loading ? (
          <p className="mt-4 text-sm text-(--text-dim)">正在加载…</p>
        ) : browsers.length === 0 ? (
          <p className="mt-4 text-sm text-(--text-dim)">
            尚未添加浏览器。点击上方按钮，手动选择浏览器程序。
          </p>
        ) : (
          <ul className="mt-2 flex flex-col divide-y divide-(--border)">
            {browsers.map((b) => (
              <li key={b.id} className="flex flex-col gap-2 py-3">
                <span className="text-sm font-medium">{b.name}</span>
                <span className="break-all font-mono text-xs text-(--text-dim)" title={b.executablePath}>
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
      </section>
    </div>
  );
}
