import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Bookmark,
  ChevronDown,
  ChevronRight,
  Folder,
  Loader2,
  Pencil,
  Plus,
  Search,
  Send,
  Trash2,
} from "lucide-react";
import {
  BookmarkService,
  InstanceService,
  type BookmarkInstanceInfo,
} from "../../bindings/github.com/onbeee/DeBrowser/internal/service";
import type { BookmarkNode } from "../../bindings/github.com/onbeee/DeBrowser/internal/model";
import { newOperationId, recordOperationFeedback } from "../lib/operations";
import { notifyError, notifySuccess, notifyWarning } from "../stores/useNotificationStore";
import {
  buildTree,
  folderOptions,
  searchVisibleIds,
  notStoppedCount,
  selectionScope,
  siblingIds,
  syncEntryDecision,
  visibleRows,
  type VisibleRow,
} from "./bookmarksLogic";
import SyncDialog from "./BookmarksPageSync";

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

type DialogTab = "bookmark" | "folder";

// 书签管理：左侧是 DeBrowser 自己的书签资源库（数据库），右侧是
// 实例列表（仅名称、书签数量与选择框）。资源库与各实例 Profile
// 中的真实书签完全独立：编辑/删除资源库不影响实例，同步弹窗中
// 对实例书签的编辑/删除也不回写资源库。实例书签只通过同步弹窗
// 操作，且只作用于已确认停止的实例（运行中或状态无法确认的
// 实例会被跳过并明确反馈），详见后端 BookmarkService。
export default function BookmarksPage() {
  const [nodes, setNodes] = useState<BookmarkNode[]>([]);
  const [instances, setInstances] = useState<BookmarkInstanceInfo[]>([]);
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState<string | null>(null);

  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(new Set());
  const [libQuery, setLibQuery] = useState("");
  const [editMode, setEditMode] = useState(false);

  const [instQuery, setInstQuery] = useState("");
  const [instSelected, setInstSelected] = useState<ReadonlySet<string>>(new Set());

  const [createTab, setCreateTab] = useState<DialogTab | null>(null);
  const [editNode, setEditNode] = useState<BookmarkNode | null>(null);
  const [deleteArmed, setDeleteArmed] = useState(false);
  const [syncOpen, setSyncOpen] = useState(false);
  // 同步入口状态：idle 无窗口；confirm 显示「关闭实例」确认窗口；
  // closing 正在关闭全部运行中实例并等待确认停止。
  const [syncGate, setSyncGate] = useState<"idle" | "confirm" | "closing">("idle");
  const closingRef = useRef(false);

  const [dragId, setDragId] = useState<string | null>(null);

  // 查询全部实例的后端书签访问门禁结果（InstanceBookmarkAccess）。
  // 是否「确认停止」以后端门禁为唯一依据：error 等状态只有后端
  // 结合运行时记录才能判定，前端不凭实例状态字符串自行判断。
  // 单个查询失败按 unknown 处理，绝不默认放行。
  const collectBookmarkGates = async (): Promise<string[]> => {
    const list = (await InstanceService.ListInstances()) ?? [];
    const gates = await Promise.all(
      list.map(async (inst) => {
        try {
          const [, , gate] = await InstanceService.InstanceBookmarkAccess(inst.id);
          return gate;
        } catch {
          return "unknown";
        }
      }),
    );
    return gates;
  };

  // 点击「同步」：先用后端门禁检查全部实例。全部确认停止才直接
  // 打开同步弹窗；存在运行中实例时先弹确认窗口；只有状态无法
  // 确认的实例时关闭也无济于事，直接提示。
  const handleSyncClick = async () => {
    setBusy("sync-check");
    try {
      const gates = await collectBookmarkGates();
      const decision = syncEntryDecision(gates);
      if (decision === "open") {
        setSyncOpen(true);
      } else if (decision === "confirm") {
        setSyncGate("confirm");
      } else {
        notifyError(
          "有 " + notStoppedCount(gates) + " 个浏览器实例的状态无法确认停止，无法打开书签同步。请先在浏览器实例页面处理后再试。",
          { important: true },
        );
      }
    } catch (err) {
      notifyError("检查浏览器实例状态失败：" + errText(err), { important: true });
    } finally {
      setBusy(null);
    }
  };

  // 确认窗口的「关闭实例」：对运行中（含关闭中）的实例并行调用
  // 既有正常关闭服务，互不取消；全部关闭调用返回后，再统一用
  // 后端门禁复查全部实例。只有全部确认停止才打开同步弹窗；任何
  // 实例关闭失败或仍无法确认停止都不打开弹窗、不修改任何书签，
  // 用户可自行处理后重新点击同步重试。
  const closeInstancesThenSync = async () => {
    if (closingRef.current) return;
    closingRef.current = true;
    setSyncGate("closing");
    try {
      const opId = newOperationId();
      const list = (await InstanceService.ListInstances()) ?? [];
      const toStop = list.filter((i) => i.status === "running" || i.status === "stopping");
      await Promise.allSettled(
        toStop.map((inst) => InstanceService.StopInstance(opId, inst.id)),
      );
      const gates = await collectBookmarkGates();
      if (gates.every((g) => g === "stopped")) {
        setSyncGate("idle");
        setSyncOpen(true);
        void reload();
      } else {
        setSyncGate("idle");
        notifyError(
          "仍有 " + notStoppedCount(gates) + " 个浏览器实例未能关闭或无法确认停止，无法打开书签同步。",
          { important: true },
        );
      }
    } catch (err) {
      setSyncGate("idle");
      notifyError("关闭浏览器实例失败：" + errText(err), { important: true });
    } finally {
      closingRef.current = false;
    }
  };

  const [loadError, setLoadError] = useState<string | null>(null);
  const loadedRef = useRef(false);

  // reload 刷新资源库与实例列表。失败时默认发一条刷新失败
  // 警告；调用方需要区分语义时（首次加载、删除后刷新）传入
  // onFail 自定义提示。返回是否成功。
  const reload = useCallback(async (onFail?: (message: string) => void): Promise<boolean> => {
    try {
      const [lib, inst] = await Promise.all([
        BookmarkService.ListLibrary(),
        BookmarkService.ListBookmarkInstances(),
      ]);
      setNodes(lib ?? []);
      setInstances(inst ?? []);
      loadedRef.current = true;
      setLoadError(null);
      return true;
    } catch (e) {
      const message = errText(e);
      if (!loadedRef.current) setLoadError(message);
      if (onFail) onFail(message);
      else notifyWarning("列表刷新失败，请重新搜索。");
      return false;
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void reload((message) => notifyError("加载书签失败：" + message));
  }, [reload]);

  const run = async (
    key: string,
    action: string,
    fn: (opId: string) => Promise<string | void>,
  ) => {
    setBusy(key);
    const opId = newOperationId();
    try {
      const feedback = await fn(opId);
      if (feedback) {
        notifySuccess(feedback, { operationId: opId, important: true });
        recordOperationFeedback(opId, true, feedback, "书签管理", action);
      }
    } catch (e) {
      const message = errText(e);
      notifyError(message, { operationId: opId, important: true });
      recordOperationFeedback(opId, false, message, "书签管理", action);
    } finally {
      setBusy(null);
    }
  };

  const tree = useMemo(() => buildTree(nodes), [nodes]);
  const searchSet = useMemo(() => searchVisibleIds(nodes, libQuery), [nodes, libQuery]);
  const rows = useMemo(() => {
    const all = visibleRows(tree, searchSet ? new Set<string>() : collapsed);
    return searchSet ? all.filter((r) => searchSet.has(r.node.id)) : all;
  }, [tree, collapsed, searchSet]);
  const scope = useMemo(() => selectionScope(nodes, selected), [nodes, selected]);
  const folders = useMemo(() => folderOptions(nodes), [nodes]);

  const filteredInstances = useMemo(() => {
    const q = instQuery.trim().toLowerCase();
    return q ? instances.filter((i) => i.name.toLowerCase().includes(q)) : instances;
  }, [instances, instQuery]);

  const toggleSelect = (id: string) => {
    // 选择变化后退出删除确认状态，避免确认的集合与实际
    // 执行删除的集合不一致。
    setDeleteArmed(false);
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const toggleCollapse = (id: string) => {
    setCollapsed((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  // ---- drag & drop: reorder within a level, or move into a folder ----
  const onDropRow = async (
    target: { id: string; kind: string; name: string; parentId?: string | null },
    position: "before" | "into" | "after",
  ) => {
    const dragged = nodes.find((n) => n.id === dragId);
    setDragId(null);
    if (!dragged || dragged.id === target.id) return;
    await run("drag", "拖动调整", async (opId) => {
      if (position === "into" && target.kind === "folder") {
        await BookmarkService.UpdateLibraryNode(opId, {
          id: dragged.id,
          name: dragged.name,
          url: dragged.url,
          parentId: target.id,
        });
        await reload();
        return "已移动到文件夹「" + target.name + "」";
      }
      const parentId = target.parentId ?? null;
      const ids = siblingIds(nodes, parentId).filter((id) => id !== dragged.id);
      const at = ids.indexOf(target.id);
      ids.splice(position === "after" ? at + 1 : at, 0, dragged.id);
      if ((dragged.parentId ?? null) !== parentId) {
        await BookmarkService.UpdateLibraryNode(opId, {
          id: dragged.id,
          name: dragged.name,
          url: dragged.url,
          parentId,
        });
      }
      await BookmarkService.ReorderLibrary(opId, parentId, ids);
      await reload();
      return "顺序已更新";
    });
  };

  const doDelete = async () => {
    await run("delete", "删除书签资源", async (opId) => {
      const count = await BookmarkService.DeleteLibraryNodes(opId, [...selected]);
      // 删除成功即为最终结果：清空选择、退出确认状态，
      // 随后保留原搜索条件重新查询。刷新失败只单独提示
      // （仅通知台），不影响删除成功的通知与日志记录。
      setSelected(new Set());
      setDeleteArmed(false);
      await reload(() => notifyWarning("删除已完成，但列表刷新失败，请重新搜索。"));
      return "已删除 " + count + " 项";
    });
  };

  return (
    <div className="flex h-full min-h-0 flex-col gap-3 p-4">
      {/* 顶部工具栏 */}
      <div className="flex items-center gap-2">
        <button
          type="button"
          className="flex items-center gap-1 rounded-md bg-(--accent) px-3 py-1.5 text-sm text-white disabled:opacity-50"
          disabled={busy !== null}
          onClick={() => setCreateTab("bookmark")}
        >
          <Plus size={15} /> 新建
        </button>
        <button
          type="button"
          className={
            "flex items-center gap-1 rounded-md px-3 py-1.5 text-sm disabled:opacity-50 " +
            (editMode ? "bg-(--accent) text-white" : "bg-(--hover-bg) text-(--text-main)")
          }
          disabled={busy !== null}
          onClick={() => setEditMode((v) => !v)}
        >
          <Pencil size={15} /> 编辑
        </button>
        {!deleteArmed ? (
          <button
            type="button"
            className="flex items-center gap-1 rounded-md bg-(--hover-bg) px-3 py-1.5 text-sm text-(--text-main) disabled:opacity-50"
            disabled={busy !== null || selected.size === 0}
            onClick={() => setDeleteArmed(true)}
          >
            <Trash2 size={15} /> 删除所选
          </button>
        ) : (
          <>
            {scope.folders > 0 && (
              <span className="text-xs text-(--danger)">所选包含文件夹，其内容将一并删除</span>
            )}
            <button
              type="button"
              className="flex items-center gap-1 rounded-md bg-(--danger) px-3 py-1.5 text-sm text-white disabled:opacity-50"
              disabled={busy !== null}
              onClick={() => void doDelete()}
            >
              确定删除
            </button>
            <button
              type="button"
              className="flex items-center gap-1 rounded-md bg-(--hover-bg) px-3 py-1.5 text-sm text-(--text-main) disabled:opacity-50"
              disabled={busy !== null}
              onClick={() => setDeleteArmed(false)}
            >
              取消
            </button>
          </>
        )}
        <button
          type="button"
          className="flex items-center gap-1 rounded-md bg-(--hover-bg) px-3 py-1.5 text-sm text-(--text-main) disabled:opacity-50"
          disabled={busy !== null}
          onClick={() => void handleSyncClick()}
        >
          <Send size={15} /> 同步
        </button>
        <span className="ml-auto text-xs text-(--text-faint)">
          资源库与实例书签相互独立，互不自动覆盖
        </span>
      </div>

      {/* 主体：左右两区独立滚动 */}
      <div className="flex min-h-0 flex-1 gap-3">
        <section className="flex min-w-0 flex-1 flex-col rounded-lg border border-(--border) bg-(--panel-bg)">
          <header className="flex items-center gap-2 border-b border-(--border) px-3 py-2">
            <h3 className="text-sm font-semibold">书签列表</h3>
            <label className="ml-auto flex items-center gap-1 rounded-md bg-(--hover-bg) px-2 py-1">
              <Search size={13} className="text-(--text-faint)" />
              <input
                className="w-36 bg-transparent text-xs outline-none"
                placeholder="搜索书签"
                value={libQuery}
                onChange={(e) => setLibQuery(e.target.value)}
              />
            </label>
          </header>
          <div className="min-h-0 flex-1 overflow-y-auto p-1">
            {loading ? (
              <p className="flex items-center gap-2 px-3 py-4 text-sm text-(--text-faint)">
                <Loader2 size={14} className="animate-spin" /> 正在加载…
              </p>
            ) : loadError && nodes.length === 0 ? (
              <div className="px-3 py-4">
                <p className="text-sm text-(--danger)">书签加载失败：{loadError}</p>
                <button
                  type="button"
                  className="mt-2 rounded-md border border-(--border-strong) bg-(--input-bg) px-3 py-1.5 text-sm text-(--text-main) hover:bg-(--hover-bg)"
                  onClick={() => void reload((message) => notifyError("加载书签失败：" + message))}
                >
                  重新加载
                </button>
              </div>
            ) : rows.length === 0 ? (
              <p className="px-3 py-4 text-sm text-(--text-faint)">
                {nodes.length === 0 ? "资源库为空，点击「新建」添加书签或文件夹。" : "没有匹配的书签。"}
              </p>
            ) : (
              rows.map((row) => (
                <LibraryRow
                  key={row.node.id}
                  row={row}
                  checked={selected.has(row.node.id)}
                  collapsed={collapsed.has(row.node.id)}
                  editMode={editMode}
                  busy={busy !== null}
                  onToggleSelect={() => toggleSelect(row.node.id)}
                  onToggleCollapse={() => toggleCollapse(row.node.id)}
                  onEdit={() => {
                    const full = nodes.find((n) => n.id === row.node.id);
                    if (full) setEditNode(full);
                  }}
                  onDragStart={() => setDragId(row.node.id)}
                  onDrop={(pos) => void onDropRow(row.node, pos)}
                />
              ))
            )}
          </div>
        </section>

        <section className="flex w-72 shrink-0 flex-col rounded-lg border border-(--border) bg-(--panel-bg)">
          <header className="flex items-center gap-2 border-b border-(--border) px-3 py-2">
            <h3 className="text-sm font-semibold">实例列表</h3>
            <label className="ml-auto flex items-center gap-1 rounded-md bg-(--hover-bg) px-2 py-1">
              <Search size={13} className="text-(--text-faint)" />
              <input
                className="w-24 bg-transparent text-xs outline-none"
                placeholder="搜索实例"
                value={instQuery}
                onChange={(e) => setInstQuery(e.target.value)}
              />
            </label>
          </header>
          <label className="flex items-center gap-2 border-b border-(--border) px-3 py-1.5 text-xs text-(--text-faint)">
            <input
              type="checkbox"
              checked={filteredInstances.length > 0 && filteredInstances.every((i) => instSelected.has(i.id))}
              onChange={(e) => {
                setInstSelected((prev) => {
                  const next = new Set(prev);
                  for (const i of filteredInstances) {
                    if (e.target.checked) next.add(i.id);
                    else next.delete(i.id);
                  }
                  return next;
                });
              }}
            />
            全选（当前显示 {filteredInstances.length} 个）
          </label>
          <div className="min-h-0 flex-1 overflow-y-auto p-1">
            {filteredInstances.length === 0 ? (
              <p className="px-3 py-4 text-sm text-(--text-faint)">没有实例。</p>
            ) : (
              filteredInstances.map((inst) => (
                <label
                  key={inst.id}
                  className="flex cursor-pointer items-center gap-2 rounded-md px-2 py-1.5 text-sm hover:bg-(--hover-bg)"
                >
                  <input
                    type="checkbox"
                    checked={instSelected.has(inst.id)}
                    onChange={() => {
                      setInstSelected((prev) => {
                        const next = new Set(prev);
                        if (next.has(inst.id)) next.delete(inst.id);
                        else next.add(inst.id);
                        return next;
                      });
                    }}
                  />
                  <span className="min-w-0 flex-1 truncate">{inst.name}</span>
                  <span className="text-xs text-(--text-faint)">
                    {inst.bookmarkCount >= 0 ? inst.bookmarkCount + " 个书签" : "书签数未知"}
                  </span>
                </label>
              ))
            )}
          </div>
        </section>
      </div>

      {createTab && (
        <CreateDialog
          tab={createTab}
          folders={folders}
          busy={busy !== null}
          onTab={setCreateTab}
          onClose={() => setCreateTab(null)}
          onSubmit={async (input) => {
            await run("create", "新建书签资源", async (opId) => {
              await BookmarkService.CreateLibraryNode(opId, input);
              setCreateTab(null);
              await reload();
              return input.kind === "folder" ? "文件夹已创建" : "书签已创建";
            });
          }}
        />
      )}

      {editNode && (
        <EditDialog
          node={editNode}
          folders={folders.filter((f) => f.id !== editNode.id)}
          busy={busy !== null}
          onClose={() => setEditNode(null)}
          onSubmit={async (input) => {
            await run("edit", "编辑书签资源", async (opId) => {
              await BookmarkService.UpdateLibraryNode(opId, input);
              setEditNode(null);
              await reload();
              return "已保存";
            });
          }}
        />
      )}

      {syncGate !== "idle" && (
        <div
          className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4"
          onClick={() => {
            // 点击窗口外区域等同于取消（关闭处理中不允许取消）。
            if (syncGate === "confirm") setSyncGate("idle");
          }}
        >
          <div
            className="w-full max-w-md rounded-lg bg-(--panel-bg) p-4 shadow-xl"
            onClick={(e) => e.stopPropagation()}
          >
            <h3 className="text-base font-semibold">同步前关闭实例</h3>
            {syncGate === "closing" ? (
              <p className="mt-2 flex items-center gap-2 text-sm text-(--text-dim)">
                <Loader2 size={15} className="animate-spin" /> 正在关闭浏览器实例，请稍候……
              </p>
            ) : (
              <>
                <p className="mt-2 text-sm text-(--text-dim)">
                  检测到仍有浏览器实例正在运行，书签同步前需要先关闭所有浏览器实例。
                </p>
                <div className="mt-4 flex flex-col gap-2">
                  <button
                    type="button"
                    className="rounded-md bg-(--accent) px-3 py-1.5 text-sm text-white"
                    onClick={() => void closeInstancesThenSync()}
                  >
                    关闭实例
                  </button>
                  <button
                    type="button"
                    className="rounded-md bg-(--hover-bg) px-3 py-1.5 text-sm"
                    onClick={() => setSyncGate("idle")}
                  >
                    取消
                  </button>
                </div>
              </>
            )}
          </div>
        </div>
      )}

      {syncOpen && (
        <SyncDialog
          nodes={nodes}
          instances={instances}
          onClose={() => {
            setSyncOpen(false);
            void reload();
          }}
        />
      )}
    </div>
  );
}

function LibraryRow(props: {
  row: VisibleRow;
  checked: boolean;
  collapsed: boolean;
  editMode: boolean;
  busy: boolean;
  onToggleSelect: () => void;
  onToggleCollapse: () => void;
  onEdit: () => void;
  onDragStart: () => void;
  onDrop: (pos: "before" | "into" | "after") => void;
}) {
  const { row } = props;
  const node = row.node;
  const isFolder = node.kind === "folder";
  return (
    <div
      className="flex items-center gap-1.5 rounded-md px-1.5 py-1 text-sm hover:bg-(--hover-bg)"
      style={{ paddingLeft: 6 + row.depth * 18 }}
      draggable
      onDragStart={props.onDragStart}
      onDragOver={(e) => e.preventDefault()}
      onDrop={(e) => {
        e.preventDefault();
        const rect = (e.currentTarget as HTMLDivElement).getBoundingClientRect();
        const ratio = (e.clientY - rect.top) / rect.height;
        props.onDrop(isFolder && ratio > 0.3 && ratio < 0.7 ? "into" : ratio <= 0.5 ? "before" : "after");
      }}
    >
      {isFolder ? (
        <button type="button" className="shrink-0 text-(--text-faint)" onClick={props.onToggleCollapse}>
          {props.collapsed ? <ChevronRight size={14} /> : <ChevronDown size={14} />}
        </button>
      ) : (
        <span className="w-3.5 shrink-0" />
      )}
      <input type="checkbox" checked={props.checked} onChange={props.onToggleSelect} />
      {isFolder ? (
        <Folder size={15} className="shrink-0 text-(--accent)" />
      ) : (
        <Bookmark size={15} className="shrink-0 text-(--text-faint)" />
      )}
      <span className="min-w-0 flex-1 truncate">
        {node.name}
        {!isFolder && <span className="ml-2 truncate text-xs text-(--text-faint)">{node.url}</span>}
      </span>
      {props.editMode && (
        <button
          type="button"
          className="shrink-0 rounded px-1.5 py-0.5 text-xs text-(--accent) hover:bg-(--panel-bg)"
          disabled={props.busy}
          onClick={props.onEdit}
        >
          编辑
        </button>
      )}
    </div>
  );
}

function FolderSelect(props: {
  folders: { id: string; name: string; depth: number }[];
  value: string;
  onChange: (id: string) => void;
}) {
  return (
    <select
      className="w-full rounded-md border border-(--border) bg-(--input-bg) px-2 py-1.5 text-sm"
      value={props.value}
      onChange={(e) => props.onChange(e.target.value)}
    >
      <option value="">根目录</option>
      {props.folders.map((f) => (
        <option key={f.id} value={f.id}>
          {"　".repeat(f.depth)}
          {f.name}
        </option>
      ))}
    </select>
  );
}

function CreateDialog(props: {
  tab: DialogTab;
  folders: { id: string; name: string; depth: number }[];
  busy: boolean;
  onTab: (t: DialogTab) => void;
  onClose: () => void;
  onSubmit: (input: { kind: string; name: string; url: string; parentId: string | null }) => Promise<void>;
}) {
  const [name, setName] = useState("");
  const [url, setUrl] = useState("");
  const [parentId, setParentId] = useState("");
  const isBookmark = props.tab === "bookmark";
  return (
    <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
      <div className="w-full max-w-md rounded-lg bg-(--panel-bg) p-4 shadow-xl">
        <div className="flex items-center gap-2">
          <h3 className="text-base font-semibold">新建</h3>
          <div className="ml-auto flex rounded-md bg-(--hover-bg) p-0.5">
            {(["bookmark", "folder"] as DialogTab[]).map((t) => (
              <button
                key={t}
                type="button"
                className={
                  "rounded px-3 py-1 text-sm " + (props.tab === t ? "bg-(--accent) text-white" : "text-(--text-main)")
                }
                onClick={() => props.onTab(t)}
              >
                {t === "bookmark" ? "书签" : "文件夹"}
              </button>
            ))}
          </div>
        </div>
        <div className="mt-3 flex flex-col gap-3">
          <label className="flex flex-col gap-1 text-sm">
            名称
            <input
              className="rounded-md border border-(--border) bg-(--input-bg) px-2 py-1.5"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder={isBookmark ? "书签名称" : "文件夹名称"}
            />
          </label>
          {isBookmark && (
            <label className="flex flex-col gap-1 text-sm">
              网址
              <input
                className="rounded-md border border-(--border) bg-(--input-bg) px-2 py-1.5"
                value={url}
                onChange={(e) => setUrl(e.target.value)}
                placeholder="https://example.com/"
              />
            </label>
          )}
          <label className="flex flex-col gap-1 text-sm">
            {isBookmark ? "文件夹（可选）" : "上级文件夹"}
            <FolderSelect folders={props.folders} value={parentId} onChange={setParentId} />
          </label>
        </div>
        <div className="mt-4 flex justify-end gap-2">
          <button type="button" className="rounded-md bg-(--hover-bg) px-3 py-1.5 text-sm" onClick={props.onClose}>
            取消
          </button>
          <button
            type="button"
            className="rounded-md bg-(--accent) px-3 py-1.5 text-sm text-white disabled:opacity-50"
            disabled={props.busy}
            onClick={() =>
              void props.onSubmit({
                kind: props.tab,
                name,
                url: isBookmark ? url : "",
                parentId: parentId === "" ? null : parentId,
              })
            }
          >
            保存
          </button>
        </div>
      </div>
    </div>
  );
}

function EditDialog(props: {
  node: BookmarkNode;
  folders: { id: string; name: string; depth: number }[];
  busy: boolean;
  onClose: () => void;
  onSubmit: (input: { id: string; name: string; url: string; parentId: string | null }) => Promise<void>;
}) {
  const [name, setName] = useState(props.node.name);
  const [url, setUrl] = useState(props.node.url);
  const [parentId, setParentId] = useState(props.node.parentId ?? "");
  const isBookmark = props.node.kind === "bookmark";
  return (
    <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/50 px-4">
      <div className="w-full max-w-md rounded-lg bg-(--panel-bg) p-4 shadow-xl">
        <h3 className="text-base font-semibold">编辑{isBookmark ? "书签" : "文件夹"}</h3>
        <div className="mt-3 flex flex-col gap-3">
          <label className="flex flex-col gap-1 text-sm">
            名称
            <input
              className="rounded-md border border-(--border) bg-(--input-bg) px-2 py-1.5"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </label>
          {isBookmark && (
            <label className="flex flex-col gap-1 text-sm">
              网址
              <input
                className="rounded-md border border-(--border) bg-(--input-bg) px-2 py-1.5"
                value={url}
                onChange={(e) => setUrl(e.target.value)}
              />
            </label>
          )}
          <label className="flex flex-col gap-1 text-sm">
            所属文件夹
            <FolderSelect folders={props.folders} value={parentId} onChange={setParentId} />
          </label>
          <p className="text-xs text-(--text-faint)">编辑只修改资源库，不会修改已同步到实例中的书签。</p>
        </div>
        <div className="mt-4 flex justify-end gap-2">
          <button type="button" className="rounded-md bg-(--hover-bg) px-3 py-1.5 text-sm" onClick={props.onClose}>
            取消
          </button>
          <button
            type="button"
            className="rounded-md bg-(--accent) px-3 py-1.5 text-sm text-white disabled:opacity-50"
            disabled={props.busy}
            onClick={() =>
              void props.onSubmit({
                id: props.node.id,
                name,
                url: isBookmark ? url : "",
                parentId: parentId === "" ? null : parentId,
              })
            }
          >
            保存
          </button>
        </div>
      </div>
    </div>
  );
}
