import { useCallback, useEffect, useMemo, useState } from "react";
import {
  AlertTriangle,
  CheckCircle2,
  FileUp,
  Globe,
  Link2,
  Loader2,
  Plus,
  RefreshCw,
  Trash2,
  X,
} from "lucide-react";
import {
  ProxyService,
  SubscriptionService,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import type {
  MihomoConfigView,
  ProxyGroupView,
  ProxyView,
  SubscriptionView,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service/models";
import { useAppStore } from "../stores/useAppStore";

// 代理配置（Stage 6C 重做）：单列布局，从上到下依次是顶部操作区
// （订阅链接快捷导入 / 新建）、配置卡片列表、当前配置的节点列表。
// 节点只能来自远程订阅或本地 Mihomo 配置导入，本页只读展示，
// 不提供单节点增删改，也不提供自定义代理组成员编辑。

type BusyKind = "" | "quick" | "create" | "refresh" | "delete" | "pick";

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

function hostOf(raw: string): string {
  try {
    return new URL(raw).host || "订阅";
  } catch {
    return "订阅";
  }
}

export default function ProxyConfigPage() {
  const log = useAppStore((state) => state.log);
  const [configs, setConfigs] = useState<MihomoConfigView[]>([]);
  const [subscriptions, setSubscriptions] = useState<SubscriptionView[]>([]);
  const [proxies, setProxies] = useState<ProxyView[]>([]);
  const [groups, setGroups] = useState<ProxyGroupView[]>([]);
  const [selectedId, setSelectedId] = useState<string>("");
  const [quickUrl, setQuickUrl] = useState("");
  const [busy, setBusy] = useState<BusyKind>("");
  const [busyId, setBusyId] = useState("");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [createOpen, setCreateOpen] = useState(false);
  const [createType, setCreateType] = useState<"remote" | "local">("remote");
  const [createName, setCreateName] = useState("");
  const [createUrl, setCreateUrl] = useState("");
  const [pickedFileName, setPickedFileName] = useState("");
  const [createError, setCreateError] = useState("");
  const [deleteTarget, setDeleteTarget] = useState<MihomoConfigView | null>(null);

  const load = useCallback(async () => {
    try {
      const [cfgs, subs, pxs] = await Promise.all([
        SubscriptionService.ListConfigs(),
        SubscriptionService.ListSubscriptions(),
        ProxyService.ListProxies(),
      ]);
      const cfgList = cfgs ?? [];
      setConfigs(cfgList);
      setSubscriptions(subs ?? []);
      setProxies(pxs ?? []);
      setSelectedId((prev) =>
        prev && cfgList.some((c) => c.id === prev) ? prev : (cfgList[0]?.id ?? ""),
      );
    } catch (err) {
      setError(errText(err));
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const selected = useMemo(
    () => configs.find((c) => c.id === selectedId) ?? null,
    [configs, selectedId],
  );

  useEffect(() => {
    if (!selectedId) {
      setGroups([]);
      return;
    }
    let live = true;
    SubscriptionService.GetConfigGroups(selectedId)
      .then((gs) => {
        if (live) setGroups(gs ?? []);
      })
      .catch(() => {
        if (live) setGroups([]);
      });
    return () => {
      live = false;
    };
  }, [selectedId, configs]);

  const subById = useMemo(() => {
    const map = new Map<string, SubscriptionView>();
    for (const s of subscriptions) map.set(s.id, s);
    return map;
  }, [subscriptions]);

  const selectedNodes = useMemo(
    () => proxies.filter((p) => p.configId === selectedId),
    [proxies, selectedId],
  );

  // 远程导入（快捷栏与新建窗口共用）：先建订阅再立即刷新；
  // 刷新失败时删除刚建的订阅，不留半成品配置，输入保留。
  const importRemote = async (name: string, url: string): Promise<string | null> => {
    const created = await SubscriptionService.CreateSubscription({
      name: name.trim(),
      url: url.trim(),
      allowLan: false,
      autoUpdateMinutes: 0,
    });
    try {
      await SubscriptionService.RefreshSubscription(created.id);
      return null;
    } catch (err) {
      await SubscriptionService.DeleteSubscription(created.id).catch(() => undefined);
      return errText(err);
    }
  };

  const quickImport = async () => {
    const url = quickUrl.trim();
    if (!url) {
      setError("请先粘贴订阅链接");
      return;
    }
    setBusy("quick");
    setError("");
    setNotice("");
    try {
      const failure = await importRemote(hostOf(url), url);
      if (failure) {
        setError(`导入失败：${failure}`);
        log("error", `订阅快捷导入失败：${failure}`);
        return;
      }
      setQuickUrl("");
      setNotice("订阅导入成功");
      await load();
    } catch (err) {
      setError(`导入失败：${errText(err)}`);
    } finally {
      setBusy("");
    }
  };

  const refreshCard = async (cfg: MihomoConfigView) => {
    if (!cfg.subscriptionId) return;
    setBusy("refresh");
    setBusyId(cfg.id);
    setError("");
    setNotice("");
    try {
      await SubscriptionService.RefreshSubscription(cfg.subscriptionId);
      setNotice(`「${cfg.name}」已刷新`);
      await load();
    } catch (err) {
      setError(`刷新失败：${errText(err)}`);
      await load();
    } finally {
      setBusy("");
      setBusyId("");
    }
  };

  const confirmDelete = async () => {
    if (!deleteTarget) return;
    setBusy("delete");
    setError("");
    try {
      if (deleteTarget.subscriptionId) {
        await SubscriptionService.DeleteSubscription(deleteTarget.subscriptionId);
      } else {
        await SubscriptionService.DeleteConfig(deleteTarget.id);
      }
      setNotice(`「${deleteTarget.name}」已删除`);
      setDeleteTarget(null);
      await load();
    } catch (err) {
      setError(`删除失败：${errText(err)}`);
      setDeleteTarget(null);
    } finally {
      setBusy("");
    }
  };

  const pickFile = async () => {
    setBusy("pick");
    setCreateError("");
    try {
      const name = await SubscriptionService.SelectLocalConfigFile();
      if (name) setPickedFileName(name);
    } catch (err) {
      setCreateError(errText(err));
    } finally {
      setBusy("");
    }
  };

  const submitCreate = async () => {
    setBusy("create");
    setCreateError("");
    setError("");
    setNotice("");
    try {
      if (createType === "remote") {
        if (!createName.trim() || !createUrl.trim()) {
          setCreateError("请填写配置名称和订阅链接");
          return;
        }
        const failure = await importRemote(createName, createUrl);
        if (failure) {
          setCreateError(`创建失败：${failure}`);
          return;
        }
        setNotice("远程配置已创建");
      } else {
        if (!pickedFileName) {
          setCreateError("请先选择配置文件");
          return;
        }
        const summary = await SubscriptionService.ImportPickedLocalConfig(createName.trim());
        if (summary.cancelled) return;
        setNotice("本地配置已导入");
      }
      setCreateOpen(false);
      setCreateName("");
      setCreateUrl("");
      setPickedFileName("");
      await load();
    } catch (err) {
      setCreateError(errText(err));
    } finally {
      setBusy("");
    }
  };

  return (
    <div className="mx-auto max-w-5xl space-y-5 p-8">
      <div>
        <h2 className="text-xl font-semibold">代理配置</h2>
        <p className="mt-1.5 text-sm leading-relaxed text-(--text-dim)">
          管理远程订阅与本地 Mihomo 配置。节点只能来自订阅或配置文件导入，此处只读展示。
        </p>
      </div>

      {/* 区域一：顶部操作区 */}
      <div className="flex items-center gap-3">
        <div className="relative flex-1">
          <Link2 size={15} className="pointer-events-none absolute top-1/2 left-3 -translate-y-1/2 text-(--text-faint)" />
          <input
            value={quickUrl}
            onChange={(e) => setQuickUrl(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") void quickImport();
            }}
            placeholder="粘贴订阅链接，直接导入远程订阅"
            className="w-full rounded-md border border-(--border-strong) bg-(--panel-bg) py-2 pr-3 pl-9 text-sm text-(--text-main)"
          />
        </div>
        <button
          onClick={() => void quickImport()}
          disabled={busy !== ""}
          className="flex items-center gap-2 rounded-md bg-(--accent) px-4 py-2 text-sm font-medium text-white transition-opacity hover:opacity-90 disabled:opacity-50"
        >
          {busy === "quick" ? <Loader2 size={15} className="animate-spin" /> : <Globe size={15} />}
          导入
        </button>
        <button
          onClick={() => {
            setCreateError("");
            setCreateOpen(true);
          }}
          disabled={busy !== ""}
          className="flex items-center gap-2 rounded-md border border-(--border-strong) px-4 py-2 text-sm text-(--text-main) transition-colors hover:bg-(--hover-bg) disabled:opacity-50"
        >
          <Plus size={15} />
          新建
        </button>
      </div>

      {error && (
        <div className="flex items-start gap-2 rounded-lg border border-(--danger-border) bg-(--danger-bg) px-4 py-3 text-sm text-(--danger)">
          <AlertTriangle size={16} className="mt-0.5 shrink-0" />
          <span>{error}</span>
        </div>
      )}
      {notice && (
        <div className="flex items-center gap-2 rounded-lg border border-(--border) bg-(--panel-bg) px-4 py-3 text-sm text-(--text-main)">
          <CheckCircle2 size={16} className="shrink-0 text-(--ok)" />
          <span>{notice}</span>
        </div>
      )}

      {/* 区域二：配置卡片列表 */}
      <div className="space-y-3">
        {configs.length === 0 && (
          <div className="rounded-lg border border-dashed border-(--border) px-4 py-10 text-center text-sm text-(--text-faint)">
            暂无配置。在上方粘贴订阅链接导入，或点击「新建」创建远程 / 本地配置。
          </div>
        )}
        {configs.map((cfg) => {
          const sub = cfg.subscriptionId ? subById.get(cfg.subscriptionId) : undefined;
          const isSelected = cfg.id === selectedId;
          const failed = sub?.lastStatus === "error";
          return (
            <div
              key={cfg.id}
              onClick={() => setSelectedId(cfg.id)}
              className={`cursor-pointer rounded-lg border bg-(--panel-bg) px-5 py-4 transition-colors ${
                isSelected ? "border-(--accent)" : "border-(--border) hover:border-(--border-strong)"
              }`}
            >
              <div className="flex items-center gap-3">
                <span className="text-sm font-semibold">{cfg.name}</span>
                <span className="rounded-full border border-(--border) px-2 py-0.5 text-[11px] text-(--text-dim)">
                  {cfg.sourceType === "subscription" ? "远程" : "本地"}
                </span>
                {isSelected && <span className="text-[11px] text-(--accent)">当前配置</span>}
                <div className="flex-1" />
                {cfg.subscriptionId && (
                  <button
                    onClick={(e) => {
                      e.stopPropagation();
                      void refreshCard(cfg);
                    }}
                    disabled={busy !== ""}
                    className="flex items-center gap-1.5 rounded-md border border-(--border-strong) px-2.5 py-1 text-xs text-(--text-dim) transition-colors hover:bg-(--hover-bg) hover:text-(--text-main) disabled:opacity-50"
                  >
                    {busy === "refresh" && busyId === cfg.id ? (
                      <Loader2 size={13} className="animate-spin" />
                    ) : (
                      <RefreshCw size={13} />
                    )}
                    刷新
                  </button>
                )}
                <button
                  onClick={(e) => {
                    e.stopPropagation();
                    setDeleteTarget(cfg);
                  }}
                  disabled={busy !== ""}
                  className="flex items-center gap-1.5 rounded-md border border-(--border-strong) px-2.5 py-1 text-xs text-(--text-dim) transition-colors hover:bg-(--hover-bg) hover:text-(--danger) disabled:opacity-50"
                >
                  <Trash2 size={13} />
                  删除
                </button>
              </div>
              <div className="mt-2 flex flex-wrap items-center gap-x-5 gap-y-1 text-xs text-(--text-faint)">
                <span>节点 {cfg.nodeCount}</span>
                <span>代理组 {cfg.groupCount}</span>
                <span>更新于 {cfg.updatedAt}</span>
                {sub && sub.lastStatus !== "" && (
                  <span>
                    最近刷新：
                    {failed ? (
                      <span className="text-(--danger)">失败</span>
                    ) : (
                      <span>成功{sub.lastUpdatedAt ? `（${sub.lastUpdatedAt}）` : ""}</span>
                    )}
                  </span>
                )}
              </div>
              {failed && sub?.lastError && (
                <p className="mt-1.5 text-xs text-(--danger)">刷新失败原因：{sub.lastError}</p>
              )}
              {cfg.issues && cfg.issues.length > 0 && (
                <p className="mt-1.5 text-xs text-(--text-faint)">
                  导入时已按安全策略净化该配置（{cfg.issues.length} 项提示），原始文件未改动。
                </p>
              )}
            </div>
          );
        })}
      </div>

      {/* 区域三：当前配置的节点列表（只读） */}
      {selected && (
        <div className="rounded-lg border border-(--border) bg-(--panel-bg)">
          <div className="border-b border-(--border) px-5 py-3">
            <h3 className="text-sm font-semibold">节点列表 · {selected.name}</h3>
            <p className="mt-0.5 text-xs text-(--text-faint)">只读展示，不在此处增删改节点。</p>
          </div>
          {selectedNodes.length === 0 ? (
            <p className="px-5 py-8 text-center text-sm text-(--text-faint)">该配置暂无节点。</p>
          ) : (
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-(--border) text-left text-xs text-(--text-faint)">
                  <th className="px-5 py-2 font-medium">节点名称</th>
                  <th className="px-3 py-2 font-medium">协议</th>
                  <th className="px-3 py-2 font-medium">服务器地址</th>
                  <th className="px-3 py-2 font-medium">端口</th>
                  <th className="px-5 py-2 font-medium">状态</th>
                </tr>
              </thead>
              <tbody>
                {selectedNodes.map((node) => (
                  <tr key={node.id} className="border-b border-(--border) last:border-0">
                    <td className="px-5 py-2">{node.name}</td>
                    <td className="px-3 py-2 text-(--text-dim)">{node.protocol}</td>
                    <td className="px-3 py-2 text-(--text-dim)">{node.address}</td>
                    <td className="px-3 py-2 text-(--text-dim)">{node.port ?? "—"}</td>
                    <td className="px-5 py-2 text-(--text-dim)">
                      {node.enabled ? "启用" : "停用"}
                      {node.credentialStatus !== "ok" && (
                        <span className="text-(--danger)"> · 凭据不可用</span>
                      )}
                      {node.lastTestedAt && (
                        <span>
                          {" "}
                          · 最近测试{node.lastTestOk ? "成功" : "失败"}
                          {node.lastTestOk && node.lastLatencyMs != null ? ` ${node.lastLatencyMs} ms` : ""}
                        </span>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          {groups.length > 0 && (
            <div className="border-t border-(--border) px-5 py-3">
              <h4 className="text-xs font-semibold text-(--text-dim)">原生代理组（只读）</h4>
              <div className="mt-2 space-y-1.5">
                {groups.map((g) => (
                  <p key={g.id} className="text-xs leading-relaxed text-(--text-faint)">
                    <span className="text-(--text-dim)">{g.name}</span>（{g.type}）：
                    {(g.members ?? []).map((m) => m.proxyName || m.name).join("、") || "无成员"}
                  </p>
                ))}
              </div>
            </div>
          )}
        </div>
      )}

      {/* 新建配置窗口 */}
      {createOpen && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4">
          <div className="w-full max-w-md rounded-xl border border-(--border) bg-(--panel-bg) p-6 shadow-xl">
            <div className="flex items-center justify-between">
              <h3 className="text-base font-semibold">新建配置</h3>
              <button
                onClick={() => setCreateOpen(false)}
                className="rounded-md p-1 text-(--text-dim) hover:bg-(--hover-bg)"
                title="关闭"
              >
                <X size={16} />
              </button>
            </div>
            <div className="mt-4 space-y-4">
              <label className="block">
                <span className="text-xs text-(--text-faint)">类型</span>
                <select
                  value={createType}
                  onChange={(e) => setCreateType(e.target.value as "remote" | "local")}
                  className="mt-1 w-full rounded-md border border-(--border-strong) bg-(--panel-bg) px-3 py-2 text-sm text-(--text-main)"
                >
                  <option value="remote">远程</option>
                  <option value="local">本地</option>
                </select>
              </label>
              <label className="block">
                <span className="text-xs text-(--text-faint)">配置名称</span>
                <input
                  value={createName}
                  onChange={(e) => setCreateName(e.target.value)}
                  placeholder={createType === "remote" ? "例如：机场订阅" : "留空则使用文件名"}
                  className="mt-1 w-full rounded-md border border-(--border-strong) bg-(--panel-bg) px-3 py-2 text-sm text-(--text-main)"
                />
              </label>
              {createType === "remote" ? (
                <label className="block">
                  <span className="text-xs text-(--text-faint)">订阅链接</span>
                  <input
                    value={createUrl}
                    onChange={(e) => setCreateUrl(e.target.value)}
                    placeholder="https://…"
                    className="mt-1 w-full rounded-md border border-(--border-strong) bg-(--panel-bg) px-3 py-2 text-sm text-(--text-main)"
                  />
                </label>
              ) : (
                <div>
                  <span className="text-xs text-(--text-faint)">配置文件</span>
                  <div className="mt-1 flex items-center gap-3">
                    <button
                      onClick={() => void pickFile()}
                      disabled={busy !== ""}
                      className="flex items-center gap-2 rounded-md border border-(--border-strong) px-3 py-2 text-sm text-(--text-main) hover:bg-(--hover-bg) disabled:opacity-50"
                    >
                      {busy === "pick" ? <Loader2 size={14} className="animate-spin" /> : <FileUp size={14} />}
                      选择文件
                    </button>
                    <span className="truncate text-sm text-(--text-dim)">
                      {pickedFileName || "尚未选择 .yaml / .yml 文件"}
                    </span>
                  </div>
                </div>
              )}
              {createError && (
                <div className="rounded-lg border border-(--danger-border) bg-(--danger-bg) px-3 py-2 text-sm text-(--danger)">
                  {createError}
                </div>
              )}
              <div className="flex justify-end gap-3 pt-1">
                <button
                  onClick={() => setCreateOpen(false)}
                  className="rounded-md border border-(--border-strong) px-4 py-2 text-sm text-(--text-dim) hover:bg-(--hover-bg)"
                >
                  取消
                </button>
                <button
                  onClick={() => void submitCreate()}
                  disabled={busy !== ""}
                  className="flex items-center gap-2 rounded-md bg-(--accent) px-4 py-2 text-sm font-medium text-white hover:opacity-90 disabled:opacity-50"
                >
                  {busy === "create" && <Loader2 size={14} className="animate-spin" />}
                  创建
                </button>
              </div>
            </div>
          </div>
        </div>
      )}

      {/* 删除确认 */}
      {deleteTarget && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4">
          <div className="w-full max-w-md rounded-xl border border-(--border) bg-(--panel-bg) p-6 shadow-xl">
            <h3 className="text-base font-semibold">删除配置</h3>
            <p className="mt-2 text-sm leading-relaxed text-(--text-dim)">
              确定删除「{deleteTarget.name}」吗？该配置的节点、原生代理组与关联数据将一并删除
              {deleteTarget.sourceType === "subscription" ? "，对应的订阅也会被删除" : ""}，此操作不可撤销。
            </p>
            <div className="mt-5 flex justify-end gap-3">
              <button
                onClick={() => setDeleteTarget(null)}
                className="rounded-md border border-(--border-strong) px-4 py-2 text-sm text-(--text-dim) hover:bg-(--hover-bg)"
              >
                取消
              </button>
              <button
                onClick={() => void confirmDelete()}
                disabled={busy !== ""}
                className="flex items-center gap-2 rounded-md bg-(--danger) px-4 py-2 text-sm font-medium text-white hover:opacity-90 disabled:opacity-50"
              >
                {busy === "delete" && <Loader2 size={14} className="animate-spin" />}
                删除
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
