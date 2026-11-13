import { useCallback, useEffect, useState } from "react";
import {
  FileUp,
  Layers,
  Pencil,
  Plus,
  RefreshCw,
  Rss,
  Trash2,
  TriangleAlert,
} from "lucide-react";
import {
  SubscriptionService,
  type GroupMemberView,
  type ImportSummary,
  type MihomoConfigView,
  type ProxyGroupView,
  type SubscriptionView,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import { useAppStore } from "../stores/useAppStore";

const AUTO_UPDATE_OPTIONS: { value: number; label: string }[] = [
  { value: 0, label: "关闭" },
  { value: 15, label: "15 分钟" },
  { value: 30, label: "30 分钟" },
  { value: 60, label: "1 小时" },
  { value: 120, label: "2 小时" },
  { value: 360, label: "6 小时" },
  { value: 720, label: "12 小时" },
  { value: 1440, label: "24 小时" },
];

const FORMAT_LABELS: Record<string, string> = {
  mihomo_yaml: "完整 Mihomo 配置",
  proxies_yaml: "代理节点 YAML",
  uri_list: "URI 订阅文本",
};

const MEMBER_KIND_LABELS: Record<string, string> = {
  proxy: "节点",
  group: "代理组",
  builtin: "内置",
};

interface SubForm {
  name: string;
  url: string;
  allowLan: boolean;
  autoUpdateMinutes: number;
}

const EMPTY_FORM: SubForm = { name: "", url: "", allowLan: false, autoUpdateMinutes: 0 };

export default function SubscriptionsPage() {
  const log = useAppStore((state) => state.log);
  const [subs, setSubs] = useState<SubscriptionView[]>([]);
  const [configs, setConfigs] = useState<MihomoConfigView[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const [formOpen, setFormOpen] = useState(false);
  const [editing, setEditing] = useState<SubscriptionView | null>(null);
  const [form, setForm] = useState<SubForm>(EMPTY_FORM);
  const [saving, setSaving] = useState(false);

  const [pendingDelete, setPendingDelete] = useState<SubscriptionView | null>(null);
  const [pendingDeleteConfig, setPendingDeleteConfig] = useState<MihomoConfigView | null>(null);
  const [deleting, setDeleting] = useState(false);

  const [refreshingId, setRefreshingId] = useState<string | null>(null);
  const [importOpen, setImportOpen] = useState(false);
  const [importName, setImportName] = useState("");
  const [importing, setImporting] = useState(false);
  const [lastSummary, setLastSummary] = useState<ImportSummary | null>(null);

  const [groupsOf, setGroupsOf] = useState<string | null>(null);
  const [groups, setGroups] = useState<ProxyGroupView[]>([]);
  const [groupsLoading, setGroupsLoading] = useState(false);

  const load = useCallback(async () => {
    try {
      const [s, c] = await Promise.all([
        SubscriptionService.ListSubscriptions(),
        SubscriptionService.ListConfigs(),
      ]);
      setSubs(s ?? []);
      setConfigs(c ?? []);
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const openCreate = () => {
    setEditing(null);
    setForm(EMPTY_FORM);
    setFormOpen(true);
  };

  const openEdit = (sub: SubscriptionView) => {
    setEditing(sub);
    setForm({
      name: sub.name,
      url: "",
      allowLan: sub.allowLan,
      autoUpdateMinutes: sub.autoUpdateMinutes,
    });
    setFormOpen(true);
  };

  const submitForm = async () => {
    setSaving(true);
    try {
      const input = {
        name: form.name,
        url: form.url,
        allowLan: form.allowLan,
        autoUpdateMinutes: form.autoUpdateMinutes,
      };
      if (editing) {
        await SubscriptionService.UpdateSubscription(editing.id, input);
        log("info", `订阅已更新：${form.name}`);
      } else {
        await SubscriptionService.CreateSubscription(input);
        log("info", `订阅已新增：${form.name}`);
      }
      setFormOpen(false);
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  };

  const doRefresh = async (sub: SubscriptionView) => {
    setRefreshingId(sub.id);
    setNotice(null);
    try {
      const sum = await SubscriptionService.RefreshSubscription(sub.id);
      setLastSummary(sum);
      setNotice(
        `「${sub.name}」刷新完成（${FORMAT_LABELS[sum.format] ?? sum.format}）：新增 ${sum.added} 个节点，更新 ${sum.updated} 个，删除 ${sum.removed} 个，代理组 ${sum.groups} 个。`,
      );
      log("info", `订阅刷新完成：${sub.name}`);
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      await load();
    } finally {
      setRefreshingId(null);
    }
  };

  const doImport = async () => {
    setImporting(true);
    setNotice(null);
    try {
      const sum = await SubscriptionService.ImportLocalConfig(importName);
      if (sum.cancelled) {
        setNotice("已取消导入。");
      } else {
        setLastSummary(sum);
        setNotice(
          `本地配置「${sum.configName}」导入完成（${FORMAT_LABELS[sum.format] ?? sum.format}）：新增 ${sum.added} 个节点，代理组 ${sum.groups} 个。`,
        );
        log("info", `本地配置导入完成：${sum.configName}`);
        setImportOpen(false);
        setImportName("");
        await load();
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setImporting(false);
    }
  };

  const doDeleteSubscription = async () => {
    if (!pendingDelete) return;
    setDeleting(true);
    try {
      await SubscriptionService.DeleteSubscription(pendingDelete.id);
      log("info", `订阅已删除：${pendingDelete.name}`);
      setPendingDelete(null);
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setDeleting(false);
    }
  };

  const doDeleteConfig = async () => {
    if (!pendingDeleteConfig) return;
    setDeleting(true);
    try {
      await SubscriptionService.DeleteConfig(pendingDeleteConfig.id);
      log("info", `配置已删除：${pendingDeleteConfig.name}`);
      setPendingDeleteConfig(null);
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setDeleting(false);
    }
  };

  const toggleGroups = async (cfg: MihomoConfigView) => {
    if (groupsOf === cfg.id) {
      setGroupsOf(null);
      setGroups([]);
      return;
    }
    setGroupsOf(cfg.id);
    setGroupsLoading(true);
    try {
      const g = await SubscriptionService.GetConfigGroups(cfg.id);
      setGroups(g ?? []);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      setGroups([]);
    } finally {
      setGroupsLoading(false);
    }
  };

  const inputCls =
    "w-full rounded-md border border-(--border) bg-(--app-bg) px-2.5 py-1.5 text-sm text-(--text-main) outline-none placeholder:text-(--text-faint)";
  const labelCls = "mb-1 block text-xs text-(--text-dim)";
  const btnCls =
    "flex items-center gap-1.5 rounded-md border border-(--border) px-3 py-1.5 text-sm text-(--text-dim) hover:bg-(--hover-bg) disabled:opacity-50";
  const primaryCls =
    "flex items-center gap-1.5 rounded-md bg-(--accent) px-3 py-1.5 text-sm font-medium text-(--accent-contrast) hover:opacity-90 disabled:opacity-50";

  const statusBadge = (sub: SubscriptionView) => {
    if (sub.lastStatus === "ok") {
      return <span className="rounded-full bg-(--badge-bg) px-2 py-0.5 text-xs text-(--text-dim)">刷新正常</span>;
    }
    if (sub.lastStatus === "error") {
      return <span className="rounded-full bg-(--danger-bg) px-2 py-0.5 text-xs text-(--danger)">刷新失败</span>;
    }
    return <span className="rounded-full bg-(--badge-bg) px-2 py-0.5 text-xs text-(--text-faint)">尚未刷新</span>;
  };

  const memberLine = (m: GroupMemberView) => {
    const kind = MEMBER_KIND_LABELS[m.kind] ?? m.kind;
    const target = m.kind === "proxy" && m.proxyName ? `（当前节点：${m.proxyName}）` : "";
    return `${m.name} · ${kind}${target}`;
  };

  return (
    <div className="mx-auto max-w-6xl px-6 py-6">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold">订阅与配置</h2>
          <p className="mt-0.5 text-sm text-(--text-dim)">
            管理代理订阅与导入的 Mihomo 配置。订阅地址只保存在凭据库中，列表仅显示站点信息。
          </p>
        </div>
        <div className="flex gap-2">
          <button type="button" onClick={() => void load()} className={btnCls}>
            <RefreshCw size={15} />
            刷新列表
          </button>
          <button type="button" onClick={() => setImportOpen(true)} className={btnCls}>
            <FileUp size={15} />
            导入本地配置
          </button>
          <button type="button" onClick={openCreate} className={primaryCls}>
            <Plus size={15} />
            新增订阅
          </button>
        </div>
      </div>

      {error && (
        <div className="mt-4 flex items-start gap-2 rounded-lg border border-(--danger-border) bg-(--danger-bg) px-3 py-2 text-sm text-(--danger)">
          <TriangleAlert size={16} className="mt-0.5 shrink-0" />
          <span>{error}</span>
        </div>
      )}
      {notice && (
        <div className="mt-4 rounded-lg border border-(--border) bg-(--panel-bg) px-3 py-2 text-sm text-(--text-dim)">
          {notice}
        </div>
      )}
      {lastSummary && lastSummary.issues && lastSummary.issues.length > 0 && (
        <div className="mt-4 rounded-lg border border-(--border) bg-(--panel-bg) px-3 py-2 text-sm text-(--text-dim)">
          <p className="font-medium text-(--text-main)">安全提示（原始配置检查结果，运行时加固将在核心集成阶段执行）</p>
          <ul className="mt-1 list-inside list-disc space-y-0.5 text-xs">
            {lastSummary.issues.map((issue) => (
              <li key={issue.code}>{issue.message}</li>
            ))}
          </ul>
        </div>
      )}

      <h3 className="mt-6 flex items-center gap-2 text-sm font-semibold">
        <Rss size={15} />
        订阅列表
      </h3>
      <div className="mt-3 overflow-hidden rounded-lg border border-(--border) bg-(--panel-bg)">
        {loading ? (
          <p className="px-4 py-10 text-center text-sm text-(--text-faint)">加载中…</p>
        ) : subs.length === 0 ? (
          <p className="px-4 py-10 text-center text-sm text-(--text-faint)">
            暂无订阅。点击右上角「新增订阅」添加第一个订阅。
          </p>
        ) : (
          <ul className="divide-y divide-(--border)">
            {subs.map((sub) => (
              <li key={sub.id} className="px-4 py-3">
                <div className="flex items-center justify-between gap-3">
                  <div className="min-w-0">
                    <p className="truncate text-sm font-medium">{sub.name}</p>
                    <p className="mt-0.5 truncate text-xs text-(--text-faint)">
                      {sub.urlHint || "未设置地址"}
                      {sub.allowLan ? " · 允许局域网" : ""}
                      {sub.autoUpdateMinutes > 0
                        ? ` · 自动更新 ${AUTO_UPDATE_OPTIONS.find((o) => o.value === sub.autoUpdateMinutes)?.label ?? `${sub.autoUpdateMinutes} 分钟`}`
                        : " · 自动更新关闭"}
                    </p>
                  </div>
                  <div className="flex shrink-0 items-center gap-2">
                    {statusBadge(sub)}
                    <button
                      type="button"
                      disabled={refreshingId === sub.id}
                      onClick={() => void doRefresh(sub)}
                      className={btnCls}
                    >
                      <RefreshCw size={14} className={refreshingId === sub.id ? "animate-spin" : ""} />
                      {refreshingId === sub.id ? "刷新中" : "刷新"}
                    </button>
                    <button type="button" onClick={() => openEdit(sub)} className={btnCls}>
                      <Pencil size={14} />
                      编辑
                    </button>
                    <button type="button" onClick={() => setPendingDelete(sub)} className={btnCls}>
                      <Trash2 size={14} />
                      删除
                    </button>
                  </div>
                </div>
                <p className="mt-1.5 text-xs text-(--text-dim)">
                  节点 {sub.nodeCount} 个 · 代理组 {sub.groupCount} 个 · 最近更新：
                  {sub.lastUpdatedAt ?? "无"}
                </p>
                {sub.lastStatus === "error" && sub.lastError && (
                  <p className="mt-1 text-xs text-(--danger)">最近一次刷新失败：{sub.lastError}</p>
                )}
              </li>
            ))}
          </ul>
        )}
      </div>

      <h3 className="mt-8 flex items-center gap-2 text-sm font-semibold">
        <Layers size={15} />
        已导入配置
      </h3>
      <div className="mt-3 overflow-hidden rounded-lg border border-(--border) bg-(--panel-bg)">
        {configs.length === 0 ? (
          <p className="px-4 py-10 text-center text-sm text-(--text-faint)">
            暂无已导入配置。刷新订阅或导入本地文件后，配置会出现在这里。
          </p>
        ) : (
          <ul className="divide-y divide-(--border)">
            {configs.map((cfg) => (
              <li key={cfg.id} className="px-4 py-3">
                <div className="flex items-center justify-between gap-3">
                  <div className="min-w-0">
                    <p className="truncate text-sm font-medium">{cfg.name}</p>
                    <p className="mt-0.5 text-xs text-(--text-faint)">
                      {cfg.sourceType === "subscription" ? "来自订阅" : "本地文件导入"} · 节点 {cfg.nodeCount} 个 · 代理组{" "}
                      {cfg.groupCount} 个 · 内容校验和 {cfg.sha256.slice(0, 12)}…
                    </p>
                  </div>
                  <div className="flex shrink-0 items-center gap-2">
                    <button type="button" onClick={() => void toggleGroups(cfg)} className={btnCls}>
                      {groupsOf === cfg.id ? "收起代理组" : "查看代理组"}
                    </button>
                    {cfg.sourceType === "file" && (
                      <button type="button" onClick={() => setPendingDeleteConfig(cfg)} className={btnCls}>
                        <Trash2 size={14} />
                        删除
                      </button>
                    )}
                  </div>
                </div>
                {cfg.issues && cfg.issues.length > 0 && (
                  <ul className="mt-1.5 list-inside list-disc space-y-0.5 text-xs text-(--text-dim)">
                    {cfg.issues.map((issue) => (
                      <li key={issue.code}>{issue.message}</li>
                    ))}
                  </ul>
                )}
                {groupsOf === cfg.id && (
                  <div className="mt-3 space-y-2">
                    {groupsLoading ? (
                      <p className="text-xs text-(--text-faint)">加载中…</p>
                    ) : groups.length === 0 ? (
                      <p className="text-xs text-(--text-faint)">该配置没有原生代理组。</p>
                    ) : (
                      groups.map((g) => (
                        <div key={g.id} className="rounded-md bg-(--app-bg) px-3 py-2">
                          <p className="text-xs font-medium">
                            {g.name} <span className="text-(--text-faint)">（{g.type}）</span>
                          </p>
                          <ul className="mt-1 space-y-0.5 text-xs text-(--text-dim)">
                            {(g.members ?? []).map((m, i) => (
                              <li key={`${g.id}-${i}`}>{memberLine(m)}</li>
                            ))}
                          </ul>
                        </div>
                      ))
                    )}
                  </div>
                )}
              </li>
            ))}
          </ul>
        )}
      </div>

      {formOpen && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="w-full max-w-lg rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">{editing ? "编辑订阅" : "新增订阅"}</h3>
            <div className="mt-4 space-y-3">
              <div>
                <label className={labelCls}>名称</label>
                <input
                  className={inputCls}
                  value={form.name}
                  onChange={(e) => setForm({ ...form, name: e.target.value })}
                  placeholder="例如：主力订阅"
                />
              </div>
              <div>
                <label className={labelCls}>订阅地址</label>
                <input
                  className={inputCls}
                  value={form.url}
                  onChange={(e) => setForm({ ...form, url: e.target.value })}
                  placeholder={editing ? `留空表示保留原地址（当前：${editing.urlHint}）` : "https://…"}
                />
                <p className="mt-1 text-xs text-(--text-faint)">
                  完整地址只保存在本机凭据库中，不会写入数据库，也不会在界面回显。
                </p>
              </div>
              <div>
                <label className={labelCls}>自动更新间隔</label>
                <select
                  className={inputCls}
                  value={form.autoUpdateMinutes}
                  onChange={(e) => setForm({ ...form, autoUpdateMinutes: Number(e.target.value) })}
                >
                  {AUTO_UPDATE_OPTIONS.map((o) => (
                    <option key={o.value} value={o.value}>
                      {o.label}
                    </option>
                  ))}
                </select>
              </div>
              <label className="flex items-start gap-2 text-sm text-(--text-dim)">
                <input
                  type="checkbox"
                  className="mt-0.5"
                  checked={form.allowLan}
                  onChange={(e) => setForm({ ...form, allowLan: e.target.checked })}
                />
                <span>
                  允许访问局域网订阅地址
                  <span className="block text-xs text-(--text-faint)">
                    仅当订阅服务器位于内网时开启；回环地址始终禁止访问。
                  </span>
                </span>
              </label>
            </div>
            <div className="mt-5 flex justify-end gap-2">
              <button type="button" onClick={() => setFormOpen(false)} className={btnCls}>
                取消
              </button>
              <button type="button" disabled={saving} onClick={() => void submitForm()} className={primaryCls}>
                {saving ? "保存中…" : "保存"}
              </button>
            </div>
          </div>
        </div>
      )}

      {importOpen && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="w-full max-w-lg rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">导入本地配置</h3>
            <p className="mt-2 text-sm text-(--text-dim)">
              支持完整 Mihomo/Clash YAML、仅含代理节点的 YAML 与 URI 订阅文本（可为 Base64
              编码）。文件中的凭据会被提取到凭据库，保存的副本不含明文凭据。
            </p>
            <div className="mt-4">
              <label className={labelCls}>配置名称（可留空，默认使用文件名）</label>
              <input
                className={inputCls}
                value={importName}
                onChange={(e) => setImportName(e.target.value)}
                placeholder="例如：机场主配置"
              />
            </div>
            <div className="mt-5 flex justify-end gap-2">
              <button type="button" onClick={() => setImportOpen(false)} className={btnCls}>
                取消
              </button>
              <button type="button" disabled={importing} onClick={() => void doImport()} className={primaryCls}>
                {importing ? "导入中…" : "选择文件并导入"}
              </button>
            </div>
          </div>
        </div>
      )}

      {pendingDelete && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="w-full max-w-md rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">删除订阅</h3>
            <p className="mt-2 text-sm text-(--text-dim)">
              确定删除订阅「{pendingDelete.name}」吗？该订阅导入的全部节点（{pendingDelete.nodeCount}{" "}
              个）、代理组与配置副本会一并删除，手动添加的代理不受影响。此操作不可恢复。
            </p>
            <div className="mt-5 flex justify-end gap-2">
              <button type="button" onClick={() => setPendingDelete(null)} className={btnCls}>
                取消
              </button>
              <button type="button" disabled={deleting} onClick={() => void doDeleteSubscription()} className={primaryCls}>
                {deleting ? "删除中…" : "确认删除"}
              </button>
            </div>
          </div>
        </div>
      )}

      {pendingDeleteConfig && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="w-full max-w-md rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">删除已导入配置</h3>
            <p className="mt-2 text-sm text-(--text-dim)">
              确定删除配置「{pendingDeleteConfig.name}」吗？该配置导入的全部节点（{pendingDeleteConfig.nodeCount}{" "}
              个）与代理组会一并删除。此操作不可恢复。
            </p>
            <div className="mt-5 flex justify-end gap-2">
              <button type="button" onClick={() => setPendingDeleteConfig(null)} className={btnCls}>
                取消
              </button>
              <button type="button" disabled={deleting} onClick={() => void doDeleteConfig()} className={primaryCls}>
                {deleting ? "删除中…" : "确认删除"}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
