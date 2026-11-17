import { useCallback, useEffect, useRef, useState } from "react";
import {
  AlertCircle,
  Info,
  LayoutGrid,
  Loader2,
  Pause,
  Play,
  Square,
  Undo2,
} from "lucide-react";
import {
  SyncService,
  type SyncInstanceInfo,
  type SyncMonitor,
  type SyncSessionState,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import { newOperationId, recordOperationFeedback } from "../lib/operations";
import {
  followerIds,
  roleText,
  sessionStateText,
  startBlockReason,
} from "./syncLogic";

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

const btnSecondary =
  "inline-flex h-9 items-center gap-1.5 rounded-md border border-(--border-strong) bg-(--input-bg) px-3 text-sm text-(--text-main) transition hover:bg-(--hover-bg) disabled:opacity-50";
const btnPrimary =
  "inline-flex h-9 items-center gap-1.5 rounded-md bg-(--accent) px-3 text-sm font-medium text-(--accent-contrast) transition hover:opacity-90 disabled:opacity-50";

// 多开同步：选择参与实例并指定主实例，开始后主实例的操作会
// 同步到全部跟随实例（网页内操作经各实例自己的 CDP 连接，
// 浏览器界面操作经 Windows 原生输入镜像）。自动排列与恢复
// 窗口可独立于同步会话使用。同步只传递操作，不读取、不共享
// 任何实例的 Profile 数据。
export default function SyncPage() {
  const [instances, setInstances] = useState<SyncInstanceInfo[]>([]);
  const [session, setSession] = useState<SyncSessionState | null>(null);
  const [monitors, setMonitors] = useState<SyncMonitor[]>([]);
  const [monitorIndex, setMonitorIndex] = useState(0);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [masterId, setMasterId] = useState("");
  const lastSessionError = useRef("");

  const refresh = useCallback(async () => {
    try {
      const [list, state, mons] = await Promise.all([
        SyncService.ListSyncInstances(),
        SyncService.GetSyncState(),
        SyncService.ListSyncMonitors(),
      ]);
      setInstances(list ?? []);
      setSession(state);
      setMonitors(mons ?? []);
      if (state.lastError && state.lastError !== lastSessionError.current) {
        lastSessionError.current = state.lastError;
        setError(state.lastError);
      }
    } catch {
      // 状态刷新失败不打断页面，下一次轮询会重试。
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
    const timer = setInterval(() => void refresh(), 1500);
    return () => clearInterval(timer);
  }, [refresh]);

  const state = session?.state ?? "idle";
  const inSession = state !== "idle";
  const blockReason = startBlockReason(selected, masterId, instances);

  const run = async (
    label: string,
    action: string,
    fn: (opId: string) => Promise<void>,
  ) => {
    const opId = newOperationId();
    setBusy(label);
    setError(null);
    setNotice(null);
    try {
      await fn(opId);
      recordOperationFeedback(opId, true, "操作成功", "多开同步", action);
      await refresh();
    } catch (err) {
      recordOperationFeedback(opId, false, errText(err), "多开同步", action);
      setError(errText(err));
    } finally {
      setBusy(null);
    }
  };

  const handleStart = () =>
    run("start", "sync.start", async (opId) => {
      if (blockReason) {
        throw new Error(blockReason);
      }
      await SyncService.StartSync(opId, masterId, followerIds(selected, masterId));
      setNotice("同步已开始：在主实例中的操作将同步到全部跟随实例");
    });

  const handleArrange = () =>
    run("arrange", "sync.arrange", async (opId) => {
      const ids = inSession
        ? [session!.masterId, ...(session!.followers ?? []).map((f) => f.id)]
        : [...selected];
      if (ids.length === 0) {
        throw new Error("请先选择参与排列的实例");
      }
      const result = await SyncService.ArrangeSyncWindows(opId, ids, monitorIndex);
      const placed = result.placed?.length ?? 0;
      const failures = result.failures ?? [];
      if (failures.length > 0) {
        const names = new Map(instances.map((i) => [i.id, i.name]));
        setNotice(
          `已排列 ${placed} 个窗口；${failures
            .map((f) => `${names.get(f.instanceId) ?? f.instanceId}：${f.reason}`)
            .join("；")}`,
        );
      } else {
        setNotice(`已自动排列 ${placed} 个窗口`);
      }
    });

  const toggleSelected = (id: string, checked: boolean) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (checked) {
        next.add(id);
      } else {
        next.delete(id);
        if (masterId === id) {
          setMasterId("");
        }
      }
      return next;
    });
  };

  const statusBadge = (inst: SyncInstanceInfo) => {
    if (inst.role === "follower" && inst.failed) {
      return (
        <span className="rounded bg-(--danger-bg) px-1.5 py-0.5 text-xs text-(--danger)">
          同步失败
        </span>
      );
    }
    if (inSession && inst.role) {
      return (
        <span className="rounded bg-(--accent-soft) px-1.5 py-0.5 text-xs text-(--text-main)">
          {roleText(inst.role, state)}
        </span>
      );
    }
    return (
      <span className="rounded bg-(--badge-bg) px-1.5 py-0.5 text-xs text-(--text-dim)">
        {inst.status === "running" ? "运行中" : "未参与"}
      </span>
    );
  };

  return (
    <div className="mx-auto flex w-full max-w-5xl flex-col px-6 py-5">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold text-(--text-main)">多开同步</h2>
          <p className="mt-0.5 text-sm text-(--text-dim)">
            选择参与实例并指定主实例，只操作主实例，其余实例同步执行相同操作。
          </p>
        </div>
        <span className="rounded bg-(--badge-bg) px-2 py-1 text-sm text-(--text-main)">
          {sessionStateText(state)}
        </span>
      </div>

      {error && (
        <div className="mt-4 flex items-start gap-2 rounded-lg border border-(--danger-border) bg-(--danger-bg) px-3 py-2 text-sm text-(--danger)">
          <AlertCircle className="mt-0.5 h-4 w-4 shrink-0" />
          <span>{error}</span>
        </div>
      )}
      {notice && (
        <div className="mt-4 flex items-start gap-2 rounded-lg border border-(--border) bg-(--accent-soft) px-3 py-2 text-sm text-(--text-main)">
          <Info className="mt-0.5 h-4 w-4 shrink-0" />
          <span>{notice}</span>
        </div>
      )}

      <div className="mt-5 overflow-hidden rounded-lg border border-(--border) bg-(--panel-bg)">
        <div className="border-b border-(--border) px-4 py-2.5 text-sm font-medium text-(--text-main)">
          参与实例
        </div>
        {loading ? (
          <div className="flex items-center gap-2 px-4 py-6 text-sm text-(--text-dim)">
            <Loader2 className="h-4 w-4 animate-spin" /> 正在加载实例……
          </div>
        ) : instances.length === 0 ? (
          <p className="px-4 py-6 text-sm text-(--text-dim)">暂无浏览器实例。</p>
        ) : (
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-(--border) text-left text-(--text-dim)">
                <th className="px-4 py-2 font-medium">参与</th>
                <th className="px-4 py-2 font-medium">实例</th>
                <th className="px-4 py-2 font-medium">内核</th>
                <th className="px-4 py-2 font-medium">运行状态</th>
                <th className="px-4 py-2 font-medium">主实例</th>
                <th className="px-4 py-2 font-medium">同步状态</th>
              </tr>
            </thead>
            <tbody>
              {instances.map((inst) => {
                const isSelected = inSession
                  ? inst.role !== ""
                  : selected.has(inst.id);
                const selectable =
                  !inSession && inst.status === "running" && inst.cdpAvailable;
                return (
                  <tr key={inst.id} className="border-b border-(--border) last:border-0">
                    <td className="px-4 py-2">
                      <input
                        type="checkbox"
                        checked={isSelected}
                        disabled={!selectable}
                        onChange={(e) => toggleSelected(inst.id, e.target.checked)}
                      />
                    </td>
                    <td className="px-4 py-2 text-(--text-main)">{inst.name}</td>
                    <td className="px-4 py-2 text-(--text-dim)">{inst.kernelName || "—"}</td>
                    <td className="px-4 py-2 text-(--text-dim)">
                      {inst.status === "running" ? "运行中" : "未运行"}
                      {!inst.cdpAvailable && inst.cdpReason && (
                        <span className="block text-xs text-(--text-faint)">
                          {inst.cdpReason}
                        </span>
                      )}
                    </td>
                    <td className="px-4 py-2">
                      <input
                        type="radio"
                        name="sync-master"
                        checked={
                          inSession ? inst.role === "master" : masterId === inst.id
                        }
                        disabled={!selectable && !inSession}
                        onChange={() => {
                          setMasterId(inst.id);
                          setSelected((prev) => new Set(prev).add(inst.id));
                        }}
                      />
                    </td>
                    <td className="px-4 py-2">
                      {statusBadge(inst)}
                      {inst.role === "follower" && inst.failed && inst.failureReason && (
                        <span className="block text-xs text-(--danger)">
                          {inst.failureReason}
                        </span>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>

      <div className="mt-5 flex flex-wrap items-center gap-2">
        {!inSession ? (
          <button
            className={btnPrimary}
            disabled={busy !== null || blockReason !== null}
            title={blockReason ?? undefined}
            onClick={() => void handleStart()}
          >
            <Play className="h-4 w-4" /> 开始同步
          </button>
        ) : (
          <>
            {state === "active" ? (
              <button
                className={btnSecondary}
                disabled={busy !== null}
                onClick={() => void run("pause", "sync.pause", (opId) => SyncService.PauseSync(opId))}
              >
                <Pause className="h-4 w-4" /> 暂停同步
              </button>
            ) : (
              <button
                className={btnPrimary}
                disabled={busy !== null}
                onClick={() => void run("resume", "sync.resume", (opId) => SyncService.ResumeSync(opId))}
              >
                <Play className="h-4 w-4" /> 恢复同步
              </button>
            )}
            <button
              className={btnSecondary}
              disabled={busy !== null}
              onClick={() => void run("stop", "sync.stop", (opId) => SyncService.StopSync(opId))}
            >
              <Square className="h-4 w-4" /> 停止同步
            </button>
          </>
        )}
        <span className="mx-1 h-6 w-px bg-(--border)" />
        <select
          className="h-9 rounded-md border border-(--border-strong) bg-(--input-bg) px-2 text-sm text-(--text-main)"
          value={monitorIndex}
          onChange={(e) => setMonitorIndex(Number(e.target.value))}
        >
          {monitors.length === 0 ? (
            <option value={0}>默认显示器</option>
          ) : (
            monitors.map((m) => (
              <option key={m.index} value={m.index}>
                显示器 {m.index + 1}
                {m.primary ? "（主显示器）" : ""} · {m.workWidth}×{m.workHeight}
              </option>
            ))
          )}
        </select>
        <button
          className={btnSecondary}
          disabled={busy !== null}
          onClick={() => void handleArrange()}
        >
          <LayoutGrid className="h-4 w-4" /> 自动排列
        </button>
        <button
          className={btnSecondary}
          disabled={busy !== null}
          onClick={() =>
            void run("restore", "sync.restore_windows", async (opId) => {
              await SyncService.RestoreSyncWindows(opId);
              setNotice("已恢复排列前的窗口位置");
            })
          }
        >
          <Undo2 className="h-4 w-4" /> 恢复窗口
        </button>
        {busy && <Loader2 className="h-4 w-4 animate-spin text-(--text-dim)" />}
      </div>
      {!inSession && blockReason && (
        <p className="mt-2 text-sm text-(--text-faint)">{blockReason}</p>
      )}
    </div>
  );
}
