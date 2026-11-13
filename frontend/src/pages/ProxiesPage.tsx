import { useCallback, useEffect, useMemo, useState } from "react";
import {
  Eye,
  Gauge,
  Pencil,
  Plus,
  RefreshCw,
  ShieldAlert,
  Trash2,
  TriangleAlert,
} from "lucide-react";
import { ProxyService } from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import type {
  ProxyPortRange,
  ProxyView,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import { useAppStore } from "../stores/useAppStore";

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

const SOURCE_LABEL: Record<string, string> = {
  manual: "手动",
  file_import: "文件导入",
  subscription: "订阅",
};

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

const EMPTY_FORM: ProxyForm = {
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

/**
 * 代理管理（Stage 4A 基础能力）：代理节点的增删改查、
 * 连接测试与延迟状态。凭据只在表单提交瞬间以明文经
 * bindings 直达后端凭据库：页面不回显、不持久化任何凭据明文。
 * 订阅导入与 Mihomo 核心管理属于后续阶段，本页不提供入口。
 */
export default function ProxiesPage() {
  const log = useAppStore((state) => state.log);
  const [proxies, setProxies] = useState<ProxyView[]>([]);
  const [portRange, setPortRange] = useState<ProxyPortRange | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [search, setSearch] = useState("");
  const [form, setForm] = useState<ProxyForm | null>(null);
  const [detail, setDetail] = useState<ProxyView | null>(null);
  const [pendingDelete, setPendingDelete] = useState<ProxyView | null>(null);
  const [testingId, setTestingId] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const [list, range] = await Promise.all([
        ProxyService.ListProxies(),
        ProxyService.GetProxyPortRange(),
      ]);
      setProxies(list ?? []);
      setPortRange(range);
      setError(null);
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setError(`加载代理列表失败：${message}`);
      log("error", `加载代理列表失败：${message}`);
    } finally {
      setLoading(false);
    }
  }, [log]);

  useEffect(() => {
    void load();
  }, [load]);

  const visible = useMemo(() => {
    const kw = search.trim().toLowerCase();
    return proxies.filter((p) => {
      if (
        kw &&
        !`${p.name} ${p.protocol} ${p.address}`.toLowerCase().includes(kw)
      )
        return false;
      return true;
    });
  }, [proxies, search]);

  const submitForm = async () => {
    if (!form) return;
    setBusy(true);
    try {
      // 合并普通配置：已知字段 + 高级配置 JSON。
      let restObj: Record<string, unknown> = {};
      const restText = form.configRest.trim() || "{}";
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
      for (const field of CONFIG_FIELDS[form.protocol] ?? []) {
        const v = (form.configValues[field.key] ?? "").trim();
        if (v) restObj[field.key] = v;
        else delete restObj[field.key];
      }
      const secrets: Record<string, string> = {};
      for (const field of SECRET_FIELDS[form.protocol] ?? []) {
        const v = form.secretValues[field.key] ?? "";
        if (v) secrets[field.key] = v;
      }
      const portNum = form.port.trim() === "" ? null : Number(form.port);
      const base = {
        name: form.name,
        protocol: form.protocol,
        address: form.address,
        port: portNum,
        configJson: JSON.stringify(restObj),
        sortOrder: Number(form.sortOrder) || 0,
        secrets: Object.keys(secrets).length > 0 ? secrets : null,
      };
      if (form.id) {
        await ProxyService.UpdateProxy({
          ...base,
          id: form.id,
          enabled: form.enabled,
          clearSecrets: form.clearSecrets && Object.keys(secrets).length === 0,
        });
        log("info", `代理已更新：${form.name}`);
      } else {
        const created = await ProxyService.CreateProxy(base);
        log("info", `代理已创建：${created.name}`);
      }
      setForm(null); // 关闭即丢弃表单中的凭据明文
      await load();
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setError(message);
      log("error", `保存代理失败：${message}`);
    } finally {
      setBusy(false);
    }
  };

  const confirmDelete = async () => {
    if (!pendingDelete) return;
    setBusy(true);
    try {
      await ProxyService.DeleteProxy(pendingDelete.id);
      log("info", `代理已删除：${pendingDelete.name}`);
      setPendingDelete(null);
      await load();
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      setError(message);
      log("error", `删除代理失败：${message}`);
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

  const inputCls =
    "w-full rounded-md border border-(--border) bg-(--app-bg) px-2.5 py-1.5 text-sm text-(--text-main) outline-none placeholder:text-(--text-faint)";
  const labelCls = "mb-1 block text-xs text-(--text-dim)";
  const btnCls =
    "flex items-center gap-1.5 rounded-md border border-(--border) px-3 py-1.5 text-sm text-(--text-dim) hover:bg-(--hover-bg)";

  const latencyCell = (p: ProxyView) => {
    if (p.lastTestOk == null) return <span className="text-(--text-faint)">未测试</span>;
    if (p.lastTestOk)
      return (
        <span className="rounded-full bg-(--badge-bg) px-2 py-0.5 text-xs text-(--text-dim)">
          {p.lastLatencyMs ?? 0} ms
        </span>
      );
    return (
      <span className="rounded-full bg-(--danger-bg) px-2 py-0.5 text-xs text-(--danger)">
        连接失败
      </span>
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

  return (
    <div className="mx-auto max-w-6xl px-6 py-6">
      <div className="flex items-center justify-between">
        <div>
          <h2 className="text-lg font-semibold">代理管理</h2>
          <p className="mt-0.5 text-sm text-(--text-dim)">
            管理代理节点。凭据保存在本地凭据库中，页面不回显明文。
          </p>
        </div>
        <div className="flex gap-2">
          <button type="button" onClick={() => void load()} className={btnCls}>
            <RefreshCw size={14} /> 刷新
          </button>
          <button
            type="button"
            onClick={() => setForm(EMPTY_FORM)}
            className="flex items-center gap-1.5 rounded-md bg-(--accent) px-3 py-1.5 text-sm font-medium text-(--accent-contrast) hover:opacity-90"
          >
            <Plus size={14} /> 新建代理
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

      <div className="mt-4 flex flex-wrap items-center gap-2">
        <input
          className="w-56 rounded-md border border-(--border) bg-(--panel-bg) px-2.5 py-1.5 text-sm text-(--text-main) outline-none placeholder:text-(--text-faint)"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          placeholder="搜索名称、协议或地址"
        />
        <span className="text-xs text-(--text-faint)">
          连接测试为 TCP 连通性测试；经 Mihomo 核心的协议级测速将在核心集成阶段提供。
          {portRange && ` 实例监听端口范围：${portRange.start}–${portRange.end}。`}
        </span>
      </div>

      <div className="mt-4 overflow-hidden rounded-lg border border-(--border) bg-(--panel-bg)">
        {loading ? (
          <p className="px-4 py-10 text-center text-sm text-(--text-faint)">加载中…</p>
        ) : visible.length === 0 ? (
          <div className="flex flex-col items-center px-4 py-12 text-center">
            <span className="flex h-12 w-12 items-center justify-center rounded-2xl bg-(--hover-bg) text-(--text-faint)">
              <Gauge size={22} />
            </span>
            <p className="mt-3 text-sm text-(--text-dim)">
              {proxies.length === 0 ? "暂无代理节点" : "没有符合筛选条件的代理"}
            </p>
            <p className="mt-1 text-xs text-(--text-faint)">点击右上角「新建代理」添加第一个节点。</p>
          </div>
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
              {visible.map((p) => (
                <tr key={p.id} className="border-b border-(--border) last:border-0 hover:bg-(--hover-bg)">
                  <td className="px-4 py-2.5 font-medium">{p.name}</td>
                  <td className="px-4 py-2.5 text-(--text-dim)">
                    {p.protocol === "direct" ? "直连" : p.protocol}
                  </td>
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
                        p.enabled
                          ? "bg-(--badge-bg) text-(--text-dim)"
                          : "bg-(--hover-bg) text-(--text-faint)"
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
                      <button
                        type="button"
                        title="编辑"
                        onClick={() => setForm(formFromProxy(p))}
                        className="rounded p-1.5 text-(--text-dim) hover:bg-(--hover-bg)"
                      >
                        <Pencil size={14} />
                      </button>
                      <button
                        type="button"
                        title="删除"
                        onClick={() => setPendingDelete(p)}
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
        <ProxyFormModal
          form={form}
          busy={busy}
          inputCls={inputCls}
          labelCls={labelCls}
          onChange={setForm}
          onClose={() => setForm(null)}
          onSubmit={() => void submitForm()}
        />
      )}

      {detail && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="max-h-[85vh] w-full max-w-lg overflow-y-auto rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">代理详情</h3>
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
                  编辑该节点并重新输入凭据即可恢复。在此之前，该节点不会被当作无凭据节点使用。
                </div>
              )}
              <div>
                <dt className="text-xs text-(--text-dim)">高级配置（非敏感参数）</dt>
                <dd className="mt-1 overflow-x-auto rounded-md bg-(--app-bg) px-2.5 py-1.5 font-mono text-xs text-(--text-dim)">
                  {detail.configJson}
                </dd>
              </div>
              <DetailRow label="创建时间" value={detail.createdAt} />
              <DetailRow label="更新时间" value={detail.updatedAt} />
            </dl>
            <div className="mt-5 flex justify-end gap-2">
              <button
                type="button"
                onClick={() => {
                  setForm(formFromProxy(detail));
                  setDetail(null);
                }}
                className="rounded-md border border-(--border) px-3 py-1.5 text-sm text-(--text-dim) hover:bg-(--hover-bg)"
              >
                编辑
              </button>
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

      {pendingDelete && (
        <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
          <div className="w-full max-w-md rounded-xl border border-(--border) bg-(--panel-bg) p-5 shadow-xl">
            <h3 className="text-base font-semibold">删除代理「{pendingDelete.name}」</h3>
            <p className="mt-2 text-sm leading-relaxed text-(--text-dim)">
              将删除该代理节点；引用它的浏览器实例将变为未绑定代理。
              {pendingDelete.hasSecrets &&
                "该节点的凭据将在确认没有其他记录引用后从凭据库中清理。"}
              此操作不可恢复。
            </p>
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
        <h3 className="text-base font-semibold">{form.id ? "编辑代理" : "新建代理"}</h3>
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
            <select
              className={inputCls}
              value={form.protocol}
              onChange={(e) => changeProtocol(e.target.value)}
            >
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
                onChange={(e) =>
                  set({ configValues: { ...form.configValues, [field.key]: e.target.value } })
                }
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
                <p className="mb-2 text-xs text-(--danger)">
                  当前凭据不可用，请重新输入凭据以恢复该节点。
                </p>
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
                      onChange={(e) =>
                        set({ secretValues: { ...form.secretValues, [field.key]: e.target.value } })
                      }
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
