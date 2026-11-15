import { useCallback, useEffect, useState } from "react";
import { AppWindow, Pencil, Plus, Trash2, TriangleAlert } from "lucide-react";
import {
  BrowserService,
  InstanceService,
  type BrowserView,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import type { Instance } from "../../bindings/github.com/onbeee/DeBrowser/internal/model";
import { useAppStore } from "../stores/useAppStore";
import { newOperationId, recordOperationFeedback } from "../lib/operations";

const STATUS_LABEL: Record<string, string> = {
  stopped: "已停止",
  running: "运行中",
  error: "异常",
};

interface FormState {
  id?: string;
  name: string;
  startUrl: string;
  // browserId is the ID of one registered browser (「浏览器内核」
  // page) associated with the instance; "" means no association.
  // The options come only from actual registrations — never from
  // free text — so an association always resolves to a real
  // registered path.
  browserId: string;
}

const EMPTY_FORM: FormState = { name: "", startUrl: "", browserId: "" };

type PendingDelete = { instance: Instance; withProfile: boolean } | null;

/**
 * 浏览器实例管理（Stage 3 基础能力）：实例元数据的增删改查。
 * 删除分为两种且含义明确区分：「删除记录」只删数据库记录，Profile
 * 文件保留；「删除记录并清理 Profile」才会删除磁盘上的浏览器数据，
 * 且必须二次确认。浏览器进程的启动/停止属于后续阶段。
 */
export default function InstancesPage() {
  const log = useAppStore((state) => state.log);
  const setPage = useAppStore((state) => state.setPage);
  const [instances, setInstances] = useState<Instance[]>([]);
  const [browsers, setBrowsers] = useState<BrowserView[]>([]);
  const [browsersError, setBrowsersError] = useState<string | null>(null);
  const [editing, setEditing] = useState<Instance | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [form, setForm] = useState<FormState | null>(null);
  const [pendingDelete, setPendingDelete] = useState<PendingDelete>(null);
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const list = await InstanceService.ListInstances();
      setInstances(list ?? []);
      setError(null);
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setError(`加载实例列表失败：${message}`);
      log("error", `加载实例列表失败：${message}`);
    } finally {
      setLoading(false);
    }
    try {
      const regs = await BrowserService.ListBrowsers();
      setBrowsers(regs ?? []);
      setBrowsersError(null);
    } catch (err) {
      setBrowsersError(err instanceof Error ? err.message : String(err));
    }
  }, [log]);

  useEffect(() => {
    void load();
  }, [load]);

  const submitForm = async () => {
    if (!form) return;
    setBusy(true);
    const opId = newOperationId();
    const action = form.id ? "instance.update" : "instance.create";
    try {
      if (form.id) {
        await InstanceService.UpdateInstance(opId, {
          id: form.id,
          name: form.name,
          startUrl: form.startUrl,
          browserId: form.browserId,
          sortOrder: 0,
        });
        log("info", `实例已更新：${form.name}`);
        recordOperationFeedback(opId, true, `实例已更新：${form.name}`, "浏览器实例", action);
      } else {
        const created = await InstanceService.CreateInstance(opId, {
          name: form.name,
          startUrl: form.startUrl,
          browserId: form.browserId,
          userDataDir: "",
        });
        log("info", `实例已创建：${created.name}`);
        recordOperationFeedback(opId, true, `实例已创建：${created.name}`, "浏览器实例", action);
      }
      setForm(null);
      setEditing(null);
      await load();
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setError(message);
      log("error", `保存实例失败：${message}`);
      recordOperationFeedback(opId, false, `保存实例失败：${message}`, "浏览器实例", action);
    } finally {
      setBusy(false);
    }
  };

  const confirmDelete = async () => {
    if (!pendingDelete) return;
    setBusy(true);
    const opId = newOperationId();
    const action = pendingDelete.withProfile ? "instance.delete_with_profile" : "instance.delete";
    try {
      if (pendingDelete.withProfile) {
        await InstanceService.DeleteInstanceAndProfile(opId, pendingDelete.instance.id);
        log("info", `实例记录及 Profile 已删除：${pendingDelete.instance.name}`);
        recordOperationFeedback(opId, true, `实例记录及 Profile 已删除：${pendingDelete.instance.name}`, "浏览器实例", action);
      } else {
        await InstanceService.DeleteInstance(opId, pendingDelete.instance.id);
        log("info", `实例记录已删除（Profile 文件保留）：${pendingDelete.instance.name}`);
        recordOperationFeedback(opId, true, `实例记录已删除（Profile 文件保留）：${pendingDelete.instance.name}`, "浏览器实例", action);
      }
      setPendingDelete(null);
      await load();
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setError(message);
      log("error", `删除实例失败：${message}`);
      recordOperationFeedback(opId, false, `删除实例失败：${message}`, "浏览器实例", action);
    } finally {
      setBusy(false);
    }
  };

  const inputCls =
    "w-full rounded-md border border-(--border) bg-(--input-bg) px-2.5 py-1.5 text-sm text-(--text-main) outline-none placeholder:text-(--text-faint)";
  const labelCls = "mb-1 block text-xs text-(--text-dim)";

  return (
    <div className="mx-auto max-w-5xl px-6 py-6">
      <div className="flex items-center justify-between">
        <p className="text-sm text-(--text-dim)">
          管理浏览器实例的元数据。启动、停止浏览器进程将在后续阶段提供。
        </p>
        <button
          type="button"
          onClick={() => {
            setEditing(null);
            setForm(EMPTY_FORM);
          }}
          className="flex h-9 shrink-0 items-center gap-1.5 rounded-md bg-(--accent) px-3 text-sm font-medium text-(--accent-contrast) hover:opacity-90"
        >
          <Plus size={14} /> 新建实例
        </button>
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
                    <span title={inst.kernelExecutablePath || undefined}>
                      {inst.kernelName
                        ? `${inst.kernelName}${inst.kernelVersion ? ` ${inst.kernelVersion}` : ""}`
                        : "未指定"}
                    </span>
                  </td>
                  <td className="px-4 py-2.5 font-mono text-xs text-(--text-faint)">{inst.userDataDir}</td>
                  <td className="px-4 py-2.5">
                    <div className="flex justify-end gap-1">
                      <button
                        type="button"
                        title="编辑"
                        onClick={() => {
                          setEditing(inst);
                          setForm({
                            id: inst.id,
                            name: inst.name,
                            startUrl: inst.startUrl,
                            // A kernelId that no longer resolves
                            // to a registration is shown as
                            // unassociated (see the note in the
                            // form); saving then clears it.
                            browserId: browsers.some((b) => b.id === inst.kernelId)
                              ? inst.kernelId
                              : "",
                          });
                        }}
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
                <label className={labelCls}>起始页（可选）</label>
                <input
                  className={inputCls}
                  value={form.startUrl}
                  onChange={(e) => setForm({ ...form, startUrl: e.target.value })}
                  placeholder="https://"
                />
              </div>
              <div>
                <label className={labelCls}>浏览器内核（可选）</label>
                <select
                  className={inputCls}
                  value={form.browserId}
                  onChange={(e) => setForm({ ...form, browserId: e.target.value })}
                >
                  <option value="">不选择内核</option>
                  {browsers.map((b) => (
                    <option key={b.id} value={b.id}>
                      {b.name} — {b.executablePath}
                    </option>
                  ))}
                </select>
                {browsersError ? (
                  <p className="mt-1 text-xs text-(--danger)">内核列表加载失败：{browsersError}</p>
                ) : browsers.length === 0 ? (
                  <p className="mt-1 text-xs text-(--text-faint)">
                    尚未登记任何浏览器内核。
                    <button
                      type="button"
                      className="text-(--accent) underline underline-offset-2"
                      onClick={() => {
                        setForm(null);
                        setEditing(null);
                        setPage("kernels");
                      }}
                    >
                      前往「浏览器内核」页面添加
                    </button>
                  </p>
                ) : null}
                {editing && editing.kernelId !== "" && !browsers.some((b) => b.id === editing.kernelId) ? (
                  <p className="mt-1 text-xs text-(--text-faint)">
                    原关联的内核登记已不存在，当前按未关联处理；保存后将清除原关联。
                  </p>
                ) : editing && editing.kernelId === "" && (editing.kernelName || editing.kernelVersion) ? (
                  <p className="mt-1 text-xs text-(--text-faint)">
                    该实例保存有历史内核记录：{editing.kernelName}
                    {editing.kernelVersion ? ` ${editing.kernelVersion}` : ""}
                    （未关联登记内核）。不选择内核时此记录会保留；选择上方内核后将改为关联该登记。
                  </p>
                ) : null}
              </div>
            </div>
            <div className="mt-5 flex justify-end gap-2">
              <button
                type="button"
                onClick={() => {
                  setForm(null);
                  setEditing(null);
                }}
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
                className="rounded-md bg-(--danger) px-3 py-1.5 text-sm font-medium text-(--danger-contrast) hover:opacity-90 disabled:opacity-50"
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
