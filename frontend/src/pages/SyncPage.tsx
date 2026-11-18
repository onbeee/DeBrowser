import { useCallback, useEffect, useRef, useState } from "react";
import {
  AlertTriangle,
  CheckCircle2,
  Monitor,
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
  eligibleInstances,
  followerSummary,
  supportLevels,
  toggleFollower,
  unsupportedInstances,
} from "./syncLogic";

/**
 * 多开同步页（Stage 10 v2）。同步采用语义操作：网页内容经各
 * 实例自己的调试连接执行，浏览器界面经浏览器语义命令与
 * UI Automation 执行，窗口经原生窗口管理调整。本页如实区分
 * 可靠支持、条件支持与当前无法可靠支持的操作，不以模拟
 * 输入伪装支持。
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
  const eligible = eligibleInstances(options);
  const unsupported = unsupportedInstances(options);

  const btnPrimary =
    "inline-flex h-9 items-center gap-1.5 rounded-md bg-(--accent) px-3 text-sm font-medium text-(--accent-contrast) transition hover:opacity-90 disabled:opacity-50";
  const btnSecondary =
    "inline-flex h-9 items-center gap-1.5 rounded-md border border-(--border-strong) bg-(--input-bg) px-3 text-sm text-(--text-main) transition hover:bg-(--hover-bg) disabled:opacity-50";

  const arrangeIds = [masterId, ...followers].filter((x) => x !== "");

  return (
    <div className="mx-auto max-w-5xl px-6 py-6">
      <p className="text-sm text-(--text-dim)">
        指定一个主实例和多个跟随实例，在主实例中的操作会同步到跟随实例。同步不共享各实例的登录状态与浏览数据。
      </p>

      {error && (
        <div className="mt-4 flex items-start gap-2 rounded-md border border-(--danger-border) bg-(--danger-bg) px-3 py-2 text-sm text-(--text-main)">
          <XCircle size={16} className="mt-0.5 shrink-0" />
          <span>{error}</span>
        </div>
      )}
      {notice && (
        <div className="mt-4 flex items-start gap-2 rounded-md bg-(--hover-bg) px-3 py-2 text-sm text-(--text-main)">
          <CheckCircle2 size={16} className="mt-0.5 shrink-0" />
          <span>{notice}</span>
        </div>
      )}

      {!inSession && (
        <div className="mt-6 grid gap-6 md:grid-cols-2">
          <section>
            <h3 className="text-sm font-medium text-(--text-main)">主实例</h3>
            <div className="mt-2 space-y-1.5">
              {eligible.length === 0 && (
                <p className="text-sm text-(--text-faint)">没有正在运行且支持同步的实例。请先启动实例。</p>
              )}
              {eligible.map((o) => (
                <label
                  key={o.id}
                  className="flex cursor-pointer items-center gap-2 rounded-md border border-(--border) px-3 py-2 text-sm text-(--text-main) hover:bg-(--hover-bg)"
                >
                  <input
                    type="radio"
                    name="sync-master"
                    checked={masterId === o.id}
                    onChange={() => {
                      setMasterId(o.id);
                      setFollowers((f) => f.filter((x) => x !== o.id));
                    }}
                  />
                  {o.name}
                </label>
              ))}
            </div>
          </section>
          <section>
            <h3 className="text-sm font-medium text-(--text-main)">跟随实例</h3>
            <div className="mt-2 space-y-1.5">
              {eligible.length === 0 && (
                <p className="text-sm text-(--text-faint)">没有可用的跟随实例。</p>
              )}
              {eligible.map((o) => (
                <label
                  key={o.id}
                  className="flex cursor-pointer items-center gap-2 rounded-md border border-(--border) px-3 py-2 text-sm text-(--text-main) hover:bg-(--hover-bg)"
                >
                  <input
                    type="checkbox"
                    disabled={o.id === masterId}
                    checked={followers.includes(o.id)}
                    onChange={() => setFollowers((f) => toggleFollower(f, o.id, masterId))}
                  />
                  {o.name}
                  {o.id === masterId && <span className="text-xs text-(--text-faint)">（主实例）</span>}
                </label>
              ))}
            </div>
          </section>
        </div>
      )}

      {unsupported.length > 0 && !inSession && (
        <div className="mt-4 flex items-start gap-2 rounded-md bg-(--warn-bg) px-3 py-2 text-sm text-(--text-main)">
          <AlertTriangle size={16} className="mt-0.5 shrink-0" />
          <span>
            {unsupported.map((o) => o.name).join("、")}
            正在运行，但不是以同步模式启动的，关闭后重新启动即可参与同步。
          </span>
        </div>
      )}

      {!inSession ? (
        <div className="mt-6 flex flex-wrap items-center gap-2">
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
        </div>
      ) : (
        <div className="mt-6 rounded-md border border-(--border) px-4 py-3">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <p className="text-sm text-(--text-main)">
              主实例：{state?.masterName || "—"}
              <span className="ml-3 text-(--text-dim)">{state ? followerSummary(state.followers ?? []) : ""}</span>
              <span className="ml-3 text-(--text-faint)">{phase === "paused" ? "已暂停" : "同步中"}</span>
            </p>
            <div className="flex gap-2">
              {phase === "running" ? (
                <button
                  type="button"
                  className={btnSecondary}
                  disabled={busy}
                  onClick={() => void run("pause", async () => { await SyncService.PauseSync(newOperationId()); }, "同步已暂停")}
                >
                  <Pause size={15} />
                  暂停同步
                </button>
              ) : (
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
              <button
                type="button"
                className={btnSecondary}
                disabled={busy}
                onClick={() => void run("stop", async () => { await SyncService.StopSync(newOperationId()); }, "同步已停止，浏览器保持运行")}
              >
                <Square size={15} />
                停止同步
              </button>
            </div>
          </div>
          <p className="mt-1 text-xs text-(--text-faint)">主实例界面能力：{state ? capsSummary(state.masterCaps) : ""}</p>
          {state?.masterError && (
            <p className="mt-1 text-xs text-(--text-dim)">主实例标签连接失败：{state.masterError}，网页输入同步暂不可用，正在自动重试。</p>
          )}
          {state && !state.tabMapOk && (
            <p className="mt-1 text-xs text-(--text-dim)">
              标签映射暂时无法确认（例如存在同名标签）：已暂停向跟随实例分发网页输入，会话保持运行，映射恢复后自动继续；期间不会向任何跟随实例发送可能发往错误标签的操作，新建、关闭标签仍会同步。
            </p>
          )}
          <ul className="mt-3 space-y-2">
            {(state?.followers ?? []).map((f) => (
              <li key={f.id} className="flex items-start justify-between gap-3 rounded-md bg-(--hover-bg) px-3 py-2">
                <div>
                  <p className="text-sm text-(--text-main)">{f.name}</p>
                  <p className="text-xs text-(--text-faint)">{capsSummary(f.caps)}</p>
                  {f.status === "failed" && f.reason && (
                    <p className="mt-0.5 text-xs text-(--text-dim)">失败原因：{f.reason}</p>
                  )}
                  {f.status === "suspended" && f.reason && (
                    <p className="mt-0.5 text-xs text-(--text-dim)">等待标签映射：{f.reason}</p>
                  )}
                  {f.opError && (
                    <p className="mt-0.5 text-xs text-(--text-dim)">最近一次界面操作未执行：{f.opError}</p>
                  )}
                </div>
                <span className="shrink-0 text-xs text-(--text-dim)">{f.status === "failed" ? "失败" : f.status === "suspended" ? "已暂停" : "正常"}</span>
              </li>
            ))}
          </ul>
          {(state?.uiSkipped ?? 0) > 0 && (
            <p className="mt-3 text-xs text-(--text-dim)">
              有 {state?.uiSkipped} 次浏览器界面操作未能在部分跟随实例上执行（见各跟随实例的未执行提示），未做模拟输入处理。
            </p>
          )}
        </div>
      )}

      <section className="mt-8">
        <h3 className="flex items-center gap-2 text-sm font-medium text-(--text-main)">
          <Monitor size={15} />
          窗口排列
        </h3>
        <div className="mt-2 flex flex-wrap items-center gap-2">
          <select
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
            自动排列所选实例
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
            恢复窗口位置
          </button>
        </div>
        <p className="mt-1 text-xs text-(--text-faint)">排列对象为上方所选的主实例与跟随实例（主实例排在首位）；不依赖同步会话是否正在运行。</p>
      </section>

      <section className="mt-8">
        <h3 className="text-sm font-medium text-(--text-main)">支持范围</h3>
        <ul className="mt-2 space-y-2">
          {supportLevels().map((s) => (
            <li key={s.label} className="rounded-md border border-(--border) px-3 py-2">
              <p className="text-sm text-(--text-main)">
                {s.label}
                <span className="ml-2 text-xs text-(--text-faint)">
                  {s.level === "reliable" ? "可靠支持" : s.level === "conditional" ? "条件支持" : "当前无法可靠支持"}
                </span>
              </p>
              <p className="mt-0.5 text-xs leading-relaxed text-(--text-dim)">{s.detail}</p>
            </li>
          ))}
        </ul>
      </section>
    </div>
  );
}
