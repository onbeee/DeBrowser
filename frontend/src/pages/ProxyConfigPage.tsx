import { useCallback, useEffect, useRef, useState } from "react";
import {
  ArrowDown,
  ArrowUp,
  FileUp,
  Gauge,
  Loader2,
  Play,
  Plus,
  RefreshCw,
  Settings,
  Square,
  Trash2,
} from "lucide-react";
import {
  ProxyService,
  type BindingView,
  type ConnectionView,
  type CoreLogsView,
  type CoreStatusView,
  type CustomRuleView,
  type NodeView,
  type ProxySettingsView,
  type RulePolicyView,
  type SourceView,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import { NodeCapsule, asNodeState } from "../components/NodeCapsule";
import { notifyError, notifySuccess, notifyWarning } from "../stores/useNotificationStore";
import { newOperationId, recordOperationFeedback } from "../lib/operations";

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

// 用户在系统文件选择器中取消选择时，后端以「未选择文件」
// 返回。这不是错误：静默结束，不显示任何提示。
const PICKER_CANCELLED = "未选择文件";

const btnPrimary =
  "flex h-9 shrink-0 items-center gap-1.5 rounded-md bg-(--accent) px-3 text-sm font-medium text-(--accent-contrast) hover:opacity-90 disabled:opacity-50";
const btnSecondary =
  "flex h-9 shrink-0 items-center gap-1.5 rounded-md border border-(--border-strong) bg-(--input-bg) px-3 text-sm text-(--text-main) hover:bg-(--hover-bg) disabled:opacity-50";
const btnSmall =
  "flex h-7 shrink-0 items-center gap-1 rounded-md border border-(--border-strong) bg-(--input-bg) px-2 text-xs text-(--text-main) hover:bg-(--hover-bg) disabled:opacity-50";
const inputCls =
  "h-9 w-full rounded-md border border-(--border-strong) bg-(--input-bg) px-2.5 text-sm text-(--text-main) outline-none focus:border-(--accent)";
const labelCls = "mb-1 block text-xs text-(--text-dim)";

interface AddSubState {
  name: string;
  url: string;
}

const POLICY_OPTIONS: { value: string; label: string }[] = [
  { value: "proxy", label: "走实例节点" },
  { value: "direct", label: "直连" },
  { value: "reject", label: "拦截" },
  { value: "disabled", label: "停用" },
];

const CUSTOM_POLICY_OPTIONS = POLICY_OPTIONS.slice(0, 3);

const RULE_TYPE_OPTIONS: { value: string; label: string }[] = [
  { value: "DOMAIN-SUFFIX", label: "域名后缀" },
  { value: "DOMAIN-KEYWORD", label: "域名关键词" },
  { value: "IP-CIDR", label: "IP 段" },
];

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

function ruleLabel(c: ConnectionView): string {
  if (c.rule === "RuleSet" && c.rulePayload) return `规则集「${c.rulePayload}」`;
  if (c.rule === "Match") return "兜底（走实例节点）";
  if (c.rule === "GeoIP") return `GeoIP ${c.rulePayload ?? ""}`.trim();
  return c.rulePayload ? `${c.rule}（${c.rulePayload}）` : c.rule;
}

/**
 * 代理配置（Stage 6C）：Mihomo 核心为全部浏览器实例提供代理。
 * 本页面管理：节点来源（订阅 / 本地 YAML）、节点列表与测速、
 * 核心运行状态与实例绑定概览、可编辑的分流规则（内置规则集
 * 策略 + 自定义置顶规则）、核心设置，以及实时连接与日志查看。
 * 节点解析、规则匹配与连接全部由 Mihomo 核心完成；实例具体
 * 使用哪个节点在「浏览器实例」的新建 / 编辑表单中选择。
 */
export default function ProxyConfigPage() {
  const [core, setCore] = useState<CoreStatusView | null>(null);
  const [sources, setSources] = useState<SourceView[]>([]);
  const [nodes, setNodes] = useState<NodeView[]>([]);
  const [bindings, setBindings] = useState<BindingView[]>([]);
  const [delays, setDelays] = useState<Map<string, number>>(new Map());
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [refreshError, setRefreshError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [showSettings, setShowSettings] = useState(false);
  const [addSub, setAddSub] = useState<AddSubState | null>(null);
  const [addFileName, setAddFileName] = useState<string | null>(null);
  const [confirmDelete, setConfirmDelete] = useState<string | null>(null);
  const loadedRef = useRef(false);
  // 分流规则（内置策略 + 自定义置顶规则）与核心设置表单。
  const [policies, setPolicies] = useState<RulePolicyView[]>([]);
  const [customRules, setCustomRules] = useState<CustomRuleView[]>([]);
  const [ruleDrafts, setRuleDrafts] = useState<Map<string, CustomRuleView>>(new Map());
  const [newRule, setNewRule] = useState({ ruleType: "DOMAIN-SUFFIX", value: "", policy: "proxy" });
  const [settingsForm, setSettingsForm] = useState<ProxySettingsView | null>(null);
  // 实时连接与日志查看。
  const [inspectTab, setInspectTab] = useState<"connections" | "logs">("connections");
  const [connections, setConnections] = useState<ConnectionView[]>([]);
  const [connInstance, setConnInstance] = useState("");
  const [logLines, setLogLines] = useState<{ seq: number; level: string; text: string }[]>([]);
  const logSeqRef = useRef(0);

  // 返回错误信息（成功返回 null）。失败保留已有数据。
  const reload = useCallback(async (): Promise<string | null> => {
    try {
      const [status, srcs, nds, bds, pols, customs] = await Promise.all([
        ProxyService.GetCoreStatus(),
        ProxyService.ListSources(),
        ProxyService.ListNodes(),
        ProxyService.ListBindings(),
        ProxyService.GetRulePolicies(),
        ProxyService.ListCustomRules(),
      ]);
      setCore(status);
      setSources(srcs ?? []);
      setNodes(nds ?? []);
      setBindings(bds ?? []);
      setPolicies(pols ?? []);
      setCustomRules(customs ?? []);
      setRuleDrafts(new Map());
      loadedRef.current = true;
      setLoadError(null);
      setRefreshError(null);
      return null;
    } catch (e) {
      const message = errText(e);
      if (!loadedRef.current) setLoadError(message);
      else setRefreshError(message);
      return message;
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void reload().then((message) => {
      if (message) notifyError(`加载代理信息失败：${message}`);
    });
  }, [reload]);

  // run executes one user operation with the shared operation-ID
  // and notification discipline (same pattern as the other pages).
  const run = async (
    key: string,
    action: string,
    fn: (opId: string) => Promise<string | void>,
    silentCancel = false,
  ) => {
    setBusy(key);
    const opId = newOperationId();
    try {
      const feedback = await fn(opId);
      if (feedback) {
        notifySuccess(feedback, { operationId: opId, important: true });
        recordOperationFeedback(opId, true, feedback, "代理配置", action);
        const refreshErr = await reload();
        if (refreshErr) notifyWarning("列表刷新失败，请重新加载。");
      }
    } catch (e) {
      const message = errText(e);
      if (!(silentCancel && message === PICKER_CANCELLED)) {
        notifyError(message, { operationId: opId, important: true });
        recordOperationFeedback(opId, false, message, "代理配置", action);
      }
    } finally {
      setBusy(null);
    }
  };

  const startCore = () =>
    run("core-start", "proxy.core_start", async (opId) => {
      await ProxyService.StartCore(opId);
      return "代理核心已启动";
    });

  const stopCore = () =>
    run("core-stop", "proxy.core_stop", async (opId) => {
      await ProxyService.StopCore(opId);
      return "代理核心已停止";
    });

  const submitAddSub = () => {
    if (!addSub) return;
    const { name, url } = addSub;
    void run("source-add-url", "proxy.source_add_url", async (opId) => {
      const view = await ProxyService.AddURLSource(opId, name, url);
      setAddSub(null);
      if (view.state === "ok") return `已添加订阅：${view.name}（${view.nodeCount} 个节点）`;
      // 来源已保存但核心未能给出节点：如实提示，不报成功。
      throw new Error(view.message || "订阅已保存，但未能获取节点");
    });
  };

  const addFile = () =>
    run(
      "source-add-file",
      "proxy.source_add_file",
      async (opId) => {
        const view = await ProxyService.AddFileSource(opId, addFileName ?? "");
        setAddFileName(null);
        if (view.state === "ok") return `已导入本地文件：${view.name}（${view.nodeCount} 个节点）`;
        throw new Error(view.message || "文件已导入，但未能获取节点");
      },
      true,
    );

  const refreshSource = (src: SourceView) =>
    run(`source-refresh-${src.id}`, "proxy.source_refresh", async (opId) => {
      await ProxyService.RefreshSource(opId, src.id);
      return `订阅已更新：${src.name}`;
    });

  const reimportSource = (src: SourceView) =>
    run(
      `source-reimport-${src.id}`,
      "proxy.source_reimport",
      async (opId) => {
        const view = await ProxyService.ReimportFileSource(opId, src.id);
        if (view.state === "ok") return `已重新导入：${view.name}（${view.nodeCount} 个节点）`;
        throw new Error(view.message || "文件已导入，但未能获取节点");
      },
      true,
    );

  const deleteSource = (src: SourceView) =>
    run(`source-delete-${src.id}`, "proxy.source_delete", async (opId) => {
      await ProxyService.DeleteSource(opId, src.id);
      setConfirmDelete(null);
      return `已删除来源：${src.name}`;
    });

  const testDelays = () =>
    run("delay-test", "proxy.delay_test", async (opId) => {
      const results = await ProxyService.TestDelays(opId);
      const map = new Map<string, number>();
      for (const r of results ?? []) map.set(`${r.sourceId}${r.name}`, r.delay);
      setDelays(map);
      return `测速完成：${map.size} 个节点`;
    });

  const delayOf = (n: NodeView): string => {
    const tested = delays.get(`${n.sourceId}${n.name}`);
    if (tested !== undefined) return tested < 0 ? "超时" : `${tested} ms`;
    return n.delay > 0 ? `${n.delay} ms` : "—";
  };

  // ---- 分流规则 ----
  const setPolicy = (p: RulePolicyView, policy: string) =>
    run(`policy-${p.name}`, "proxy.rule_policy_set", async (opId) => {
      await ProxyService.SetRulePolicy(opId, p.name, policy);
      return `规则集「${p.name}」已改为「${POLICY_OPTIONS.find((o) => o.value === policy)?.label ?? policy}」`;
    });

  const resetPolicies = () =>
    run("policies-reset", "proxy.rule_policies_reset", async (opId) => {
      await ProxyService.ResetRulePolicies(opId);
      return "内置规则集策略已恢复默认";
    });

  const refreshRules = () =>
    run("rules-refresh", "proxy.rule_providers_refresh", async (opId) => {
      await ProxyService.RefreshRuleProviders(opId);
      return "规则集已更新";
    });

  const addCustomRule = () =>
    run("custom-add", "proxy.custom_rule_add", async (opId) => {
      await ProxyService.AddCustomRule(opId, newRule.ruleType, newRule.value, newRule.policy);
      setNewRule({ ...newRule, value: "" });
      return "自定义规则已添加";
    });

  const saveCustomRule = (draft: CustomRuleView) =>
    run(`custom-save-${draft.id}`, "proxy.custom_rule_update", async (opId) => {
      await ProxyService.UpdateCustomRule(opId, draft.id, draft.ruleType, draft.value, draft.policy, draft.enabled);
      return "自定义规则已保存";
    });

  const toggleCustomRule = (r: CustomRuleView) =>
    run(`custom-toggle-${r.id}`, "proxy.custom_rule_update", async (opId) => {
      await ProxyService.UpdateCustomRule(opId, r.id, r.ruleType, r.value, r.policy, !r.enabled);
      return r.enabled ? "自定义规则已停用" : "自定义规则已启用";
    });

  const deleteCustomRule = (r: CustomRuleView) =>
    run(`custom-delete-${r.id}`, "proxy.custom_rule_delete", async (opId) => {
      await ProxyService.DeleteCustomRule(opId, r.id);
      return "自定义规则已删除";
    });

  const moveCustomRule = (index: number, dir: -1 | 1) => {
    const next = [...customRules];
    const target = index + dir;
    if (target < 0 || target >= next.length) return;
    [next[index], next[target]] = [next[target], next[index]];
    void run("custom-reorder", "proxy.custom_rule_reorder", async (opId) => {
      await ProxyService.ReorderCustomRules(
        opId,
        next.map((r) => r.id),
      );
      return "自定义规则顺序已更新";
    });
  };

  const draftOf = (r: CustomRuleView): CustomRuleView => ruleDrafts.get(r.id) ?? r;
  const setDraft = (r: CustomRuleView, patch: Partial<CustomRuleView>) =>
    setRuleDrafts((prev) => new Map(prev).set(r.id, { ...draftOf(r), ...patch }));

  // ---- 核心设置 ----
  const openSettings = async () => {
    try {
      setSettingsForm(await ProxyService.GetProxySettings());
      setShowSettings(true);
    } catch (e) {
      notifyError(`读取代理设置失败：${errText(e)}`, { important: true });
    }
  };

  const saveSettings = () => {
    if (!settingsForm) return;
    const form = settingsForm;
    void run("settings-save", "proxy.settings_update", async (opId) => {
      await ProxyService.UpdateProxySettings(opId, form);
      setShowSettings(false);
      return "代理设置已保存";
    });
  };

  const coreRunning = core?.state === "running";

  // 实时连接轮询（仅在查看连接且核心运行时）。
  useEffect(() => {
    if (inspectTab !== "connections" || !coreRunning) return;
    let alive = true;
    const fetchOnce = async () => {
      try {
        const list = await ProxyService.ListConnections(connInstance);
        if (alive) setConnections(list ?? []);
      } catch {
        /* 轮询失败静默：下一次轮询会重试 */
      }
    };
    void fetchOnce();
    const timer = setInterval(() => void fetchOnce(), 2500);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, [inspectTab, coreRunning, connInstance]);

  // 核心日志轮询（仅在查看日志且核心运行时）：按序号增量拉取。
  useEffect(() => {
    if (inspectTab !== "logs" || !coreRunning) return;
    let alive = true;
    const fetchOnce = async () => {
      try {
        const view: CoreLogsView = await ProxyService.GetCoreLogs(logSeqRef.current);
        if (!alive) return;
        if (view.lines && view.lines.length > 0) {
          logSeqRef.current = view.latestSeq;
          setLogLines((prev) => [...prev, ...(view.lines ?? [])].slice(-500));
        }
      } catch {
        /* 轮询失败静默 */
      }
    };
    void fetchOnce();
    const timer = setInterval(() => void fetchOnce(), 2000);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, [inspectTab, coreRunning]);

  return (
    <div className="mx-auto flex h-full max-w-5xl flex-col px-6 py-6">
      {/* 顶部功能栏：按功能组整体换行（来源组 / 测速组 / 核心组）。 */}
      <div className="flex shrink-0 flex-wrap items-center gap-x-3 gap-y-2">
        <div className="flex shrink-0 flex-nowrap items-center gap-2">
          <button type="button" className={btnPrimary} disabled={busy != null} onClick={() => setAddSub({ name: "", url: "" })}>
            <Plus size={14} /> 添加订阅
          </button>
          <button type="button" className={btnSecondary} disabled={busy != null} onClick={() => setAddFileName("")}>
            <FileUp size={14} /> 导入本地 YAML
          </button>
        </div>
        <div className="flex shrink-0 flex-nowrap items-center gap-2">
          <button type="button" className={btnSecondary} disabled={busy != null} onClick={testDelays}>
            {busy === "delay-test" ? <Loader2 size={14} className="animate-spin" /> : <Gauge size={14} />} 节点测速
          </button>
        </div>
        <div className="flex shrink-0 flex-nowrap items-center gap-2">
          {coreRunning ? (
            <button type="button" className={btnSecondary} disabled={busy != null} onClick={stopCore}>
              {busy === "core-stop" ? <Loader2 size={14} className="animate-spin" /> : <Square size={14} />} 停止核心
            </button>
          ) : (
            <button type="button" className={btnSecondary} disabled={busy != null} onClick={startCore}>
              {busy === "core-start" ? <Loader2 size={14} className="animate-spin" /> : <Play size={14} />} 启动核心
            </button>
          )}
          <button type="button" className={btnSecondary} disabled={busy != null} onClick={() => void openSettings()}>
            <Settings size={14} /> 设置
          </button>
        </div>
      </div>

      {refreshError && (
        <div className="mt-4 flex items-center gap-2 rounded-lg border border-(--border) bg-(--panel-bg) px-3 py-2 text-sm text-(--text-dim)">
          <span className="flex-1">列表刷新失败：{refreshError}</span>
          <button type="button" className="shrink-0 font-medium text-(--accent) underline underline-offset-2" onClick={() => void reload()}>
            重新加载
          </button>
        </div>
      )}

      <div className="mt-5 min-h-0 flex-1 overflow-y-auto">
        {loading ? (
          <p className="py-10 text-center text-sm text-(--text-faint)">加载中…</p>
        ) : loadError ? (
          <div className="flex flex-col items-center px-4 py-12 text-center">
            <p className="text-sm text-(--danger)">代理信息加载失败：{loadError}</p>
            <button
              type="button"
              className="mt-3 rounded-md border border-(--border-strong) bg-(--input-bg) px-3 py-1.5 text-sm text-(--text-main) hover:bg-(--hover-bg)"
              onClick={() => void reload()}
            >
              重新加载
            </button>
          </div>
        ) : (
          <div className="space-y-6 pb-2">
            {/* 核心状态 */}
            <section>
              <h3 className="text-sm font-semibold">代理核心</h3>
              <div className="mt-2 rounded-lg border border-(--border) bg-(--panel-bg) px-4 py-3 text-sm">
                {core?.state === "running" ? (
                  <p className="text-(--text-dim)">
                    运行中{core.version ? ` · Mihomo ${core.version}` : ""}。每个已绑定实例使用各自的本地入口与所选节点。
                  </p>
                ) : core?.state === "error" ? (
                  <p className="text-(--danger)">核心异常{core.message ? `：${core.message}` : ""}</p>
                ) : (
                  <p className="text-(--text-dim)">
                    未运行{core && !core.binaryReady ? "（手动开启时将下载官方核心）" : ""}。核心只由您手动开启，DeBrowser 启动时不会自动开启。
                    {core?.message ? ` ${core.message}` : ""}
                  </p>
                )}
              </div>
            </section>

            {/* 来源 */}
            <section>
              <h3 className="text-sm font-semibold">节点来源</h3>
              <p className="mt-1 text-xs text-(--text-faint)">
                本地 YAML 需为包含 proxies 节点列表的 Mihomo 节点文件；完整 Clash 配置中的代理组、规则与 DNS
                等内容不会被采用，仅提取其中的节点。订阅与本地文件的节点都由 Mihomo 核心解析。
              </p>
              <div className="mt-2 overflow-hidden rounded-lg border border-(--border) bg-(--panel-bg)">
                {sources.length === 0 ? (
                  <p className="px-4 py-8 text-center text-sm text-(--text-faint)">
                    尚未添加任何来源。点击左上角「添加订阅」或「导入本地 YAML」获取代理节点。
                  </p>
                ) : (
                  <ul className="divide-y divide-(--border)">
                    {sources.map((src) => (
                      <li key={src.id} className="px-4 py-3 hover:bg-(--hover-bg)">
                        <div className="flex flex-wrap items-center gap-2">
                          <span className="text-sm font-medium">{src.name}</span>
                          <span className="rounded bg-(--hover-bg) px-1.5 py-0.5 text-xs text-(--text-dim)">
                            {src.kind === "url" ? "订阅" : "本地文件"}
                          </span>
                          <span className="text-xs text-(--text-faint)">
                            {src.nodeCount >= 0 ? `${src.nodeCount} 个节点` : "节点数未知"}
                            {src.boundInstances > 0 ? ` · ${src.boundInstances} 个实例在用` : ""}
                          </span>
                          <span className="ml-auto flex shrink-0 items-center gap-2">
                            {src.kind === "url" ? (
                              <button type="button" className={btnSmall} disabled={busy != null} onClick={() => refreshSource(src)}>
                                {busy === `source-refresh-${src.id}` ? <Loader2 size={12} className="animate-spin" /> : <RefreshCw size={12} />} 更新
                              </button>
                            ) : (
                              <button type="button" className={btnSmall} disabled={busy != null} onClick={() => reimportSource(src)}>
                                <FileUp size={12} /> 重新导入
                              </button>
                            )}
                            {confirmDelete === src.id ? (
                              <>
                                <button
                                  type="button"
                                  className="flex h-7 shrink-0 items-center gap-1 rounded-md bg-(--danger) px-2 text-xs font-medium text-white hover:opacity-90 disabled:opacity-50"
                                  disabled={busy != null}
                                  onClick={() => deleteSource(src)}
                                >
                                  确认删除
                                </button>
                                <button type="button" className={btnSmall} disabled={busy != null} onClick={() => setConfirmDelete(null)}>
                                  取消
                                </button>
                              </>
                            ) : (
                              <button type="button" className={btnSmall} disabled={busy != null} onClick={() => setConfirmDelete(src.id)}>
                                <Trash2 size={12} /> 删除
                              </button>
                            )}
                          </span>
                        </div>
                        {src.message && (
                          <p className={`mt-1 text-xs ${src.state === "error" || src.state === "empty" ? "text-(--danger)" : "text-(--text-faint)"}`}>
                            {src.message}
                          </p>
                        )}
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            </section>

            {/* 节点列表 */}
            <section>
              <h3 className="text-sm font-semibold">节点列表</h3>
              <div className="mt-2 overflow-hidden rounded-lg border border-(--border) bg-(--panel-bg)">
                {!coreRunning ? (
                  <p className="px-4 py-8 text-center text-sm text-(--text-faint)">核心未运行，暂无节点信息。</p>
                ) : nodes.length === 0 ? (
                  <p className="px-4 py-8 text-center text-sm text-(--text-faint)">暂无节点。请先添加订阅或导入本地 YAML。</p>
                ) : (
                  <ul className="divide-y divide-(--border)">
                    {nodes.map((n) => (
                      <li key={`${n.sourceId}${n.name}`} className="flex items-center gap-3 px-4 py-2.5 hover:bg-(--hover-bg)">
                        <span className={`h-2 w-2 shrink-0 rounded-full ${n.alive ? "bg-(--success)" : "bg-(--danger)"}`} />
                        <span className="min-w-0 flex-1 truncate text-sm" title={n.name}>{n.name}</span>
                        <span className="shrink-0 text-xs text-(--text-faint)">{n.sourceName}</span>
                        <span className="shrink-0 text-xs text-(--text-faint)">{n.type}</span>
                        <span className="w-16 shrink-0 text-right text-xs text-(--text-dim)">{delayOf(n)}</span>
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            </section>

            {/* 实例绑定 */}
            <section>
              <h3 className="text-sm font-semibold">实例代理绑定</h3>
              <div className="mt-2 overflow-hidden rounded-lg border border-(--border) bg-(--panel-bg)">
                {bindings.length === 0 ? (
                  <p className="px-4 py-8 text-center text-sm text-(--text-faint)">
                    尚无实例绑定代理。在「浏览器实例」的新建 / 编辑表单中为实例选择节点。
                  </p>
                ) : (
                  <ul className="divide-y divide-(--border)">
                    {bindings.map((b) => (
                      <li key={b.instanceId} className="px-4 py-2.5 hover:bg-(--hover-bg)">
                        <div className="flex flex-wrap items-center gap-2 text-sm">
                          <span className="font-medium">{b.instanceName || b.instanceId}</span>
                          <span className="text-(--text-faint)">→</span>
                          <NodeCapsule name={b.nodeName} state={asNodeState(b.nodeState)} />
                          <span className="text-xs text-(--text-faint)">
                            {b.sourceName} · 本地端口 {b.port} ·{" "}
                            {b.routeMode === "global"
                              ? "全局代理"
                              : b.routeMode === "direct"
                                ? "直连"
                                : "规则分流"}
                          </span>
                        </div>
                        {(b.nodeState === "missing" || b.nodeState === "down" || b.listenerState === "down") && (
                          <p className="mt-1 text-xs text-(--danger)">
                            {b.nodeState === "missing"
                              ? "该节点已从来源中消失。绑定保持不变，节点重新出现后将自动恢复使用。"
                              : ""}
                            {b.nodeState === "down" ? "该节点当前连接失败。绑定保持不变，不会改用其他节点。" : ""}
                            {b.listenerState === "down" ? "该实例的本地监听端口当前无响应，实例暂时无法使用代理。" : ""}
                          </p>
                        )}
                      </li>
                    ))}
                  </ul>
                )}
              </div>
            </section>
            {/* 分流规则 */}
            <section>
              <div className="flex flex-wrap items-center gap-2">
                <h3 className="text-sm font-semibold">分流规则</h3>
                <span className="ml-auto flex shrink-0 items-center gap-2">
                  <button type="button" className={btnSmall} disabled={busy != null} onClick={resetPolicies}>
                    恢复默认
                  </button>
                  <button type="button" className={btnSmall} disabled={busy != null || !coreRunning} onClick={refreshRules}>
                    {busy === "rules-refresh" ? <Loader2 size={12} className="animate-spin" /> : <RefreshCw size={12} />} 立即更新规则集
                  </button>
                </span>
              </div>
              <p className="mt-1 text-xs text-(--text-faint)">
                内置规则集仅对「规则分流」模式的实例生效：命中后按所选策略走该实例自己的节点、直连或拦截；停用后该规则集不再参与分流。修改会立即应用到运行中的核心，无需重启实例。
              </p>
              <div className="mt-2 overflow-hidden rounded-lg border border-(--border) bg-(--panel-bg)">
                <ul className="divide-y divide-(--border)">
                  {policies.map((p) => (
                    <li key={p.name} className="flex flex-wrap items-center gap-2 px-4 py-2">
                      <span className="w-24 shrink-0 text-sm">{p.name}</span>
                      {p.policy !== p.defaultPolicy && <span className="text-xs text-(--text-faint)">已修改</span>}
                      <span className="ml-auto flex shrink-0 items-center gap-1">
                        {POLICY_OPTIONS.map((o) => (
                          <button
                            key={o.value}
                            type="button"
                            disabled={busy != null}
                            onClick={() => p.policy !== o.value && setPolicy(p, o.value)}
                            className={`h-7 rounded-md px-2 text-xs disabled:opacity-50 ${
                              p.policy === o.value
                                ? "bg-(--accent) font-medium text-(--accent-contrast)"
                                : "border border-(--border-strong) bg-(--input-bg) text-(--text-main) hover:bg-(--hover-bg)"
                            }`}
                          >
                            {o.label}
                          </button>
                        ))}
                      </span>
                    </li>
                  ))}
                </ul>
              </div>

              {/* 自定义置顶规则 */}
              <h4 className="mt-4 text-sm font-medium">自定义置顶规则</h4>
              <p className="mt-1 text-xs text-(--text-faint)">
                自定义规则排在所有内置规则集之前逐条匹配，先命中先执行；「走实例节点」对每个实例都指它自己绑定的节点。IP 段规则自动以 no-resolve 方式匹配。
              </p>
              <div className="mt-2 overflow-hidden rounded-lg border border-(--border) bg-(--panel-bg)">
                {customRules.length === 0 ? (
                  <p className="px-4 py-6 text-center text-sm text-(--text-faint)">暂无自定义规则。</p>
                ) : (
                  <ul className="divide-y divide-(--border)">
                    {customRules.map((r, i) => {
                      const draft = draftOf(r);
                      const dirty =
                        draft.ruleType !== r.ruleType || draft.value !== r.value || draft.policy !== r.policy;
                      return (
                        <li key={r.id} className="flex flex-wrap items-center gap-2 px-4 py-2">
                          <select
                            className="h-8 shrink-0 rounded-md border border-(--border-strong) bg-(--input-bg) px-1.5 text-xs text-(--text-main)"
                            value={draft.ruleType}
                            disabled={busy != null}
                            onChange={(e) => setDraft(r, { ruleType: e.target.value })}
                          >
                            {RULE_TYPE_OPTIONS.map((o) => (
                              <option key={o.value} value={o.value}>
                                {o.label}
                              </option>
                            ))}
                          </select>
                          <input
                            className="h-8 min-w-36 flex-1 rounded-md border border-(--border-strong) bg-(--input-bg) px-2 text-sm text-(--text-main) outline-none focus:border-(--accent)"
                            value={draft.value}
                            disabled={busy != null}
                            onChange={(e) => setDraft(r, { value: e.target.value })}
                            placeholder={draft.ruleType === "IP-CIDR" ? "例如 10.0.0.0/8" : "例如 example.com"}
                          />
                          <select
                            className="h-8 shrink-0 rounded-md border border-(--border-strong) bg-(--input-bg) px-1.5 text-xs text-(--text-main)"
                            value={draft.policy}
                            disabled={busy != null}
                            onChange={(e) => setDraft(r, { policy: e.target.value })}
                          >
                            {CUSTOM_POLICY_OPTIONS.map((o) => (
                              <option key={o.value} value={o.value}>
                                {o.label}
                              </option>
                            ))}
                          </select>
                          <button
                            type="button"
                            className={btnSmall}
                            disabled={busy != null}
                            onClick={() => toggleCustomRule(r)}
                          >
                            {r.enabled ? "已启用" : "已停用"}
                          </button>
                          {dirty && (
                            <button type="button" className={btnSmall} disabled={busy != null} onClick={() => saveCustomRule(draft)}>
                              保存
                            </button>
                          )}
                          <button type="button" className={btnSmall} disabled={busy != null || i === 0} onClick={() => moveCustomRule(i, -1)}>
                            <ArrowUp size={12} />
                          </button>
                          <button
                            type="button"
                            className={btnSmall}
                            disabled={busy != null || i === customRules.length - 1}
                            onClick={() => moveCustomRule(i, 1)}
                          >
                            <ArrowDown size={12} />
                          </button>
                          <button type="button" className={btnSmall} disabled={busy != null} onClick={() => deleteCustomRule(r)}>
                            <Trash2 size={12} />
                          </button>
                        </li>
                      );
                    })}
                  </ul>
                )}
                <div className="flex flex-wrap items-center gap-2 border-t border-(--border) px-4 py-2.5">
                  <select
                    className="h-8 shrink-0 rounded-md border border-(--border-strong) bg-(--input-bg) px-1.5 text-xs text-(--text-main)"
                    value={newRule.ruleType}
                    disabled={busy != null}
                    onChange={(e) => setNewRule({ ...newRule, ruleType: e.target.value })}
                  >
                    {RULE_TYPE_OPTIONS.map((o) => (
                      <option key={o.value} value={o.value}>
                        {o.label}
                      </option>
                    ))}
                  </select>
                  <input
                    className="h-8 min-w-36 flex-1 rounded-md border border-(--border-strong) bg-(--input-bg) px-2 text-sm text-(--text-main) outline-none focus:border-(--accent)"
                    value={newRule.value}
                    disabled={busy != null}
                    onChange={(e) => setNewRule({ ...newRule, value: e.target.value })}
                    placeholder={newRule.ruleType === "IP-CIDR" ? "例如 10.0.0.0/8" : newRule.ruleType === "DOMAIN-KEYWORD" ? "例如 tracker" : "例如 example.com"}
                  />
                  <select
                    className="h-8 shrink-0 rounded-md border border-(--border-strong) bg-(--input-bg) px-1.5 text-xs text-(--text-main)"
                    value={newRule.policy}
                    disabled={busy != null}
                    onChange={(e) => setNewRule({ ...newRule, policy: e.target.value })}
                  >
                    {CUSTOM_POLICY_OPTIONS.map((o) => (
                      <option key={o.value} value={o.value}>
                        {o.label}
                      </option>
                    ))}
                  </select>
                  <button type="button" className={btnSmall} disabled={busy != null || !newRule.value.trim()} onClick={addCustomRule}>
                    <Plus size={12} /> 添加规则
                  </button>
                </div>
              </div>
            </section>

            {/* 实时连接与日志 */}
            <section>
              <div className="flex flex-wrap items-center gap-2">
                <h3 className="text-sm font-semibold">实时连接与日志</h3>
                <span className="ml-auto flex shrink-0 items-center gap-1">
                  <button
                    type="button"
                    onClick={() => setInspectTab("connections")}
                    className={`h-7 rounded-md px-2 text-xs ${
                      inspectTab === "connections"
                        ? "bg-(--accent) font-medium text-(--accent-contrast)"
                        : "border border-(--border-strong) bg-(--input-bg) text-(--text-main) hover:bg-(--hover-bg)"
                    }`}
                  >
                    当前连接
                  </button>
                  <button
                    type="button"
                    onClick={() => setInspectTab("logs")}
                    className={`h-7 rounded-md px-2 text-xs ${
                      inspectTab === "logs"
                        ? "bg-(--accent) font-medium text-(--accent-contrast)"
                        : "border border-(--border-strong) bg-(--input-bg) text-(--text-main) hover:bg-(--hover-bg)"
                    }`}
                  >
                    核心日志
                  </button>
                </span>
              </div>
              <div className="mt-2 overflow-hidden rounded-lg border border-(--border) bg-(--panel-bg)">
                {!coreRunning ? (
                  <p className="px-4 py-8 text-center text-sm text-(--text-faint)">核心未运行，暂无连接与日志。</p>
                ) : inspectTab === "connections" ? (
                  <div>
                    <div className="flex items-center gap-2 border-b border-(--border) px-4 py-2">
                      <span className="text-xs text-(--text-dim)">按实例筛选</span>
                      <select
                        className="h-8 rounded-md border border-(--border-strong) bg-(--input-bg) px-1.5 text-xs text-(--text-main)"
                        value={connInstance}
                        onChange={(e) => setConnInstance(e.target.value)}
                      >
                        <option value="">全部实例</option>
                        {bindings.map((b) => (
                          <option key={b.instanceId} value={b.instanceId}>
                            {b.instanceName || b.instanceId}
                          </option>
                        ))}
                      </select>
                      <span className="ml-auto text-xs text-(--text-faint)">每 2.5 秒自动刷新</span>
                    </div>
                    {connections.length === 0 ? (
                      <p className="px-4 py-8 text-center text-sm text-(--text-faint)">暂无活跃连接。</p>
                    ) : (
                      <ul className="max-h-80 divide-y divide-(--border) overflow-y-auto">
                        {connections.map((c) => (
                          <li key={c.id} className="px-4 py-2 text-xs hover:bg-(--hover-bg)">
                            <div className="flex flex-wrap items-center gap-x-3 gap-y-0.5">
                              <span className="min-w-0 flex-1 truncate font-medium text-(--text-main)" title={c.target}>
                                {c.target}
                              </span>
                              <span className="shrink-0 text-(--text-dim)">{ruleLabel(c)}</span>
                              <span className="shrink-0 text-(--text-dim)">出口：{(c.chains ?? []).join(" → ") || "—"}</span>
                            </div>
                            <div className="mt-0.5 flex flex-wrap gap-x-3 text-(--text-faint)">
                              <span>
                                实例：
                                {bindings.find((b) => b.instanceId === c.instanceId)?.instanceName || c.instanceId || "未知"}
                              </span>
                              <span>上传 {formatBytes(c.upload)}</span>
                              <span>下载 {formatBytes(c.download)}</span>
                              <span>开始 {c.start ? c.start.slice(11, 19) : ""}</span>
                            </div>
                          </li>
                        ))}
                      </ul>
                    )}
                  </div>
                ) : (
                  <div>
                    <div className="flex items-center gap-2 border-b border-(--border) px-4 py-2">
                      <span className="flex-1 text-xs text-(--text-dim)">
                        日志内容取决于核心日志等级（在「设置」中调整）；连接级明细需要 info 或 debug 等级。
                      </span>
                      <button
                        type="button"
                        className={btnSmall}
                        onClick={() => {
                          setLogLines([]);
                        }}
                      >
                        清空显示
                      </button>
                    </div>
                    {logLines.length === 0 ? (
                      <p className="px-4 py-8 text-center text-sm text-(--text-faint)">暂无日志。</p>
                    ) : (
                      <div className="max-h-80 overflow-y-auto px-4 py-2 font-mono text-xs leading-5">
                        {logLines.map((l) => (
                          <p
                            key={l.seq}
                            className={
                              l.level === "error"
                                ? "text-(--danger)"
                                : l.level === "warning"
                                  ? "text-(--text-main)"
                                  : "text-(--text-dim)"
                            }
                          >
                            <span className="text-(--text-faint)">[{l.level}]</span> {l.text}
                          </p>
                        ))}
                      </div>
                    )}
                  </div>
                )}
              </div>
            </section>
          </div>
        )}
      </div>

      {/* 添加订阅弹窗 */}
      {addSub && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="w-full max-w-md rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">添加订阅</h3>
            <div className="mt-4 space-y-3">
              <div>
                <label className={labelCls}>名称</label>
                <input className={inputCls} value={addSub.name} onChange={(e) => setAddSub({ ...addSub, name: e.target.value })} placeholder="例如：机场一" />
              </div>
              <div>
                <label className={labelCls}>订阅地址</label>
                <input className={inputCls} value={addSub.url} onChange={(e) => setAddSub({ ...addSub, url: e.target.value })} placeholder="https://…" />
                <p className="mt-1 text-xs text-(--text-faint)">订阅地址只保存在本机凭据存储中，不会写入日志。</p>
              </div>
            </div>
            <div className="mt-5 flex justify-end gap-2">
              <button type="button" className={btnSecondary} disabled={busy != null} onClick={() => setAddSub(null)}>
                取消
              </button>
              <button type="button" className={btnPrimary} disabled={busy != null || !addSub.name.trim() || !addSub.url.trim()} onClick={submitAddSub}>
                {busy === "source-add-url" ? <Loader2 size={14} className="animate-spin" /> : null} 添加
              </button>
            </div>
          </div>
        </div>
      )}

      {/* 设置弹窗：固定事实（版本/目录/控制接口）只读展示；
          其余核心设置为可编辑表单，保存后经配置重载生效。 */}
      {showSettings && settingsForm && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="max-h-[90vh] w-full max-w-md overflow-y-auto rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">代理设置</h3>
            <dl className="mt-4 space-y-2 text-sm">
              <div className="flex gap-3">
                <dt className="w-24 shrink-0 text-(--text-faint)">核心版本</dt>
                <dd className="text-(--text-main)">Mihomo {core?.coreVersion ?? ""}（官方版本，固定）</dd>
              </div>
              <div className="flex gap-3">
                <dt className="w-24 shrink-0 text-(--text-faint)">数据目录</dt>
                <dd className="break-all text-(--text-main)">{core?.dataDir ?? "—"}</dd>
              </div>
              <div className="flex gap-3">
                <dt className="w-24 shrink-0 text-(--text-faint)">控制接口</dt>
                <dd className="text-(--text-main)">仅监听本机 127.0.0.1，带随机密钥</dd>
              </div>
            </dl>
            <div className="mt-4 space-y-3 border-t border-(--border) pt-4">
              <div>
                <label className={labelCls}>测速地址</label>
                <input
                  className={inputCls}
                  value={settingsForm.testUrl}
                  onChange={(e) => setSettingsForm({ ...settingsForm, testUrl: e.target.value })}
                />
              </div>
              <div className="flex gap-3">
                <div className="flex-1">
                  <label className={labelCls}>测速超时（毫秒）</label>
                  <input
                    className={inputCls}
                    type="number"
                    min={100}
                    max={60000}
                    value={settingsForm.testTimeoutMs}
                    onChange={(e) => setSettingsForm({ ...settingsForm, testTimeoutMs: Number(e.target.value) })}
                  />
                </div>
                <div className="flex-1">
                  <label className={labelCls}>自动更新间隔（秒）</label>
                  <input
                    className={inputCls}
                    type="number"
                    min={60}
                    max={2592000}
                    value={settingsForm.providerIntervalSec}
                    onChange={(e) => setSettingsForm({ ...settingsForm, providerIntervalSec: Number(e.target.value) })}
                  />
                </div>
              </div>
              <div>
                <label className={labelCls}>DoH 上游（每行一个，须为 https 地址）</label>
                <textarea
                  className="h-16 w-full rounded-md border border-(--border-strong) bg-(--input-bg) px-2.5 py-2 text-sm text-(--text-main) outline-none focus:border-(--accent)"
                  value={(settingsForm.dohServers ?? []).join("\n")}
                  onChange={(e) =>
                    setSettingsForm({
                      ...settingsForm,
                      dohServers: e.target.value
                        .split("\n")
                        .map((s) => s.trim())
                        .filter((s) => s !== ""),
                    })
                  }
                />
              </div>
              <div className="flex gap-3">
                <div className="flex-1">
                  <label className={labelCls}>核心日志等级</label>
                  <select
                    className={inputCls}
                    value={settingsForm.logLevel}
                    onChange={(e) => setSettingsForm({ ...settingsForm, logLevel: e.target.value })}
                  >
                    <option value="warning">warning（仅警告与错误）</option>
                    <option value="info">info（含连接明细）</option>
                    <option value="debug">debug（最详细）</option>
                  </select>
                </div>
                <div className="flex-1">
                  <label className={labelCls}>实例端口起始值</label>
                  <input
                    className={inputCls}
                    type="number"
                    min={1024}
                    max={64535}
                    value={settingsForm.listenerPortStart}
                    onChange={(e) => setSettingsForm({ ...settingsForm, listenerPortStart: Number(e.target.value) })}
                  />
                </div>
              </div>
              <p className="text-xs text-(--text-faint)">
                端口起始值只影响之后新建的绑定，已有绑定的端口不变。DeBrowser 不修改 Windows 系统代理、不启用 TUN，也不接管电脑上已有的 Clash。
              </p>
            </div>
            <div className="mt-5 flex justify-end gap-2">
              <button type="button" className={btnSecondary} disabled={busy != null} onClick={() => setShowSettings(false)}>
                取消
              </button>
              <button type="button" className={btnPrimary} disabled={busy != null} onClick={saveSettings}>
                {busy === "settings-save" ? <Loader2 size={14} className="animate-spin" /> : null} 保存设置
              </button>
            </div>
          </div>
        </div>
      )}

      {/* 导入本地 YAML：先填名称，再打开系统文件选择器 */}
      {addFileName !== null && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="w-full max-w-md rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">导入本地 YAML</h3>
            <div className="mt-4">
              <label className={labelCls}>名称</label>
              <input className={inputCls} value={addFileName} onChange={(e) => setAddFileName(e.target.value)} placeholder="例如：本地节点" />
              <p className="mt-1 text-xs text-(--text-faint)">文件会被复制到 DeBrowser 的数据目录中保存，原文件不会被修改。</p>
            </div>
            <div className="mt-5 flex justify-end gap-2">
              <button type="button" className={btnSecondary} disabled={busy != null} onClick={() => setAddFileName(null)}>
                取消
              </button>
              <button type="button" className={btnPrimary} disabled={busy != null || !addFileName.trim()} onClick={() => addFile()}>
                {busy === "source-add-file" ? <Loader2 size={14} className="animate-spin" /> : null} 选择文件
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
