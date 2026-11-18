// Pure, framework-free helpers for the bookmarks page: tree
// assembly, visibility under collapse/search, subtree accounting
// for delete confirmations and sibling ordering for drag & drop.
// Keeping them free of React and of the generated bindings makes
// them directly unit-testable (see frontend/tests/bookmarksLogic.test.ts).

export interface LibraryNodeLike {
  id: string;
  kind: string; // "bookmark" | "folder"
  name: string;
  url: string;
  parentId?: string | null;
  sortOrder: number;
  createdAt: string;
}

export interface TreeNode {
  node: LibraryNodeLike;
  children: TreeNode[];
}

function compareNodes(a: LibraryNodeLike, b: LibraryNodeLike): number {
  if (a.sortOrder !== b.sortOrder) return a.sortOrder - b.sortOrder;
  if (a.createdAt !== b.createdAt) return a.createdAt < b.createdAt ? -1 : 1;
  return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
}

export function parentKeyOf(node: LibraryNodeLike): string {
  return node.parentId ?? "";
}

/** Assembles the flat node list into a sorted tree (roots first). */
export function buildTree(nodes: LibraryNodeLike[]): TreeNode[] {
  const byId = new Map<string, TreeNode>();
  for (const n of nodes) byId.set(n.id, { node: n, children: [] });
  const roots: TreeNode[] = [];
  for (const n of nodes) {
    const treeNode = byId.get(n.id)!;
    const parent = n.parentId ? byId.get(n.parentId) : undefined;
    if (parent) {
      parent.children.push(treeNode);
    } else {
      roots.push(treeNode);
    }
  }
  const sortLevel = (level: TreeNode[]) => {
    level.sort((a, b) => compareNodes(a.node, b.node));
    for (const t of level) sortLevel(t.children);
  };
  sortLevel(roots);
  return roots;
}

export interface VisibleRow {
  node: LibraryNodeLike;
  depth: number;
  hasChildren: boolean;
}

/** Flattens the tree into renderable rows, honoring collapsed folders. */
export function visibleRows(tree: TreeNode[], collapsed: ReadonlySet<string>): VisibleRow[] {
  const rows: VisibleRow[] = [];
  const walk = (level: TreeNode[], depth: number) => {
    for (const t of level) {
      rows.push({ node: t.node, depth, hasChildren: t.children.length > 0 });
      if (t.children.length > 0 && !collapsed.has(t.node.id)) {
        walk(t.children, depth + 1);
      }
    }
  };
  walk(tree, 0);
  return rows;
}

/** The id set of a node plus every descendant. */
export function descendantIds(nodes: LibraryNodeLike[], id: string): Set<string> {
  const children = new Map<string, LibraryNodeLike[]>();
  for (const n of nodes) {
    const key = parentKeyOf(n);
    const list = children.get(key) ?? [];
    list.push(n);
    children.set(key, list);
  }
  const out = new Set<string>();
  const walk = (cur: string) => {
    if (out.has(cur)) return;
    out.add(cur);
    for (const child of children.get(cur) ?? []) walk(child.id);
  };
  walk(id);
  return out;
}

export interface SelectionScope {
  total: number;
  bookmarks: number;
  folders: number;
}

/**
 * Counts what deleting (or syncing) the selected nodes actually
 * covers: the union of their subtrees, each node counted once even
 * when a folder and its child are both selected.
 */
// libraryCounts tallies the resource library by kind, for the
// header summary: bookmarks and folders are reported separately,
// never lumped into one "nodes" number.
export function libraryCounts(nodes: LibraryNodeLike[]): { bookmarks: number; folders: number } {
  let bookmarks = 0;
  let folders = 0;
  for (const n of nodes) {
    if (n.kind === "folder") folders++;
    else bookmarks++;
  }
  return { bookmarks, folders };
}

export function selectionScope(nodes: LibraryNodeLike[], selected: ReadonlySet<string>): SelectionScope {
  const covered = new Set<string>();
  for (const id of selected) {
    for (const sub of descendantIds(nodes, id)) covered.add(sub);
  }
  let bookmarks = 0;
  let folders = 0;
  for (const n of nodes) {
    if (!covered.has(n.id)) continue;
    if (n.kind === "folder") folders++;
    else bookmarks++;
  }
  return { total: bookmarks + folders, bookmarks, folders };
}

/**
 * For library search: returns the ids that stay visible — matching
 * nodes plus all their ancestors — or null when the query is empty.
 */
export function searchVisibleIds(nodes: LibraryNodeLike[], query: string): Set<string> | null {
  const q = query.trim().toLowerCase();
  if (!q) return null;
  const byId = new Map(nodes.map((n) => [n.id, n]));
  const visible = new Set<string>();
  for (const n of nodes) {
    const hit =
      n.name.toLowerCase().includes(q) || (n.kind === "bookmark" && n.url.toLowerCase().includes(q));
    if (!hit) continue;
    visible.add(n.id);
    let parentId = n.parentId ?? null;
    while (parentId) {
      visible.add(parentId);
      parentId = byId.get(parentId)?.parentId ?? null;
    }
  }
  return visible;
}

/** Ordered sibling ids of one level (parentId null = root). */
export function siblingIds(nodes: LibraryNodeLike[], parentId: string | null): string[] {
  return nodes
    .filter((n) => parentKeyOf(n) === (parentId ?? ""))
    .sort(compareNodes)
    .map((n) => n.id);
}

/** All folders in tree order with their depth, for parent pickers. */
export function folderOptions(nodes: LibraryNodeLike[]): { id: string; name: string; depth: number }[] {
  const tree = buildTree(nodes);
  const out: { id: string; name: string; depth: number }[] = [];
  const walk = (level: TreeNode[], depth: number) => {
    for (const t of level) {
      if (t.node.kind === "folder") {
        out.push({ id: t.node.id, name: t.node.name, depth });
        walk(t.children, depth + 1);
      }
    }
  };
  walk(tree, 0);
  return out;
}

/**
 * 点击「同步」后的入口决策，输入是后端书签访问门禁
 * （InstanceBookmarkAccess）对全部实例的判定结果，而不是实例
 * 列表里的状态字符串——error 等状态只有后端结合运行时记录
 * 才能判定是否确认停止：
 * - 全部 stopped → 直接打开同步弹窗（open）。
 * - 存在 running → 先弹确认窗口（confirm）。
 * - 没有 running、但存在 unknown → 不能打开（blocked），
 *   关闭操作解决不了这类实例，直接提示用户处理。
 */
export type SyncEntryDecision = "open" | "confirm" | "blocked";

export function syncEntryDecision(gates: string[]): SyncEntryDecision {
  if (gates.every((g) => g === "stopped")) return "open";
  if (gates.some((g) => g === "running")) return "confirm";
  return "blocked";
}

/** 未能确认停止的实例数量（用于失败提示）。 */
export function notStoppedCount(gates: string[]): number {
  return gates.filter((g) => g !== "stopped").length;
}
