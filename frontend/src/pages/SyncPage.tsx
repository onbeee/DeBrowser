import { useCallback, useEffect, useRef, useState } from "react";
import {
  CheckCircle2,
  LayoutGrid,
  Pause,
  Play,
  Square,
  Undo2,
  XCircle,
} from "lucide-react";
import { SyncService } from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import type {
  SyncInstanceOption,
  SyncMonitorInfo,
  SyncState,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import { newOperationId, recordOperationFeedback } from "../lib/operations";
import {
  canStart,
  capsSummary,
  followerSummary,
  sessionFollower,
  toggleFollower,
  toggleMaster,
} from "./syncLogic";

/**
 * 多开同步页（Stage 10 v2）。布局为固定工具栏 + 独立滚动的实例
 * 列表：工具栏集中全部操作（开始/暂停/恢复/停止、自动排列、
 * 恢复窗口、显示器选择），列表只做主/跟随选择与紧凑的行内
 * 状态反馈。同步的技术边界见项目文档，本页只保留必要的状态
 * 提示与真实失败原因。
 */
export default function SyncPage() {
  const [options, setOptions] = useState<SyncInstanceOption[]>([]);
  const [state, setState] = useState<SyncState | null>(null);
  const [monitors, setMonitors] = useState<SyncMonitorInfo[]>([]);
  const [masterId, setMasterId] = useState("");
  const [followers, setFollowers] = useState<string[]>([]);
  const [monitorIdx, setMonitorIdx] = useState(0);
  const [busy, setBusy] = useState(false);
  const [notice, setNotice] = useState("");
  const [error, setError] = useState("");
  const timer = useRef<number | undefined>(undefined);

  const reload = useCallback(async () => {
    try {
      const [opts, st, mons] = await Promise.all([
        SyncService.ListSyncInstances(newOperationId()),
        SyncService.GetSyncState(newOperationId()),
        SyncService.ListSyncMonitors(newOperationId()).catch(() => [] as SyncMonitorInfo[]),
      ]);
      setOptions(opts ?? []);
      setState(st);
      setMonitors(mons ?? []);
    } catch {
      // 列表读取失败时保留已有界面，下一次刷新会重试。
    }
  }, []);

  useEffect(() => {
    void reload();
    timer.current = window.setInterval(() => void reload(), 2000);
    return () => window.clearInterval(timer.current);
  }, [reload]);

  const run = async (action: string, fn: () => Promise<void>, okMsg: string) => {
    const opId = newOperationId();
    setBusy(true);
    setError("");
    setNotice("");
    try {
      await fn();
      setNotice(okMsg);
      recordOperationFeedback(opId, true, okMsg, "sync", action);
    } catch (e) {
      const msg = e instanceof Error ? e.message : String(e);
      setError(msg);
      recordOperationFeedback(opId, false, msg, "sync", action);
    } finally {
      setBusy(false);
      void reload();
    }
  };

  const phase = state?.phase ?? "idle";
  const inSession = phase === "running" || phase === "paused";
  // While a session runs, the selection is the session's own and
  // stays locked until the session stops.
  const selMaster = inSession ? (state?.masterId ?? "") : masterId;
  const selFollowers = inSession ? (state?.followers ?? []).map((f) => f.id) : followers;
  const arrangeIds = [selMaster, ...selFollowers].filter((x) => x !== "");

  const btnPrimary =
    "inline-flex h-9 items-center gap-1.5 rounded-md bg-(--accent) px-3 text-sm font-medium text-(--accent-contrast) transition hover:opacity-90 disabled:opacity-50";
  const btnSecondary =
    "inline-flex h-9 items-center gap-1.5 rounded-md border border-(--border-strong) bg-(--input-bg) px-3 text-sm text-(--text-main) transition hover:bg-(--hover-bg) disabled:opacity-50";
  const thCls =
    "sticky top-0 z-[1] border-b border-(--border) bg-(--panel-bg) px-4 py-2.5 font-medium";

  const rowStatus = (o: SyncInstanceOption) => {
    if (inSession) {
      if (o.id === state?.masterId) {
        return `主实例 · ${capsSummary(state.masterCaps)}`;
      }
      const f = sessionFollower(state?.followers ?? [], o.id);
      if (f) {
        const parts: string[] = [
          f.status === "failed" ? "失败" : f.status === "suspended" ? "已暂停" : "正常",
        ];
        if (f.status === "failed" && f.reason) parts.push(`失败原因：${f.reason}`);
        if (f.status === "suspended" && f.reason) parts.push(`等待标签映射：${f.reason}`);
        if (f.opError) parts.push(`最近一次界面操作未执行：${f.opError}`);
        parts.push(capsSummary(f.caps));
        return parts.join(" · ");
      }
      return "未参与本次同步";
    }
    if (!o.running) return "未运行";
    if (!o.cdpSupported) return o.unsupportedNote || "不是以同步模式启动，关闭后重新启动即可参与同步";
    return "运行中，可参与同步";
  };

  return (
    <div className="mx-auto flex h-full min-h-0 w-full max-w-5xl flex-col overflow-hidden px-6 py-6">
      <div className="flex-none">
        <div className="flex flex-wrap items-center gap-2">
          {!inSession && (
            <button
              type="button"
              className={btnPrimary}
              disabled={busy || !canStart(masterId, followers)}
              onClick={() =>
                void run("start", async () => {
                  await SyncService.StartSync(newOperationId(), masterId, followers);
                }, "同步已开始")
              }
            >
              <Play size={15} />
              开始同步
            </button>
          )}
          {phase === "running" && (
            <button
              type="button"
              className={btnSecondary}
              disabled={busy}
              onClick={() => void run("pause", async () => { await SyncService.PauseSync(newOperationId()); }, "同步已暂停")}
            >
              <Pause size={15} />
              暂停同步
            </button>
          )}
          {phase === "paused" && (
            <button
              type="button"
              className={btnSecondary}
              disabled={busy}
              onClick={() => void run("resume", async () => { await SyncService.ResumeSync(newOperationId()); }, "同步已恢复")}
            >
              <Play size={15} />
              恢复同步
            </button>
          )}
          {inSession && (
            <button
              type="button"
              className={btnSecondary}
              disabled={busy}
              onClick={() => void run("stop", async () => { await SyncService.StopSync(newOperationId()); }, "同步已停止，浏览器保持运行")}
            >
              <Square size={15} />
              停止同步
            </button>
          )}
          <button
            type="button"
            className={btnSecondary}
            disabled={busy || arrangeIds.length === 0}
            onClick={() =>
              void run("arrange", async () => {
                const results = (await SyncService.ArrangeSyncWindows(newOperationId(), monitorIdx, arrangeIds)) ?? [];
                const failed = results.filter((r) => !r.ok);
                if (failed.length > 0) {
                  throw new Error(`部分窗口排列失败：${failed.map((r) => `${r.name}（${r.reason ?? "未知原因"}）`).join("；")}`);
                }
              }, "窗口已排列")
            }
          >
            <LayoutGrid size={15} />
            自动排列
          </button>
          <button
            type="button"
            className={btnSecondary}
            disabled={busy || !state?.hasArranged}
            onClick={() =>
              void run("restore", async () => {
                const results = (await SyncService.RestoreSyncWindows(newOperationId())) ?? [];
                const failed = results.filter((r) => !r.ok);
                if (failed.length > 0) {
                  throw new Error(`部分窗口恢复失败：${failed.map((r) => `${r.name}（${r.reason ?? "未知原因"}）`).join("；")}`);
                }
              }, "窗口位置已恢复")
            }
          >
            <Undo2 size={15} />
            恢复窗口
          </button>
          <select
            aria-label="显示器选择"
            className="h-9 rounded-md border border-(--border-strong) bg-(--input-bg) px-2 text-sm text-(--text-main)"
            value={monitorIdx}
            onChange={(e) => setMonitorIdx(Number(e.target.value))}
          >
            {monitors.length === 0 && <option value={0}>显示器 1</option>}
            {monitors.map((m) => (
              <option key={m.index} value={m.index}>
                显示器 {m.index + 1}（{m.width}×{m.height}，缩放 {Math.round(m.scale * 100)}%）{m.primary ? " · 主显示器" : ""}
              </option>
            ))}
          </select>
          {inSession && (
            <span className="ml-auto text-sm text-(--text-dim)">
              {phase === "paused" ? "已暂停" : "同步中"} · {state ? followerSummary(state.followers ?? []) : ""}
            </span>
          )}
        </div>

        {error && (
          <div className="mt-3 flex items-start gap-2 rounded-md border border-(--danger-border) bg-(--danger-bg) px-3 py-2 text-sm text-(--text-main)">
            <XCircle size={16} className="mt-0.5 shrink-0" />
            <span>{error}</span>
          </div>
        )}
        {notice && (
          <div className="mt-3 flex items-start gap-2 rounded-md bg-(--hover-bg) px-3 py-2 text-sm text-(--text-main)">
            <CheckCircle2 size={16} className="mt-0.5 shrink-0" />
            <span>{notice}</span>
          </div>
        )}
        {inSession && state?.masterError && (
          <p className="mt-2 text-xs text-(--text-dim)">主实例标签连接失败：{state.masterError}，网页输入同步暂不可用，正在自动重试。</p>
        )}
        {inSession && state && !state.tabMapOk && (
          <p className="mt-2 text-xs text-(--text-dim)">
            标签映射暂时无法确认（例如存在同名标签）：已暂停向跟随实例分发网页输入，会话保持运行，映射恢复后自动继续；期间不会向任何跟随实例发送可能发往错误标签的操作，新建、关闭标签仍会同步。
          </p>
        )}
        {inSession && (state?.uiSkipped ?? 0) > 0 && (
          <p className="mt-2 text-xs text-(--text-dim)">
            有 {state?.uiSkipped} 次浏览器界面操作未能在部分跟随实例上执行（见各跟随实例行内的未执行提示），未做模拟输入处理。
          </p>
        )}
      </div>

      <div className="mt-4 min-h-0 flex-1 overflow-y-auto rounded-md border border-(--border)">
        {options.length === 0 ? (
          <p className="px-4 py-6 text-sm text-(--text-faint)">没有实例。请先在「浏览器实例」中创建并启动实例。</p>
        ) : (
          <table className="w-full text-sm">
            <thead>
              <tr className="text-left text-xs text-(--text-faint)">
                <th className={thCls}>实例名称</th>
                <th className={`${thCls} w-24 text-center`}>主实例</th>
                <th className={`${thCls} w-24 text-center`}>跟随实例</th>
              </tr>
            </thead>
            <tbody>
              {options.map((o) => {
                const eligible = o.running && o.cdpSupported;
                return (
                  <tr key={o.id} className="border-b border-(--border) last:border-b-0 hover:bg-(--hover-bg)">
                    <td className="px-4 py-2.5">
                      <span className="text-(--text-main)">{o.name}</span>
                      <span className="mt-0.5 block text-xs text-(--text-faint)">{rowStatus(o)}</span>
                    </td>
                    <td className="px-4 py-2.5 text-center">
                      <input
                        type="radio"
                        name="sync-master"
                        aria-label={`${o.name} 设为主实例`}
                        disabled={inSession || !eligible}
                        checked={selMaster === o.id}
                        onClick={() => {
                          // onClick, not onChange: clicking the
                          // already-selected master must fire so it
                          // can be deselected again.
                          const next = toggleMaster(masterId, o.id);
                          setMasterId(next);
                          if (next !== "") setFollowers((f) => f.filter((x) => x !== next));
                        }}
                        onChange={() => undefined}
                      />
                    </td>
                    <td className="px-4 py-2.5 text-center">
                      <input
                        type="checkbox"
                        aria-label={`${o.name} 设为跟随实例`}
                        disabled={inSession || !eligible || selMaster === o.id}
                        checked={selFollowers.includes(o.id)}
                        onChange={() => setFollowers((f) => toggleFollower(f, o.id, masterId))}
                      />
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}
