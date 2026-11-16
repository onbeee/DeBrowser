import { useCallback, useEffect, useState } from "react";
import { AppWindow, Pencil, Play, Plus, Square, Trash2, TriangleAlert } from "lucide-react";
import {
  BrowserService,
  InstanceService,
  type BrowserView,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import type { Instance, ProfileDirInfo } from "../../bindings/github.com/onbeee/DeBrowser/internal/model";
import { useAppStore } from "../stores/useAppStore";
import { newOperationId, recordOperationFeedback } from "../lib/operations";

const STATUS_LABEL: Record<string, string> = {
  stopped: "已关闭",
  starting: "启动中",
  running: "运行中",
  stopping: "关闭中",
  error: "异常",
  unknown: "无法确认",
};

// Statuses in which the instance's browser may be alive: kernel
// and instance-file changes, and deletion, are refused (the
// backend enforces the same gates).
const ACTIVE_STATUSES = new Set(["running", "starting", "stopping", "unknown"]);

function profileFolderOf(inst: Instance): string {
  return inst.userDataDir.startsWith("profiles/")
    ? inst.userDataDir.slice("profiles/".length)
    : inst.userDataDir;
}

interface FormState {
  id?: string;
  name: string;
  // browserId is the ID of one registered browser (「浏览器内核」
  // page) associated with the instance. The options come only
  // from actual registrations — never from free text — so an
  // association always resolves to a real registered path.
  // Required when creating; on edit, "" clears an association.
  browserId: string;
  // Create: "default" | "custom" | "existing".
  // Edit: "rename" | "swap" (no default option on edit).
  profileMode: string;
  profileName: string;
}

const EMPTY_FORM: FormState = { name: "", browserId: "", profileMode: "default", profileName: "" };

type PendingDelete = { instance: Instance; withProfile: boolean } | null;

/**
 * 浏览器实例管理（Stage 9B）：实例的新建、编辑、删除与真实运行。
 * 每个实例绑定一个独立的实例文件（data/profiles 下的文件夹），
 * 启动时以该文件夹作为浏览器的数据目录。删除分两种且含义明确
 * 区分：「仅删除实例」保留实例文件；「删除实例及其实例文件」才
 * 会删除磁盘上的浏览器数据，且必须由用户明确选择。
 */
export default function InstancesPage() {
  const log = useAppStore((state) => state.log);
  const setPage = useAppStore((state) => state.setPage);
  const [instances, setInstances] = useState<Instance[]>([]);
  const [browsers, setBrowsers] = useState<BrowserView[]>([]);
  // browsersLoaded is true only after ListBrowsers has succeeded.
  // While it is false (still loading, or the load failed), saving
  // is disabled: judging an association against a not-yet-loaded
  // list would mistake it for a missing registration, and saving
  // would then clear the association in the database.
  const [browsersLoaded, setBrowsersLoaded] = useState(false);
  const [browsersError, setBrowsersError] = useState<string | null>(null);
  const [availableProfiles, setAvailableProfiles] = useState<ProfileDirInfo[]>([]);
  const [editing, setEditing] = useState<Instance | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [form, setForm] = useState<FormState | null>(null);
  const [pendingDelete, setPendingDelete] = useState<PendingDelete>(null);
  const [busy, setBusy] = useState(false);
  const [actingId, setActingId] = useState<string | null>(null);

  const refreshInstances = useCallback(async () => {
    try {
      const list = await InstanceService.ListInstances();
      setInstances(list ?? []);
    } catch {
      // A failed background refresh keeps the last known list;
      // the next poll or action retries.
    }
  }, []);

  const load = useCallback(async () => {
    // Close the editing gate FIRST, before any await: on a reload,
    // a stale browsersLoaded=true from the previous load must not
    // keep editing enabled while the fresh lists are in flight.
    // Only a successful ListBrowsers below re-opens it.
    setBrowsersLoaded(false);
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
      setBrowsersLoaded(true);
    } catch (err) {
      // A failed load is NOT "no registrations": browsersLoaded
      // stays false, editing stays disabled, and nothing about
      // existing associations is inferred or touched.
      setBrowsersError(err instanceof Error ? err.message : String(err));
    }
  }, [log]);

  useEffect(() => {
    void load();
  }, [load]);

  // Keep run states fresh while the page is open (a browser the
  // user closed directly must flip to 已关闭 without a reload).
  useEffect(() => {
    const timer = setInterval(() => void refreshInstances(), 2500);
    return () => clearInterval(timer);
  }, [refreshInstances]);

  const loadAvailableProfiles = useCallback(async () => {
    try {
      const list = await InstanceService.ListAvailableProfiles();
      setAvailableProfiles(list ?? []);
    } catch {
      setAvailableProfiles([]);
    }
  }, []);

  const openCreate = () => {
    setEditing(null);
    setForm(EMPTY_FORM);
    void loadAvailableProfiles();
  };

  const openEdit = (inst: Instance) => {
    setEditing(inst);
    setForm({
      id: inst.id,
      name: inst.name,
      // A kernelId that no longer resolves to a registration is
      // shown as unassociated (see the note in the form); saving
      // then clears it.
      browserId: browsers.some((b) => b.id === inst.kernelId) ? inst.kernelId : "",
      profileMode: "rename",
      profileName: profileFolderOf(inst),
    });
    void loadAvailableProfiles();
  };

  const submitForm = async () => {
    if (!form) return;
    if (!browsersLoaded) {
      // Never let a save carry a browserId that was derived from
      // an incompletely loaded registration list.
      setError("内核列表尚未加载完成，无法保存。请等待加载完成或重新加载页面。");
      return;
    }
    if (!form.id && !form.browserId) {
      setError("请选择实例内核。");
      return;
    }
    setBusy(true);
    const opId = newOperationId();
    const action = form.id ? "instance.update" : "instance.create";
    try {
      if (form.id && editing) {
        // Translate the form's instance-file choice into a backend
        // action only when it actually changes something.
        let profileAction = "";
        let profileName = "";
        const currentFolder = profileFolderOf(editing);
        if (form.profileMode === "rename" && form.profileName.trim() !== currentFolder) {
          profileAction = "rename";
          profileName = form.profileName;
        } else if (form.profileMode === "swap" && form.profileName) {
          profileAction = "swap";
          profileName = form.profileName;
        }
        await InstanceService.UpdateInstance(opId, {
          id: form.id,
          name: form.name,
          browserId: form.browserId,
          sortOrder: editing.sortOrder,
          profileAction,
          profileName,
        });
        log("info", `实例已更新：${form.name}`);
        recordOperationFeedback(opId, true, `实例已更新：${form.name}`, "浏览器实例", action);
      } else {
        const created = await InstanceService.CreateInstance(opId, {
          name: form.name,
          browserId: form.browserId,
          profileMode: form.profileMode,
          profileName: form.profileMode === "default" ? "" : form.profileName,
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

  const runAction = async (inst: Instance, kind: "start" | "stop") => {
    setActingId(inst.id);
    const opId = newOperationId();
    const action = kind === "start" ? "instance.start" : "instance.stop";
    try {
      if (kind === "start") {
        await InstanceService.StartInstance(opId, inst.id);
        log("info", `实例已启动：${inst.name}`);
        recordOperationFeedback(opId, true, `实例已启动：${inst.name}`, "浏览器实例", action);
      } else {
        await InstanceService.StopInstance(opId, inst.id);
        log("info", `实例已关闭：${inst.name}`);
        recordOperationFeedback(opId, true, `实例已关闭：${inst.name}`, "浏览器实例", action);
      }
      setError(null);
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setError(message);
      log("error", `${kind === "start" ? "启动" : "关闭"}实例失败：${message}`);
      recordOperationFeedback(opId, false, message, "浏览器实例", action);
    } finally {
      setActingId(null);
      await refreshInstances();
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
        log("info", `实例及其实例文件已删除：${pendingDelete.instance.name}`);
        recordOperationFeedback(opId, true, `实例及其实例文件已删除：${pendingDelete.instance.name}`, "浏览器实例", action);
      } else {
        await InstanceService.DeleteInstance(opId, pendingDelete.instance.id);
        log("info", `实例已删除（实例文件保留）：${pendingDelete.instance.name}`);
        recordOperationFeedback(opId, true, `实例已删除（实例文件保留）：${pendingDelete.instance.name}`, "浏览器实例", action);
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
    "w-full rounded-md border border-(--border) bg-(--input-bg) px-2.5 py-1.5 text-sm text-(--text-main) outline-none placeholder:text-(--text-faint) disabled:opacity-50";
  const labelCls = "mb-1 block text-xs text-(--text-dim)";
  const radioCls = "flex items-center gap-2 text-sm text-(--text-main)";
  const editingActive = editing ? ACTIVE_STATUSES.has(editing.status) : false;
  const editingRunning = editing ? ["running", "starting", "stopping"].includes(editing.status) : false;
  const editingUnknown = editing?.status === "unknown";

  return (
    <div className="mx-auto max-w-5xl px-6 py-6">
      <div className="flex items-center justify-between">
        <p className="text-sm text-(--text-dim)">
          每个实例使用独立的实例文件，可独立启动、运行与关闭。
        </p>
        <button
          type="button"
          onClick={openCreate}
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

      {browsersError && (
        <div className="mt-4 flex items-start gap-2 rounded-lg border border-(--danger-border) bg-(--danger-bg) px-3 py-2 text-sm text-(--danger)">
          <TriangleAlert size={16} className="mt-0.5 shrink-0" />
          <span className="flex-1">
            内核列表加载失败：{browsersError}。保存功能已暂停，以免误清除实例的内核关联。
          </span>
          <button
            type="button"
            onClick={() => void load()}
            className="shrink-0 font-medium underline underline-offset-2"
          >
            重新加载
          </button>
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
                <th className="px-4 py-2.5 font-medium">实例名称</th>
                <th className="px-4 py-2.5 font-medium">代理节点</th>
                <th className="px-4 py-2.5 font-medium">实例内核</th>
                <th className="px-4 py-2.5 font-medium">状态</th>
                <th className="px-4 py-2.5 text-right font-medium">操作</th>
              </tr>
            </thead>
            <tbody>
              {instances.map((inst) => {
                const active = ACTIVE_STATUSES.has(inst.status);
                const transitioning = inst.status === "starting" || inst.status === "stopping";
                return (
                  <tr key={inst.id} className="border-b border-(--border) last:border-0 hover:bg-(--hover-bg)">
                    <td className="px-4 py-2.5 font-medium">{inst.name}</td>
                    <td className="px-4 py-2.5 text-(--text-dim)">
                      {inst.proxyId ? "已设置（历史关联）" : "未设置"}
                    </td>
                    <td className="px-4 py-2.5 text-(--text-dim)">
                      <span title={inst.kernelExecutablePath || undefined}>
                        {inst.kernelName
                          ? `${inst.kernelName}${inst.kernelVersion ? ` ${inst.kernelVersion}` : ""}`
                          : "未指定"}
                      </span>
                    </td>
                    <td className="px-4 py-2.5">
                      <span className="rounded-full bg-(--badge-bg) px-2 py-0.5 text-xs text-(--text-dim)">
                        {STATUS_LABEL[inst.status] ?? inst.status}
                      </span>
                    </td>
                    <td className="px-4 py-2.5">
                      <div className="flex justify-end gap-1">
                        {inst.status === "running" || inst.status === "stopping" ? (
                          <button
                            type="button"
                            title="关闭"
                            disabled={transitioning || actingId === inst.id || inst.status === "stopping"}
                            onClick={() => void runAction(inst, "stop")}
                            className="flex items-center gap-1 rounded px-2 py-1.5 text-xs text-(--text-dim) hover:bg-(--hover-bg) disabled:cursor-not-allowed disabled:opacity-40"
                          >
                            <Square size={13} /> 关闭
                          </button>
                        ) : (
                          <button
                            type="button"
                            title="启动"
                            disabled={active || transitioning || actingId === inst.id}
                            onClick={() => void runAction(inst, "start")}
                            className="flex items-center gap-1 rounded px-2 py-1.5 text-xs text-(--text-dim) hover:bg-(--hover-bg) disabled:cursor-not-allowed disabled:opacity-40"
                          >
                            <Play size={13} /> 启动
                          </button>
                        )}
                        <button
                          type="button"
                          title={browsersLoaded ? "编辑" : "内核列表加载完成后可编辑"}
                          disabled={!browsersLoaded || transitioning}
                          className="rounded p-1.5 text-(--text-dim) hover:bg-(--hover-bg) disabled:cursor-not-allowed disabled:opacity-40"
                          onClick={() => openEdit(inst)}
                        >
                          <Pencil size={14} />
                        </button>
                        <button
                          type="button"
                          title={active ? "实例运行期间不能删除" : "删除实例"}
                          disabled={active}
                          onClick={() => setPendingDelete({ instance: inst, withProfile: false })}
                          className="rounded p-1.5 text-(--text-dim) hover:bg-(--hover-bg) disabled:cursor-not-allowed disabled:opacity-40"
                        >
                          <Trash2 size={14} />
                        </button>
                      </div>
                    </td>
                  </tr>
                );
              })}
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
                <label className={labelCls}>实例名称</label>
                <input
                  className={inputCls}
                  value={form.name}
                  onChange={(e) => setForm({ ...form, name: e.target.value })}
                  placeholder="例如：工作号"
                />
              </div>
              <div>
                <label className={labelCls}>实例内核{form.id ? "" : "（必选）"}</label>
                <select
                  className={inputCls}
                  value={form.browserId}
                  disabled={!browsersLoaded || editingRunning}
                  onChange={(e) => setForm({ ...form, browserId: e.target.value })}
                >
                  <option value="">{form.id ? "不选择内核" : "请选择内核"}</option>
                  {browsers.map((b) => (
                    <option key={b.id} value={b.id}>
                      {b.name} — {b.executablePath}
                    </option>
                  ))}
                </select>
                {browsersError ? (
                  <p className="mt-1 text-xs text-(--danger)">内核列表加载失败：{browsersError}</p>
                ) : !browsersLoaded ? (
                  <p className="mt-1 text-xs text-(--text-faint)">内核列表加载中…</p>
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
              <div>
                <label className={labelCls}>代理节点</label>
                <select className={inputCls} value="" disabled>
                  <option value="">未设置（代理节点功能暂未开放）</option>
                </select>
                <p className="mt-1 text-xs text-(--text-faint)">不设置代理节点时，实例可正常启动和运行。</p>
              </div>
              <div>
                <label className={labelCls}>实例文件</label>
                {!form.id ? (
                  <div className="space-y-2">
                    <label className={radioCls}>
                      <input
                        type="radio"
                        name="profile-mode"
                        checked={form.profileMode === "default"}
                        onChange={() => setForm({ ...form, profileMode: "default", profileName: "" })}
                      />
                      默认（自动分配）
                    </label>
                    {form.profileMode === "default" && (
                      <p className="pl-6 text-xs text-(--text-faint)">
                        按顺序自动分配实例文件：P-1、P-2、P-3……已分配过的编号不会重复使用。
                      </p>
                    )}
                    <label className={radioCls}>
                      <input
                        type="radio"
                        name="profile-mode"
                        checked={form.profileMode === "custom"}
                        onChange={() => setForm({ ...form, profileMode: "custom", profileName: "" })}
                      />
                      自定（新建实例文件）
                    </label>
                    {form.profileMode === "custom" && (
                      <input
                        className={inputCls}
                        value={form.profileName}
                        onChange={(e) => setForm({ ...form, profileName: e.target.value })}
                        placeholder="实例文件名称，例如：工作账号"
                      />
                    )}
                    <label className={radioCls}>
                      <input
                        type="radio"
                        name="profile-mode"
                        checked={form.profileMode === "existing"}
                        onChange={() => setForm({ ...form, profileMode: "existing", profileName: "" })}
                      />
                      选择（使用保留的实例文件）
                    </label>
                    {form.profileMode === "existing" &&
                      (availableProfiles.length === 0 ? (
                        <p className="pl-6 text-xs text-(--text-faint)">暂无可选择的实例文件。</p>
                      ) : (
                        <select
                          className={inputCls}
                          value={form.profileName}
                          onChange={(e) => setForm({ ...form, profileName: e.target.value })}
                        >
                          <option value="">请选择实例文件</option>
                          {availableProfiles.map((p) => (
                            <option key={p.name} value={p.name}>
                              {p.name} — {p.path}
                            </option>
                          ))}
                        </select>
                      ))}
                  </div>
                ) : (
                  <div className="space-y-2">
                    <p className="text-xs text-(--text-faint)">
                      当前实例文件：{editing ? profileFolderOf(editing) : ""}
                      <span className="font-mono">（{editing?.userDataDir}）</span>
                    </p>
                    <label className={radioCls}>
                      <input
                        type="radio"
                        name="profile-mode-edit"
                        checked={form.profileMode === "rename"}
                        disabled={editingActive}
                        onChange={() =>
                          setForm({ ...form, profileMode: "rename", profileName: editing ? profileFolderOf(editing) : "" })
                        }
                      />
                      自定（重命名当前实例文件）
                    </label>
                    {form.profileMode === "rename" && (
                      <input
                        className={inputCls}
                        value={form.profileName}
                        disabled={editingActive}
                        onChange={(e) => setForm({ ...form, profileName: e.target.value })}
                        placeholder="新的实例文件名称"
                      />
                    )}
                    <label className={radioCls}>
                      <input
                        type="radio"
                        name="profile-mode-edit"
                        checked={form.profileMode === "swap"}
                        disabled={editingActive}
                        onChange={() => setForm({ ...form, profileMode: "swap", profileName: "" })}
                      />
                      选择（更换为保留的实例文件）
                    </label>
                    {form.profileMode === "swap" &&
                      (availableProfiles.length === 0 ? (
                        <p className="pl-6 text-xs text-(--text-faint)">暂无可选择的实例文件。</p>
                      ) : (
                        <select
                          className={inputCls}
                          value={form.profileName}
                          disabled={editingActive}
                          onChange={(e) => setForm({ ...form, profileName: e.target.value })}
                        >
                          <option value="">请选择要更换的实例文件</option>
                          {availableProfiles.map((p) => (
                            <option key={p.name} value={p.name}>
                              {p.name} — {p.path}
                            </option>
                          ))}
                        </select>
                      ))}
                    {editingActive && (
                      <p className="text-xs text-(--text-faint)">
                        {editingUnknown
                          ? "实例运行状态无法确认时，不能更换实例文件；实例名称与内核可以修改。"
                          : "实例运行中，不能更换内核或实例文件。"}
                      </p>
                    )}
                  </div>
                )}
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
                将同时删除实例记录和实例文件
                <span className="font-mono text-xs"> {pendingDelete.instance.userDataDir} </span>
                中的全部浏览器数据（Cookie、登录状态、历史记录等），此操作不可恢复。
              </p>
            ) : (
              <>
                <p className="mt-2 text-sm leading-relaxed text-(--text-dim)">
                  仅删除实例记录，实例文件
                  <span className="font-mono text-xs"> {pendingDelete.instance.userDataDir} </span>
                  会保留在磁盘上，之后可以重新绑定到其他实例。
                </p>
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => setPendingDelete({ ...pendingDelete, withProfile: true })}
                  className="mt-3 text-sm text-(--danger) underline underline-offset-2"
                >
                  改为同时删除实例文件
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
                {pendingDelete.withProfile ? "确认删除实例及实例文件" : "确认仅删除实例"}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
