-- Stage 9C: the DeBrowser bookmark library.
--
-- The library is DeBrowser's own bookmark resource tree: folders
-- nested to any reasonable depth and bookmarks inside them, with a
-- stable per-level order. It is fully independent from the actual
-- bookmarks stored in each browser profile (the Chromium Bookmarks
-- file, managed through the file, never through this table) and from
-- the historical flat bookmarks table of migration 0003, which is
-- kept untouched for data compatibility and no longer written.
--
-- parent_id NULL means the node sits at the library root. Folder
-- cycles are impossible by construction at the service layer (a
-- folder can never be moved into itself or a descendant); the
-- foreign key keeps parents pointing at real nodes.
CREATE TABLE bookmark_nodes (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL CHECK (kind IN ('bookmark', 'folder')),
    name       TEXT NOT NULL,
    url        TEXT NOT NULL DEFAULT '',
    parent_id  TEXT REFERENCES bookmark_nodes (id),
    sort_order INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_bookmark_nodes_parent_order
    ON bookmark_nodes (parent_id, sort_order, created_at);
