import { useCallback, useEffect, useRef, useState } from "react";
import {
  FileUp,
  Gauge,
  Loader2,
  Play,
  Plus,
  RefreshCw,
  Square,
  Trash2,
} from "lucide-react";
import {
  ProxyService,
  type BindingView,
  type CoreStatusView,
  type NodeView,
  type SourceView,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import { notifyError, notifySuccess, notifyWarning } from "../stores/useNotificationStore";
import { newOperationId, recordOperationFeedback } from "../lib/operations";

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// 用户在系统文件选择器中取消选择时，后端以「未选择文件」
// 返回。这不是错误：静默结束，不显示任何提示。
const PICKER_CANCELLED = "未选择文件";

const btnPrimary =
  "flex h-9 shrink-0 items-center gap-1.5 rounded-md bg-(--accent) px-3 text-sm font-medium text-(--accent-contrast) hover:opacity-90 disabled:opacity-50";
const btnSecondary =
  "flex h-9 shrink-0 items-center gap-1.5 rounded-md border border-(--border-strong) bg-(--input-bg) px-3 text-sm text-(--text-main) hover:bg-(--hover-bg) disabled:opacity-50";
const btnSmall =
  "flex h-7 shrink-0 items-center gap-1 rounded-md border border-(--border-strong) bg-(--input-bg) px-2 text-xs text-(--text-main) hover:bg-(--hover-bg) disabled:opacity-50";
const inputCls =
  "h-9 w-full rounded-md border border-(--border-strong) bg-(--input-bg) px-2.5 text-sm text-(--text-main) outline-none focus:border-(--accent)";
const labelCls = "mb-1 block text-xs text-(--text-dim)";

interface AddSubState {
  name: string;
  url: string;
}

/**
 * 代理配置（Stage 6C）：Mihomo 核心为全部浏览器实例提供代理。
 * 本页面只管理三件事：节点来源（订阅 / 本地 YAML）、节点列表与
 * 测速、核心运行状态与实例绑定概览。节点解析、规则匹配与连接
 * 全部由 Mihomo 核心完成；实例具体使用哪个节点在「浏览器实例」
 * 的新建 / 编辑表单中选择。
 */
export default function ProxyConfigPage() {
  const [core, setCore] = useState<CoreStatusView | null>(null);
  const [sources, setSources] = useState<SourceView[]>([]);
  const [nodes, setNodes] = useState<NodeView[]>([]);
  const [bindings, setBindings] = useState<BindingView[]>([]);
  const [delays, setDelays] = useState<Map<string, number>>(new Map());
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [refreshError, setRefreshError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [addSub, setAddSub] = useState<AddSubState | null>(null);
  const [addFileName, setAddFileName] = useState<string | null>(null);
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);
  const loadedRef = useRef(false);

  // 返回错误信息（成功返回 null）。失败保留已有数据。
  const reload = useCallback(async (): Promise<string | null> => {
    try {
      const [status, srcs, nds, bds] = await Promise.all([
        ProxyService.GetCoreStatus(),
        ProxyService.ListSources(),
        ProxyService.ListNodes(),
        ProxyService.ListBindings(),
      ]);
      setCore(status);
      setSources(srcs ?? []);
      setNodes(nds ?? []);
      setBindings(bds ?? []);
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
      if (message) notifyError(`加载代理信息失败：${message}`);
    });
  }, [reload]);

  // run executes one user operation with the shared operation-ID
  // and notification discipline (same pattern as the other pages).
  const run = async (
    key: string,
    action: string,
    fn: (opId: string) => Promise<string | void>,
    silentCancel = false,
  ) => {
    setBusy(key);
    const opId = newOperationId();
    try {
      const feedback = await fn(opId);
      if (feedback) {
        notifySuccess(feedback, { operationId: opId, important: true });
        recordOperationFeedback(opId, true, feedback, "代理配置", action);
        const refreshErr = await reload();
        if (refreshErr) notifyWarning("列表刷新失败，请重新加载。");
      }
    } catch (e) {
      const message = errText(e);
      if (!(silentCancel && message === PICKER_CANCELLED)) {
        notifyError(message, { operationId: opId, important: true });
        recordOperationFeedback(opId, false, message, "代理配置", action);
      }
    } finally {
      setBusy(null);
    }
  };

  const startCore = () =>
    run("core-start", "proxy.core_start", async (opId) => {
      await ProxyService.StartCore(opId);
      return "代理核心已启动";
    });

  const stopCore = () =>
    run("core-stop", "proxy.core_stop", async (opId) => {
      await ProxyService.StopCore(opId);
      return "代理核心已停止";
    });

  const submitAddSub = () => {
    if (!addSub) return;
    const { name, url } = addSub;
    void run("source-add-url", "proxy.source_add_url", async (opId) => {
      const view = await ProxyService.AddURLSource(opId, name, url);
      setAddSub(null);
      if (view.state === "ok") return `已添加订阅：${view.name}（${view.nodeCount} 个节点）`;
      // 来源已保存但核心未能给出节点：如实提示，不报成功。
      throw new Error(view.message || "订阅已保存，但未能获取节点");
    });
  };

  const addFile = () =>
    run(
      "source-add-file",
      "proxy.source_add_file",
      async (opId) => {
        const view = await ProxyService.AddFileSource(opId, addFileName ?? "");
        setAddFileName(null);
        if (view.state === "ok") return `已导入本地文件：${view.name}（${view.nodeCount} 个节点）`;
        throw new Error(view.message || "文件已导入，但未能获取节点");
      },
      true,
    );

  const refreshSource = (src: SourceView) =>
    run(`source-refresh-${src.id}`, "proxy.source_refresh", async (opId) => {
      await ProxyService.RefreshSource(opId, src.id);
      return `订阅已更新：${src.name}`;
    });

  const reimportSource = (src: SourceView) =>
    run(
      `source-reimport-${src.id}`,
      "proxy.source_reimport",
      async (opId) => {
        const view = await ProxyService.ReimportFileSource(opId, src.id);
        if (view.state === "ok") return `已重新导入：${view.name}（${view.nodeCount} 个节点）`;
        throw new Error(view.message || "文件已导入，但未能获取节点");
      },
      true,
    );

  const deleteSource = (src: SourceView) =>
    run(`source-delete-${src.id}`, "proxy.source_delete", async (opId) => {
      await ProxyService.DeleteSource(opId, src.id);
      setConfirmDelete(null);
      return `已删除来源：${src.name}`;
    });

  const testDelays = () =>
    run("delay-test", "proxy.delay_test", async (opId) => {
      const results = await ProxyService.TestDelays(opId);
      const map = new Map<string, number>();
      for (const r of results ?? []) map.set(`${r.sourceId}${r.name}`, r.delay);
      setDelays(map);
      return `测速完成：${map.size} 个节点`;
    });

  const delayOf = (n: NodeView): string => {
    const tested = delays.get(`${n.sourceId}${n.name}`);
    if (tested !== undefined) return tested < 0 ? "超时" : `${tested} ms`;
    return n.delay > 0 ? `${n.delay} ms` : "—";
  };

  const coreRunning = core?.state === "running";

  return (
    <div className="mx-auto flex h-full max-w-5xl flex-col px-6 py-6">
      {/* 顶部功能栏：按功能组整体换行（来源组 / 测速组 / 核心组）。 */}
      <div className="flex shrink-0 flex-wrap items-center gap-x-3 gap-y-2">
        <div className="flex shrink-0 flex-nowrap items-center gap-2">
          <button type="button" className={btnPrimary} disabled={busy != null} onClick={() => setAddSub({ name: "", url: "" })}>
            <Plus size={14} /> 添加订阅
          </button>
          <button type="button" className={btnSecondary} disabled={busy != null} onClick={() => setAddFileName("")}>
            <FileUp size={14} /> 导入本地 YAML
          </button>
        </div>
        <div className="flex shrink-0 flex-nowrap items-center gap-2">
          <button type="button" className={btnSecondary} disabled={busy != null} onClick={testDelays}>
            {busy === "delay-test" ? <Loader2 size={14} className="animate-spin" /> : <Gauge size={14} />} 节点测速
          </button>
        </div>
        <div className="flex shrink-0 flex-nowrap items-center gap-2">
          {coreRunning ? (
            <button type="button" className={btnSecondary} disabled={busy != null} onClick={stopCore}>
              {busy === "core-stop" ? <Loader2 size={14} className="animate-spin" /> : <Square size={14} />} 停止核心
            </button>
          ) : (
            <button type="button" className={btnSecondary} disabled={busy != null} onClick={startCore}>
              {busy === "core-start" ? <Loader2 size={14} className="animate-spin" /> : <Play size={14} />} 启动核心
            </button>
          )}
        </div>
      </div>

      {refreshError && (
        <div className="mt-4 flex items-center gap-2 rounded-lg border border-(--border) bg-(--panel-bg) px-3 py-2 text-sm text-(--text-dim)">
          <span className="flex-1">列表刷新失败：{refreshError}</span>
          <button type="button" className="shrink-0 font-medium text-(--accent) underline underline-offset-2" onClick={() => void reload()}>
            重新加载
          </button>
        </div>
      )}

      <div className="mt-5 min-h-0 flex-1 overflow-y-auto">
        {loading ? (
          <p className="py-10 text-center text-sm text-(--text-faint)">加载中…</p>
        ) : loadError ? (
          <div className="flex flex-col items-center px-4 py-12 text-center">
            <p className="text-sm text-(--danger)">代理信息加载失败：{loadError}</p>
            <button
              type="button"
              className="mt-3 rounded-md border border-(--border-strong) bg-(--input-bg) px-3 py-1.5 text-sm text-(--text-main) hover:bg-(--hover-bg)"
              onClick={() => void reload()}
            >
              重新加载
            </button>
          </div>
        ) : (
          <div className="space-y-6 pb-2">
            {/* 核心状态 */}
            <section>
              <h3 className="text-sm font-semibold">代理核心</h3>
              <div className="mt-2 rounded-lg border border-(--border) bg-(--panel-bg) px-4 py-3 text-sm">
                {core?.state === "running" ? (
                  <p className="text-(--text-dim)">
                    运行中{core.version ? ` · Mihomo ${core.version}` : ""}。每个已绑定实例使用各自的本地入口与所选节点。
                  </p>
                ) : core?.state === "error" ? (
                  <p className="text-(--danger)">核心异常{core.message ? `：${core.message}` : ""}</p>
                ) : (
                  <p className="text-(--text-dim)">
                    未运行{core && !core.binaryReady ? "（首次启动时将自动下载官方核心）" : ""}。
                    {core?.message ? ` ${core.message}` : ""}
                  </p>
                )}
              </div>
            </section>

            {/* 来源 */}
            <section>
              <h3 className="text-sm font-semibold">节点来源</h3>
              <div className="mt-2 overflow-hidden rounded-lg border border-(--border) bg-(--panel-bg)">
                {sources.length === 0 ? (
                  <p className="px-4 py-8 text-center text-sm text-(--text-faint)">
                    尚未添加任何来源。点击左上角「添加订阅」或「导入本地 YAML」获取代理节点。
                  </p>
                ) : (
                  <ul className="divide-y divide-(--border)">
                    {sources.map((src) => (
                      <li key={src.id} className="px-4 py-3 hover:bg-(--hover-bg)">
                        <div className="flex flex-wrap items-center gap-2">
                          <span className="text-sm font-medium">{src.name}</span>
                          <span className="rounded bg-(--hover-bg) px-1.5 py-0.5 text-xs text-(--text-dim)">
                            {src.kind === "url" ? "订阅" : "本地文件"}
                          </span>
                          <span className="text-xs text-(--text-faint)">
                            {src.nodeCount >= 0 ? `${src.nodeCount} 个节点` : "节点数未知"}
                            {src.boundInstances > 0 ? ` · ${src.boundInstances} 个实例在用` : ""}
                          </span>
                          <span className="ml-auto flex shrink-0 items-center gap-2">
                            {src.kind === "url" ? (
                              <button type="button" className={btnSmall} disabled={busy != null} onClick={() => refreshSource(src)}>
                                {busy === `source-refresh-${src.id}` ? <Loader2 size={12} className="animate-spin" /> : <RefreshCw size={12} />} 更新
                              </button>
                            ) : (
                              <button type="button" className={btnSmall} disabled={busy != null} onClick={() => reimportSource(src)}>
                                <FileUp size={12} /> 重新导入
                              </button>
                            )}
                            {confirmDelete === src.id ? (
                              <>
                                <button
                                  type="button"
                                  className="flex h-7 shrink-0 items-center gap-1 rounded-md bg-(--danger) px-2 text-xs font-medium text-white hover:opacity-90 disabled:opacity-50"
                                  disabled={busy != null}
                                  onClick={() => deleteSource(src)}
                                >
                                  确认删除
                                </button>
                                <button type="button" className={btnSmall} disabled={busy != null} onClick={() => setConfirmDelete(null)}>
                                  取消
                                </button>
                              </>
                            ) : (
                              <button type="button" className={btnSmall} disabled={busy != null} onClick={() => setConfirmDelete(src.id)}>
                                <Trash2 size={12} /> 删除
                              </button>
                            )}
                          </span>
                        </div>
                        {src.message && (
                          <p className={`mt-1 text-xs ${src.state === "error" || src.state === "empty" ? "text-(--danger)" : "text-(--text-faint)"}`}>
                            {src.message}
                          </p>
                        )}
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            </section>

            {/* 节点列表 */}
            <section>
              <h3 className="text-sm font-semibold">节点列表</h3>
              <div className="mt-2 overflow-hidden rounded-lg border border-(--border) bg-(--panel-bg)">
                {!coreRunning ? (
                  <p className="px-4 py-8 text-center text-sm text-(--text-faint)">核心未运行，暂无节点信息。</p>
                ) : nodes.length === 0 ? (
                  <p className="px-4 py-8 text-center text-sm text-(--text-faint)">暂无节点。请先添加订阅或导入本地 YAML。</p>
                ) : (
                  <ul className="divide-y divide-(--border)">
                    {nodes.map((n) => (
                      <li key={`${n.sourceId}${n.name}`} className="flex items-center gap-3 px-4 py-2.5 hover:bg-(--hover-bg)">
                        <span className={`h-2 w-2 shrink-0 rounded-full ${n.alive ? "bg-(--success)" : "bg-(--danger)"}`} />
                        <span className="min-w-0 flex-1 truncate text-sm" title={n.name}>{n.name}</span>
                        <span className="shrink-0 text-xs text-(--text-faint)">{n.sourceName}</span>
                        <span className="shrink-0 text-xs text-(--text-faint)">{n.type}</span>
                        <span className="w-16 shrink-0 text-right text-xs text-(--text-dim)">{delayOf(n)}</span>
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            </section>

            {/* 实例绑定 */}
            <section>
              <h3 className="text-sm font-semibold">实例代理绑定</h3>
              <div className="mt-2 overflow-hidden rounded-lg border border-(--border) bg-(--panel-bg)">
                {bindings.length === 0 ? (
                  <p className="px-4 py-8 text-center text-sm text-(--text-faint)">
                    尚无实例绑定代理。在「浏览器实例」的新建 / 编辑表单中为实例选择节点。
                  </p>
                ) : (
                  <ul className="divide-y divide-(--border)">
                    {bindings.map((b) => (
                      <li key={b.instanceId} className="px-4 py-2.5 hover:bg-(--hover-bg)">
                        <div className="flex flex-wrap items-center gap-2 text-sm">
                          <span className="font-medium">{b.instanceName || b.instanceId}</span>
                          <span className="text-(--text-faint)">→</span>
                          <span>{b.nodeName}</span>
                          <span className="text-xs text-(--text-faint)">
                            {b.sourceName} · {b.mode === "global" ? "全局" : "规则"} · 本地端口 {b.port}
                          </span>
                        </div>
                        {(b.nodeState === "missing" || b.listenerState === "down") && (
                          <p className="mt-1 text-xs text-(--danger)">
                            {b.nodeState === "missing" ? "该节点当前不可用（订阅更新后可能已消失），请为实例重新选择节点。" : ""}
                            {b.listenerState === "down" ? "该实例的本地监听端口当前无响应，实例暂时无法使用代理。" : ""}
                          </p>
                        )}
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            </section>
          </div>
        )}
      </div>

      {/* 添加订阅弹窗 */}
      {addSub && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="w-full max-w-md rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">添加订阅</h3>
            <div className="mt-4 space-y-3">
              <div>
                <label className={labelCls}>名称</label>
                <input className={inputCls} value={addSub.name} onChange={(e) => setAddSub({ ...addSub, name: e.target.value })} placeholder="例如：机场一" />
              </div>
              <div>
                <label className={labelCls}>订阅地址</label>
                <input className={inputCls} value={addSub.url} onChange={(e) => setAddSub({ ...addSub, url: e.target.value })} placeholder="https://…" />
                <p className="mt-1 text-xs text-(--text-faint)">订阅地址只保存在本机凭据存储中，不会写入日志。</p>
              </div>
            </div>
            <div className="mt-5 flex justify-end gap-2">
              <button type="button" className={btnSecondary} disabled={busy != null} onClick={() => setAddSub(null)}>
                取消
              </button>
              <button type="button" className={btnPrimary} disabled={busy != null || !addSub.name.trim() || !addSub.url.trim()} onClick={submitAddSub}>
                {busy === "source-add-url" ? <Loader2 size={14} className="animate-spin" /> : null} 添加
              </button>
            </div>
          </div>
        </div>
      )}

      {/* 导入本地 YAML：先填名称，再打开系统文件选择器 */}
      {addFileName !== null && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="w-full max-w-md rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">导入本地 YAML</h3>
            <div className="mt-4">
              <label className={labelCls}>名称</label>
              <input className={inputCls} value={addFileName} onChange={(e) => setAddFileName(e.target.value)} placeholder="例如：本地节点" />
              <p className="mt-1 text-xs text-(--text-faint)">文件会被复制到 DeBrowser 的数据目录中保存，原文件不会被修改。</p>
            </div>
            <div className="mt-5 flex justify-end gap-2">
              <button type="button" className={btnSecondary} disabled={busy != null} onClick={() => setAddFileName(null)}>
                取消
              </button>
              <button type="button" className={btnPrimary} disabled={busy != null || !addFileName.trim()} onClick={() => addFile()}>
                {busy === "source-add-file" ? <Loader2 size={14} className="animate-spin" /> : null} 选择文件
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
