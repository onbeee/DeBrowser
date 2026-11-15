import { useCallback, useEffect, useState } from "react";
import { Events } from "@wailsio/runtime";
import {
  BadgeCheck,
  CircleAlert,
  Cpu,
  Download,
  FolderOpen,
  MonitorDown,
  RefreshCw,
  ShieldCheck,
  Trash2,
} from "lucide-react";
import {
  BrowserService,
  type BrowserView,
  type InstallProgress,
  type KernelView,
  type StableVersionView,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import { KernelInstallProgress } from "../../bindings/events";
import { useAppStore } from "../stores/useAppStore";

const SCOPE_LABELS: Record<string, string> = {
  user: "当前用户",
  system: "系统级",
  managed: "DeBrowser 管理",
};

const SIGNATURE_LABELS: Record<string, string> = {
  verified: "签名已验证",
  publisher_mismatch: "签名发布者不符",
  invalid: "签名无效",
  unsigned: "未签名",
  unavailable: "无法验证",
};

const DETECTION_LABELS: Record<string, string> = {
  verified: "已验证",
  verification_failed: "验证失败",
  unverifiable: "无法验证",
};

const KERNEL_STATUS_LABELS: Record<string, string> = {
  ready: "就绪",
  verification_failed: "验证失败",
  missing: "文件缺失",
};

const PHASE_LABELS: Record<string, string> = {
  downloading: "下载中",
  extracting: "解压中",
  verifying: "验证中",
  installing: "安装中",
  done: "安装完成",
  failed: "安装失败",
};

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/**
 * 浏览器内核（Stage 9A）：上方是电脑已安装的浏览器（实时检测，
 * 只查看与重新检测，DeBrowser 不修改、不删除它们，也不读取其
 * 日常 Profile）；下方是 DeBrowser 独立下载管理的 Chromium
 * 内核（官方 Chrome for Testing，验证后原子安装，可删除）。
 */
export default function KernelsPage() {
  const log = useAppStore((state) => state.log);
  const [systemBrowsers, setSystemBrowsers] = useState<BrowserView[]>([]);
  const [kernels, setKernels] = useState<KernelView[]>([]);
  const [detecting, setDetecting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [stable, setStable] = useState<StableVersionView | null>(null);
  const [stableError, setStableError] = useState<string | null>(null);
  const [installing, setInstalling] = useState(false);
  const [progress, setProgress] = useState<InstallProgress | null>(null);
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);
  const [busyKernel, setBusyKernel] = useState<string | null>(null);

  const refreshSystem = useCallback(() => {
    setDetecting(true);
    BrowserService.DetectSystemBrowsers()
      .then((list) => {
        setSystemBrowsers(list ?? []);
        setError(null);
      })
      .catch((err: unknown) => setError(errText(err)))
      .finally(() => setDetecting(false));
  }, []);

  const refreshKernels = useCallback(() => {
    BrowserService.ListManagedKernels()
      .then((list) => setKernels(list ?? []))
      .catch((err: unknown) => setError(errText(err)));
  }, []);

  useEffect(() => {
    refreshSystem();
    refreshKernels();
  }, [refreshSystem, refreshKernels]);

  useEffect(() => {
    const cancel = Events.On(KernelInstallProgress, (event) => {
      const data = (event as { data?: InstallProgress }).data;
      if (data) setProgress(data);
    });
    return () => {
      if (typeof cancel === "function") cancel();
    };
  }, []);

  const redetect = useCallback(() => {
    setDetecting(true);
    setNotice(null);
    BrowserService.RedetectBrowsers()
      .then((list) => {
        const found = list ?? [];
        setSystemBrowsers(found);
        setError(null);
        setNotice(`重新检测完成，共发现 ${found.length} 个浏览器。`);
      })
      .catch((err: unknown) => setError(errText(err)))
      .finally(() => setDetecting(false));
  }, []);

  const addManual = useCallback(() => {
    setNotice(null);
    BrowserService.AddBrowserManually()
      .then((added) => {
        setNotice(`已添加并通过验证：${added.name} ${added.version}`);
        log("info", `手动添加浏览器 ${added.name} ${added.version}`);
        refreshSystem();
      })
      .catch((err: unknown) => setError(errText(err)));
  }, [log, refreshSystem]);

  const checkStable = useCallback(() => {
    setStableError(null);
    BrowserService.GetOfficialStableVersion()
      .then((v) => setStable(v))
      .catch((err: unknown) => setStableError(errText(err)));
  }, []);

  const install = useCallback(() => {
    setInstalling(true);
    setProgress(null);
    setNotice(null);
    setError(null);
    BrowserService.InstallManagedKernel()
      .then((k) => {
        setNotice(`独立内核 Chromium ${k.version} 已安装并通过验证。`);
        log("info", `已安装独立内核 ${k.version}`);
        refreshKernels();
      })
      .catch((err: unknown) => setError(errText(err)))
      .finally(() => setInstalling(false));
  }, [log, refreshKernels]);

  const verifyKernel = useCallback(
    (id: string) => {
      setBusyKernel(id);
      BrowserService.VerifyManagedKernel(id)
        .then((k) => {
          setNotice(
            k.status === "ready"
              ? `内核 ${k.version} 重新验证通过。`
              : `内核 ${k.version} 验证结果：${KERNEL_STATUS_LABELS[k.status] ?? k.status}。`,
          );
          refreshKernels();
        })
        .catch((err: unknown) => setError(errText(err)))
        .finally(() => setBusyKernel(null));
    },
    [refreshKernels],
  );

  const deleteKernel = useCallback(
    (id: string) => {
      setBusyKernel(id);
      BrowserService.DeleteManagedKernel(id)
        .then(() => {
          setNotice("独立内核已删除。");
          setConfirmDelete(null);
          refreshKernels();
        })
        .catch((err: unknown) => setError(errText(err)))
        .finally(() => setBusyKernel(null));
    },
    [refreshKernels],
  );

  return (
    <div className="flex h-full flex-col gap-6 overflow-y-auto px-6 py-5">
      <div>
        <h2 className="flex items-center gap-2 text-lg font-semibold">
          <Cpu size={20} /> 浏览器内核
        </h2>
        <p className="mt-1 max-w-2xl text-sm leading-relaxed text-(--text-dim)">
          优先使用电脑已安装的浏览器。只有需要独立版本时，才下载 DeBrowser 管理的 Chromium。
        </p>
      </div>

      {error && (
        <div className="flex items-start gap-2 rounded-xl bg-(--hover-bg) px-4 py-3 text-sm">
          <CircleAlert size={16} className="mt-0.5 shrink-0" />
          <span>{error}</span>
        </div>
      )}
      {notice && (
        <div className="flex items-start gap-2 rounded-xl bg-(--hover-bg) px-4 py-3 text-sm">
          <BadgeCheck size={16} className="mt-0.5 shrink-0" />
          <span>{notice}</span>
        </div>
      )}

      <section>
        <div className="mb-3 flex items-center justify-between">
          <h3 className="text-sm font-semibold">电脑已安装的浏览器</h3>
          <div className="flex gap-2">
            <button
              type="button"
              onClick={redetect}
              disabled={detecting}
              className="flex items-center gap-1.5 rounded-lg bg-(--hover-bg) px-3 py-1.5 text-sm disabled:opacity-50"
            >
              <RefreshCw size={14} /> {detecting ? "检测中…" : "重新检测"}
            </button>
            <button
              type="button"
              onClick={addManual}
              className="flex items-center gap-1.5 rounded-lg bg-(--hover-bg) px-3 py-1.5 text-sm"
            >
              <FolderOpen size={14} /> 手动添加浏览器
            </button>
          </div>
        </div>
        {systemBrowsers.length === 0 && !detecting && (
          <p className="rounded-xl bg-(--hover-bg) px-4 py-3 text-sm text-(--text-dim)">
            未检测到已安装的 Google Chrome 或 Brave。可以手动添加浏览器可执行文件，或在下方下载 DeBrowser 独立内核。
          </p>
        )}
        <div className="flex flex-col gap-2">
          {systemBrowsers.map((b) => (
            <div key={b.id} className="rounded-xl bg-(--hover-bg) px-4 py-3">
              <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                <span className="font-medium">{b.name}</span>
                <span className="text-sm text-(--text-dim)">{b.version || "版本未知"}</span>
                <span className="text-sm text-(--text-dim)">{SCOPE_LABELS[b.scope] ?? b.scope}</span>
                <span className="flex items-center gap-1 text-sm">
                  <ShieldCheck size={14} />
                  {SIGNATURE_LABELS[b.signatureStatus] ?? b.signatureStatus}
                </span>
                <span className="text-sm">
                  {DETECTION_LABELS[b.detectionStatus] ?? b.detectionStatus}
                </span>
              </div>
              <p className="mt-1 break-all text-xs text-(--text-faint)">{b.executablePath}</p>
              {b.statusReason && (
                <p className="mt-1 text-xs text-(--text-dim)">{b.statusReason}</p>
              )}
            </div>
          ))}
        </div>
        <p className="mt-2 text-xs text-(--text-faint)">
          系统浏览器由 DeBrowser 实时检测，仅供查看；DeBrowser 不修改、不复制、不删除它们，也不读取其日常使用的浏览器 Profile。
        </p>
      </section>

      <section>
        <div className="mb-3 flex items-center justify-between">
          <h3 className="text-sm font-semibold">DeBrowser 独立内核</h3>
          <div className="flex gap-2">
            <button
              type="button"
              onClick={checkStable}
              className="flex items-center gap-1.5 rounded-lg bg-(--hover-bg) px-3 py-1.5 text-sm"
            >
              <MonitorDown size={14} /> 查看官方 Stable 版本
            </button>
            <button
              type="button"
              onClick={install}
              disabled={installing}
              className="flex items-center gap-1.5 rounded-lg bg-(--hover-bg) px-3 py-1.5 text-sm disabled:opacity-50"
            >
              <Download size={14} /> {installing ? "安装中…" : "下载并安装"}
            </button>
          </div>
        </div>
        {stable && (
          <p className="mb-2 text-sm text-(--text-dim)">
            官方 Stable 最新版本：Chromium {stable.version}（{stable.platform}），来源为 Google 官方 Chrome for Testing。
          </p>
        )}
        {stableError && <p className="mb-2 text-sm">{stableError}</p>}
        {installing && (
          <p className="mb-2 text-sm text-(--text-dim)">
            正在安装{progress ? `：${PHASE_LABELS[progress.phase] ?? progress.phase}` : ""}
            {progress && progress.percent >= 0 ? `（${progress.percent}%）` : ""}
          </p>
        )}
        {kernels.length === 0 && (
          <p className="rounded-xl bg-(--hover-bg) px-4 py-3 text-sm text-(--text-dim)">
            尚未安装 DeBrowser 独立内核。系统浏览器可直接使用，独立内核仅在需要特定版本时下载。
          </p>
        )}
        <div className="flex flex-col gap-2">
          {kernels.map((k) => (
            <div key={k.id} className="rounded-xl bg-(--hover-bg) px-4 py-3">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                  <span className="font-medium">Chromium {k.version}</span>
                  <span className="text-sm text-(--text-dim)">通道 {k.channel}</span>
                  <span className="text-sm text-(--text-dim)">{k.platform}</span>
                  <span className="text-sm">{KERNEL_STATUS_LABELS[k.status] ?? k.status}</span>
                </div>
                <div className="flex gap-2">
                  <button
                    type="button"
                    onClick={() => verifyKernel(k.id)}
                    disabled={busyKernel === k.id}
                    className="flex items-center gap-1.5 rounded-lg bg-(--card-bg) px-3 py-1.5 text-sm disabled:opacity-50"
                  >
                    <ShieldCheck size={14} /> 重新验证
                  </button>
                  {confirmDelete === k.id ? (
                    <>
                      <button
                        type="button"
                        onClick={() => deleteKernel(k.id)}
                        disabled={busyKernel === k.id}
                        className="flex items-center gap-1.5 rounded-lg bg-(--card-bg) px-3 py-1.5 text-sm disabled:opacity-50"
                      >
                        <Trash2 size={14} /> 确认删除
                      </button>
                      <button
                        type="button"
                        onClick={() => setConfirmDelete(null)}
                        className="rounded-lg bg-(--card-bg) px-3 py-1.5 text-sm"
                      >
                        取消
                      </button>
                    </>
                  ) : (
                    <button
                      type="button"
                      onClick={() => setConfirmDelete(k.id)}
                      className="flex items-center gap-1.5 rounded-lg bg-(--card-bg) px-3 py-1.5 text-sm"
                    >
                      <Trash2 size={14} /> 删除
                    </button>
                  )}
                </div>
              </div>
              <p className="mt-1 break-all text-xs text-(--text-faint)">{k.absInstallPath}</p>
            </div>
          ))}
        </div>
      </section>
    </div>
  );
}
