import { useMemo, useState } from "react";
import { Bookmark, ChevronDown, ChevronRight, Folder, Loader2, Search, X } from "lucide-react";
import {
  BookmarkService,
  type ApplySearchResult,
  type BookmarkInstanceInfo,
  type BookmarkSearchResult,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import type { BookmarkNode } from "../../bindings/github.com/onbeee/DeBrowser/internal/model";
import { newOperationId, recordOperationFeedback } from "../lib/operations";
import { notify, notifyError, notifyWarning } from "../stores/useNotificationStore";
import { classifyApplyResult, classifySyncResult } from "../lib/operationResult";
import type { NotificationItem } from "../lib/notifications";
import { buildTree, visibleRows } from "./bookmarksLogic";

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

type SyncTab = "bookmarks" | "edit" | "delete";

// 同步弹窗：三个页签。
// - 书签：把资源库所选节点分发到所选实例的书签栏。
// - 编辑/删除：跨实例聚合搜索真实书签（只查已确认停止且可读的
//   实例），结果只显示名称、网址和实例数量，绝不显示实例明细；
//   执行时后端会重新核对实例状态并按节点 ID 精确定位。
export default function SyncDialog(props: {
  nodes: BookmarkNode[];
  instances: BookmarkInstanceInfo[];
  onClose: () => void;
}) {
  const [tab, setTab] = useState<SyncTab>("bookmarks");
  const [busy, setBusy] = useState(false);

  const run = async (
    action: string,
    fn: (opId: string) => Promise<{ ok: boolean; message: string } | void>,
  ) => {
    setBusy(true);
    const opId = newOperationId();
    try {
      const outcome = await fn(opId);
      if (outcome) {
        recordOperationFeedback(opId, outcome.ok, outcome.message, "书签管理", action);
      } else {
        recordOperationFeedback(opId, true, action + "完成", "书签管理", action);
      }
    } catch (e) {
      const message = errText(e);
      notifyError(message, { operationId: opId, important: true });
      recordOperationFeedback(opId, false, message, "书签管理", action);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
      <div className="flex max-h-[88vh] w-full max-w-3xl flex-col rounded-lg bg-(--panel-bg) p-4 shadow-xl">
        <div className="flex items-center gap-2">
          <h3 className="text-base font-semibold">同步</h3>
          <div className="ml-2 flex rounded-md bg-(--hover-bg) p-0.5">
            {(
              [
                ["bookmarks", "书签"],
                ["edit", "编辑"],
                ["delete", "删除"],
              ] as [SyncTab, string][]
            ).map(([t, label]) => (
              <button
                key={t}
                type="button"
                className={
                  "rounded px-3 py-1 text-sm " + (tab === t ? "bg-(--accent) text-white" : "text-(--text-main)")
                }
                onClick={() => setTab(t)}
              >
                {label}
              </button>
            ))}
          </div>
          <button type="button" className="ml-auto text-(--text-faint)" onClick={props.onClose}>
            <X size={17} />
          </button>
        </div>
        <div className="mt-3 min-h-0 flex-1 overflow-y-auto">
          {tab === "bookmarks" && (
            <SyncBookmarksTab nodes={props.nodes} instances={props.instances} busy={busy} run={run} />
          )}
          {tab === "edit" && <SearchTab mode="edit" busy={busy} run={run} />}
          {tab === "delete" && <SearchTab mode="delete" busy={busy} run={run} />}
        </div>
      </div>
    </div>
  );
}

function SyncBookmarksTab(props: {
  nodes: BookmarkNode[];
  instances: BookmarkInstanceInfo[];
  busy: boolean;
  run: (
    action: string,
    fn: (opId: string) => Promise<{ ok: boolean; message: string } | void>,
  ) => Promise<void>;
}) {
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(new Set());
  const [instSelected, setInstSelected] = useState<ReadonlySet<string>>(new Set());
  const [instQuery, setInstQuery] = useState("");

  const tree = useMemo(() => buildTree(props.nodes), [props.nodes]);
  const rows = useMemo(() => visibleRows(tree, collapsed), [tree, collapsed]);
  const shownInstances = useMemo(() => {
    const q = instQuery.trim().toLowerCase();
    return q ? props.instances.filter((i) => i.name.toLowerCase().includes(q)) : props.instances;
  }, [props.instances, instQuery]);

  const toggle = (set: ReadonlySet<string>, id: string, apply: (s: Set<string>) => void) => {
    const next = new Set(set);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    apply(next);
  };

  return (
    <div className="flex flex-col gap-3">
      <div className="flex gap-3">
        <div className="min-w-0 flex-1 rounded-md border border-(--border)">
          <p className="border-b border-(--border) px-2 py-1.5 text-xs font-semibold">选择书签</p>
          <div className="max-h-64 overflow-y-auto p-1">
            {rows.length === 0 ? (
              <p className="px-2 py-3 text-sm text-(--text-faint)">资源库为空。</p>
            ) : (
              rows.map((row) => (
                <div
                  key={row.node.id}
                  className="flex items-center gap-1.5 rounded px-1.5 py-1 text-sm hover:bg-(--hover-bg)"
                  style={{ paddingLeft: 6 + row.depth * 18 }}
                >
                  {row.node.kind === "folder" ? (
                    <button
                      type="button"
                      className="shrink-0 text-(--text-faint)"
                      onClick={() =>
                        setCollapsed((prev) => {
                          const next = new Set(prev);
                          if (next.has(row.node.id)) next.delete(row.node.id);
                          else next.add(row.node.id);
                          return next;
                        })
                      }
                    >
                      {collapsed.has(row.node.id) ? <ChevronRight size={14} /> : <ChevronDown size={14} />}
                    </button>
                  ) : (
                    <span className="w-3.5 shrink-0" />
                  )}
                  <input
                    type="checkbox"
                    checked={selected.has(row.node.id)}
                    onChange={() => toggle(selected, row.node.id, setSelected)}
                  />
                  {row.node.kind === "folder" ? (
                    <Folder size={15} className="shrink-0 text-(--accent)" />
                  ) : (
                    <Bookmark size={15} className="shrink-0 text-(--text-faint)" />
                  )}
                  <span className="min-w-0 flex-1 truncate">{row.node.name}</span>
                </div>
              ))
            )}
          </div>
        </div>
        <div className="w-64 shrink-0 rounded-md border border-(--border)">
          <p className="flex items-center gap-1 border-b border-(--border) px-2 py-1.5 text-xs font-semibold">
            选择实例
            <label className="ml-auto flex items-center gap-1 rounded bg-(--hover-bg) px-1.5 py-0.5 font-normal">
              <Search size={12} className="text-(--text-faint)" />
              <input
                className="w-20 bg-transparent text-xs outline-none"
                placeholder="搜索"
                value={instQuery}
                onChange={(e) => setInstQuery(e.target.value)}
              />
            </label>
          </p>
          <label className="flex items-center gap-2 border-b border-(--border) px-2 py-1 text-xs text-(--text-faint)">
            <input
              type="checkbox"
              checked={shownInstances.length > 0 && shownInstances.every((i) => instSelected.has(i.id))}
              onChange={(e) => {
                setInstSelected((prev) => {
                  const next = new Set(prev);
                  for (const i of shownInstances) {
                    if (e.target.checked) next.add(i.id);
                    else next.delete(i.id);
                  }
                  return next;
                });
              }}
            />
            全选
          </label>
          <div className="max-h-56 overflow-y-auto p-1">
            {shownInstances.map((inst) => (
              <label
                key={inst.id}
                className="flex cursor-pointer items-center gap-2 rounded px-2 py-1 text-sm hover:bg-(--hover-bg)"
              >
                <input
                  type="checkbox"
                  checked={instSelected.has(inst.id)}
                  onChange={() => toggle(instSelected, inst.id, setInstSelected)}
                />
                <span className="min-w-0 flex-1 truncate">{inst.name}</span>
              </label>
            ))}
          </div>
        </div>
      </div>
      <p className="text-xs text-(--text-faint)">
        同步到实例的书签栏；同文件夹内网址相同的书签会跳过，同名文件夹会复用。运行中或状态无法确认的实例会被跳过。
      </p>
      <div className="flex items-center gap-2">
        <button
          type="button"
          className="rounded-md bg-(--accent) px-3 py-1.5 text-sm text-white disabled:opacity-50"
          disabled={props.busy || selected.size === 0 || instSelected.size === 0}
          onClick={() =>
            void props.run("同步书签", async (opId) => {
              const res = await BookmarkService.SyncBookmarks(opId, [...selected], [...instSelected]);
              const items: NotificationItem[] = (res.instances ?? []).map((i) => ({
                name: i.instanceName,
                status:
                  i.status === "success" ? "success" : i.status === "skipped" ? "skipped" : "failed",
                reason: i.reason || undefined,
              }));
              const kind = classifySyncResult(res);
              const summary =
                kind === "success"
                  ? "书签同步完成"
                  : kind === "warning"
                    ? "书签同步部分完成"
                    : kind === "error"
                      ? "书签同步失败"
                      : "书签同步未执行（全部跳过）";
              const detail =
                `成功 ${res.successInstances} 个实例 · 失败 ${res.failedInstances} · 跳过 ${res.skippedInstances}；` +
                `新增书签 ${res.addedBookmarks} · 跳过 ${res.skippedBookmarks} · 失败 ${res.failedBookmarks}`;
              notify(kind, summary, {
                detail,
                items,
                operationId: opId,
                important: true,
              });
              // 日志与通知一致：存在失败时不记为整体成功，
              // 记与用户看到的相同的汇总文字。
              return {
                ok: res.failedInstances + res.failedBookmarks === 0,
                message: `${summary}：${detail}`,
              };
            })
          }
        >
          {props.busy ? <Loader2 size={14} className="animate-spin" /> : "开始同步"}
        </button>
      </div>
    </div>
  );
}

function SearchTab(props: {
  mode: "edit" | "delete";
  busy: boolean;
  run: (
    action: string,
    fn: (opId: string) => Promise<{ ok: boolean; message: string } | void>,
  ) => Promise<void>;
}) {
  const [kind, setKind] = useState<string>("bookmark");
  const [query, setQuery] = useState("");
  const [search, setSearch] = useState<BookmarkSearchResult | null>(null);
  const [picked, setPicked] = useState<ReadonlySet<string>>(new Set());
  const [newName, setNewName] = useState("");
  const [newUrl, setNewUrl] = useState("");
  const [armDelete, setArmDelete] = useState(false);

  const doSearch = () =>
    void props.run("搜索实例书签", async (opId) => {
      const res = await BookmarkService.SearchInstanceBookmarks(opId, query, kind);
      setSearch(res);
      setPicked(new Set());
      setArmDelete(false);
    });

  // 编辑/删除执行后：用原搜索条件重新查询，结果列表立即
  // 反映最新状态（已删除项消失、改名项显示新名称），并清
  // 空选择。刷新失败与操作结果分开提示，不误报操作失败。
  // 汇总数字全部来自后端返回，不展示实例名称（聚合规则）。
  const applyAndRefresh = async (
    opId: string,
    apply: () => Promise<ApplySearchResult>,
  ): Promise<{ ok: boolean; message: string } | void> => {
    const res = await apply();
    const verb = props.mode === "delete" ? "删除" : "修改";
    const kind = classifyApplyResult(res);
    if (kind === null) return; // 没有执行任何项目：不通知
    const summary =
      kind === "error"
        ? `${verb}失败：处理 ${res.updatedInstances} 个实例 · 失败 ${res.failedInstances} · 跳过 ${res.skippedInstances}`
        : kind === "info"
          ? `${verb}未执行（全部跳过）：跳过 ${res.skippedInstances} 个实例`
          : `${verb}完成：处理 ${res.updatedInstances} 个实例 · 失败 ${res.failedInstances} · 跳过 ${res.skippedInstances}`;
    const detail = `处理 ${res.affectedNodes} 项 · 失败 ${res.failedNodes} 项`;
    notify(kind, summary, {
      detail,
      operationId: opId,
      important: true,
    });
    setPicked(new Set());
    setArmDelete(false);
    try {
      const fresh = await BookmarkService.SearchInstanceBookmarks(newOperationId(), query, kind);
      setSearch(fresh);
    } catch {
      // 操作结果已按上方通知为准；刷新失败单独提示（仅通知台）。
      notifyWarning(`${verb}已完成，但列表刷新失败，请重新搜索。`);
    }
    return {
      ok: res.failedInstances + res.failedNodes === 0,
      message: `${summary}：${detail}`,
    };
  };

  const pickedGroups = (search?.groups ?? []).filter((g) => picked.has(g.key));
  const impactedInstances = pickedGroups.reduce((max, g) => Math.max(max, g.instanceCount), 0);

  return (
    <div className="flex flex-col gap-3">
      <div className="flex items-center gap-2">
        <div className="flex rounded-md bg-(--hover-bg) p-0.5">
          {(
            [
              ["bookmark", "书签"],
              ["folder", "文件夹"],
            ] as [string, string][]
          ).map(([k, label]) => (
            <button
              key={k}
              type="button"
              className={"rounded px-3 py-1 text-sm " + (kind === k ? "bg-(--accent) text-white" : "text-(--text-main)")}
              onClick={() => {
                setKind(k);
                setSearch(null);
                setPicked(new Set());
              }}
            >
              {label}
            </button>
          ))}
        </div>
        <label className="flex flex-1 items-center gap-1 rounded-md bg-(--hover-bg) px-2 py-1.5">
          <Search size={14} className="text-(--text-faint)" />
          <input
            className="w-full bg-transparent text-sm outline-none"
            placeholder={kind === "bookmark" ? "输入书签名称或网址" : "输入文件夹名称"}
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") doSearch();
            }}
          />
        </label>
        <button
          type="button"
          className="rounded-md bg-(--accent) px-3 py-1.5 text-sm text-white disabled:opacity-50"
          disabled={props.busy}
          onClick={doSearch}
        >
          搜索
        </button>
      </div>
      <p className="text-xs text-(--text-faint)">
        搜索范围仅包括已确认停止且书签可读取的实例。
        {search && search.uncheckedInstances > 0
          ? "另有 " + search.uncheckedInstances + " 个实例当前无法检查，搜索结果不包含这些实例。"
          : ""}
      </p>
      {search && (
        <div className="rounded-md border border-(--border)">
          {(search.groups ?? []).length === 0 ? (
            <p className="px-3 py-3 text-sm text-(--text-faint)">没有找到匹配的{kind === "bookmark" ? "书签" : "文件夹"}。</p>
          ) : (
            (search.groups ?? []).map((g) => (
              <label
                key={g.key}
                className="flex cursor-pointer items-center gap-2 border-b border-(--border) px-3 py-1.5 text-sm last:border-0 hover:bg-(--hover-bg)"
              >
                <input
                  type="checkbox"
                  checked={picked.has(g.key)}
                  onChange={() => {
                    setPicked((prev) => {
                      const next = new Set(prev);
                      if (next.has(g.key)) next.delete(g.key);
                      else next.add(g.key);
                      return next;
                    });
                    setArmDelete(false);
                  }}
                />
                <span className="min-w-0 flex-1 truncate">
                  {g.name}
                  {kind === "bookmark" && <span className="ml-2 text-xs text-(--text-faint)">{g.url}</span>}
                </span>
                <span className="text-xs text-(--text-faint)">{g.instanceCount} 个实例</span>
              </label>
            ))
          )}
        </div>
      )}
      {search && picked.size > 0 && props.mode === "edit" && (
        <div className="flex flex-col gap-2">
          <div className="flex gap-2">
            <label className="flex flex-1 flex-col gap-1 text-sm">
              替换为名称
              <input
                className="rounded-md border border-(--border) bg-(--input-bg) px-2 py-1.5"
                value={newName}
                onChange={(e) => setNewName(e.target.value)}
                placeholder="留空表示不修改名称"
              />
            </label>
            {kind === "bookmark" && (
              <label className="flex flex-1 flex-col gap-1 text-sm">
                替换为网址
                <input
                  className="rounded-md border border-(--border) bg-(--input-bg) px-2 py-1.5"
                  value={newUrl}
                  onChange={(e) => setNewUrl(e.target.value)}
                  placeholder="留空表示不修改网址"
                />
              </label>
            )}
          </div>
          <div>
            <button
              type="button"
              className="rounded-md bg-(--accent) px-3 py-1.5 text-sm text-white disabled:opacity-50"
              disabled={props.busy || (newName.trim() === "" && (kind === "folder" || newUrl.trim() === ""))}
              onClick={() =>
                void props.run("同步编辑", async (opId) => {
                  return applyAndRefresh(opId, () =>
                    BookmarkService.ApplySearchEdit(opId, search.token, [...picked], newName, newUrl),
                  );
                })
              }
            >
              确认修改所选 {picked.size} 项
            </button>
          </div>
        </div>
      )}
      {search && picked.size > 0 && props.mode === "delete" && (
        <div className="flex items-center gap-2">
          {!armDelete ? (
            <button
              type="button"
              className="rounded-md bg-(--danger) px-3 py-1.5 text-sm text-white disabled:opacity-50"
              disabled={props.busy}
              onClick={() => setArmDelete(true)}
            >
              删除所选
            </button>
          ) : (
            <>
              <span className="text-sm text-(--danger)">
                将从最多 {impactedInstances} 个实例中删除所选{kind === "bookmark" ? "书签" : "文件夹及其全部内容"}，资源库不受影响。
              </span>
              <button
                type="button"
                className="rounded-md bg-(--danger) px-3 py-1.5 text-sm text-white disabled:opacity-50"
                disabled={props.busy}
                onClick={() =>
                  void props.run("同步删除", async (opId) => {
                    return applyAndRefresh(opId, () =>
                      BookmarkService.ApplySearchDelete(opId, search.token, [...picked]),
                    );
                  })
                }
              >
                确认删除
              </button>
              <button
                type="button"
                className="rounded-md bg-(--hover-bg) px-3 py-1.5 text-sm"
                onClick={() => setArmDelete(false)}
              >
                取消
              </button>
            </>
          )}
        </div>
      )}
    </div>
  );
}
