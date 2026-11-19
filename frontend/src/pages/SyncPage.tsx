import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  InstanceService,
  SyncService,
  type SyncMonitor,
  type SyncState,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import type { Instance } from "../../bindings/github.com/onbeee/DeBrowser/internal/model";
import { useAppStore } from "../stores/useAppStore";
import { newOperationId, recordOperationFeedback } from "../lib/operations";
import { sortInstancesByName } from "./instancesLogic";
import {
  canStart,
  followerStateText,
  refreshSelection,
  sessionSummary,
  toggleFollower,
  toggleMaster,
} from "./syncLogic";

// 多开同步（Stage 10 v3）：基于 CDP 页面事件的主控—被控同步。
// 主实例的网页输入由页面内观察脚本经 CDP 通道回传，跟随实例在
// 各自的 CDP 连接上执行；标签对应关系由 CDP 的 Target 标识显
// 式建立。本页只呈现会话与每个实例的真实状态，不提供任何诊
// 断或配置项。
export default function SyncPage() {
  const log = useAppStore((state) => state.log);
  const [instances, setInstances] = useState<Instance[]>([]);
  const [syncState, setSyncState] = useState<SyncState | null>(null);
  const [masterId, setMasterId] = useState("");
  const [followerIds, setFollowerIds] = useState<string[]>([]);
  const [monitors, setMonitors] = useState<SyncMonitor[]>([]);
  const [monitorIndex, setMonitorIndex] = useState(0);
  const [message, setMessage] = useState("");
  const [busy, setBusy] = useState(false);
  const busyRef = useRef(false);
  // The selection's source of truth for the polling refresh: the
  // refresh callback is created once and must never read the
  // selection from a stale render closure (that once reset the
  // followers to the initial empty array on every 2s poll). The
  // refs always hold the latest selection; state mirrors them
  // for rendering.
  const masterRef = useRef("");
  const followerRef = useRef<string[]>([]);

  const phase = syncState?.phase ?? "idle";
  const sessionActive = phase !== "idle";

  const applyMaster = useCallback((id: string) => {
    masterRef.current = id;
    setMasterId(id);
  }, []);
  const applyFollowers = useCallback((ids: string[]) => {
    followerRef.current = ids;
    setFollowerIds(ids);
  }, []);

  const refresh = useCallback(async () => {
    try {
      const [list, state] = await Promise.all([
        InstanceService.ListInstances(),
        SyncService.GetSyncState(),
      ]);
      const sorted = sortInstancesByName(list ?? []);
      setInstances(sorted);
      setSyncState(state);
      const ids = new Set(sorted.map((i) => i.id));
      // Prune ONLY what no longer exists (or clashes with the
      // master) from the CURRENT selection, read from the refs —
      // a poll never resets a still-valid selection. While a
      // session is live the toolbar/table follow the session's
      // own state, as before.
      const next = refreshSelection(
        masterRef.current,
        followerRef.current,
        ids,
        state.phase !== "idle" ? state.masterId : "",
      );
      applyMaster(next.masterId);
      applyFollowers(next.followerIds);
    } catch (err) {
      log("error", `加载同步状态失败：${err instanceof Error ? err.message : String(err)}`);
    }
  }, [log, applyMaster, applyFollowers]);

  useEffect(() => {
    void refresh();
    const timer = setInterval(() => {
      if (!busyRef.current) void refresh();
    }, 2000);
    return () => clearInterval(timer);
  }, [refresh]);

  useEffect(() => {
    SyncService.ListSyncMonitors()
      .then((list) => {
        const monitors = list ?? [];
        setMonitors(monitors);
        const primary = monitors.find((m) => m.primary);
        if (primary) setMonitorIndex(primary.index);
      })
      .catch(() => setMonitors([]));
  }, []);

  const runAction = useCallback(
    async (label: string, action: (operationId: string) => Promise<void>) => {
      if (busyRef.current) return;
      busyRef.current = true;
      setBusy(true);
      setMessage("");
      const operationId = newOperationId();
      try {
        await action(operationId);
        recordOperationFeedback(operationId, true, `${label}成功`, "多开同步", label);
      } catch (err) {
        const text = err instanceof Error ? err.message : String(err);
        setMessage(text);
        recordOperationFeedback(operationId, false, `${label}失败：${text}`, "多开同步", label);
      } finally {
        busyRef.current = false;
        setBusy(false);
        void refresh();
      }
    },
    [refresh],
  );

  const followerById = useMemo(() => {
    const map = new Map<string, { instanceId: string; connected: boolean; pageReady: boolean; state: string; reason: string }>();
    for (const f of syncState?.followers ?? []) map.set(f.instanceId, f);
    return map;
  }, [syncState]);

  const statusText = (inst: Instance): string => {
    if (sessionActive && syncState) {
      if (inst.id === syncState.masterId) {
        if (!syncState.masterConnected) return "主实例 · 未连接";
        return syncState.masterPageReady
          ? "主实例 · 已连接 · 活动页面已确认"
          : "主实例 · 已连接 · 活动页面未确认";
      }
      const f = followerById.get(inst.id);
      if (f) return followerStateText(f);
    }
    switch (inst.status) {
      case "running":
        return "运行中";
      case "starting":
        return "启动中";
      case "stopping":
        return "停止中";
      case "error":
        return "状态异常";
      case "unknown":
        return "无法确认";
      default:
        return "已停止";
    }
  };

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex flex-wrap items-center gap-2 border-b border-(--border) px-4 py-3">
        {phase === "idle" ? (
          <button
            type="button"
            disabled={busy || !canStart(phase, masterId, followerIds.length)}
            onClick={() =>
              void runAction("开始同步", (op) => SyncService.StartSync(op, masterId, followerIds))
            }
            className="rounded-md bg-(--accent) px-3 py-1.5 text-sm text-(--accent-contrast) disabled:opacity-50"
          >
            开始同步
          </button>
        ) : phase === "running" ? (
          <>
            <button
              type="button"
              disabled={busy}
              onClick={() => void runAction("暂停同步", (op) => SyncService.PauseSync(op))}
              className="rounded-md border border-(--border) px-3 py-1.5 text-sm disabled:opacity-50"
            >
              暂停同步
            </button>
            <button
              type="button"
              disabled={busy}
              onClick={() => void runAction("停止同步", (op) => SyncService.StopSync(op))}
              className="rounded-md border border-(--border) px-3 py-1.5 text-sm disabled:opacity-50"
            >
              停止同步
            </button>
          </>
        ) : (
          <>
            <button
              type="button"
              disabled={busy}
              onClick={() => void runAction("恢复同步", (op) => SyncService.ResumeSync(op))}
              className="rounded-md bg-(--accent) px-3 py-1.5 text-sm text-(--accent-contrast) disabled:opacity-50"
            >
              恢复同步
            </button>
            <button
              type="button"
              disabled={busy}
              onClick={() => void runAction("停止同步", (op) => SyncService.StopSync(op))}
              className="rounded-md border border-(--border) px-3 py-1.5 text-sm disabled:opacity-50"
            >
              停止同步
            </button>
          </>
        )}
        <span className="mx-1 h-5 w-px bg-(--border)" />
        <button
          type="button"
          disabled={busy || monitors.length === 0}
          onClick={() => void runAction("自动排列", (op) => SyncService.ArrangeWindows(op, monitorIndex))}
          className="rounded-md border border-(--border) px-3 py-1.5 text-sm disabled:opacity-50"
        >
          自动排列
        </button>
        <button
          type="button"
          disabled={busy}
          onClick={() => void runAction("恢复窗口", (op) => SyncService.RestoreWindows(op))}
          className="rounded-md border border-(--border) px-3 py-1.5 text-sm disabled:opacity-50"
        >
          恢复窗口
        </button>
        {monitors.length > 1 && (
          <select
            value={monitorIndex}
            onChange={(e) => setMonitorIndex(Number(e.target.value))}
            className="rounded-md border border-(--border) bg-(--panel-bg) px-2 py-1.5 text-sm"
            aria-label="选择显示器"
          >
            {monitors.map((m) => (
              <option key={m.index} value={m.index}>
                显示器 {m.index + 1}（{m.width}×{m.height}）
              </option>
            ))}
          </select>
        )}
        <span className="ml-auto text-sm text-(--text-dim)">
          {syncState ? sessionSummary({ ...syncState, followers: syncState.followers ?? [] }) : ""}
        </span>
      </div>

      <p className="border-b border-(--border) px-4 py-2 text-xs text-(--text-dim)">
        同步范围为网页内的鼠标、滚动、键盘与文本输入，页面导航和标签的创建、关闭与切换，以及主窗口的移动、调整大小、最大化与还原（最小化不同步）。粘贴只通过文本增量同步一次，粘贴到画布等不产生文本输入的位置不支持；iframe 内的输入不支持。地址栏未提交的输入、浏览器菜单、扩展弹窗与文件选择框等浏览器自身界面不在同步范围内；暂停会停止全部分发。跟随窗口不会被带到前台，标签切换在跟随窗口中的可见效果取决于浏览器自身的标签激活行为。
      </p>
      {message && (
        <p className="border-b border-(--border) px-4 py-2 text-sm text-(--danger)">{message}</p>
      )}

      <div className="min-h-0 flex-1 overflow-y-auto">
        <table className="w-full text-sm">
          <thead>
            <tr className="border-b border-(--border) text-left text-(--text-dim)">
              <th className="px-4 py-2 font-medium">实例名称</th>
              <th className="px-4 py-2 font-medium">主实例</th>
              <th className="px-4 py-2 font-medium">跟随实例</th>
              <th className="px-4 py-2 font-medium">状态</th>
            </tr>
          </thead>
          <tbody>
            {instances.map((inst) => {
              const isMaster = sessionActive ? syncState?.masterId === inst.id : masterId === inst.id;
              const isFollower = sessionActive
                ? followerById.has(inst.id)
                : followerIds.includes(inst.id);
              return (
                <tr key={inst.id} className="border-b border-(--border)">
                  <td className="px-4 py-2">{inst.name}</td>
                  <td className="px-4 py-2">
                    <input
                      type="radio"
                      name="sync-master"
                      checked={isMaster}
                      disabled={sessionActive}
                      onClick={() => applyMaster(toggleMaster(masterRef.current, inst.id))}
                      onChange={() => undefined}
                      aria-label={`将 ${inst.name} 设为主实例`}
                    />
                  </td>
                  <td className="px-4 py-2">
                    <input
                      type="checkbox"
                      checked={isFollower}
                      disabled={sessionActive || isMaster}
                      onChange={() =>
                        applyFollowers(toggleFollower(followerRef.current, inst.id, masterRef.current))
                      }
                      aria-label={`将 ${inst.name} 设为跟随实例`}
                    />
                  </td>
                  <td className="px-4 py-2 text-(--text-dim)">{statusText(inst)}</td>
                </tr>
              );
            })}
            {instances.length === 0 && (
              <tr>
                <td colSpan={4} className="px-4 py-6 text-center text-(--text-dim)">
                  暂无浏览器实例
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}
