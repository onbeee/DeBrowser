import { useCallback, useEffect, useState } from "react";
import {
  AlertTriangle,
  CheckCircle2,
  Download,
  FolderOpen,
  Loader2,
  RefreshCw,
  ShieldCheck,
  Trash2,
  XCircle,
} from "lucide-react";
import { Events } from "@wailsio/runtime";
import {
  BrowserService,
  type BrowserView,
  type InstallProgress,
  type KernelView,
  type StableVersionView,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import { KernelInstallProgress } from "../../bindings/events";

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// Browser kernels page (Stage 9A, revised product decision).
// DeBrowser never scans the computer for browsers. Section A lists
// the browsers the user added BY HAND (persisted registrations of
// an installed Chrome or Brave). Section B lists the Chromium
// kernels DeBrowser manages itself, downloaded only when the user
// explicitly asks. Nothing on this page detects, scans or
// discovers anything automatically.

const kindLabels: Record<string, string> = {
  chrome: "Google Chrome",
  brave: "Brave",
  chromium: "Chromium",
};

const signatureLabels: Record<string, string> = {
  verified: "签名有效",
  publisher_mismatch: "发布者不符",
  invalid: "签名无效",
  unsigned: "无签名",
  unavailable: "无法验证",
};

const kernelStatusLabels: Record<string, string> = {
  ready: "可用",
  verification_failed: "验证失败",
  missing: "文件缺失",
};

const phaseLabels: Record<string, string> = {
  downloading: "正在下载",
  extracting: "正在解压",
  verifying: "正在验证",
  installing: "正在安装",
  done: "安装完成",
  failed: "安装失败",
};

function StatusBadge({ view }: { view: BrowserView }) {
  if (view.detectionStatus === "verified") {
    return (
      <span className="inline-flex items-center gap-1 rounded-full bg-emerald-500/10 px-2 py-0.5 text-xs font-medium text-emerald-600 dark:text-emerald-400">
        <CheckCircle2 className="h-3.5 w-3.5" aria-hidden />
        已验证
      </span>
    );
  }
  return (
    <span className="inline-flex items-center gap-1 rounded-full bg-red-500/10 px-2 py-0.5 text-xs font-medium text-red-600 dark:text-red-400">
      <XCircle className="h-3.5 w-3.5" aria-hidden />
      {view.detectionStatus === "unverifiable" ? "无法验证" : "验证未通过"}
    </span>
  );
}

export default function KernelsPage() {
  const [manual, setManual] = useState<BrowserView[]>([]);
  const [kernels, setKernels] = useState<KernelView[]>([]);
  const [stable, setStable] = useState<StableVersionView | null>(null);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [progress, setProgress] = useState<InstallProgress | null>(null);
  const [confirmRemove, setConfirmRemove] = useState<string | null>(null);
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);

  const reload = useCallback(async () => {
    try {
      const [browsers, managed] = await Promise.all([
        BrowserService.ListBrowsers(),
        BrowserService.ListManagedKernels(),
      ]);
      setManual((browsers ?? []).filter((b) => b.source === "manual"));
      setKernels(managed ?? []);
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

  useEffect(() => {
    const off = Events.On(KernelInstallProgress, (ev) => {
      const data = (ev as { data?: InstallProgress }).data;
      if (data) setProgress(data);
    });
    return () => {
      if (typeof off === "function") off();
    };
  }, []);

  const run = async (key: string, fn: () => Promise<void>) => {
    setBusy(key);
    setError(null);
    setNotice(null);
    try {
      await fn();
    } catch (e) {
      setError(errText(e));
    } finally {
      setBusy(null);
    }
  };

  const addManual = () =>
    run("add-manual", async () => {
      const added = await BrowserService.AddBrowserManually();
      setNotice(`已添加浏览器：${added.name} ${added.version}`);
      await reload();
    });

  const verifyManual = (b: BrowserView) =>
    run(`verify-${b.id}`, async () => {
      const v = await BrowserService.VerifyBrowser(b.id);
      setNotice(
        v.detectionStatus === "verified"
          ? `验证通过：${v.name} ${v.version}`
          : `验证未通过：${v.statusReason || "请检查浏览器文件"}`,
      );
      await reload();
    });

  const removeRegistration = (b: BrowserView) =>
    run(`remove-${b.id}`, async () => {
      await BrowserService.RemoveBrowserRegistration(b.id);
      setConfirmRemove(null);
      setNotice(`已移除登记：${b.name}（浏览器本身未被删除或修改）`);
      await reload();
    });

  const fetchStable = () =>
    run("stable", async () => {
      setStable(await BrowserService.GetOfficialStableVersion());
    });

  const install = () =>
    run("install", async () => {
      setProgress(null);
      const k = await BrowserService.InstallManagedKernel();
      setNotice(`已安装独立内核：${k.version}`);
      await reload();
    });

  const verifyKernel = (k: KernelView) =>
    run(`verify-k-${k.id}`, async () => {
      const v = await BrowserService.VerifyManagedKernel(k.id);
      setNotice(`内核 ${v.version} 状态：${kernelStatusLabels[v.status] ?? v.status}`);
      await reload();
    });

  const deleteKernel = (k: KernelView) =>
    run(`delete-k-${k.id}`, async () => {
      await BrowserService.DeleteManagedKernel(k.id);
      setConfirmDelete(null);
      setNotice(`已删除独立内核：${k.version}`);
      await reload();
    });

  const stableInstalled = stable != null && kernels.some((k) => k.version === stable.version);

  return (
    <div className="mx-auto flex w-full max-w-5xl flex-col gap-6">
      <div>
        <h1 className="text-xl font-semibold text-zinc-900 dark:text-zinc-50">浏览器内核</h1>
        <p className="mt-1 text-sm text-zinc-500 dark:text-zinc-400">
          DeBrowser 不会自动扫描电脑中的浏览器。你可以手动添加已经安装的 Chrome 或 Brave，也可以下载 DeBrowser
          独立管理的 Chromium。
        </p>
      </div>

      {error && (
        <div
          role="alert"
          className="flex items-start gap-2 rounded-xl border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700 dark:border-red-900 dark:bg-red-950 dark:text-red-300"
        >
          <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
          <span>{error}</span>
        </div>
      )}
      {notice && (
        <div className="flex items-start gap-2 rounded-xl border border-emerald-200 bg-emerald-50 px-4 py-3 text-sm text-emerald-700 dark:border-emerald-900 dark:bg-emerald-950 dark:text-emerald-300">
          <CheckCircle2 className="mt-0.5 h-4 w-4 shrink-0" aria-hidden />
          <span>{notice}</span>
        </div>
      )}

      {/* A. Manually added browsers */}
      <section className="rounded-2xl border border-zinc-200 bg-white p-5 dark:border-zinc-800 dark:bg-zinc-900">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <h2 className="text-base font-semibold text-zinc-900 dark:text-zinc-50">已添加的浏览器</h2>
          <button
            type="button"
            onClick={addManual}
            disabled={busy != null}
            className="inline-flex items-center gap-1.5 rounded-lg bg-zinc-900 px-3 py-1.5 text-sm font-medium text-white transition hover:bg-zinc-700 disabled:opacity-50 dark:bg-zinc-100 dark:text-zinc-900 dark:hover:bg-zinc-300"
          >
            {busy === "add-manual" ? (
              <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
            ) : (
              <FolderOpen className="h-4 w-4" aria-hidden />
            )}
            手动添加浏览器
          </button>
        </div>

        {loading ? (
          <p className="mt-4 text-sm text-zinc-500 dark:text-zinc-400">正在加载…</p>
        ) : manual.length === 0 ? (
          <p className="mt-4 text-sm text-zinc-500 dark:text-zinc-400">
            尚未添加浏览器。点击下方按钮，手动选择电脑中已安装的 Chrome 或 Brave。
          </p>
        ) : (
          <ul className="mt-4 flex flex-col divide-y divide-zinc-100 dark:divide-zinc-800">
            {manual.map((b) => (
              <li key={b.id} className="flex flex-col gap-2 py-3">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="font-medium text-zinc-900 dark:text-zinc-50">{b.name}</span>
                  <span className="text-sm text-zinc-500 dark:text-zinc-400">{b.version || "版本未知"}</span>
                  <StatusBadge view={b} />
                  <span className="inline-flex items-center gap-1 text-xs text-zinc-500 dark:text-zinc-400">
                    <ShieldCheck className="h-3.5 w-3.5" aria-hidden />
                    {signatureLabels[b.signatureStatus] ?? b.signatureStatus}
                    {b.publisher ? ` · ${b.publisher}` : ""}
                  </span>
                </div>
                <div className="text-xs text-zinc-500 dark:text-zinc-400">
                  类型：{kindLabels[b.kind] ?? b.kind}
                  {b.lastVerifiedAt ? ` · 最近验证：${b.lastVerifiedAt}` : ""}
                </div>
                <div className="break-all font-mono text-xs text-zinc-500 dark:text-zinc-400">{b.executablePath}</div>
                {b.detectionStatus !== "verified" && b.statusReason && (
                  <div className="text-xs text-red-600 dark:text-red-400">{b.statusReason}</div>
                )}
                <div className="flex gap-2">
                  <button
                    type="button"
                    onClick={() => verifyManual(b)}
                    disabled={busy != null}
                    className="inline-flex items-center gap-1.5 rounded-lg border border-zinc-300 px-3 py-1.5 text-sm text-zinc-700 transition hover:bg-zinc-100 disabled:opacity-50 dark:border-zinc-700 dark:text-zinc-200 dark:hover:bg-zinc-800"
                  >
                    {busy === `verify-${b.id}` ? (
                      <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
                    ) : (
                      <RefreshCw className="h-4 w-4" aria-hidden />
                    )}
                    重新验证
                  </button>
                  {confirmRemove === b.id ? (
                    <>
                      <button
                        type="button"
                        onClick={() => removeRegistration(b)}
                        disabled={busy != null}
                        className="inline-flex items-center gap-1.5 rounded-lg bg-red-600 px-3 py-1.5 text-sm font-medium text-white transition hover:bg-red-500 disabled:opacity-50"
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
                        className="rounded-lg border border-zinc-300 px-3 py-1.5 text-sm text-zinc-700 transition hover:bg-zinc-100 dark:border-zinc-700 dark:text-zinc-200 dark:hover:bg-zinc-800"
                      >
                        取消
                      </button>
                      <span className="self-center text-xs text-zinc-500 dark:text-zinc-400">
                        只删除 DeBrowser 中的登记记录，不会删除或修改浏览器本身。
                      </span>
                    </>
                  ) : (
                    <button
                      type="button"
                      onClick={() => setConfirmRemove(b.id)}
                      disabled={busy != null}
                      className="inline-flex items-center gap-1.5 rounded-lg border border-zinc-300 px-3 py-1.5 text-sm text-zinc-700 transition hover:bg-zinc-100 disabled:opacity-50 dark:border-zinc-700 dark:text-zinc-200 dark:hover:bg-zinc-800"
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

      {/* B. DeBrowser-managed Chromium kernels */}
      <section className="rounded-2xl border border-zinc-200 bg-white p-5 dark:border-zinc-800 dark:bg-zinc-900">
        <h2 className="text-base font-semibold text-zinc-900 dark:text-zinc-50">DeBrowser 独立内核</h2>
        <p className="mt-1 text-sm text-zinc-500 dark:text-zinc-400">
          如果不想依赖电脑中已经安装的浏览器，可以下载并使用 DeBrowser 独立管理的 Chromium。
        </p>

        <div className="mt-4 flex flex-wrap items-center gap-2">
          <button
            type="button"
            onClick={fetchStable}
            disabled={busy != null}
            className="inline-flex items-center gap-1.5 rounded-lg border border-zinc-300 px-3 py-1.5 text-sm text-zinc-700 transition hover:bg-zinc-100 disabled:opacity-50 dark:border-zinc-700 dark:text-zinc-200 dark:hover:bg-zinc-800"
          >
            {busy === "stable" ? (
              <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
            ) : (
              <RefreshCw className="h-4 w-4" aria-hidden />
            )}
            查看官方 Stable 版本
          </button>
          {stable && (
            <span className="text-sm text-zinc-600 dark:text-zinc-300">
              官方 Stable：{stable.version}（{stable.platform}）
              {stableInstalled ? " · 已安装" : ""}
            </span>
          )}
          <button
            type="button"
            onClick={install}
            disabled={busy != null || stableInstalled}
            className="inline-flex items-center gap-1.5 rounded-lg bg-zinc-900 px-3 py-1.5 text-sm font-medium text-white transition hover:bg-zinc-700 disabled:opacity-50 dark:bg-zinc-100 dark:text-zinc-900 dark:hover:bg-zinc-300"
          >
            {busy === "install" ? (
              <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
            ) : (
              <Download className="h-4 w-4" aria-hidden />
            )}
            下载并安装
          </button>
        </div>

        {progress && busy === "install" && (
          <p className="mt-3 text-sm text-zinc-600 dark:text-zinc-300" role="status">
            {phaseLabels[progress.phase] ?? progress.phase}
            {progress.phase === "downloading" && progress.percent >= 0 ? ` ${progress.percent}%` : ""} ·{" "}
            {progress.version}
          </p>
        )}

        {loading ? (
          <p className="mt-4 text-sm text-zinc-500 dark:text-zinc-400">正在加载…</p>
        ) : kernels.length === 0 ? (
          <p className="mt-4 text-sm text-zinc-500 dark:text-zinc-400">尚未安装独立内核。</p>
        ) : (
          <ul className="mt-4 flex flex-col divide-y divide-zinc-100 dark:divide-zinc-800">
            {kernels.map((k) => (
              <li key={k.id} className="flex flex-col gap-2 py-3">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="font-medium text-zinc-900 dark:text-zinc-50">Chromium {k.version}</span>
                  <span className="text-sm text-zinc-500 dark:text-zinc-400">
                    {k.channel} · {k.platform}
                  </span>
                  <span
                    className={`inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-xs font-medium ${
                      k.status === "ready"
                        ? "bg-emerald-500/10 text-emerald-600 dark:text-emerald-400"
                        : "bg-red-500/10 text-red-600 dark:text-red-400"
                    }`}
                  >
                    {k.status === "ready" ? (
                      <CheckCircle2 className="h-3.5 w-3.5" aria-hidden />
                    ) : (
                      <XCircle className="h-3.5 w-3.5" aria-hidden />
                    )}
                    {kernelStatusLabels[k.status] ?? k.status}
                  </span>
                </div>
                <div className="break-all font-mono text-xs text-zinc-500 dark:text-zinc-400">{k.absExecutablePath}</div>
                <div className="flex gap-2">
                  <button
                    type="button"
                    onClick={() => verifyKernel(k)}
                    disabled={busy != null}
                    className="inline-flex items-center gap-1.5 rounded-lg border border-zinc-300 px-3 py-1.5 text-sm text-zinc-700 transition hover:bg-zinc-100 disabled:opacity-50 dark:border-zinc-700 dark:text-zinc-200 dark:hover:bg-zinc-800"
                  >
                    {busy === `verify-k-${k.id}` ? (
                      <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
                    ) : (
                      <RefreshCw className="h-4 w-4" aria-hidden />
                    )}
                    重新验证
                  </button>
                  {confirmDelete === k.id ? (
                    <>
                      <button
                        type="button"
                        onClick={() => deleteKernel(k)}
                        disabled={busy != null}
                        className="inline-flex items-center gap-1.5 rounded-lg bg-red-600 px-3 py-1.5 text-sm font-medium text-white transition hover:bg-red-500 disabled:opacity-50"
                      >
                        {busy === `delete-k-${k.id}` ? (
                          <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
                        ) : (
                          <Trash2 className="h-4 w-4" aria-hidden />
                        )}
                        确认删除
                      </button>
                      <button
                        type="button"
                        onClick={() => setConfirmDelete(null)}
                        className="rounded-lg border border-zinc-300 px-3 py-1.5 text-sm text-zinc-700 transition hover:bg-zinc-100 dark:border-zinc-700 dark:text-zinc-200 dark:hover:bg-zinc-800"
                      >
                        取消
                      </button>
                    </>
                  ) : (
                    <button
                      type="button"
                      onClick={() => setConfirmDelete(k.id)}
                      disabled={busy != null}
                      className="inline-flex items-center gap-1.5 rounded-lg border border-zinc-300 px-3 py-1.5 text-sm text-zinc-700 transition hover:bg-zinc-100 disabled:opacity-50 dark:border-zinc-700 dark:text-zinc-200 dark:hover:bg-zinc-800"
                    >
                      <Trash2 className="h-4 w-4" aria-hidden />
                      删除内核
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
