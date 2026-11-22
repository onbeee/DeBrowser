import { useCallback, useEffect, useMemo, useState } from "react";
import {
  AppWindow,
  ChevronDown,
  Layers,
  Pencil,
  Play,
  Plus,
  Square,
  Trash2,
  TriangleAlert,
} from "lucide-react";
import {
  BrowserService,
  InstanceService,
  ProxyService,
  type BindingView,
  type BrowserView,
  type NodeView,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import type { Instance, ProfileDirInfo } from "../../bindings/github.com/onbeee/DeBrowser/internal/model";
import { useAppStore } from "../stores/useAppStore";
import { notify, notifyError, notifySuccess } from "../stores/useNotificationStore";
import { classifyCounts } from "../lib/operationResult";
import type { NotificationItem } from "../lib/notifications";
import { newOperationId, recordOperationFeedback } from "../lib/operations";
import {
  kernelOptionLabels,
  planBatch,
  planBatchCreate,
  pruneSelection,
  sortInstancesByName,
  MAX_BATCH_CREATE,
  type BatchAction,
  type BatchPlanItem,
} from "./instancesLogic";

const STATUS_LABEL: Record<string, string> = {
  stopped: "已关闭",
  starting: "启动中",
  running: "运行中",
  stopping: "关闭中",
  error: "异常",
  unknown: "无法确认",
};

// Three statuses get a small colored dot plus matching text
// color, using the shared theme tokens (gray / ok-green /
// danger-red, each already low-saturation per theme). All other
// statuses keep the neutral badge rendering.
const STATUS_DOT: Record<string, { dot: string; text: string }> = {
  stopped: { dot: "bg-(--text-dim)", text: "text-(--text-dim)" },
  running: { dot: "bg-(--ok)", text: "text-(--ok)" },
  unknown: { dot: "bg-(--danger)", text: "text-(--danger)" },
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
  // proxyNodeId is the ID of the proxy node this instance is
  // bound to ("" = 不使用代理). The binding itself lives in
  // the proxy module's single binding table: this form and the
  // proxy page read and write the same relation through
  // ProxyService.
  proxyNodeId: string;
}

const EMPTY_FORM: FormState = { name: "", browserId: "", profileMode: "default", profileName: "", proxyNodeId: "" };

type PendingDelete = { instance: Instance; withProfile: boolean } | null;

// The outcome of one batch operation goes to the global
// notification system as ONE summary notification; the
// per-item results (succeeded / skipped with reason / failed
// with reason) ride along as expandable items in the
// notification center. Partial success is reported as partial.
function notifyBatchResult(
  title: string,
  okNames: string[],
  skipped: { name: string; reason: string }[],
  failed: { name: string; reason: string }[],
): void {
  const items: NotificationItem[] = [
    ...okNames.map((name) => ({ name, status: "success" as const })),
    ...skipped.map((it) => ({ name: it.name, status: "skipped" as const, reason: it.reason })),
    ...failed.map((it) => ({ name: it.name, status: "failed" as const, reason: it.reason })),
  ];
  const summary =
    `${title}：成功 ${okNames.length} 个` +
    (skipped.length > 0 ? `，跳过 ${skipped.length} 个` : "") +
    (failed.length > 0 ? `，失败 ${failed.length} 个` : "");
  const kind = classifyCounts(okNames.length, failed.length, skipped.length);
  if (kind !== null) notify(kind, summary, { items, important: true });
}

interface BatchCreateState {
  prefix: string;
  count: string;
  browserId: string;
}

// The instance-file picker dialog: the options are fetched fresh
// at open time (never the form-open cache), exactly one may be
// picked, and nothing is applied until 「选择」 confirms.
interface ProfilePickerState {
  options: ProfileDirInfo[];
  picked: string;
}

const BATCH_VERB: Record<BatchAction, string> = { start: "启动", stop: "关闭", delete: "删除" };
const BATCH_OP_ACTION: Record<BatchAction, string> = {
  start: "instance.start",
  stop: "instance.stop",
  delete: "instance.delete",
};

/**
 * 浏览器实例管理（Stage 9B）：实例的新建、编辑、删除与真实运行。
 * 每个实例绑定一个独立的实例文件（data/profiles 下的文件夹），
 * 启动时以该文件夹作为浏览器的数据目录。删除分两种且含义明确
 * 区分：「仅删除实例」保留实例文件；「删除实例及其实例文件」才
 * 会删除磁盘上的浏览器数据，且必须由用户明确选择。
 *
 * 列表按实例名称自然排序显示；批量启动/关闭/删除与批量创建都
 * 逐项复用单实例操作与其安全门禁，逐项记录结果，部分失败如实
 * 汇报，不引入额外的任务调度机制。
 */
export default function InstancesPage() {
  const setPage = useAppStore((state) => state.setPage);
  const [instances, setInstances] = useState<Instance[]>([]);
  const [instancesError, setInstancesError] = useState<string | null>(null);
  const [browsers, setBrowsers] = useState<BrowserView[]>([]);
  // browsersLoaded is true only after ListBrowsers has succeeded.
  // While it is false (still loading, or the load failed), saving
  // is disabled: judging an association against a not-yet-loaded
  // list would mistake it for a missing registration, and saving
  // would then clear the association in the database.
  const [browsersLoaded, setBrowsersLoaded] = useState(false);
  const [browsersError, setBrowsersError] = useState<string | null>(null);
  const [editing, setEditing] = useState<Instance | null>(null);
  const [loading, setLoading] = useState(true);
  const [formError, setFormError] = useState<string | null>(null);
  const [form, setForm] = useState<FormState | null>(null);
  const [pendingDelete, setPendingDelete] = useState<PendingDelete>(null);
  const [busy, setBusy] = useState(false);
  const [actingId, setActingId] = useState<string | null>(null);
  // Batch management state.
  const [selectedIds, setSelectedIds] = useState<Set<string>>(new Set());
  const [batchBusy, setBatchBusy] = useState(false);
  const [batchDeletePlan, setBatchDeletePlan] = useState<BatchPlanItem[] | null>(null);
  const [batchCreate, setBatchCreate] = useState<BatchCreateState | null>(null);
  const [batchCreateError, setBatchCreateError] = useState<string | null>(null);
  const [batchCreateBusy, setBatchCreateBusy] = useState(false);
  // Proxy binding display + form data (from the proxy module's
  // single binding relation; see FormState.proxyNodeId).
  const [proxyNodes, setProxyNodes] = useState<NodeView[]>([]);
  const [proxyBindings, setProxyBindings] = useState<BindingView[]>([]);
  // Instance-file picker state (inside the create/edit form).
  const [profilePicker, setProfilePicker] = useState<ProfilePickerState | null>(null);
  const [pickerBusy, setPickerBusy] = useState(false);
  const [profileNote, setProfileNote] = useState<string | null>(null);

  // The list is displayed in natural name order; selection and
  // select-all always refer to exactly this displayed list.
  const displayInstances = useMemo(() => sortInstancesByName(instances), [instances]);
  const kernelLabels = useMemo(() => kernelOptionLabels(browsers), [browsers]);

  const refreshInstances = useCallback(async () => {
    try {
      const list = await InstanceService.ListInstances();
      setInstances(list ?? []);
    } catch {
      // A failed background refresh keeps the last known list;
      // the next poll or action retries.
    }
    // Bindings changed on the proxy page show up here too:
    // both pages read the same relation.
    try {
      const bds = await ProxyService.ListBindings();
      setProxyBindings(bds ?? []);
    } catch {
      // Keep the last known bindings on failure.
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
      setInstancesError(null);
    } catch (err) {
      // 读取失败与真实空列表严格区分：保留失败原因供页面
      // 显示加载失败状态（不伪装成暂无实例），同时发一条
      // 通知台错误。已有列表时不清空。
      const message = err instanceof Error ? err.message : String(err);
      setInstancesError(message);
      notifyError(`加载实例列表失败：${message}`);
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
    // Proxy nodes + bindings for the table column and the form
    // selector. A failure here only empties the proxy display;
    // it never touches instance or kernel data.
    try {
      const [nds, bds] = await Promise.all([ProxyService.ListNodes(), ProxyService.ListBindings()]);
      setProxyNodes(nds ?? []);
      setProxyBindings(bds ?? []);
    } catch {
      // Keep the last known proxy data.
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  // Keep run states fresh while the page is open (a browser the
  // user closed directly must flip to 已关闭 without a reload).
  useEffect(() => {
    const timer = setInterval(() => void refreshInstances(), 2500);
    return () => clearInterval(timer);
  }, [refreshInstances]);

  // Selection never outlives the displayed list: ids that vanish
  // (deleted instances, reloads) are dropped from the selection.
  useEffect(() => {
    setSelectedIds((prev) => {
      const next = pruneSelection(
        prev,
        instances.map((i) => i.id),
      );
      return next.size === prev.size ? prev : next;
    });
  }, [instances]);

  const openCreate = () => {
    setEditing(null);
    setForm(EMPTY_FORM);
    setFormError(null);
    setProfileNote(null);
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
      // The current binding from the shared relation; a binding
      // whose node has vanished cannot occur (the database
      // cascades), so a missing entry simply means unbound.
      proxyNodeId: proxyBindings.find((b) => b.instanceId === inst.id)?.nodeId ?? "",
    });
    setFormError(null);
    setProfileNote(null);
  };

  const submitForm = async () => {
    if (!form) return;
    if (!browsersLoaded) {
      // Never let a save carry a browserId that was derived from
      // an incompletely loaded registration list.
      setFormError("内核列表尚未加载完成，无法保存。请等待加载完成或重新加载页面。");
      return;
    }
    if (!form.id && !form.browserId) {
      setFormError("请选择实例内核。");
      return;
    }
    if (!form.id && form.profileMode === "existing" && !form.profileName) {
      setFormError("请选择实例文件。");
      return;
    }
    if (form.id && form.profileMode === "swap" && !form.profileName) {
      setFormError("请选择要更换的实例文件。");
      return;
    }
    setFormError(null);
    setBusy(true);
    const opId = newOperationId();
    const action = form.id ? "instance.update" : "instance.create";
    let savedInstanceID = "";
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
        notifySuccess(`实例已更新：${form.name}`, { operationId: opId, important: true });
        recordOperationFeedback(opId, true, `实例已更新：${form.name}`, "浏览器实例", action);
        savedInstanceID = form.id;
      } else {
        const created = await InstanceService.CreateInstance(opId, {
          name: form.name,
          browserId: form.browserId,
          profileMode: form.profileMode,
          profileName: form.profileMode === "default" ? "" : form.profileName,
        });
        notifySuccess(`实例已创建：${created.name}`, { operationId: opId, important: true });
        recordOperationFeedback(opId, true, `实例已创建：${created.name}`, "浏览器实例", action);
        savedInstanceID = created.id;
      }
      // Sync the proxy binding to the form's choice — the same
      // single relation the proxy page writes. The instance
      // itself is already saved; a binding failure is reported
      // separately and never rolls the instance back.
      const currentNodeID = proxyBindings.find((b) => b.instanceId === savedInstanceID)?.nodeId ?? "";
      if (savedInstanceID && form.proxyNodeId !== currentNodeID) {
        const bindOpId = newOperationId();
        try {
          if (form.proxyNodeId === "") {
            await ProxyService.ClearBinding(bindOpId, savedInstanceID);
          } else {
            await ProxyService.SetBinding(bindOpId, savedInstanceID, form.proxyNodeId);
          }
          recordOperationFeedback(bindOpId, true, `实例代理已更新：${form.name}`, "代理", "proxy.binding_set");
        } catch (bindErr) {
          const message = bindErr instanceof Error ? bindErr.message : String(bindErr);
          notifyError(`实例已保存，但代理设置失败：${message}`, { operationId: bindOpId, important: true });
          recordOperationFeedback(bindOpId, false, `实例代理设置失败：${message}`, "代理", "proxy.binding_set");
        }
      }
      setForm(null);
      setEditing(null);
      await load();
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      notifyError(`保存实例失败：${message}`, { operationId: opId, important: true });
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
        notifySuccess(`实例已启动：${inst.name}`, { operationId: opId, important: true });
        recordOperationFeedback(opId, true, `实例已启动：${inst.name}`, "浏览器实例", action);
      } else {
        await InstanceService.StopInstance(opId, inst.id);
        notifySuccess(`实例已关闭：${inst.name}`, { operationId: opId, important: true });
        recordOperationFeedback(opId, true, `实例已关闭：${inst.name}`, "浏览器实例", action);
      }
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      notifyError(`${kind === "start" ? "启动" : "关闭"}实例失败：${message}`, { operationId: opId, important: true });
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
        notifySuccess(`实例及其实例文件已删除：${pendingDelete.instance.name}`, { operationId: opId, important: true });
        recordOperationFeedback(opId, true, `实例及其实例文件已删除：${pendingDelete.instance.name}`, "浏览器实例", action);
      } else {
        await InstanceService.DeleteInstance(opId, pendingDelete.instance.id);
        notifySuccess(`实例已删除（实例文件保留）：${pendingDelete.instance.name}`, { operationId: opId, important: true });
        recordOperationFeedback(opId, true, `实例已删除（实例文件保留）：${pendingDelete.instance.name}`, "浏览器实例", action);
      }
      setPendingDelete(null);
      await load();
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      notifyError(`删除实例失败：${message}`, { operationId: opId, important: true });
      recordOperationFeedback(opId, false, `删除实例失败：${message}`, "浏览器实例", action);
    } finally {
      setBusy(false);
    }
  };

  // --- Batch operations -------------------------------------------------
  // Batch start/stop/delete run the existing single-instance
  // operations sequentially, in displayed order. Eligibility is
  // planned up front (planBatch mirrors the single-instance
  // gates); one failure never stops the remaining items, and the
  // outcome is reported per item when the batch finishes.

  const toggleSelected = (id: string, checked: boolean) => {
    setSelectedIds((prev) => {
      const next = new Set(prev);
      if (checked) next.add(id);
      else next.delete(id);
      return next;
    });
  };

  const toggleSelectAll = (checked: boolean) => {
    setSelectedIds(checked ? new Set(displayInstances.map((i) => i.id)) : new Set());
  };

  const executeBatch = async (action: BatchAction, plan: BatchPlanItem[]) => {
    setBatchBusy(true);
    const verb = BATCH_VERB[action];
    const okNames: string[] = [];
    const failed: { name: string; reason: string }[] = [];
    const skipped = plan
      .filter((p) => !p.run)
      .map((p) => ({ name: p.name, reason: p.reason }));
    for (const item of plan.filter((p) => p.run)) {
      const opId = newOperationId();
      try {
        if (action === "start") await InstanceService.StartInstance(opId, item.id);
        else if (action === "stop") await InstanceService.StopInstance(opId, item.id);
        else await InstanceService.DeleteInstance(opId, item.id);
        okNames.push(item.name);
        const doneMessage =
          action === "delete" ? `实例已删除（实例文件保留）：${item.name}` : `实例已${verb}：${item.name}`;
        recordOperationFeedback(opId, true, doneMessage, "浏览器实例", BATCH_OP_ACTION[action]);
      } catch (err) {
        const message = err instanceof Error ? err.message : String(err);
        failed.push({ name: item.name, reason: message });
        recordOperationFeedback(opId, false, `${verb}实例失败：${message}`, "浏览器实例", BATCH_OP_ACTION[action]);
      }
    }
    await refreshInstances();
    setSelectedIds(new Set());
    notifyBatchResult(`批量${verb}完成`, okNames, skipped, failed);
    setBatchBusy(false);
  };

  const runBatch = (action: BatchAction) => {
    const plan = planBatch(
      action,
      displayInstances.filter((i) => selectedIds.has(i.id)),
    );
    // Deletion always asks first — unless nothing is deletable,
    // in which case the skip reasons are the whole answer.
    if (action === "delete" && plan.some((p) => p.run)) {
      setBatchDeletePlan(plan);
      return;
    }
    void executeBatch(action, plan);
  };

  // --- Batch create -------------------------------------------------------

  const openBatchCreate = () => {
    setBatchCreate({ prefix: "", count: "1", browserId: "" });
    setBatchCreateError(null);
  };

  const submitBatchCreate = async () => {
    if (!batchCreate) return;
    if (!browsersLoaded) {
      setBatchCreateError("内核列表尚未加载完成，无法创建。请等待加载完成或重新加载页面。");
      return;
    }
    if (!batchCreate.browserId) {
      setBatchCreateError("请选择实例内核。");
      return;
    }
    setBatchCreateBusy(true);
    try {
      // Conflict-check against a fresh list, not the polled
      // cache; the backend still re-checks every single name.
      const fresh = await InstanceService.ListInstances();
      const plan = planBatchCreate(
        batchCreate.prefix,
        Number(batchCreate.count),
        new Set((fresh ?? []).map((i) => i.name)),
      );
      if (!plan.ok) {
        setBatchCreateError(plan.error);
        return;
      }
      const okNames: string[] = [];
      const failed: { name: string; reason: string }[] = [];
      for (const name of plan.names) {
        const opId = newOperationId();
        try {
          const created = await InstanceService.CreateInstance(opId, {
            name,
            browserId: batchCreate.browserId,
            profileMode: "default",
            profileName: "",
          });
          okNames.push(created.name);
          recordOperationFeedback(opId, true, `实例已创建：${created.name}`, "浏览器实例", "instance.create");
        } catch (err) {
          const message = err instanceof Error ? err.message : String(err);
          failed.push({ name, reason: message });
          recordOperationFeedback(opId, false, `创建实例失败：${message}`, "浏览器实例", "instance.create");
        }
      }
      setBatchCreate(null);
      await load();
      notifyBatchResult("批量创建完成", okNames, [], failed);
    } catch (err) {
      setBatchCreateError(err instanceof Error ? err.message : String(err));
    } finally {
      setBatchCreateBusy(false);
    }
  };

  // --- Instance-file picker -------------------------------------------------
  // The picker list is fetched fresh on every open attempt: if no
  // unbound instance file exists right now, the dialog does not
  // open at all and the form keeps its current value.

  const openProfilePicker = async () => {
    if (!form) return;
    setProfileNote(null);
    setPickerBusy(true);
    try {
      const list = await InstanceService.ListAvailableProfiles();
      const options = list ?? [];
      if (options.length === 0) {
        setProfileNote("目前没有未绑定的实例文件。");
        return;
      }
      setProfilePicker({
        options,
        picked: options.some((o) => o.name === form.profileName) ? form.profileName : "",
      });
    } catch (err) {
      setProfileNote(`获取实例文件列表失败：${err instanceof Error ? err.message : String(err)}`);
    } finally {
      setPickerBusy(false);
    }
  };

  const confirmProfilePicker = () => {
    if (!profilePicker || !form || !profilePicker.picked) return;
    setForm({ ...form, profileName: profilePicker.picked });
    setProfilePicker(null);
  };

  const inputCls =
    "w-full rounded-md border border-(--border) bg-(--input-bg) px-2.5 py-1.5 text-sm text-(--text-main) outline-none placeholder:text-(--text-faint) disabled:opacity-50";
  const labelCls = "mb-1 block text-xs text-(--text-dim)";
  const modeGroupCls = "inline-flex overflow-hidden rounded-md border border-(--border-strong)";
  const modeButtonCls = (active: boolean) =>
    `h-8 px-3.5 text-sm transition-colors ${
      active
        ? "bg-(--accent) font-medium text-(--accent-contrast)"
        : "bg-(--input-bg) text-(--text-dim) hover:bg-(--hover-bg)"
    } disabled:cursor-not-allowed disabled:opacity-50`;
  const editingActive = editing ? ACTIVE_STATUSES.has(editing.status) : false;
  const editingRunning = editing ? ["running", "starting", "stopping"].includes(editing.status) : false;
  const editingUnknown = editing?.status === "unknown";
  const allSelected =
    displayInstances.length > 0 && displayInstances.every((i) => selectedIds.has(i.id));
  const batchDeleteRunnable = batchDeletePlan?.filter((p) => p.run) ?? [];
  const batchDeleteSkipped = batchDeletePlan?.filter((p) => !p.run) ?? [];

  // The clickable box shown in the form's 选择 mode: it never
  // expands inline; it opens the picker dialog (or explains why
  // there is nothing to pick).
  const profilePickerBox = (disabled: boolean) => (
    <>
      <button
        type="button"
        disabled={disabled || pickerBusy}
        onClick={() => void openProfilePicker()}
        className={`${inputCls} flex items-center justify-between text-left`}
      >
        <span className={form?.profileName ? "" : "text-(--text-faint)"}>
          {form?.profileName || "点击选择实例文件"}
        </span>
        <ChevronDown size={14} className="shrink-0 text-(--text-faint)" />
      </button>
      {profileNote && <p className="mt-1 text-xs text-(--text-faint)">{profileNote}</p>}
    </>
  );

  return (
    <div className="mx-auto flex h-full max-w-5xl flex-col px-6 py-6">
      {/* 顶部功能栏：常驻显示，按功能组整体换行。创建组
          （新建实例 / 批量创建）与批量操作组（批量启动 / 批量
          关闭 / 批量删除 / 取消选择）各自保持单行不拆分；宽度
          不足时批量操作组整体换到下一行。批量按钮只在存在
          选择时可用，不再使用勾选后弹出的批量操作栏，也不再
          单独显示选择计数。 */}
      <div className="flex shrink-0 flex-wrap items-center gap-x-3 gap-y-2">
        <div className="flex shrink-0 flex-nowrap items-center gap-2">
          <button
            type="button"
            onClick={openCreate}
            className="flex h-9 shrink-0 items-center gap-1.5 rounded-md bg-(--accent) px-3 text-sm font-medium text-(--accent-contrast) hover:opacity-90"
          >
            <Plus size={14} /> 新建实例
          </button>
          <button
            type="button"
            onClick={openBatchCreate}
            className="flex h-9 shrink-0 items-center gap-1.5 rounded-md border border-(--border-strong) px-3 text-sm text-(--text-main) hover:bg-(--hover-bg)"
          >
            <Layers size={14} /> 批量创建
          </button>
        </div>
        <div className="flex shrink-0 flex-nowrap items-center gap-2">
          <button
            type="button"
            disabled={batchBusy || selectedIds.size === 0}
            onClick={() => runBatch("start")}
            className="flex h-9 shrink-0 items-center rounded-md border border-(--border-strong) px-3 text-sm text-(--text-main) hover:bg-(--hover-bg) disabled:cursor-not-allowed disabled:opacity-40"
          >
            批量启动
          </button>
          <button
            type="button"
            disabled={batchBusy || selectedIds.size === 0}
            onClick={() => runBatch("stop")}
            className="flex h-9 shrink-0 items-center rounded-md border border-(--border-strong) px-3 text-sm text-(--text-main) hover:bg-(--hover-bg) disabled:cursor-not-allowed disabled:opacity-40"
          >
            批量关闭
          </button>
          <button
            type="button"
            disabled={batchBusy || selectedIds.size === 0}
            onClick={() => runBatch("delete")}
            className="flex h-9 shrink-0 items-center rounded-md border border-(--border-strong) px-3 text-sm text-(--danger) hover:bg-(--hover-bg) disabled:cursor-not-allowed disabled:opacity-40"
          >
            批量删除
          </button>
          <button
            type="button"
            disabled={batchBusy || selectedIds.size === 0}
            onClick={() => setSelectedIds(new Set())}
            className="flex h-9 shrink-0 items-center rounded-md border border-(--border-strong) px-3 text-sm text-(--text-main) hover:bg-(--hover-bg) disabled:cursor-not-allowed disabled:opacity-40"
          >
            取消选择
          </button>
        </div>
      </div>

      {browsersError && (
        <div className="mt-4 flex shrink-0 items-start gap-2 rounded-lg border border-(--danger-border) bg-(--danger-bg) px-3 py-2 text-sm text-(--danger)">
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


      <div className="mt-5 min-h-0 flex-1 overflow-y-auto rounded-lg border border-(--border) bg-(--panel-bg)">
        {loading ? (
          <p className="px-4 py-10 text-center text-sm text-(--text-faint)">加载中…</p>
        ) : instancesError && instances.length === 0 ? (
          <div className="flex flex-col items-center px-4 py-12 text-center">
            <p className="text-sm text-(--danger)">实例列表加载失败：{instancesError}</p>
            <button
              type="button"
              onClick={() => void load()}
              className="mt-3 rounded-md border border-(--border-strong) bg-(--input-bg) px-3 py-1.5 text-sm text-(--text-main) hover:bg-(--hover-bg)"
            >
              重新加载
            </button>
          </div>
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
              <tr className="text-left text-xs text-(--text-faint)">
                <th className="sticky top-0 z-[1] w-10 border-b border-(--border) bg-(--panel-bg) px-2 py-2.5 text-center">
                  <input
                    type="checkbox"
                    aria-label="全选"
                    className="h-4 w-4 accent-(--accent)"
                    checked={allSelected}
                    disabled={batchBusy}
                    ref={(el) => {
                      if (el) el.indeterminate = !allSelected && selectedIds.size > 0;
                    }}
                    onChange={(e) => toggleSelectAll(e.target.checked)}
                  />
                </th>
                <th className="sticky top-0 z-[1] border-b border-(--border) bg-(--panel-bg) px-4 py-2.5 font-medium">实例名称</th>
                <th className="sticky top-0 z-[1] border-b border-(--border) bg-(--panel-bg) px-4 py-2.5 text-center font-medium">代理节点</th>
                <th className="sticky top-0 z-[1] border-b border-(--border) bg-(--panel-bg) px-4 py-2.5 text-center font-medium">实例内核</th>
                <th className="sticky top-0 z-[1] border-b border-(--border) bg-(--panel-bg) px-4 py-2.5 text-center font-medium">状态</th>
                <th className="sticky top-0 z-[1] border-b border-(--border) bg-(--panel-bg) px-4 py-2.5 text-center font-medium">操作</th>
              </tr>
            </thead>
            <tbody>
              {displayInstances.map((inst) => {
                const active = ACTIVE_STATUSES.has(inst.status);
                const transitioning = inst.status === "starting" || inst.status === "stopping";
                return (
                  <tr key={inst.id} className="border-b border-(--border) last:border-0 hover:bg-(--hover-bg)">
                    <td className="px-2 py-2.5 text-center">
                      <input
                        type="checkbox"
                        aria-label={`选择实例 ${inst.name}`}
                        className="h-4 w-4 accent-(--accent)"
                        checked={selectedIds.has(inst.id)}
                        disabled={batchBusy}
                        onChange={(e) => toggleSelected(inst.id, e.target.checked)}
                      />
                    </td>
                    <td className="px-4 py-2.5 font-medium">{inst.name}</td>
                    <td className="px-4 py-2.5 text-center text-(--text-dim)">
                      {(() => {
                        const b = proxyBindings.find((x) => x.instanceId === inst.id);
                        return b ? `${b.nodeName}（${b.sourceName}）` : "未设置";
                      })()}
                    </td>
                    <td className="px-4 py-2.5 text-center text-(--text-dim)">
                      <span title={inst.kernelExecutablePath || undefined}>
                        {inst.kernelName
                          ? `${inst.kernelName}${inst.kernelVersion ? ` ${inst.kernelVersion}` : ""}`
                          : "未指定"}
                      </span>
                    </td>
                    <td className="px-4 py-2.5 text-center">
                      {STATUS_DOT[inst.status] ? (
                        <span
                          className={`inline-flex items-center gap-1.5 text-xs ${STATUS_DOT[inst.status].text}`}
                        >
                          <span className={`h-[7px] w-[7px] shrink-0 rounded-full ${STATUS_DOT[inst.status].dot}`} />
                          {STATUS_LABEL[inst.status]}
                        </span>
                      ) : (
                        <span className="rounded-full bg-(--badge-bg) px-2 py-0.5 text-xs text-(--text-dim)">
                          {STATUS_LABEL[inst.status] ?? inst.status}
                        </span>
                      )}
                    </td>
                    <td className="px-4 py-2.5">
                      <div className="flex justify-center gap-1">
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
                      {kernelLabels.get(b.id) ?? b.name}
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
                <select
                  className={inputCls}
                  value={form.proxyNodeId}
                  disabled={editingRunning}
                  onChange={(e) => setForm({ ...form, proxyNodeId: e.target.value })}
                >
                  <option value="">不使用代理</option>
                  {[...new Set(proxyNodes.map((n) => n.sourceName))].map((srcName) => (
                    <optgroup key={srcName} label={srcName}>
                      {proxyNodes
                        .filter((n) => n.sourceName === srcName)
                        .map((n) => (
                          <option key={n.id} value={n.id}>
                            {n.name}
                          </option>
                        ))}
                    </optgroup>
                  ))}
                </select>
                {proxyNodes.length === 0 && (
                  <p className="mt-1 text-xs text-(--text-faint)">
                    尚未导入任何代理节点。
                    <button
                      type="button"
                      className="text-(--accent) underline underline-offset-2"
                      onClick={() => {
                        setForm(null);
                        setEditing(null);
                        setPage("proxyconfig");
                      }}
                    >
                      前往「代理配置」页面添加
                    </button>
                  </p>
                )}
              </div>
              <div>
                <label className={labelCls}>实例文件</label>
                {!form.id ? (
                  <div>
                    <div className={modeGroupCls} role="group" aria-label="实例文件方式">
                      <button
                        type="button"
                        aria-pressed={form.profileMode === "default"}
                        className={modeButtonCls(form.profileMode === "default")}
                        onClick={() => {
                          setForm({ ...form, profileMode: "default", profileName: "" });
                          setProfileNote(null);
                        }}
                      >
                        默认
                      </button>
                      <button
                        type="button"
                        aria-pressed={form.profileMode === "custom"}
                        className={modeButtonCls(form.profileMode === "custom")}
                        onClick={() => {
                          setForm({ ...form, profileMode: "custom", profileName: "" });
                          setProfileNote(null);
                        }}
                      >
                        自定
                      </button>
                      <button
                        type="button"
                        aria-pressed={form.profileMode === "existing"}
                        className={modeButtonCls(form.profileMode === "existing")}
                        onClick={() => {
                          setForm({ ...form, profileMode: "existing", profileName: "" });
                          setProfileNote(null);
                        }}
                      >
                        选择
                      </button>
                    </div>
                    {form.profileMode === "default" && (
                      <p className="mt-2 text-xs text-(--text-faint)">
                        自动分配实例文件：P-1、P-2、P-3……已分配过的编号不会重复使用。
                      </p>
                    )}
                    {form.profileMode === "custom" && (
                      <input
                        className={`${inputCls} mt-2`}
                        value={form.profileName}
                        onChange={(e) => setForm({ ...form, profileName: e.target.value })}
                        placeholder="实例文件名称，例如：工作账号"
                      />
                    )}
                    {form.profileMode === "existing" && <div className="mt-2">{profilePickerBox(false)}</div>}
                  </div>
                ) : (
                  <div>
                    <p className="text-xs text-(--text-faint)">
                      当前实例文件：{editing ? profileFolderOf(editing) : ""}
                      <span className="font-mono">（{editing?.userDataDir}）</span>
                    </p>
                    <div className={`${modeGroupCls} mt-2`} role="group" aria-label="实例文件方式">
                      <button
                        type="button"
                        aria-pressed={form.profileMode === "rename"}
                        disabled={editingActive}
                        className={modeButtonCls(form.profileMode === "rename")}
                        onClick={() => {
                          setForm({
                            ...form,
                            profileMode: "rename",
                            profileName: editing ? profileFolderOf(editing) : "",
                          });
                          setProfileNote(null);
                        }}
                      >
                        自定
                      </button>
                      <button
                        type="button"
                        aria-pressed={form.profileMode === "swap"}
                        disabled={editingActive}
                        className={modeButtonCls(form.profileMode === "swap")}
                        onClick={() => {
                          setForm({ ...form, profileMode: "swap", profileName: "" });
                          setProfileNote(null);
                        }}
                      >
                        选择
                      </button>
                    </div>
                    {form.profileMode === "rename" && (
                      <input
                        className={`${inputCls} mt-2`}
                        value={form.profileName}
                        disabled={editingActive}
                        onChange={(e) => setForm({ ...form, profileName: e.target.value })}
                        placeholder="新的实例文件名称"
                      />
                    )}
                    {form.profileMode === "swap" && (
                      <div className="mt-2">{profilePickerBox(editingActive)}</div>
                    )}
                    {editingActive && (
                      <p className="mt-2 text-xs text-(--text-faint)">
                        {editingUnknown
                          ? "实例运行状态无法确认时，不能更换实例文件；实例名称与内核可以修改。"
                          : "实例运行中，不能更换内核或实例文件。"}
                      </p>
                    )}
                  </div>
                )}
              </div>
              {formError && <p className="text-sm text-(--danger)">{formError}</p>}
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

      {profilePicker && form && (
        <div className="fixed inset-0 z-20 flex items-center justify-center bg-black/50 px-4">
          <div className="flex max-h-[80vh] w-full max-w-md flex-col rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">选择实例文件</h3>
            <div className="mt-3 min-h-0 flex-1 overflow-y-auto rounded-md border border-(--border)">
              {profilePicker.options.map((p) => (
                <button
                  key={p.name}
                  type="button"
                  aria-pressed={profilePicker.picked === p.name}
                  onClick={() => setProfilePicker({ ...profilePicker, picked: p.name })}
                  className={`block w-full px-3 py-2 text-left hover:bg-(--hover-bg) ${
                    profilePicker.picked === p.name ? "bg-(--hover-bg)" : ""
                  }`}
                >
                  <span className="text-sm font-medium text-(--text-main)">{p.name}</span>
                  <span className="block truncate text-xs text-(--text-faint)">{p.path}</span>
                </button>
              ))}
            </div>
            <div className="mt-4 flex shrink-0 justify-end gap-2">
              <button
                type="button"
                onClick={() => setProfilePicker(null)}
                className="rounded-md border border-(--border) px-3 py-1.5 text-sm text-(--text-dim) hover:bg-(--hover-bg)"
              >
                取消
              </button>
              <button
                type="button"
                disabled={!profilePicker.picked}
                onClick={confirmProfilePicker}
                className="rounded-md bg-(--accent) px-3 py-1.5 text-sm font-medium text-(--accent-contrast) hover:opacity-90 disabled:opacity-50"
              >
                选择
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

      {batchDeletePlan && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="flex max-h-[80vh] w-full max-w-md flex-col rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">批量删除实例</h3>
            <p className="mt-2 shrink-0 text-sm leading-relaxed text-(--text-dim)">
              将删除 {batchDeleteRunnable.length} 个实例，仅删除实例记录；实例文件会保留在磁盘上，之后可以重新绑定到其他实例。
            </p>
            <div className="mt-3 min-h-0 flex-1 overflow-y-auto rounded-md border border-(--border) px-3 py-2">
              {batchDeleteRunnable.map((p) => (
                <p key={p.id} className="py-0.5 text-sm text-(--text-main)">
                  {p.name}
                </p>
              ))}
            </div>
            {batchDeleteSkipped.length > 0 && (
              <p className="mt-2 shrink-0 text-xs text-(--text-faint)">
                另有 {batchDeleteSkipped.length} 个所选实例不会删除：
                {batchDeleteSkipped.map((p) => `「${p.name}」${p.reason}`).join("；")}。
              </p>
            )}
            <div className="mt-5 flex shrink-0 justify-end gap-2">
              <button
                type="button"
                onClick={() => setBatchDeletePlan(null)}
                className="rounded-md border border-(--border) px-3 py-1.5 text-sm text-(--text-dim) hover:bg-(--hover-bg)"
              >
                取消
              </button>
              <button
                type="button"
                disabled={batchBusy}
                onClick={() => {
                  const plan = batchDeletePlan;
                  setBatchDeletePlan(null);
                  void executeBatch("delete", plan);
                }}
                className="rounded-md bg-(--danger) px-3 py-1.5 text-sm font-medium text-(--danger-contrast) hover:opacity-90 disabled:opacity-50"
              >
                确认删除 {batchDeleteRunnable.length} 个实例
              </button>
            </div>
          </div>
        </div>
      )}

      {batchCreate && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="w-full max-w-md rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">批量创建实例</h3>
            <div className="mt-4 space-y-3">
              <div>
                <label className={labelCls}>实例名称前缀（必填）</label>
                <input
                  className={inputCls}
                  value={batchCreate.prefix}
                  onChange={(e) => setBatchCreate({ ...batchCreate, prefix: e.target.value })}
                  placeholder="例如：测试浏览器"
                />
              </div>
              <div>
                <label className={labelCls}>创建数量（必填，一次最多 {MAX_BATCH_CREATE} 个）</label>
                <input
                  className={inputCls}
                  type="number"
                  min={1}
                  max={MAX_BATCH_CREATE}
                  step={1}
                  value={batchCreate.count}
                  onChange={(e) => setBatchCreate({ ...batchCreate, count: e.target.value })}
                />
                {batchCreate.prefix.trim() !== "" &&
                  Number.isInteger(Number(batchCreate.count)) &&
                  Number(batchCreate.count) >= 1 &&
                  Number(batchCreate.count) <= MAX_BATCH_CREATE && (
                    <p className="mt-1 text-xs text-(--text-faint)">
                      将创建：{batchCreate.prefix.trim()}1、{batchCreate.prefix.trim()}2
                      {Number(batchCreate.count) > 2 && ` …… ${batchCreate.prefix.trim()}${Number(batchCreate.count)}`}
                      （共 {Number(batchCreate.count)} 个），每个实例自动分配独立的实例文件。
                    </p>
                  )}
              </div>
              <div>
                <label className={labelCls}>实例内核（必选）</label>
                <select
                  className={inputCls}
                  value={batchCreate.browserId}
                  disabled={!browsersLoaded}
                  onChange={(e) => setBatchCreate({ ...batchCreate, browserId: e.target.value })}
                >
                  <option value="">请选择内核</option>
                  {browsers.map((b) => (
                    <option key={b.id} value={b.id}>
                      {kernelLabels.get(b.id) ?? b.name}
                    </option>
                  ))}
                </select>
                {!browsersLoaded && <p className="mt-1 text-xs text-(--text-faint)">内核列表加载中…</p>}
                {browsersLoaded && browsers.length === 0 && (
                  <p className="mt-1 text-xs text-(--text-faint)">尚未登记任何浏览器内核，请先在「浏览器内核」页面添加。</p>
                )}
              </div>
              {batchCreateError && <p className="text-sm text-(--danger)">{batchCreateError}</p>}
            </div>
            <div className="mt-5 flex justify-end gap-2">
              <button
                type="button"
                onClick={() => setBatchCreate(null)}
                className="rounded-md border border-(--border) px-3 py-1.5 text-sm text-(--text-dim) hover:bg-(--hover-bg)"
              >
                取消
              </button>
              <button
                type="button"
                disabled={batchCreateBusy}
                onClick={() => void submitBatchCreate()}
                className="rounded-md bg-(--accent) px-3 py-1.5 text-sm font-medium text-(--accent-contrast) hover:opacity-90 disabled:opacity-50"
              >
                创建
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
