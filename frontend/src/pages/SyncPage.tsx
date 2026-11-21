import { useCallback, useEffect, useRef, useState } from "react";
import { AppWindow, Monitor, Pause, Play, RotateCcw, Square } from "lucide-react";
import {
  SyncService,
  type SyncCandidate,
  type SyncMonitor,
  type SyncStateView,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import { newOperationId, recordOperationFeedback } from "../lib/operations";
import { notifyError, notifySuccess } from "../stores/useNotificationStore";
import {
  canStart,
  phaseControls,
  refreshSelection,
  type SyncSelection,
} from "./syncLogic";

/**
 * 多开同步（Stage 10）：选择一个主实例与若干跟随实例，将主
 * 实例的网页操作（经 CDP）、已确认的导航与窗口状态同步到
 * 跟随实例。页面只呈现会话控制、参与实例状态、窗口排列与
 * 能力说明，不读取或显示任何网页内容。
 */
export default function SyncPage() {
  const [candidates, setCandidates] = useState<SyncCandidate[]>([]);
  const [state, setState] = useState<SyncStateView | null>(null);
  const [monitors, setMonitors] = useState<SyncMonitor[]>([]);
  const [monitorId, setMonitorId] = useState(0);
  // 在途请求保护：任一同步操作执行期间禁用全部操作入口，
  // 防止连续点击造成重复请求；请求结束（成功或失败）后恢复。
  const [busy, setBusy] = useState(false);
  const refreshFailedRef = useRef(false);
  const [statusError, setStatusError] = useState<string | null>(null);
  const hasStateRef = useRef(false);
  const [monitorsError, setMonitorsError] = useState<string | null>(null);
  const selectionRef = useRef<SyncSelection>({ masterId: "", followerIds: [] });
  const [selection, setSelectionState] = useState<SyncSelection>(selectionRef.current);
  const setSelection = (next: SyncSelection) => {
    selectionRef.current = next;
    setSelectionState(next);
  };

  const sessionActive = state !== null && state.phase !== "已停止";
  const controls = phaseControls(state?.phase ?? "已停止");

  const refresh = useCallback(async () => {
    try {
      const [cands, st] = await Promise.all([
        SyncService.GetSyncCandidates(newOperationId()),
        SyncService.GetSyncState(newOperationId()),
      ]);
      const list = cands ?? [];
      setCandidates(list);
      setState(st);
      hasStateRef.current = true;
      setStatusError(null);
      refreshFailedRef.current = false;
      setSelection(
        refreshSelection(
          selectionRef.current,
          list.map((c) => ({ instanceId: c.instanceId, cdpAvailable: c.cdpAvailable })),
          st.phase !== "已停止",
          st.masterId,
        ),
      );
    } catch (err) {
      // 状态读取失败时保留上次已知状态；只在首次失败时提示
      // 一次，避免轮询期间反复弹出相同错误。从未成功读取
      // 时保留失败原因，页面显示读取失败而非伪造的停止状态。
      const message = err instanceof Error ? err.message : String(err);
      if (!hasStateRef.current) setStatusError(message);
      if (!refreshFailedRef.current) {
        refreshFailedRef.current = true;
        notifyError(`同步状态读取失败：${message}`);
      }
    }
  }, []);

  // 显示器列表只在页面挂载时读取一次（不是轮询）；
  // 加载失败时通过全局通知显示真实原因，不静默吞掉，
  // 并保留失败原因供页面区分读取失败与真实空列表。
  const loadMonitors = useCallback(() => {
    SyncService.ListSyncMonitors(newOperationId())
      .then((ms) => {
        const list = ms ?? [];
        setMonitors(list);
        setMonitorsError(null);
        const primary = list.find((m) => m.primary);
        if (primary) setMonitorId(primary.id);
      })
      .catch((err: unknown) => {
        const message = err instanceof Error ? err.message : String(err);
        setMonitorsError(message);
        notifyError(`加载显示器列表失败：${message}`);
      });
  }, []);

  useEffect(() => {
    void refresh();
    loadMonitors();
    const t = setInterval(() => void refresh(), 2500);
    return () => clearInterval(t);
  }, [refresh, loadMonitors]);

  const run = async (
    action: string,
    fn: (op: string) => Promise<unknown>,
    okMessage: string,
  ) => {
    if (busy) return;
    setBusy(true);
    const op = newOperationId();
    try {
      await fn(op);
      recordOperationFeedback(op, true, okMessage, "sync", action);
      notifySuccess(okMessage, { operationId: op, important: true });
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      recordOperationFeedback(op, false, message, "sync", action);
      notifyError(message, { operationId: op, important: true });
    } finally {
      setBusy(false);
      void refresh();
    }
  };

  const toggleFollower = (id: string, checked: boolean) => {
    const cur = selectionRef.current;
    const followerIds = checked
      ? [...cur.followerIds, id]
      : cur.followerIds.filter((f) => f !== id);
    setSelection({ ...cur, followerIds });
  };

  const arrangeIds = [
    ...(selection.masterId ? [selection.masterId] : []),
    ...selection.followerIds,
  ];

  return (
    <div className="mx-auto flex max-w-5xl flex-col gap-5 px-6 py-5">
      <div className="flex items-center gap-3">
        <span className="rounded-full bg-(--hover-bg) px-2.5 py-0.5 text-xs text-(--text-dim)">
          {state ? state.phase : statusError ? "读取失败" : "加载中"}
        </span>
      </div>

      {state === null && statusError && (
        <div className="flex items-center gap-2 rounded-xl border border-(--border) bg-(--panel-bg) px-4 py-3">
          <p className="flex-1 text-sm text-(--danger)">同步状态读取失败：{statusError}</p>
          <button
            type="button"
            onClick={() => void refresh()}
            className="shrink-0 rounded-md border border-(--border-strong) bg-(--input-bg) px-3 py-1.5 text-sm text-(--text-main) hover:bg-(--hover-bg)"
          >
            重新加载
          </button>
        </div>
      )}
      {state === null && !statusError && (
        <p className="text-sm text-(--text-faint)">正在加载同步状态…</p>
      )}

      <section className="rounded-xl border border-(--border) bg-(--panel-bg) p-4">
        <div className="mb-3 flex items-center justify-between">
          <h2 className="text-sm font-medium text-(--text-main)">同步控制</h2>
          <div className="flex gap-2">
            <button
              className="flex h-9 items-center gap-1.5 rounded-md bg-(--accent) px-3 text-sm font-medium text-(--accent-contrast) hover:opacity-90 disabled:opacity-50"
              disabled={busy || !controls.start || !canStart(selection)}
              onClick={() =>
                void run("start", (op) =>
                  SyncService.StartSync(op, selection.masterId, selection.followerIds),
                "同步已开始")
              }
            >
              <Play size={14} /> 开始同步
            </button>
            <button
              className="flex h-9 items-center gap-1.5 rounded-md border border-(--border-strong) px-3 text-sm text-(--text-main) hover:bg-(--hover-bg) disabled:opacity-50"
              disabled={busy || !controls.pause}
              onClick={() => void run("pause", (op) => SyncService.PauseSync(op), "同步已暂停")}
            >
              <Pause size={14} /> 暂停
            </button>
            <button
              className="flex h-9 items-center gap-1.5 rounded-md border border-(--border-strong) px-3 text-sm text-(--text-main) hover:bg-(--hover-bg) disabled:opacity-50"
              disabled={busy || !controls.resume}
              onClick={() => void run("resume", (op) => SyncService.ResumeSync(op), "同步已恢复")}
            >
              <RotateCcw size={14} /> 恢复
            </button>
            <button
              className="flex h-9 items-center gap-1.5 rounded-md border border-(--border-strong) px-3 text-sm text-(--text-main) hover:bg-(--hover-bg) disabled:opacity-50"
              disabled={busy || !controls.stop}
              onClick={() => void run("stop", (op) => SyncService.StopSync(op), "同步已停止")}
            >
              <Square size={14} /> 停止
            </button>
          </div>
        </div>
        <table className="w-full text-sm">
          <thead>
            <tr className="text-left text-(--text-faint)">
              <th className="py-1 pr-2 font-normal">实例</th>
              <th className="py-1 pr-2 font-normal">内核</th>
              <th className="py-1 pr-2 font-normal">主控</th>
              <th className="py-1 pr-2 font-normal">跟随</th>
              <th className="py-1 font-normal">同步状态</th>
            </tr>
          </thead>
          <tbody>
            {candidates.map((c) => {
              const participant = state?.participants?.find((p) => p.instanceId === c.instanceId);
              return (
                <tr key={c.instanceId} className="border-t border-(--border)">
                  <td className="py-1.5 pr-2 text-(--text-main)">{c.name}</td>
                  <td className="py-1.5 pr-2 text-(--text-dim)">{c.kernelName || "—"}</td>
                  <td className="py-1.5 pr-2">
                    <input
                      type="radio"
                      name="sync-master"
                      disabled={sessionActive || !c.cdpAvailable}
                      checked={selection.masterId === c.instanceId}
                      onChange={() =>
                        setSelection({
                          masterId: c.instanceId,
                          followerIds: selectionRef.current.followerIds.filter(
                            (f) => f !== c.instanceId,
                          ),
                        })
                      }
                    />
                  </td>
                  <td className="py-1.5 pr-2">
                    <input
                      type="checkbox"
                      disabled={
                        sessionActive || !c.cdpAvailable || selection.masterId === c.instanceId
                      }
                      checked={selection.followerIds.includes(c.instanceId)}
                      onChange={(e) => toggleFollower(c.instanceId, e.target.checked)}
                    />
                  </td>
                  <td className="py-1.5 text-(--text-dim)">
                    {participant
                      ? participant.status +
                        (participant.reason ? `（${participant.reason}）` : "")
                      : c.cdpAvailable
                        ? "可同步"
                        : c.reason}
                  </td>
                </tr>
              );
            })}
            {candidates.length === 0 && state !== null && (
              <tr>
                <td colSpan={5} className="py-3 text-(--text-faint)">
                  尚无浏览器实例。请先在「浏览器实例」中创建并启动实例。
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </section>

      <section className="rounded-xl border border-(--border) bg-(--panel-bg) p-4">
        <h2 className="mb-3 flex items-center gap-2 text-sm font-medium text-(--text-main)">
          <Monitor size={15} /> 窗口排列
        </h2>
        <div className="flex flex-wrap items-center gap-2">
          <select
            className="h-9 rounded-md border border-(--border-strong) bg-(--panel-bg) px-2 text-sm text-(--text-main)"
            value={monitorId}
            onChange={(e) => setMonitorId(Number(e.target.value))}
          >
            {monitors.map((m) => (
              <option key={m.id} value={m.id}>
                {m.label}
              </option>
            ))}
          </select>
          {monitorsError && monitors.length === 0 && (
            <>
              <span className="text-sm text-(--danger)">显示器列表读取失败：{monitorsError}</span>
              <button
                type="button"
                onClick={loadMonitors}
                className="rounded-md border border-(--border-strong) bg-(--input-bg) px-3 py-1.5 text-sm text-(--text-main) hover:bg-(--hover-bg)"
              >
                重新加载
              </button>
            </>
          )}
          <button
            className="flex h-9 items-center gap-1.5 rounded-md border border-(--border-strong) px-3 text-sm text-(--text-main) hover:bg-(--hover-bg) disabled:opacity-50"
            disabled={busy || arrangeIds.length === 0}
            onClick={() =>
              void run("arrange", (op) =>
                SyncService.ArrangeSyncWindows(op, arrangeIds, monitorId),
              "窗口已排列")
            }
          >
            <AppWindow size={14} /> 自动排列所选实例窗口
          </button>
          <button
            className="flex h-9 items-center gap-1.5 rounded-md border border-(--border-strong) px-3 text-sm text-(--text-main) hover:bg-(--hover-bg) disabled:opacity-50"
            disabled={busy}
            onClick={() =>
              void run("restore", (op) => SyncService.RestoreSyncWindows(op), "窗口已恢复")
            }
          >
            <RotateCcw size={14} /> 恢复窗口
          </button>
        </div>
        <p className="mt-2 text-xs leading-relaxed text-(--text-faint)">
          排列使用所选显示器的工作区，恢复会还原排列前的完整窗口状态。窗口移动与尺寸调整不会激活窗口；
          最大化、还原等状态切换经系统窗口状态接口执行，个别情况下可能影响前台窗口，属已知限制。
        </p>
      </section>

      <section className="rounded-xl border border-(--border) bg-(--panel-bg) p-4 text-sm leading-relaxed text-(--text-dim)">
        <h2 className="mb-2 text-sm font-medium text-(--text-main)">能力说明</h2>
        <ul className="list-disc space-y-1 pl-5">
          <li>可靠支持：页面点击、移动、滚动、拖动、键盘与文本输入（含中文输入法最终文本）、新建/关闭/切换标签、页面导航与刷新。</li>
          <li>条件支持：前进/后退取决于各实例自身的浏览历史；扩展弹窗与个别原生控件无法保证识别，不做猜测性操作。</li>
          <li>不支持：浏览器原生界面文字的逐字同步（地址栏未提交文字等）、剪贴板同步、页面缩放同步、最小化状态同步、跨域 iframe 内的操作。</li>
          <li>同步不移动系统鼠标、不抢占前台焦点；不记录网页内容、输入文本与浏览历史。</li>
        </ul>
      </section>
    </div>
  );
}
