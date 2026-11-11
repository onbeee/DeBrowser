import { useCallback, useEffect, useState } from "react";
import { AppWindow, Pencil, Plus, RefreshCw, Trash2, TriangleAlert } from "lucide-react";
import { InstanceService } from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import type { Instance, Proxy } from "../../bindings/github.com/onbeee/DeBrowser/internal/model";
import { useAppStore } from "../stores/useAppStore";

const STATUS_LABEL: Record<string, string> = {
  stopped: "已停止",
  running: "运行中",
  error: "异常",
};

interface FormState {
  id?: string;
  name: string;
  proxyId: string; // "" = 不使用代理
  startUrl: string;
  kernelName: string;
  kernelVersion: string;
}

const EMPTY_FORM: FormState = { name: "", proxyId: "", startUrl: "", kernelName: "", kernelVersion: "" };

type PendingDelete = { instance: Instance; withProfile: boolean } | null;

/**
 * 浏览器实例管理（Stage 3 基础能力）：实例元数据的增删改查。
 * 删除分为两种且含义明确区分：「删除记录」只删数据库记录，Profile
 * 文件保留；「删除记录并清理 Profile」才会删除磁盘上的浏览器数据，
 * 且必须二次确认。浏览器进程的启动/停止属于后续阶段。
 */
export default function InstancesPage() {
  const log = useAppStore((state) => state.log);
  const [instances, setInstances] = useState<Instance[]>([]);
  const [proxies, setProxies] = useState<Proxy[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [form, setForm] = useState<FormState | null>(null);
  const [pendingDelete, setPendingDelete] = useState<PendingDelete>(null);
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const [list, proxyList] = await Promise.all([
        InstanceService.ListInstances(),
        InstanceService.ListProxies(),
      ]);
      setInstances(list ?? []);
      setProxies(proxyList ?? []);
      setError(null);
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setError(`加载实例列表失败：${message}`);
      log("error", `加载实例列表失败：${message}`);
    } finally {
      setLoading(false);
    }
  }, [log]);

  useEffect(() => {
    void load();
  }, [load]);

  const proxyName = (id?: string | null) =>
    id ? (proxies.find((p) => p.id === id)?.name ?? "未知代理") : "不使用代理";

  const submitForm = async () => {
    if (!form) return;
    setBusy(true);
    try {
      const proxyId = form.proxyId === "" ? null : form.proxyId;
      if (form.id) {
        await InstanceService.UpdateInstance({
          id: form.id,
          name: form.name,
          proxyId,
          startUrl: form.startUrl,
          kernelName: form.kernelName,
          kernelVersion: form.kernelVersion,
          sortOrder: 0,
        });
        log("info", `实例已更新：${form.name}`);
      } else {
        const created = await InstanceService.CreateInstance({
          name: form.name,
          proxyId,
          startUrl: form.startUrl,
          kernelName: form.kernelName,
          kernelVersion: form.kernelVersion,
          userDataDir: "",
        });
        log("info", `实例已创建：${created.name}`);
      }
      setForm(null);
      await load();
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setError(message);
      log("error", `保存实例失败：${message}`);
    } finally {
      setBusy(false);
    }
  };

  const confirmDelete = async () => {
    if (!pendingDelete) return;
    setBusy(true);
    try {
      if (pendingDelete.withProfile) {
        await InstanceService.DeleteInstanceAndProfile(pendingDelete.instance.id);
        log("info", `实例记录及 Profile 已删除：${pendingDelete.instance.name}`);
      } else {
        await InstanceService.DeleteInstance(pendingDelete.instance.id);
        log("info", `实例记录已删除（Profile 文件保留）：${pendingDelete.instance.name}`);
      }
      setPendingDelete(null);
      await load();
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setError(message);
      log("error", `删除实例失败：${message}`);
    } finally {
      setBusy(false);
    }
  };

  const inputCls =
    "w-full rounded-md border border-(--border) bg-(--app-bg) px-2.5 py-1.5 text-sm text-(--text-main) outline-none placeholder:text-(--text-faint)";
  const labelCls = "mb-1 block text-xs text-(--text-dim)";

  return (
    <div className="mx-auto max-w-5xl px-6 py-6">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold">浏览器实例</h2>
          <p className="mt-0.5 text-sm text-(--text-dim)">
            管理浏览器实例的元数据。启动、停止浏览器进程将在后续阶段提供。
          </p>
        </div>
        <div className="flex gap-2">
          <button
            type="button"
            onClick={() => void load()}
            className="flex items-center gap-1.5 rounded-md border border-(--border) px-3 py-1.5 text-sm text-(--text-dim) hover:bg-(--hover-bg)"
          >
            <RefreshCw size={14} /> 刷新
          </button>
          <button
            type="button"
            onClick={() => setForm(EMPTY_FORM)}
            className="flex items-center gap-1.5 rounded-md bg-(--accent) px-3 py-1.5 text-sm font-medium text-(--accent-contrast) hover:opacity-90"
          >
            <Plus size={14} /> 新建实例
          </button>
        </div>
      </div>

      {error && (
        <div className="mt-4 flex items-start gap-2 rounded-lg border border-(--danger-border) bg-(--danger-bg) px-3 py-2 text-sm text-(--danger)">
          <TriangleAlert size={16} className="mt-0.5 shrink-0" />
          <span>{error}</span>
        </div>
      )}

      <div className="mt-5 overflow-hidden rounded-lg border border-(--border) bg-(--panel-bg)">
        {loading ? (
          <p className="px-4 py-10 text-center text-sm text-(--text-faint)">加载中…</p>
        ) : instances.length === 0 ? (
          <div className="flex flex-col items-center px-4 py-12 text-center">
            <span className="flex h-12 w-12 items-center justify-center rounded-2xl bg-(--hover-bg) text-(--text-faint)">
              <AppWindow size={22} />
            </span>
            <p className="mt-3 text-sm text-(--text-dim)">暂无浏览器实例</p>
            <p className="mt-1 text-xs text-(--text-faint)">点击右上角「新建实例」创建第一个实例。</p>
          </div>
        ) : (
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-(--border) text-left text-xs text-(--text-faint)">
                <th className="px-4 py-2.5 font-medium">名称</th>
                <th className="px-4 py-2.5 font-medium">状态</th>
                <th className="px-4 py-2.5 font-medium">内核</th>
                <th className="px-4 py-2.5 font-medium">代理</th>
                <th className="px-4 py-2.5 font-medium">数据目录</th>
                <th className="px-4 py-2.5 text-right font-medium">操作</th>
              </tr>
            </thead>
            <tbody>
              {instances.map((inst) => (
                <tr key={inst.id} className="border-b border-(--border) last:border-0 hover:bg-(--hover-bg)">
                  <td className="px-4 py-2.5 font-medium">{inst.name}</td>
                  <td className="px-4 py-2.5">
                    <span className="rounded-full bg-(--badge-bg) px-2 py-0.5 text-xs text-(--text-dim)">
                      {STATUS_LABEL[inst.status] ?? inst.status}
                    </span>
                  </td>
                  <td className="px-4 py-2.5 text-(--text-dim)">
                    {inst.kernelName
                      ? `${inst.kernelName}${inst.kernelVersion ? ` ${inst.kernelVersion}` : ""}`
                      : "未指定"}
                  </td>
                  <td className="px-4 py-2.5 text-(--text-dim)">{proxyName(inst.proxyId)}</td>
                  <td className="px-4 py-2.5 font-mono text-xs text-(--text-faint)">{inst.userDataDir}</td>
                  <td className="px-4 py-2.5">
                    <div className="flex justify-end gap-1">
                      <button
                        type="button"
                        title="编辑"
                        onClick={() =>
                          setForm({
                            id: inst.id,
                            name: inst.name,
                            proxyId: inst.proxyId ?? "",
                            startUrl: inst.startUrl,
                            kernelName: inst.kernelName,
                            kernelVersion: inst.kernelVersion,
                          })
                        }
                        className="rounded p-1.5 text-(--text-dim) hover:bg-(--hover-bg)"
                      >
                        <Pencil size={14} />
                      </button>
                      <button
                        type="button"
                        title="删除记录（保留 Profile 文件）"
                        onClick={() => setPendingDelete({ instance: inst, withProfile: false })}
                        className="rounded p-1.5 text-(--text-dim) hover:bg-(--hover-bg)"
                      >
                        <Trash2 size={14} />
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {form && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="w-full max-w-md rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">{form.id ? "编辑实例" : "新建实例"}</h3>
            <div className="mt-4 space-y-3">
              <div>
                <label className={labelCls}>名称</label>
                <input
                  className={inputCls}
                  value={form.name}
                  onChange={(e) => setForm({ ...form, name: e.target.value })}
                  placeholder="例如：工作号"
                />
              </div>
              <div>
                <label className={labelCls}>代理</label>
                <select
                  className={inputCls}
                  value={form.proxyId}
                  onChange={(e) => setForm({ ...form, proxyId: e.target.value })}
                >
                  <option value="">不使用代理</option>
                  {proxies.map((p) => (
                    <option key={p.id} value={p.id}>
                      {p.name}（{p.protocol}）
                    </option>
                  ))}
                </select>
              </div>
              <div>
                <label className={labelCls}>起始页（可选）</label>
                <input
                  className={inputCls}
                  value={form.startUrl}
                  onChange={(e) => setForm({ ...form, startUrl: e.target.value })}
                  placeholder="https://"
                />
              </div>
              <div className="grid grid-cols-2 gap-3">
                <div>
                  <label className={labelCls}>内核名称（可选）</label>
                  <input
                    className={inputCls}
                    value={form.kernelName}
                    onChange={(e) => setForm({ ...form, kernelName: e.target.value })}
                    placeholder="Chromium"
                  />
                </div>
                <div>
                  <label className={labelCls}>内核版本（可选）</label>
                  <input
                    className={inputCls}
                    value={form.kernelVersion}
                    onChange={(e) => setForm({ ...form, kernelVersion: e.target.value })}
                    placeholder="130"
                  />
                </div>
              </div>
            </div>
            <div className="mt-5 flex justify-end gap-2">
              <button
                type="button"
                onClick={() => setForm(null)}
                className="rounded-md border border-(--border) px-3 py-1.5 text-sm text-(--text-dim) hover:bg-(--hover-bg)"
              >
                取消
              </button>
              <button
                type="button"
                disabled={busy}
                onClick={() => void submitForm()}
                className="rounded-md bg-(--accent) px-3 py-1.5 text-sm font-medium text-(--accent-contrast) hover:opacity-90 disabled:opacity-50"
              >
                保存
              </button>
            </div>
          </div>
        </div>
      )}

      {pendingDelete && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="w-full max-w-md rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">删除实例「{pendingDelete.instance.name}」</h3>
            {pendingDelete.withProfile ? (
              <p className="mt-2 text-sm leading-relaxed text-(--text-dim)">
                将同时删除数据库记录和磁盘上的 Profile 目录
                <span className="font-mono text-xs"> {pendingDelete.instance.userDataDir} </span>
                中的全部浏览器数据（Cookie、登录状态、历史记录等），此操作不可恢复。
              </p>
            ) : (
              <>
                <p className="mt-2 text-sm leading-relaxed text-(--text-dim)">
                  仅删除数据库中的实例记录，磁盘上的 Profile 数据
                  <span className="font-mono text-xs"> {pendingDelete.instance.userDataDir} </span>
                  会保留，不会受影响。
                </p>
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => setPendingDelete({ ...pendingDelete, withProfile: true })}
                  className="mt-3 text-sm text-(--danger) underline underline-offset-2"
                >
                  改为同时清理 Profile 数据
                </button>
              </>
            )}
            <div className="mt-5 flex justify-end gap-2">
              <button
                type="button"
                onClick={() => setPendingDelete(null)}
                className="rounded-md border border-(--border) px-3 py-1.5 text-sm text-(--text-dim) hover:bg-(--hover-bg)"
              >
                取消
              </button>
              <button
                type="button"
                disabled={busy}
                onClick={() => void confirmDelete()}
                className="rounded-md bg-(--danger) px-3 py-1.5 text-sm font-medium text-white hover:opacity-90 disabled:opacity-50"
              >
                {pendingDelete.withProfile ? "确认删除并清理 Profile" : "确认删除记录"}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
