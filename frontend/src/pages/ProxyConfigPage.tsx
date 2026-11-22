import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Loader2, Play, Plus, RefreshCw, Square, Trash2 } from "lucide-react";
import {
  InstanceService,
  ProxyService,
  type BindingView,
  type CoreStatusView,
  type NodeView,
  type SourceView,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import type { Instance } from "../../bindings/github.com/onbeee/DeBrowser/internal/model";
import { notifyError, notifySuccess } from "../stores/useNotificationStore";
import { newOperationId, recordOperationFeedback } from "../lib/operations";

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// 用户在系统文件选择器中取消选择时，后端以「未选择文件」
// 返回。这不是错误：静默结束，不显示任何提示。
const PICKER_CANCELLED = "未选择文件";

// 代理配置页面（重新设计）：单页完成全部代理操作——
// 顶部启动/关闭核心与添加配置；下方左右分栏，左侧是全部
// 已导入节点（按来源分组显示），右侧是全部浏览器实例，
// 每张实例卡片可直接选择代理节点或「不使用代理」。
// 绑定数据只有一份：本页与浏览器实例页面读写同一个
// ProxyService 绑定关系。
export default function ProxyConfigPage() {
  const [status, setStatus] = useState<CoreStatusView | null>(null);
  const [sources, setSources] = useState<SourceView[]>([]);
  const [nodes, setNodes] = useState<NodeView[]>([]);
  const [bindings, setBindings] = useState<BindingView[]>([]);
  const [instances, setInstances] = useState<Instance[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [addOpen, setAddOpen] = useState(false);
  const [addTab, setAddTab] = useState<"subscription" | "file">("subscription");
  const [addName, setAddName] = useState("");
  const [addURL, setAddURL] = useState("");
  const [addError, setAddError] = useState<string | null>(null);
  const [confirmDeleteSource, setConfirmDeleteSource] = useState<SourceView | null>(null);
  const loadedRef = useRef(false);

  const reload = useCallback(async (): Promise<string | null> => {
    try {
      const [st, srcs, nds, bds, insts] = await Promise.all([
        ProxyService.GetCoreStatus(),
        ProxyService.ListSources(),
        ProxyService.ListNodes(),
        ProxyService.ListBindings(),
        InstanceService.ListInstances(),
      ]);
      setStatus(st);
      setSources(srcs ?? []);
      setNodes(nds ?? []);
      setBindings(bds ?? []);
      setInstances(insts ?? []);
      loadedRef.current = true;
      setLoadError(null);
      return null;
    } catch (e) {
      const message = errText(e);
      if (!loadedRef.current) setLoadError(message);
      return message;
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void reload().then((message) => {
      if (message) notifyError(`加载代理配置失败：${message}`);
    });
  }, [reload]);

  // 页面打开期间保持核心状态、绑定与实例列表新鲜（新建的
  // 实例自动出现，删除的实例自动消失）。
  useEffect(() => {
    const timer = setInterval(() => void reload(), 3000);
    return () => clearInterval(timer);
  }, [reload]);

  const bindingByInstance = useMemo(() => {
    const map = new Map<string, BindingView>();
    for (const b of bindings) map.set(b.instanceId, b);
    return map;
  }, [bindings]);

  const nodesBySource = useMemo(() => {
    const map = new Map<string, NodeView[]>();
    for (const n of nodes) {
      const list = map.get(n.sourceId) ?? [];
      list.push(n);
      map.set(n.sourceId, list);
    }
    return map;
  }, [nodes]);

  const run = async (key: string, action: string, label: string, fn: (opId: string) => Promise<void>) => {
    if (busy) return;
    setBusy(key);
    const opId = newOperationId();
    try {
      await fn(opId);
      notifySuccess(label, { operationId: opId, important: true });
      recordOperationFeedback(opId, true, label, "代理", action);
    } catch (e) {
      const message = errText(e);
      notifyError(`${label}失败：${message}`, { operationId: opId, important: true });
      recordOperationFeedback(opId, false, `${label}失败：${message}`, "代理", action);
    } finally {
      setBusy(null);
      await reload();
    }
  };

  const toggleCore = () => {
    if (!status) return;
    if (status.running) {
      void run("core", "proxy.core_stop", "代理核心已关闭", async (opId) => {
        await ProxyService.StopCore(opId);
      });
    } else {
      void run("core", "proxy.core_start", "代理核心已启动", async (opId) => {
        await ProxyService.StartCore(opId);
      });
    }
  };

  const submitAdd = async () => {
    setAddError(null);
    const name = addName.trim();
    if (!name) {
      setAddError("请填写名称。");
      return;
    }
    if (addTab === "subscription" && !addURL.trim()) {
      setAddError("请填写订阅地址。");
      return;
    }
    setBusy("add");
    const opId = newOperationId();
    try {
      if (addTab === "subscription") {
        const view = await ProxyService.AddSubscriptionSource(opId, name, addURL.trim());
        notifySuccess(`订阅已添加：${view.name}（${view.nodeCount} 个节点）`, { operationId: opId, important: true });
        recordOperationFeedback(opId, true, `订阅已添加：${view.name}`, "代理", "proxy.source_add_subscription");
      } else {
        const view = await ProxyService.AddFileSource(opId, name);
        notifySuccess(`配置文件已添加：${view.name}（${view.nodeCount} 个节点）`, { operationId: opId, important: true });
        recordOperationFeedback(opId, true, `配置文件已添加：${view.name}`, "代理", "proxy.source_add_file");
      }
      setAddOpen(false);
      setAddName("");
      setAddURL("");
    } catch (e) {
      const message = errText(e);
      if (message !== PICKER_CANCELLED) {
        setAddError(message);
        recordOperationFeedback(opId, false, `添加配置失败：${message}`, "代理", "proxy.source_add");
      }
    } finally {
      setBusy(null);
      await reload();
    }
  };

  const changeBinding = (instance: Instance, nodeID: string) => {
    const label = nodeID === "" ? `实例「${instance.name}」已取消代理` : `实例「${instance.name}」的代理已更新`;
    void run(`bind-${instance.id}`, "proxy.binding_set", label, async (opId) => {
      if (nodeID === "") {
        await ProxyService.ClearBinding(opId, instance.id);
      } else {
        await ProxyService.SetBinding(opId, instance.id, nodeID);
      }
    });
  };

  const inputCls =
    "w-full rounded-md border border-(--border) bg-(--input-bg) px-2.5 py-1.5 text-sm text-(--text-main) outline-none placeholder:text-(--text-faint) disabled:opacity-50";
  const labelCls = "mb-1 block text-xs text-(--text-dim)";
  const btnPrimary =
    "inline-flex h-9 items-center gap-1.5 rounded-md bg-(--accent) px-3 text-sm font-medium text-(--accent-contrast) transition hover:opacity-90 disabled:opacity-50";
  const btnSecondary =
    "inline-flex h-9 items-center gap-1.5 rounded-md border border-(--border-strong) bg-(--input-bg) px-3 text-sm text-(--text-main) transition hover:bg-(--hover-bg) disabled:opacity-50";
  const btnSmall =
    "inline-flex h-7 items-center gap-1 rounded-md border border-(--border-strong) bg-(--input-bg) px-2 text-xs text-(--text-main) transition hover:bg-(--hover-bg) disabled:opacity-50";
  const btnDanger =
    "inline-flex h-9 items-center gap-1.5 rounded-md bg-(--danger) px-3 text-sm font-medium text-(--danger-contrast) transition hover:opacity-90 disabled:opacity-50";

  return (
    <div className="flex h-full flex-col">
      {/* 顶部工具栏：启动/关闭 + 添加 */}
      <div className="flex flex-wrap items-center gap-3 px-5 py-3">
        <div className="flex items-center gap-2">
          <button type="button" className={status?.running ? btnSecondary : btnPrimary} disabled={busy !== null || !status} onClick={toggleCore}>
            {busy === "core" ? <Loader2 size={15} className="animate-spin" /> : status?.running ? <Square size={15} /> : <Play size={15} />}
            {status?.running ? "关闭" : "启动"}
          </button>
          <button type="button" className={btnSecondary} disabled={busy !== null} onClick={() => { setAddError(null); setAddOpen(true); }}>
            <Plus size={15} />
            添加
          </button>
        </div>
        <span className="text-sm text-(--text-dim)">
          {status === null
            ? "正在读取核心状态…"
            : status.running
              ? `代理核心运行中（Mihomo ${status.version}）`
              : "代理核心未启动"}
        </span>
        {status?.message && <span className="text-sm text-(--danger)">{status.message}</span>}
      </div>

      {loading && !loadedRef.current ? (
        <div className="flex flex-1 items-center justify-center text-sm text-(--text-dim)">
          <Loader2 size={16} className="mr-2 animate-spin" /> 正在加载…
        </div>
      ) : loadError ? (
        <div className="flex flex-1 flex-col items-center justify-center gap-3 px-8 text-center">
          <p className="text-sm text-(--danger)">加载代理配置失败：{loadError}</p>
          <button type="button" className={btnSecondary} onClick={() => void reload()}>
            重试
          </button>
        </div>
      ) : (
        <div className="flex min-h-0 flex-1 border-t border-(--border)">
          {/* 左侧：节点列表（按来源分组，独立滚动） */}
          <div className="flex min-w-0 flex-1 flex-col border-r border-(--border)">
            <h3 className="border-b border-(--border) px-5 py-2.5 text-sm font-medium">代理节点</h3>
            <div className="min-h-0 flex-1 overflow-y-auto px-5 py-3">
              {sources.length === 0 ? (
                <p className="text-sm text-(--text-faint)">尚未添加任何代理配置。点击上方「添加」导入订阅或本地配置文件。</p>
              ) : (
                <div className="space-y-5">
                  {sources.map((src) => (
                    <div key={src.id}>
                      <div className="mb-2 flex items-center gap-2">
                        <span className="text-sm font-medium">{src.name}</span>
                        <span className="text-xs text-(--text-faint)">
                          {src.kind === "subscription" ? "订阅" : "本地文件"} · {src.nodeCount} 个节点
                        </span>
                        <span className="ml-auto flex items-center gap-1.5">
                          {src.kind === "subscription" && (
                            <button
                              type="button"
                              className={btnSmall}
                              disabled={busy !== null}
                              onClick={() =>
                                void run(`refresh-${src.id}`, "proxy.source_refresh", `订阅「${src.name}」已更新`, async (opId) => {
                                  await ProxyService.RefreshSource(opId, src.id);
                                })
                              }
                            >
                              <RefreshCw size={12} />
                              更新
                            </button>
                          )}
                          <button
                            type="button"
                            className={btnSmall}
                            disabled={busy !== null}
                            onClick={() => setConfirmDeleteSource(src)}
                          >
                            <Trash2 size={12} />
                            删除
                          </button>
                        </span>
                      </div>
                      <div className="space-y-1.5">
                        {(nodesBySource.get(src.id) ?? []).map((n) => (
                          <div key={n.id} className="flex items-center gap-2 rounded-md border border-(--border) bg-(--panel-bg) px-3 py-2">
                            <span className="min-w-0 flex-1 truncate text-sm">{n.name}</span>
                            <span className="shrink-0 text-xs text-(--text-faint)">{n.type}</span>
                          </div>
                        ))}
                        {(nodesBySource.get(src.id) ?? []).length === 0 && (
                          <p className="text-xs text-(--text-faint)">该配置暂无节点。</p>
                        )}
                      </div>
                    </div>
                  ))}
                </div>
              )}
            </div>
          </div>

          {/* 右侧：实例列表（独立滚动） */}
          <div className="flex min-w-0 flex-1 flex-col">
            <h3 className="border-b border-(--border) px-5 py-2.5 text-sm font-medium">浏览器实例</h3>
            <div className="min-h-0 flex-1 overflow-y-auto px-5 py-3">
              {instances.length === 0 ? (
                <p className="text-sm text-(--text-faint)">尚未创建任何浏览器实例。</p>
              ) : (
                <div className="space-y-1.5">
                  {instances.map((inst) => {
                    const binding = bindingByInstance.get(inst.id);
                    return (
                      <div key={inst.id} className="flex items-center gap-3 rounded-md border border-(--border) bg-(--panel-bg) px-3 py-2">
                        <span className="min-w-0 flex-1 truncate text-sm font-medium">{inst.name}</span>
                        <select
                          className="w-56 shrink-0 rounded-md border border-(--border) bg-(--input-bg) px-2 py-1.5 text-sm text-(--text-main) outline-none disabled:opacity-50"
                          value={binding?.nodeId ?? ""}
                          disabled={busy !== null}
                          onChange={(e) => changeBinding(inst, e.target.value)}
                          aria-label={`实例 ${inst.name} 的代理节点`}
                        >
                          <option value="">不使用代理</option>
                          {sources.map((src) => (
                            <optgroup key={src.id} label={src.name}>
                              {(nodesBySource.get(src.id) ?? []).map((n) => (
                                <option key={n.id} value={n.id}>
                                  {n.name}
                                </option>
                              ))}
                            </optgroup>
                          ))}
                        </select>
                        <span className="w-16 shrink-0 text-right text-xs text-(--text-faint)">
                          {binding ? `端口 ${binding.listenPort}` : "未设置"}
                        </span>
                      </div>
                    );
                  })}
                </div>
              )}
            </div>
          </div>
        </div>
      )}

      {/* 添加弹窗 */}
      {addOpen && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="w-full max-w-md rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">添加代理配置</h3>
            <div className="mt-4 inline-flex overflow-hidden rounded-md border border-(--border-strong)">
              <button
                type="button"
                className={`h-8 px-3.5 text-sm transition-colors ${addTab === "subscription" ? "bg-(--accent) font-medium text-(--accent-contrast)" : "text-(--text-main) hover:bg-(--hover-bg)"}`}
                onClick={() => { setAddTab("subscription"); setAddError(null); }}
              >
                添加订阅
              </button>
              <button
                type="button"
                className={`h-8 px-3.5 text-sm transition-colors ${addTab === "file" ? "bg-(--accent) font-medium text-(--accent-contrast)" : "text-(--text-main) hover:bg-(--hover-bg)"}`}
                onClick={() => { setAddTab("file"); setAddError(null); }}
              >
                本地添加
              </button>
            </div>
            <div className="mt-4 space-y-3">
              <div>
                <label className={labelCls}>名称</label>
                <input className={inputCls} value={addName} onChange={(e) => setAddName(e.target.value)} placeholder="例如：我的订阅" />
              </div>
              {addTab === "subscription" ? (
                <div>
                  <label className={labelCls}>订阅地址</label>
                  <input className={inputCls} value={addURL} onChange={(e) => setAddURL(e.target.value)} placeholder="https://…" />
                </div>
              ) : (
                <p className="text-sm text-(--text-dim)">点击「添加」后将打开系统文件选择窗口，请选择 Clash/Mihomo 的 YAML 配置文件。</p>
              )}
              {addError && <p className="text-sm text-(--danger)">{addError}</p>}
            </div>
            <div className="mt-5 flex justify-end gap-2">
              <button type="button" className={btnSecondary} disabled={busy !== null} onClick={() => setAddOpen(false)}>
                取消
              </button>
              <button type="button" className={btnPrimary} disabled={busy !== null} onClick={() => void submitAdd()}>
                {busy === "add" ? <Loader2 size={15} className="animate-spin" /> : null}
                添加
              </button>
            </div>
          </div>
        </div>
      )}

      {/* 删除来源确认 */}
      {confirmDeleteSource && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="w-full max-w-md rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">删除代理配置</h3>
            <p className="mt-3 text-sm leading-relaxed text-(--text-dim)">
              确定删除「{confirmDeleteSource.name}」吗？其全部节点将被移除，正在使用这些节点的实例将变为未设置代理。此操作不可恢复。
            </p>
            <div className="mt-5 flex justify-end gap-2">
              <button type="button" className={btnSecondary} disabled={busy !== null} onClick={() => setConfirmDeleteSource(null)}>
                取消
              </button>
              <button
                type="button"
                className={btnDanger}
                disabled={busy !== null}
                onClick={() => {
                  const src = confirmDeleteSource;
                  setConfirmDeleteSource(null);
                  void run(`delete-${src.id}`, "proxy.source_delete", `代理配置「${src.name}」已删除`, async (opId) => {
                    await ProxyService.DeleteSource(opId, src.id);
                  });
                }}
              >
                删除
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
