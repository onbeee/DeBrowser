// Unit tests for the bookmarks page's pure helpers
// (src/pages/bookmarksLogic.ts). Run with: npm test
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  buildTree,
  visibleRows,
  descendantIds,
  selectionScope,
  searchVisibleIds,
  siblingIds,
  folderOptions,
  type LibraryNodeLike,
} from "../src/pages/bookmarksLogic.ts";

function n(
  id: string,
  kind: string,
  name: string,
  parentId: string | null,
  sortOrder = 0,
  url = "",
): LibraryNodeLike {
  return { id, kind, name, url, parentId, sortOrder, createdAt: "2026-01-01 00:00:00" };
}

const nodes: LibraryNodeLike[] = [
  n("f1", "folder", "Work", null, 0),
  n("f2", "folder", "Sub", "f1", 0),
  n("b1", "bookmark", "Doc", "f2", 0, "https://doc.example/"),
  n("b2", "bookmark", "Root mark", null, 1, "https://root.example/"),
  n("b3", "bookmark", "Work mark", "f1", 1, "https://work.example/"),
];

test("buildTree nests and orders children", () => {
  const tree = buildTree(nodes);
  assert.equal(tree.length, 2);
  assert.equal(tree[0].node.id, "f1");
  assert.deepEqual(
    tree[0].children.map((c) => c.node.id),
    ["f2", "b3"],
  );
  assert.equal(tree[0].children[0].children[0].node.id, "b1");
});

test("visibleRows honors collapsed folders", () => {
  const tree = buildTree(nodes);
  const all = visibleRows(tree, new Set());
  assert.equal(all.length, 5);
  const collapsed = visibleRows(tree, new Set(["f1"]));
  assert.deepEqual(
    collapsed.map((r) => r.node.id),
    ["f1", "b2"],
  );
});

test("descendantIds covers the whole subtree once", () => {
  const ids = descendantIds(nodes, "f1");
  assert.deepEqual([...ids].sort(), ["b1", "b3", "f1", "f2"]);
});

test("selectionScope dedupes overlapping selections", () => {
  const scope = selectionScope(nodes, new Set(["f1", "f2", "b2"]));
  assert.deepEqual(scope, { total: 5, bookmarks: 3, folders: 2 });
  const single = selectionScope(nodes, new Set(["b1"]));
  assert.deepEqual(single, { total: 1, bookmarks: 1, folders: 0 });
});

test("searchVisibleIds keeps matches and their ancestors", () => {
  const visible = searchVisibleIds(nodes, "doc");
  assert.ok(visible);
  assert.deepEqual([...visible].sort(), ["b1", "f1", "f2"]);
  assert.equal(searchVisibleIds(nodes, "  "), null);
  const byUrl = searchVisibleIds(nodes, "root.example");
  assert.deepEqual([...(byUrl ?? [])], ["b2"]);
});

test("siblingIds and folderOptions", () => {
  assert.deepEqual(siblingIds(nodes, null), ["f1", "b2"]);
  assert.deepEqual(siblingIds(nodes, "f1"), ["f2", "b3"]);
  assert.deepEqual(folderOptions(nodes), [
    { id: "f1", name: "Work", depth: 0 },
    { id: "f2", name: "Sub", depth: 1 },
  ]);
});
