import { useCallback, useEffect, useMemo, useState } from "react";
import {
  Eye,
  FileUp,
  Gauge,
  Layers,
  Pencil,
  Plus,
  RefreshCw,
  Rss,
  ShieldAlert,
  Trash2,
  TriangleAlert,
  Wrench,
} from "lucide-react";
import {
  ProxyService,
  SubscriptionService,
  type CustomMembershipView,
  type GroupMemberView,
  type ImportSummary,
  type MihomoConfigView,
  type OrphanedCustomGroupView,
  type ProxyGroupView,
  type ProxyPortRange,
  type ProxyView,
  type SubscriptionView,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import { useAppStore } from "../stores/useAppStore";

// ------------------------------------------------------------------
// 统一「代理配置」页（Stage 6C）：订阅、本地配置、自定义节点三种
// 来源在同一模块内管理。订阅地址与节点凭据只保存在凭据库中，
// 本页不回显、不持久化任何明文。
// ------------------------------------------------------------------

const AUTO_UPDATE_OPTIONS: { value: number; label: string }[] = [
  { value: 0, label: "关闭" },
  { value: 15, label: "15 分钟" },
  { value: 30, label: "30 分钟" },
  { value: 60, label: "1 小时" },
  { value: 120, label: "2 小时" },
  { value: 360, label: "6 小时" },
  { value: 720, label: "12 小时" },
  { value: 1440, label: "24 小时" },
];

const FORMAT_LABELS: Record<string, string> = {
  mihomo_yaml: "完整 Mihomo 配置",
  proxies_yaml: "代理节点 YAML",
  uri_list: "URI 订阅文本",
};

const MEMBER_KIND_LABELS: Record<string, string> = {
  proxy: "节点",
  group: "代理组",
  builtin: "内置",
};

const SOURCE_LABEL: Record<string, string> = {
  custom: "自定义",
  file: "本地配置",
  subscription: "订阅",
};

const PROTOCOLS = [
  "http",
  "https",
  "socks5",
  "shadowsocks",
  "trojan",
  "vmess",
  "vless",
  "hysteria2",
  "tuic",
  "wireguard",
  "direct",
] as const;

// 协议对应的凭据字段（写入凭据库，永不回显明文）与普通配置字段
// （写入 config_json，可回显编辑）。
const SECRET_FIELDS: Record<string, { key: string; label: string }[]> = {
  http: [{ key: "password", label: "密码" }],
  https: [{ key: "password", label: "密码" }],
  socks5: [{ key: "password", label: "密码" }],
  shadowsocks: [{ key: "password", label: "密码" }],
  trojan: [{ key: "password", label: "密码" }],
  vmess: [{ key: "uuid", label: "UUID" }],
  vless: [{ key: "uuid", label: "UUID" }],
  hysteria2: [
    { key: "password", label: "密码" },
    { key: "obfs_password", label: "混淆密码" },
  ],
  tuic: [
    { key: "uuid", label: "UUID" },
    { key: "password", label: "密码" },
  ],
  wireguard: [{ key: "private_key", label: "私钥" }],
  direct: [],
};

const CONFIG_FIELDS: Record<string, { key: string; label: string; placeholder?: string }[]> = {
  http: [{ key: "username", label: "用户名" }],
  https: [{ key: "username", label: "用户名" }],
  socks5: [{ key: "username", label: "用户名" }],
  shadowsocks: [{ key: "cipher", label: "加密方法", placeholder: "aes-256-gcm" }],
  trojan: [{ key: "sni", label: "服务器名称（SNI）" }],
  hysteria2: [{ key: "sni", label: "服务器名称（SNI）" }],
};

interface SubForm {
  name: string;
  url: string;
  allowLan: boolean;
  autoUpdateMinutes: number;
}

const EMPTY_SUB_FORM: SubForm = { name: "", url: "", allowLan: false, autoUpdateMinutes: 0 };

interface ProxyForm {
  id?: string;
  name: string;
  protocol: string;
  address: string;
  port: string;
  sortOrder: string;
  enabled: boolean;
  hasSecrets: boolean;
  secretFields: string[];
  credentialStatus: string;
  configValues: Record<string, string>;
  configRest: string;
  secretValues: Record<string, string>;
  clearSecrets: boolean;
}

const EMPTY_PROXY_FORM: ProxyForm = {
  name: "",
  protocol: "socks5",
  address: "",
  port: "",
  sortOrder: "0",
  enabled: true,
  hasSecrets: false,
  secretFields: [],
  credentialStatus: "none",
  configValues: {},
  configRest: "{}",
  secretValues: {},
  clearSecrets: false,
};

// 把已存 config_json 拆成「已知字段」（单独输入框）与「其余部分」
// （高级配置文本域），保证同一字段只有一个编辑入口。
function splitConfig(protocol: string, configJson: string) {
  let obj: Record<string, unknown> = {};
  try {
    const parsed: unknown = JSON.parse(configJson || "{}");
    if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
      obj = parsed as Record<string, unknown>;
    }
  } catch {
    obj = {};
  }
  const values: Record<string, string> = {};
  for (const field of CONFIG_FIELDS[protocol] ?? []) {
    const v = obj[field.key];
    if (typeof v === "string") values[field.key] = v;
    delete obj[field.key];
  }
  return { values, rest: JSON.stringify(obj, null, 2) };
}

function formFromProxy(p: ProxyView): ProxyForm {
  const { values, rest } = splitConfig(p.protocol, p.configJson);
  return {
    id: p.id,
    name: p.name,
    protocol: p.protocol,
    address: p.address,
    port: p.port != null ? String(p.port) : "",
    sortOrder: String(p.sortOrder),
    enabled: p.enabled,
    hasSecrets: p.hasSecrets,
    secretFields: p.secretFields ?? [],
    credentialStatus: p.credentialStatus,
    configValues: values,
    configRest: rest,
    secretValues: {},
    clearSecrets: false,
  };
}

// 当前选中的配置来源：某个订阅、某个本地配置文件，或自定义节点集合。
type Selection =
  | { kind: "subscription"; id: string }
  | { kind: "file"; id: string }
  | { kind: "custom" };

export default function ProxyConfigPage() {
  const log = useAppStore((state) => state.log);
  const [subs, setSubs] = useState<SubscriptionView[]>([]);
  const [configs, setConfigs] = useState<MihomoConfigView[]>([]);
  const [proxies, setProxies] = useState<ProxyView[]>([]);
  const [portRange, setPortRange] = useState<ProxyPortRange | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [selection, setSelection] = useState<Selection>({ kind: "custom" });

  // 订阅表单与删除确认。
  const [subFormOpen, setSubFormOpen] = useState(false);
  const [editingSub, setEditingSub] = useState<SubscriptionView | null>(null);
  const [subForm, setSubForm] = useState<SubForm>(EMPTY_SUB_FORM);
  const [savingSub, setSavingSub] = useState(false);
  const [pendingDeleteSub, setPendingDeleteSub] = useState<SubscriptionView | null>(null);
  const [pendingDeleteConfig, setPendingDeleteConfig] = useState<MihomoConfigView | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [refreshingId, setRefreshingId] = useState<string | null>(null);

  // 本地导入。
  const [importOpen, setImportOpen] = useState(false);
  const [importName, setImportName] = useState("");
  const [importing, setImporting] = useState(false);
  const [lastSummary, setLastSummary] = useState<ImportSummary | null>(null);

  // 选中配置的原生代理组。
  const [groups, setGroups] = useState<ProxyGroupView[]>([]);
  const [groupsLoading, setGroupsLoading] = useState(false);
  const [orphans, setOrphans] = useState<OrphanedCustomGroupView[]>([]);
  const [pickerGroup, setPickerGroup] = useState<string | null>(null);
  const [candidates, setCandidates] = useState<ProxyView[]>([]);
  const [detailMemberships, setDetailMemberships] = useState<CustomMembershipView[]>([]);
  const [deleteMembershipCount, setDeleteMembershipCount] = useState(0);

  // 自定义节点表单/详情/删除/测试。
  const [search, setSearch] = useState("");
  const [proxyForm, setProxyForm] = useState<ProxyForm | null>(null);
  const [detail, setDetail] = useState<ProxyView | null>(null);
  const [pendingDeleteProxy, setPendingDeleteProxy] = useState<ProxyView | null>(null);
  const [testingId, setTestingId] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const [s, c, p, range] = await Promise.all([
        SubscriptionService.ListSubscriptions(),
        SubscriptionService.ListConfigs(),
        ProxyService.ListProxies(),
        ProxyService.GetProxyPortRange(),
      ]);
      setSubs(s ?? []);
      setConfigs(c ?? []);
      setProxies(p ?? []);
      setPortRange(range);
      setError(null);
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setError(`加载代理配置失败：${message}`);
      log("error", `加载代理配置失败：${message}`);
    } finally {
      setLoading(false);
    }
  }, [log]);

  useEffect(() => {
    void load();
  }, [load]);

  const fileConfigs = useMemo(() => configs.filter((c) => c.sourceType === "file"), [configs]);
  const customProxies = useMemo(() => proxies.filter((p) => p.source === "custom"), [proxies]);

  const selectedSub = selection.kind === "subscription" ? subs.find((s) => s.id === selection.id) : undefined;
  const selectedFile = selection.kind === "file" ? fileConfigs.find((c) => c.id === selection.id) : undefined;
  const selectedConfig: MihomoConfigView | undefined =
    selection.kind === "subscription"
      ? configs.find((c) => c.subscriptionId === selection.id)
      : selection.kind === "file"
        ? selectedFile
        : undefined;

  // 选中带配置的来源时加载其原生代理组（含用户添加成员与失效组）。
  const reloadGroups = useCallback(
    async (configID: string) => {
      const [g, o] = await Promise.all([
        SubscriptionService.GetConfigGroups(configID),
        SubscriptionService.ListOrphanedCustomMembers(configID),
      ]);
      setGroups(g ?? []);
      setOrphans(o ?? []);
    },
    [],
  );

  useEffect(() => {
    if (!selectedConfig) {
      setGroups([]);
      setOrphans([]);
      return;
    }
    let alive = true;
    setGroupsLoading(true);
    reloadGroups(selectedConfig.id)
      .catch((err: unknown) => {
        if (alive) {
          setError(err instanceof Error ? err.message : String(err));
          setGroups([]);
          setOrphans([]);
        }
      })
      .finally(() => {
        if (alive) setGroupsLoading(false);
      });
    return () => {
      alive = false;
    };
  }, [selectedConfig, reloadGroups]);

  const openPicker = useCallback(
    async (groupName: string) => {
      if (!selectedConfig) return;
      setPickerGroup(groupName);
      try {
        setCandidates((await SubscriptionService.ListCustomCandidates(selectedConfig.id, groupName)) ?? []);
      } catch {
        setCandidates([]);
      }
    },
    [selectedConfig],
  );

  const addToGroup = async (proxyID: string) => {
    if (!selectedConfig || !pickerGroup) return;
    try {
      await SubscriptionService.AddCustomGroupMember(selectedConfig.id, pickerGroup, proxyID);
      setPickerGroup(null);
      await reloadGroups(selectedConfig.id);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  const removeFromGroup = async (groupName: string, proxyID: string) => {
    if (!selectedConfig) return;
    try {
      await SubscriptionService.RemoveCustomGroupMember(selectedConfig.id, groupName, proxyID);
      await reloadGroups(selectedConfig.id);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    }
  };

  // 节点详情（自定义节点）加载其代理组成员关系。
  useEffect(() => {
    if (!detail || detail.source !== "custom") {
      setDetailMemberships([]);
      return;
    }
    let alive = true;
    SubscriptionService.ListCustomMemberships(detail.id)
      .then((m) => {
        if (alive) setDetailMemberships(m ?? []);
      })
      .catch(() => {
        if (alive) setDetailMemberships([]);
      });
    return () => {
      alive = false;
    };
  }, [detail]);

  // 删除自定义节点前统计其成员关系数量，用于确认提示。
  useEffect(() => {
    if (!pendingDeleteProxy) {
      setDeleteMembershipCount(0);
      return;
    }
    let alive = true;
    SubscriptionService.ListCustomMemberships(pendingDeleteProxy.id)
      .then((m) => {
        if (alive) setDeleteMembershipCount((m ?? []).length);
      })
      .catch(() => {
        if (alive) setDeleteMembershipCount(0);
      });
    return () => {
      alive = false;
    };
  }, [pendingDeleteProxy]);

  const visibleNodes = useMemo(() => {
    let list: ProxyView[];
    if (selection.kind === "subscription") {
      list = proxies.filter((p) => p.subscriptionId === selection.id);
    } else if (selection.kind === "file") {
      list = proxies.filter((p) => p.configId === selection.id);
    } else {
      list = customProxies;
    }
    const kw = search.trim().toLowerCase();
    if (kw) {
      list = list.filter((p) => `${p.name} ${p.protocol} ${p.address}`.toLowerCase().includes(kw));
    }
    return list;
  }, [selection, proxies, customProxies, search]);

  // ---------------- 订阅操作 ----------------

  const openCreateSub = () => {
    setEditingSub(null);
    setSubForm(EMPTY_SUB_FORM);
    setSubFormOpen(true);
  };

  const openEditSub = (sub: SubscriptionView) => {
    setEditingSub(sub);
    setSubForm({
      name: sub.name,
      url: "",
      allowLan: sub.allowLan,
      autoUpdateMinutes: sub.autoUpdateMinutes,
    });
    setSubFormOpen(true);
  };

  const submitSubForm = async () => {
    setSavingSub(true);
    try {
      const input = {
        name: subForm.name,
        url: subForm.url,
        allowLan: subForm.allowLan,
        autoUpdateMinutes: subForm.autoUpdateMinutes,
      };
      if (editingSub) {
        await SubscriptionService.UpdateSubscription(editingSub.id, input);
        log("info", `订阅已更新：${subForm.name}`);
      } else {
        const created = await SubscriptionService.CreateSubscription(input);
        log("info", `订阅已新增：${subForm.name}`);
        setSelection({ kind: "subscription", id: created.id });
      }
      setSubFormOpen(false);
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setSavingSub(false);
    }
  };

  const doRefreshSub = async (sub: SubscriptionView) => {
    setRefreshingId(sub.id);
    setNotice(null);
    try {
      const sum = await SubscriptionService.RefreshSubscription(sub.id);
      setLastSummary(sum);
      setNotice(
        `「${sub.name}」刷新完成（${FORMAT_LABELS[sum.format] ?? sum.format}）：新增 ${sum.added} 个节点，更新 ${sum.updated} 个，删除 ${sum.removed} 个，代理组 ${sum.groups} 个。`,
      );
      log("info", `订阅刷新完成：${sub.name}`);
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
      await load();
    } finally {
      setRefreshingId(null);
    }
  };

  const doDeleteSub = async () => {
    if (!pendingDeleteSub) return;
    setDeleting(true);
    try {
      await SubscriptionService.DeleteSubscription(pendingDeleteSub.id);
      log("info", `订阅已删除：${pendingDeleteSub.name}`);
      setPendingDeleteSub(null);
      setSelection({ kind: "custom" });
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setDeleting(false);
    }
  };

  // ---------------- 本地配置操作 ----------------

  const doImport = async () => {
    setImporting(true);
    setNotice(null);
    try {
      const sum = await SubscriptionService.ImportLocalConfig(importName);
      if (sum.cancelled) {
        setNotice("已取消导入。");
      } else {
        setLastSummary(sum);
        setNotice(
          `本地配置「${sum.configName}」导入完成（${FORMAT_LABELS[sum.format] ?? sum.format}）：新增 ${sum.added} 个节点，代理组 ${sum.groups} 个。`,
        );
        log("info", `本地配置导入完成：${sum.configName}`);
        setImportOpen(false);
        setImportName("");
        await load();
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setImporting(false);
    }
  };

  const doDeleteConfig = async () => {
    if (!pendingDeleteConfig) return;
    setDeleting(true);
    try {
      await SubscriptionService.DeleteConfig(pendingDeleteConfig.id);
      log("info", `配置已删除：${pendingDeleteConfig.name}`);
      setPendingDeleteConfig(null);
      setSelection({ kind: "custom" });
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setDeleting(false);
    }
  };

  // ---------------- 节点操作（自定义节点完整 CRUD；导入节点可测试/启停） ----------------

  const submitProxyForm = async () => {
    if (!proxyForm) return;
    setBusy(true);
    try {
      let restObj: Record<string, unknown> = {};
      const restText = proxyForm.configRest.trim() || "{}";
      try {
        const parsed: unknown = JSON.parse(restText);
        if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
          throw new Error("not-object");
        }
        restObj = parsed as Record<string, unknown>;
      } catch {
        setError("高级配置必须是有效的 JSON 对象");
        setBusy(false);
        return;
      }
      for (const field of CONFIG_FIELDS[proxyForm.protocol] ?? []) {
        const v = (proxyForm.configValues[field.key] ?? "").trim();
        if (v) restObj[field.key] = v;
        else delete restObj[field.key];
      }
      const secrets: Record<string, string> = {};
      for (const field of SECRET_FIELDS[proxyForm.protocol] ?? []) {
        const v = proxyForm.secretValues[field.key] ?? "";
        if (v) secrets[field.key] = v;
      }
      const portNum = proxyForm.port.trim() === "" ? null : Number(proxyForm.port);
      const base = {
        name: proxyForm.name,
        protocol: proxyForm.protocol,
        address: proxyForm.address,
        port: portNum,
        configJson: JSON.stringify(restObj),
        sortOrder: Number(proxyForm.sortOrder) || 0,
        secrets: Object.keys(secrets).length > 0 ? secrets : null,
      };
      if (proxyForm.id) {
        await ProxyService.UpdateProxy({
          ...base,
          id: proxyForm.id,
          enabled: proxyForm.enabled,
          clearSecrets: proxyForm.clearSecrets && Object.keys(secrets).length === 0,
        });
        log("info", `节点已更新：${proxyForm.name}`);
      } else {
        const created = await ProxyService.CreateProxy(base);
        log("info", `自定义节点已创建：${created.name}`);
      }
      setProxyForm(null); // 关闭即丢弃表单中的凭据明文
      await load();
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setError(message);
      log("error", `保存节点失败：${message}`);
    } finally {
      setBusy(false);
    }
  };

  const confirmDeleteProxy = async () => {
    if (!pendingDeleteProxy) return;
    setBusy(true);
    try {
      await ProxyService.DeleteProxy(pendingDeleteProxy.id);
      log("info", `节点已删除：${pendingDeleteProxy.name}`);
      setPendingDeleteProxy(null);
      await load();
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setError(message);
      log("error", `删除节点失败：${message}`);
    } finally {
      setBusy(false);
    }
  };

  const runTest = async (p: ProxyView) => {
    setTestingId(p.id);
    setNotice(null);
    try {
      const result = await ProxyService.TestProxy(p.id);
      setNotice(`「${p.name}」${result.message}`);
      log(result.ok ? "info" : "error", `连接测试「${p.name}」：${result.message}`);
      await load();
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setError(message);
      log("error", `连接测试失败：${message}`);
    } finally {
      setTestingId(null);
    }
  };

  const toggleEnabled = async (p: ProxyView) => {
    try {
      await ProxyService.SetProxyEnabled(p.id, !p.enabled);
      await load();
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setError(message);
      log("error", `切换启用状态失败：${message}`);
    }
  };

  // ---------------- 渲染 ----------------

  const inputCls =
    "w-full rounded-md border border-(--border) bg-(--app-bg) px-2.5 py-1.5 text-sm text-(--text-main) outline-none placeholder:text-(--text-faint)";
  const labelCls = "mb-1 block text-xs text-(--text-dim)";
  const btnCls =
    "flex items-center gap-1.5 rounded-md border border-(--border) px-3 py-1.5 text-sm text-(--text-dim) hover:bg-(--hover-bg) disabled:opacity-50";
  const primaryCls =
    "flex items-center gap-1.5 rounded-md bg-(--accent) px-3 py-1.5 text-sm font-medium text-(--accent-contrast) hover:opacity-90 disabled:opacity-50";

  const statusBadge = (sub: SubscriptionView) => {
    if (sub.lastStatus === "ok") {
      return <span className="rounded-full bg-(--badge-bg) px-2 py-0.5 text-xs text-(--text-dim)">刷新正常</span>;
    }
    if (sub.lastStatus === "error") {
      return <span className="rounded-full bg-(--danger-bg) px-2 py-0.5 text-xs text-(--danger)">刷新失败</span>;
    }
    return <span className="rounded-full bg-(--badge-bg) px-2 py-0.5 text-xs text-(--text-faint)">尚未刷新</span>;
  };

  const memberLine = (m: GroupMemberView) => {
    const kind = MEMBER_KIND_LABELS[m.kind] ?? m.kind;
    const target = m.kind === "proxy" && m.proxyName ? `（当前节点：${m.proxyName}）` : "";
    return `${m.name} · ${kind}${target}`;
  };

  const latencyCell = (p: ProxyView) => {
    if (p.lastTestOk == null) return <span className="text-(--text-faint)">未测试</span>;
    if (p.lastTestOk)
      return (
        <span className="rounded-full bg-(--badge-bg) px-2 py-0.5 text-xs text-(--text-dim)">
          {p.lastLatencyMs ?? 0} ms
        </span>
      );
    return (
      <span className="rounded-full bg-(--danger-bg) px-2 py-0.5 text-xs text-(--danger)">连接失败</span>
    );
  };

  const credentialCell = (p: ProxyView) => {
    if (p.credentialStatus === "unavailable")
      return (
        <span className="flex items-center gap-1 text-xs text-(--danger)">
          <ShieldAlert size={13} /> 凭据不可用
        </span>
      );
    if (p.credentialStatus === "ok")
      return (
        <span className="text-xs text-(--text-dim)" title={`凭据字段：${(p.secretFields ?? []).join("、")}`}>
          已设置
        </span>
      );
    return <span className="text-xs text-(--text-faint)">无</span>;
  };

  const railItemCls = (active: boolean) =>
    `w-full rounded-lg px-3 py-2.5 text-left transition-colors ${active ? "bg-(--hover-bg)" : "hover:bg-(--hover-bg)"}`;

  return (
    <div className="mx-auto max-w-6xl px-6 py-6">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold">代理配置</h2>
          <p className="mt-0.5 text-sm text-(--text-dim)">
            在同一处管理订阅、本地配置与自定义节点。订阅地址与节点凭据只保存在凭据库中，页面不回显明文。
          </p>
        </div>
        <div className="flex gap-2">
          <button type="button" onClick={() => void load()} className={btnCls}>
            <RefreshCw size={15} />
            刷新列表
          </button>
          <button type="button" onClick={() => setImportOpen(true)} className={btnCls}>
            <FileUp size={15} />
            导入本地配置
          </button>
          <button
            type="button"
            onClick={() => {
              setSelection({ kind: "custom" });
              setProxyForm(EMPTY_PROXY_FORM);
            }}
            className={btnCls}
          >
            <Wrench size={15} />
            添加自定义节点
          </button>
          <button type="button" onClick={openCreateSub} className={primaryCls}>
            <Plus size={15} />
            添加订阅
          </button>
        </div>
      </div>

      {error && (
        <div className="mt-4 flex items-start gap-2 rounded-lg border border-(--danger-border) bg-(--danger-bg) px-3 py-2 text-sm text-(--danger)">
          <TriangleAlert size={16} className="mt-0.5 shrink-0" />
          <span>{error}</span>
        </div>
      )}
      {notice && (
        <div className="mt-4 rounded-lg border border-(--border) bg-(--panel-bg) px-3 py-2 text-sm text-(--text-dim)">
          {notice}
        </div>
      )}
      {lastSummary && lastSummary.issues && lastSummary.issues.length > 0 && (
        <div className="mt-4 rounded-lg border border-(--border) bg-(--panel-bg) px-3 py-2 text-sm text-(--text-dim)">
          <p className="font-medium text-(--text-main)">安全提示（原始配置检查结果，运行时加固将在核心集成阶段执行）</p>
          <ul className="mt-1 list-inside list-disc space-y-0.5 text-xs">
            {lastSummary.issues.map((issue) => (
              <li key={issue.code}>{issue.message}</li>
            ))}
          </ul>
        </div>
      )}

      <div className="mt-6 flex items-start gap-4">
        {/* 配置来源列表 */}
        <aside className="w-72 shrink-0 rounded-lg border border-(--border) bg-(--panel-bg) p-2">
          <p className="flex items-center gap-1.5 px-2 pb-1 pt-1.5 text-xs font-semibold text-(--text-dim)">
            <Rss size={13} /> 订阅
          </p>
          {subs.length === 0 ? (
            <p className="px-2 pb-2 text-xs text-(--text-faint)">暂无订阅，点击右上角「添加订阅」。</p>
          ) : (
            subs.map((sub) => (
              <button
                key={sub.id}
                type="button"
                onClick={() => setSelection({ kind: "subscription", id: sub.id })}
                className={railItemCls(selection.kind === "subscription" && selection.id === sub.id)}
              >
                <span className="flex items-center justify-between gap-2">
                  <span className="truncate text-sm font-medium">{sub.name}</span>
                  {statusBadge(sub)}
                </span>
                <span className="mt-0.5 block text-xs text-(--text-faint)">
                  节点 {sub.nodeCount} 个 · 代理组 {sub.groupCount} 个
                </span>
              </button>
            ))
          )}

          <p className="flex items-center gap-1.5 px-2 pb-1 pt-3 text-xs font-semibold text-(--text-dim)">
            <Layers size={13} /> 本地配置
          </p>
          {fileConfigs.length === 0 ? (
            <p className="px-2 pb-2 text-xs text-(--text-faint)">暂无本地配置，点击「导入本地配置」。</p>
          ) : (
            fileConfigs.map((cfg) => (
              <button
                key={cfg.id}
                type="button"
                onClick={() => setSelection({ kind: "file", id: cfg.id })}
                className={railItemCls(selection.kind === "file" && selection.id === cfg.id)}
              >
                <span className="block truncate text-sm font-medium">{cfg.name}</span>
                <span className="mt-0.5 block text-xs text-(--text-faint)">
                  节点 {cfg.nodeCount} 个 · 代理组 {cfg.groupCount} 个
                </span>
              </button>
            ))
          )}

          <p className="flex items-center gap-1.5 px-2 pb-1 pt-3 text-xs font-semibold text-(--text-dim)">
            <Wrench size={13} /> 自定义
          </p>
          <button type="button" onClick={() => setSelection({ kind: "custom" })} className={railItemCls(selection.kind === "custom")}>
            <span className="block text-sm font-medium">自定义节点</span>
            <span className="mt-0.5 block text-xs text-(--text-faint)">手动添加的节点 {customProxies.length} 个</span>
          </button>
        </aside>

        {/* 详情区域 */}
        <section className="min-w-0 flex-1">
          {selection.kind === "subscription" && selectedSub && (
            <div className="rounded-lg border border-(--border) bg-(--panel-bg) px-4 py-3">
              <div className="flex items-center justify-between gap-3">
                <div className="min-w-0">
                  <p className="truncate text-sm font-medium">{selectedSub.name}</p>
                  <p className="mt-0.5 truncate text-xs text-(--text-faint)">
                    {selectedSub.urlHint || "未设置地址"}
                    {selectedSub.allowLan ? " · 允许局域网" : ""}
                    {selectedSub.autoUpdateMinutes > 0
                      ? ` · 自动更新 ${AUTO_UPDATE_OPTIONS.find((o) => o.value === selectedSub.autoUpdateMinutes)?.label ?? `${selectedSub.autoUpdateMinutes} 分钟`}`
                      : " · 自动更新关闭"}
                    {` · 最近更新：${selectedSub.lastUpdatedAt ?? "无"}`}
                  </p>
                </div>
                <div className="flex shrink-0 items-center gap-2">
                  {statusBadge(selectedSub)}
                  <button
                    type="button"
                    disabled={refreshingId === selectedSub.id}
                    onClick={() => void doRefreshSub(selectedSub)}
                    className={btnCls}
                  >
                    <RefreshCw size={14} className={refreshingId === selectedSub.id ? "animate-spin" : ""} />
                    {refreshingId === selectedSub.id ? "刷新中" : "刷新订阅"}
                  </button>
                  <button type="button" onClick={() => openEditSub(selectedSub)} className={btnCls}>
                    <Pencil size={14} />
                    编辑
                  </button>
                  <button type="button" onClick={() => setPendingDeleteSub(selectedSub)} className={btnCls}>
                    <Trash2 size={14} />
                    删除
                  </button>
                </div>
              </div>
              {selectedSub.lastStatus === "error" && selectedSub.lastError && (
                <p className="mt-1.5 text-xs text-(--danger)">最近一次刷新失败：{selectedSub.lastError}</p>
              )}
            </div>
          )}

          {selection.kind === "file" && selectedFile && (
            <div className="rounded-lg border border-(--border) bg-(--panel-bg) px-4 py-3">
              <div className="flex items-center justify-between gap-3">
                <div className="min-w-0">
                  <p className="truncate text-sm font-medium">{selectedFile.name}</p>
                  <p className="mt-0.5 text-xs text-(--text-faint)">
                    本地文件导入 · 节点 {selectedFile.nodeCount} 个 · 代理组 {selectedFile.groupCount} 个 · 内容校验和{" "}
                    {selectedFile.sha256.slice(0, 12)}…
                  </p>
                </div>
                <div className="flex shrink-0 items-center gap-2">
                  <button type="button" onClick={() => setImportOpen(true)} className={btnCls}>
                    <FileUp size={14} />
                    重新导入
                  </button>
                  <button type="button" onClick={() => setPendingDeleteConfig(selectedFile)} className={btnCls}>
                    <Trash2 size={14} />
                    删除配置
                  </button>
                </div>
              </div>
              {selectedFile.issues && selectedFile.issues.length > 0 && (
                <ul className="mt-1.5 list-inside list-disc space-y-0.5 text-xs text-(--text-dim)">
                  {selectedFile.issues.map((issue) => (
                    <li key={issue.code}>{issue.message}</li>
                  ))}
                </ul>
              )}
            </div>
          )}

          {selection.kind === "custom" && (
            <div className="rounded-lg border border-(--border) bg-(--panel-bg) px-4 py-3">
              <div className="flex items-center justify-between gap-3">
                <div>
                  <p className="text-sm font-medium">自定义节点</p>
                  <p className="mt-0.5 text-xs text-(--text-faint)">
                    手动添加的节点，不依赖订阅或本地文件。
                    {portRange && ` 实例监听端口范围：${portRange.start}–${portRange.end}。`}
                  </p>
                </div>
                <button type="button" onClick={() => setProxyForm(EMPTY_PROXY_FORM)} className={primaryCls}>
                  <Plus size={14} />
                  添加自定义节点
                </button>
              </div>
            </div>
          )}

          {/* 节点列表 */}
          <div className="mt-4 flex items-center gap-2">
            <h3 className="text-sm font-semibold">节点列表</h3>
            <input
              className="w-56 rounded-md border border-(--border) bg-(--panel-bg) px-2.5 py-1.5 text-sm text-(--text-main) outline-none placeholder:text-(--text-faint)"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="搜索名称、协议或地址"
            />
            <span className="text-xs text-(--text-faint)">
              连接测试为 TCP 连通性测试；协议级测速将在核心集成阶段提供。
              {selection.kind !== "custom" && " 导入的节点由来源统一维护，刷新时同步，删除请使用来源操作。"}
            </span>
          </div>
          <div className="mt-3 overflow-hidden rounded-lg border border-(--border) bg-(--panel-bg)">
            {loading ? (
              <p className="px-4 py-10 text-center text-sm text-(--text-faint)">加载中…</p>
            ) : visibleNodes.length === 0 ? (
              <p className="px-4 py-10 text-center text-sm text-(--text-faint)">
                {selection.kind === "custom"
                  ? "暂无自定义节点。点击「添加自定义节点」创建第一个节点。"
                  : selection.kind === "subscription"
                    ? "该订阅暂无节点。点击「刷新订阅」从订阅地址导入。"
                    : "该配置暂无节点。"}
              </p>
            ) : (
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b border-(--border) text-left text-xs text-(--text-faint)">
                    <th className="px-4 py-2.5 font-medium">名称</th>
                    <th className="px-4 py-2.5 font-medium">协议</th>
                    <th className="px-4 py-2.5 font-medium">地址</th>
                    <th className="px-4 py-2.5 font-medium">来源</th>
                    <th className="px-4 py-2.5 font-medium">延迟</th>
                    <th className="px-4 py-2.5 font-medium">凭据</th>
                    <th className="px-4 py-2.5 font-medium">启用</th>
                    <th className="px-4 py-2.5 text-right font-medium">操作</th>
                  </tr>
                </thead>
                <tbody>
                  {visibleNodes.map((p) => (
                    <tr key={p.id} className="border-b border-(--border) last:border-0 hover:bg-(--hover-bg)">
                      <td className="px-4 py-2.5 font-medium">{p.name}</td>
                      <td className="px-4 py-2.5 text-(--text-dim)">{p.protocol === "direct" ? "直连" : p.protocol}</td>
                      <td className="px-4 py-2.5 font-mono text-xs text-(--text-dim)">
                        {p.protocol === "direct" ? "—" : `${p.address}${p.port != null ? `:${p.port}` : ""}`}
                      </td>
                      <td className="px-4 py-2.5 text-(--text-dim)">{SOURCE_LABEL[p.source] ?? p.source}</td>
                      <td className="px-4 py-2.5">{latencyCell(p)}</td>
                      <td className="px-4 py-2.5">{credentialCell(p)}</td>
                      <td className="px-4 py-2.5">
                        <button
                          type="button"
                          onClick={() => void toggleEnabled(p)}
                          className={`rounded-full px-2 py-0.5 text-xs ${
                            p.enabled ? "bg-(--badge-bg) text-(--text-dim)" : "bg-(--hover-bg) text-(--text-faint)"
                          }`}
                        >
                          {p.enabled ? "已启用" : "已停用"}
                        </button>
                      </td>
                      <td className="px-4 py-2.5">
                        <div className="flex justify-end gap-1">
                          <button
                            type="button"
                            title="详情"
                            onClick={() => setDetail(p)}
                            className="rounded p-1.5 text-(--text-dim) hover:bg-(--hover-bg)"
                          >
                            <Eye size={14} />
                          </button>
                          <button
                            type="button"
                            title="连接测试"
                            disabled={testingId === p.id || p.protocol === "direct"}
                            onClick={() => void runTest(p)}
                            className="rounded p-1.5 text-(--text-dim) hover:bg-(--hover-bg) disabled:opacity-40"
                          >
                            <Gauge size={14} />
                          </button>
                          {p.source === "custom" && (
                            <>
                              <button
                                type="button"
                                title="编辑"
                                onClick={() => setProxyForm(formFromProxy(p))}
                                className="rounded p-1.5 text-(--text-dim) hover:bg-(--hover-bg)"
                              >
                                <Pencil size={14} />
                              </button>
                              <button
                                type="button"
                                title="删除"
                                onClick={() => setPendingDeleteProxy(p)}
                                className="rounded p-1.5 text-(--text-dim) hover:bg-(--hover-bg)"
                              >
                                <Trash2 size={14} />
                              </button>
                            </>
                          )}
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>

          {/* 原生代理组（订阅/本地配置来源） */}
          {selection.kind !== "custom" && (
            <>
              <h3 className="mt-6 flex items-center gap-2 text-sm font-semibold">
                <Layers size={15} />
                Mihomo 原生代理组
              </h3>
              <div className="mt-3 rounded-lg border border-(--border) bg-(--panel-bg) px-4 py-3">
                {!selectedConfig ? (
                  <p className="text-sm text-(--text-faint)">该来源尚无已导入配置。</p>
                ) : groupsLoading ? (
                  <p className="text-sm text-(--text-faint)">加载中…</p>
                ) : groups.length === 0 ? (
                  <p className="text-sm text-(--text-faint)">该配置没有原生代理组。</p>
                ) : (
                  <div className="space-y-2">
                    {groups.map((g) => (
                      <div key={g.id} className="rounded-md bg-(--app-bg) px-3 py-2">
                        <div className="flex items-center justify-between gap-2">
                          <p className="text-xs font-medium">
                            {g.name} <span className="text-(--text-faint)">（{g.type}）</span>
                          </p>
                          <button
                            type="button"
                            onClick={() => void openPicker(g.name)}
                            className="flex shrink-0 items-center gap-1 rounded border border-(--border) px-1.5 py-0.5 text-[11px] text-(--text-dim) hover:bg-(--hover-bg)"
                          >
                            <Plus size={11} />
                            添加自定义节点
                          </button>
                        </div>
                        <ul className="mt-1 space-y-0.5 text-xs text-(--text-dim)">
                          {(g.members ?? []).map((m, i) => (
                            <li key={`${g.id}-${i}`} className="flex items-center gap-1.5">
                              <span>{memberLine(m)}</span>
                              {m.origin === "custom" ? (
                                <>
                                  <span className="rounded bg-(--accent) px-1 text-[10px] leading-4 text-(--accent-contrast)">自定义</span>
                                  <button
                                    type="button"
                                    onClick={() => void removeFromGroup(g.name, m.proxyId ?? "")}
                                    className="text-[11px] text-(--danger) hover:underline"
                                  >
                                    移除
                                  </button>
                                </>
                              ) : (
                                <span className="rounded border border-(--border) px-1 text-[10px] leading-4 text-(--text-faint)">导入</span>
                              )}
                            </li>
                          ))}
                        </ul>
                      </div>
                    ))}
                  </div>
                )}
              </div>
              {selectedConfig && orphans.length > 0 && (
                <div className="mt-3 rounded-lg border border-(--danger-border) bg-(--danger-bg) px-4 py-3">
                  <p className="flex items-center gap-1.5 text-xs font-medium text-(--danger)">
                    <TriangleAlert size={13} />
                    以下代理组已在来源的最新配置中不存在，你添加的成员关系仍保留；组恢复后会自动重新显示，也可以现在移除。
                  </p>
                  <div className="mt-2 space-y-2">
                    {orphans.map((o) => (
                      <div key={o.groupName} className="rounded-md bg-(--panel-bg) px-3 py-2">
                        <p className="text-xs font-medium">
                          {o.groupName} <span className="text-(--danger)">（原代理组已不存在）</span>
                        </p>
                        <ul className="mt-1 space-y-0.5 text-xs text-(--text-dim)">
                          {(o.members ?? []).map((m) => (
                            <li key={m.proxyId} className="flex items-center gap-1.5">
                              <span>{m.proxyName ?? m.name}</span>
                              <span className="rounded bg-(--accent) px-1 text-[10px] leading-4 text-(--accent-contrast)">自定义</span>
                              <button
                                type="button"
                                onClick={() => void removeFromGroup(o.groupName, m.proxyId ?? "")}
                                className="text-[11px] text-(--danger) hover:underline"
                              >
                                移除关系
                              </button>
                            </li>
                          ))}
                        </ul>
                      </div>
                    ))}
                  </div>
                </div>
              )}
            </>
          )}
        </section>
      </div>

      {subFormOpen && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="w-full max-w-lg rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">{editingSub ? "编辑订阅" : "添加订阅"}</h3>
            <div className="mt-4 space-y-3">
              <div>
                <label className={labelCls}>名称</label>
                <input
                  className={inputCls}
                  value={subForm.name}
                  onChange={(e) => setSubForm({ ...subForm, name: e.target.value })}
                  placeholder="例如：主力订阅"
                />
              </div>
              <div>
                <label className={labelCls}>订阅地址</label>
                <input
                  className={inputCls}
                  value={subForm.url}
                  onChange={(e) => setSubForm({ ...subForm, url: e.target.value })}
                  placeholder={editingSub ? `留空表示保留原地址（当前：${editingSub.urlHint}）` : "https://…"}
                />
                <p className="mt-1 text-xs text-(--text-faint)">
                  完整地址只保存在本机凭据库中，不会写入数据库，也不会在界面回显。
                </p>
              </div>
              <div>
                <label className={labelCls}>自动更新间隔</label>
                <select
                  className={inputCls}
                  value={subForm.autoUpdateMinutes}
                  onChange={(e) => setSubForm({ ...subForm, autoUpdateMinutes: Number(e.target.value) })}
                >
                  {AUTO_UPDATE_OPTIONS.map((o) => (
                    <option key={o.value} value={o.value}>
                      {o.label}
                    </option>
                  ))}
                </select>
              </div>
              <label className="flex items-start gap-2 text-sm text-(--text-dim)">
                <input
                  type="checkbox"
                  className="mt-0.5"
                  checked={subForm.allowLan}
                  onChange={(e) => setSubForm({ ...subForm, allowLan: e.target.checked })}
                />
                <span>
                  允许访问局域网订阅地址
                  <span className="block text-xs text-(--text-faint)">
                    仅当订阅服务器位于内网时开启；回环地址与特殊用途地址始终禁止访问。
                  </span>
                </span>
              </label>
            </div>
            <div className="mt-5 flex justify-end gap-2">
              <button type="button" onClick={() => setSubFormOpen(false)} className={btnCls}>
                取消
              </button>
              <button type="button" disabled={savingSub} onClick={() => void submitSubForm()} className={primaryCls}>
                {savingSub ? "保存中…" : "保存"}
              </button>
            </div>
          </div>
        </div>
      )}

      {importOpen && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="w-full max-w-lg rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">导入本地配置</h3>
            <p className="mt-2 text-sm text-(--text-dim)">
              支持完整 Mihomo/Clash YAML、仅含代理节点的 YAML 与 URI 订阅文本（可为 Base64
              编码）。文件中的凭据会被提取到凭据库，保存的副本不含明文凭据。每次导入都会生成一份新的本地配置；
              如需替换旧配置，请先删除旧配置再导入。
            </p>
            <div className="mt-4">
              <label className={labelCls}>配置名称（可留空，默认使用文件名）</label>
              <input
                className={inputCls}
                value={importName}
                onChange={(e) => setImportName(e.target.value)}
                placeholder="例如：机场主配置"
              />
            </div>
            <div className="mt-5 flex justify-end gap-2">
              <button type="button" onClick={() => setImportOpen(false)} className={btnCls}>
                取消
              </button>
              <button type="button" disabled={importing} onClick={() => void doImport()} className={primaryCls}>
                {importing ? "导入中…" : "选择文件并导入"}
              </button>
            </div>
          </div>
        </div>
      )}

      {pendingDeleteSub && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="w-full max-w-md rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">删除订阅</h3>
            <p className="mt-2 text-sm text-(--text-dim)">
              确定删除订阅「{pendingDeleteSub.name}」吗？该订阅导入的全部节点（{pendingDeleteSub.nodeCount}{" "}
              个）、代理组与配置副本会一并删除；本地配置与自定义节点不受影响。此操作不可恢复。
            </p>
            <div className="mt-5 flex justify-end gap-2">
              <button type="button" onClick={() => setPendingDeleteSub(null)} className={btnCls}>
                取消
              </button>
              <button type="button" disabled={deleting} onClick={() => void doDeleteSub()} className={primaryCls}>
                {deleting ? "删除中…" : "确认删除"}
              </button>
            </div>
          </div>
        </div>
      )}

      {pendingDeleteConfig && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="w-full max-w-md rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">删除本地配置</h3>
            <p className="mt-2 text-sm text-(--text-dim)">
              确定删除配置「{pendingDeleteConfig.name}」吗？该配置导入的全部节点（{pendingDeleteConfig.nodeCount}{" "}
              个）与代理组会一并删除；订阅与自定义节点不受影响。此操作不可恢复。
            </p>
            <div className="mt-5 flex justify-end gap-2">
              <button type="button" onClick={() => setPendingDeleteConfig(null)} className={btnCls}>
                取消
              </button>
              <button type="button" disabled={deleting} onClick={() => void doDeleteConfig()} className={primaryCls}>
                {deleting ? "删除中…" : "确认删除"}
              </button>
            </div>
          </div>
        </div>
      )}

      {proxyForm && (
        <ProxyFormModal
          form={proxyForm}
          busy={busy}
          inputCls={inputCls}
          labelCls={labelCls}
          onChange={setProxyForm}
          onClose={() => setProxyForm(null)}
          onSubmit={() => void submitProxyForm()}
        />
      )}

      {pickerGroup && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="w-full max-w-md rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">添加自定义节点到「{pickerGroup}」</h3>
            <p className="mt-1 text-xs text-(--text-faint)">
              仅加入成员关系，不复制节点或凭据；订阅刷新后仍会保留。
            </p>
            <div className="mt-4 max-h-64 space-y-1 overflow-y-auto">
              {candidates.length === 0 ? (
                <p className="text-sm text-(--text-faint)">没有可添加的自定义节点（可能已全部加入该组）。</p>
              ) : (
                candidates.map((cand) => (
                  <button
                    key={cand.id}
                    type="button"
                    onClick={() => void addToGroup(cand.id)}
                    className="flex w-full items-center justify-between rounded-md border border-(--border) px-3 py-2 text-left text-sm hover:bg-(--hover-bg)"
                  >
                    <span>{cand.name}</span>
                    <span className="text-xs text-(--text-faint)">
                      {cand.protocol} · {cand.address}
                      {cand.port != null ? `:${cand.port}` : ""}
                    </span>
                  </button>
                ))
              )}
            </div>
            <div className="mt-5 flex justify-end">
              <button
                type="button"
                onClick={() => setPickerGroup(null)}
                className="rounded-md border border-(--border) px-3 py-1.5 text-sm text-(--text-dim) hover:bg-(--hover-bg)"
              >
                取消
              </button>
            </div>
          </div>
        </div>
      )}

      {detail && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="max-h-[85vh] w-full max-w-lg overflow-y-auto rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">节点详情</h3>
            <dl className="mt-4 space-y-2 text-sm">
              <DetailRow label="名称" value={detail.name} />
              <DetailRow label="协议" value={detail.protocol === "direct" ? "直连" : detail.protocol} />
              <DetailRow
                label="地址"
                value={detail.protocol === "direct" ? "—" : `${detail.address}${detail.port != null ? `:${detail.port}` : ""}`}
              />
              <DetailRow label="来源" value={SOURCE_LABEL[detail.source] ?? detail.source} />
              <DetailRow label="状态" value={detail.enabled ? "已启用" : "已停用"} />
              <DetailRow
                label="最近测试"
                value={
                  detail.lastTestOk == null
                    ? "未测试"
                    : detail.lastTestOk
                      ? `连接成功，延迟 ${detail.lastLatencyMs ?? 0} ms（${detail.lastTestedAt ?? ""}）`
                      : `连接失败（${detail.lastTestedAt ?? ""}）`
                }
              />
              <DetailRow
                label="凭据"
                value={
                  detail.credentialStatus === "ok"
                    ? `已设置（字段：${(detail.secretFields ?? []).join("、")}，明文不回显）`
                    : detail.credentialStatus === "unavailable"
                      ? "凭据不可用"
                      : "无"
                }
              />
              {detail.credentialStatus === "unavailable" && (
                <div className="rounded-lg border border-(--danger-border) bg-(--danger-bg) px-3 py-2 text-xs leading-relaxed text-(--danger)">
                  该节点的凭据无法从凭据库读取（例如更换了 Windows 用户或机器后无法解密）。节点数据本身完好：
                  {detail.source === "custom"
                    ? "编辑该节点并重新输入凭据即可恢复。"
                    : "可通过刷新来源重新导入凭据。"}
                  在此之前，该节点不会被当作无凭据节点使用。
                </div>
              )}
              <div>
                <dt className="text-xs text-(--text-dim)">高级配置（非敏感参数）</dt>
                <dd className="mt-1 overflow-x-auto rounded-md bg-(--app-bg) px-2.5 py-1.5 font-mono text-xs text-(--text-dim)">
                  {detail.configJson}
                </dd>
              </div>
              {detail.source === "custom" && (
                <div>
                  <dt className="text-xs text-(--text-dim)">已加入的代理组</dt>
                  <dd className="mt-1 text-sm">
                    {detailMemberships.length === 0 ? (
                      <span className="text-(--text-faint)">尚未加入任何配置的代理组</span>
                    ) : (
                      <ul className="space-y-1">
                        {detailMemberships.map((m) => (
                          <li key={`${m.configId}-${m.groupName}`} className="flex items-center gap-1.5">
                            <span>
                              {m.configName} · {m.groupName}
                            </span>
                            {!m.groupExists && (
                              <span className="rounded border border-(--danger-border) px-1 text-[10px] leading-4 text-(--danger)">原代理组已不存在</span>
                            )}
                          </li>
                        ))}
                      </ul>
                    )}
                  </dd>
                </div>
              )}
              <DetailRow label="创建时间" value={detail.createdAt} />
              <DetailRow label="更新时间" value={detail.updatedAt} />
            </dl>
            <div className="mt-5 flex justify-end gap-2">
              {detail.source === "custom" && (
                <button
                  type="button"
                  onClick={() => {
                    setProxyForm(formFromProxy(detail));
                    setDetail(null);
                  }}
                  className="rounded-md border border-(--border) px-3 py-1.5 text-sm text-(--text-dim) hover:bg-(--hover-bg)"
                >
                  编辑
                </button>
              )}
              <button
                type="button"
                onClick={() => setDetail(null)}
                className="rounded-md bg-(--accent) px-3 py-1.5 text-sm font-medium text-(--accent-contrast) hover:opacity-90"
              >
                关闭
              </button>
            </div>
          </div>
        </div>
      )}

      {pendingDeleteProxy && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="w-full max-w-md rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">删除节点「{pendingDeleteProxy.name}」</h3>
            <p className="mt-2 text-sm leading-relaxed text-(--text-dim)">
              将删除该自定义节点；引用它的浏览器实例将变为未绑定代理。
              {deleteMembershipCount > 0 &&
                `该节点已加入 ${deleteMembershipCount} 个配置的代理组，删除后将同时移除这些自定义成员关系。`}
              {pendingDeleteProxy.hasSecrets &&
                "该节点的凭据将在确认没有其他记录引用后从凭据库中清理。"}
              此操作不可恢复。
            </p>
            <div className="mt-5 flex justify-end gap-2">
              <button
                type="button"
                onClick={() => setPendingDeleteProxy(null)}
                className="rounded-md border border-(--border) px-3 py-1.5 text-sm text-(--text-dim) hover:bg-(--hover-bg)"
              >
                取消
              </button>
              <button
                type="button"
                disabled={busy}
                onClick={() => void confirmDeleteProxy()}
                className="rounded-md bg-(--danger) px-3 py-1.5 text-sm font-medium text-white hover:opacity-90 disabled:opacity-50"
              >
                确认删除
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

function DetailRow({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex gap-3">
      <dt className="w-20 shrink-0 text-xs leading-5 text-(--text-dim)">{label}</dt>
      <dd className="min-w-0 flex-1 text-(--text-main)">{value}</dd>
    </div>
  );
}

function ProxyFormModal({
  form,
  busy,
  inputCls,
  labelCls,
  onChange,
  onClose,
  onSubmit,
}: {
  form: ProxyForm;
  busy: boolean;
  inputCls: string;
  labelCls: string;
  onChange: (f: ProxyForm) => void;
  onClose: () => void;
  onSubmit: () => void;
}) {
  const set = (patch: Partial<ProxyForm>) => onChange({ ...form, ...patch });
  const secretFields = SECRET_FIELDS[form.protocol] ?? [];
  const configFields = CONFIG_FIELDS[form.protocol] ?? [];
  const isDirect = form.protocol === "direct";

  // 切换协议时，按新协议重新拆分已有高级配置。
  const changeProtocol = (protocol: string) => {
    let restObj: Record<string, unknown> = {};
    try {
      const parsed: unknown = JSON.parse(form.configRest || "{}");
      if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
        restObj = parsed as Record<string, unknown>;
      }
    } catch {
      restObj = {};
    }
    for (const field of CONFIG_FIELDS[form.protocol] ?? []) {
      const v = form.configValues[field.key];
      if (v) restObj[field.key] = v;
    }
    const merged = JSON.stringify(restObj);
    const { values, rest } = splitConfig(protocol, merged);
    onChange({ ...form, protocol, configValues: values, configRest: rest, secretValues: {} });
  };

  return (
    <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
      <div className="max-h-[88vh] w-full max-w-lg overflow-y-auto rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
        <h3 className="text-base font-semibold">{form.id ? "编辑自定义节点" : "添加自定义节点"}</h3>
        <div className="mt-4 space-y-3">
          <div>
            <label className={labelCls}>名称</label>
            <input
              className={inputCls}
              value={form.name}
              onChange={(e) => set({ name: e.target.value })}
              placeholder="例如：香港节点 01"
            />
          </div>
          <div>
            <label className={labelCls}>协议</label>
            <select className={inputCls} value={form.protocol} onChange={(e) => changeProtocol(e.target.value)}>
              {PROTOCOLS.map((p) => (
                <option key={p} value={p}>
                  {p === "direct" ? "直连" : p}
                </option>
              ))}
            </select>
          </div>
          {!isDirect && (
            <div className="grid grid-cols-3 gap-3">
              <div className="col-span-2">
                <label className={labelCls}>服务器地址</label>
                <input
                  className={inputCls}
                  value={form.address}
                  onChange={(e) => set({ address: e.target.value })}
                  placeholder="例如：hk.example.com 或 203.0.113.10"
                />
              </div>
              <div>
                <label className={labelCls}>端口</label>
                <input
                  className={inputCls}
                  value={form.port}
                  inputMode="numeric"
                  onChange={(e) => set({ port: e.target.value })}
                  placeholder="1080"
                />
              </div>
            </div>
          )}
          {configFields.map((field) => (
            <div key={field.key}>
              <label className={labelCls}>{field.label}</label>
              <input
                className={inputCls}
                value={form.configValues[field.key] ?? ""}
                placeholder={field.placeholder}
                onChange={(e) => set({ configValues: { ...form.configValues, [field.key]: e.target.value } })}
              />
            </div>
          ))}
          {secretFields.length > 0 && (
            <fieldset className="rounded-lg border border-(--border) px-3 py-2.5">
              <legend className="px-1 text-xs text-(--text-dim)">凭据（保存在凭据库，不回显）</legend>
              {form.id && form.hasSecrets && form.credentialStatus === "ok" && (
                <p className="mb-2 text-xs text-(--text-faint)">
                  已设置凭据字段：{form.secretFields.join("、")}。下方留空表示不修改。
                </p>
              )}
              {form.id && form.credentialStatus === "unavailable" && (
                <p className="mb-2 text-xs text-(--danger)">当前凭据不可用，请重新输入凭据以恢复该节点。</p>
              )}
              <div className="space-y-3">
                {secretFields.map((field) => (
                  <div key={field.key}>
                    <label className={labelCls}>{field.label}</label>
                    <input
                      className={inputCls}
                      type="password"
                      autoComplete="new-password"
                      value={form.secretValues[field.key] ?? ""}
                      onChange={(e) => set({ secretValues: { ...form.secretValues, [field.key]: e.target.value } })}
                      placeholder={form.id ? "留空表示不修改" : ""}
                    />
                  </div>
                ))}
              </div>
              {form.id && form.hasSecrets && (
                <label className="mt-3 flex items-center gap-2 text-xs text-(--text-dim)">
                  <input
                    type="checkbox"
                    checked={form.clearSecrets}
                    onChange={(e) => set({ clearSecrets: e.target.checked })}
                  />
                  清除已保存的凭据（填写上方凭据时以新凭据为准）
                </label>
              )}
            </fieldset>
          )}
          <div className="grid grid-cols-2 gap-3">
            <div>
              <label className={labelCls}>排序（数字小者靠前）</label>
              <input
                className={inputCls}
                value={form.sortOrder}
                inputMode="numeric"
                onChange={(e) => set({ sortOrder: e.target.value })}
              />
            </div>
            {form.id && (
              <div>
                <label className={labelCls}>状态</label>
                <select
                  className={inputCls}
                  value={form.enabled ? "on" : "off"}
                  onChange={(e) => set({ enabled: e.target.value === "on" })}
                >
                  <option value="on">已启用</option>
                  <option value="off">已停用</option>
                </select>
              </div>
            )}
          </div>
          <div>
            <label className={labelCls}>高级配置（JSON，非敏感参数）</label>
            <textarea
              className={`${inputCls} font-mono text-xs`}
              rows={4}
              value={form.configRest}
              onChange={(e) => set({ configRest: e.target.value })}
              placeholder="{}"
            />
            <p className="mt-1 text-xs text-(--text-faint)">
              密码、UUID、私钥等凭据字段不能写入此处，请使用上方凭据输入框。
            </p>
          </div>
        </div>
        <div className="mt-5 flex justify-end gap-2">
          <button
            type="button"
            onClick={onClose}
            className="rounded-md border border-(--border) px-3 py-1.5 text-sm text-(--text-dim) hover:bg-(--hover-bg)"
          >
            取消
          </button>
          <button
            type="button"
            disabled={busy}
            onClick={onSubmit}
            className="rounded-md bg-(--accent) px-3 py-1.5 text-sm font-medium text-(--accent-contrast) hover:opacity-90 disabled:opacity-50"
          >
            保存
          </button>
        </div>
      </div>
    </div>
  );
}
