-- Stage 2 core models, part 2: bookmarks.
--
-- A bookmark either belongs to one browser instance (instance_id set,
-- deleted together with the instance) or is global (instance_id NULL,
-- applies to every instance). The same URL may exist once per scope.

CREATE TABLE bookmarks (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    url         TEXT NOT NULL,
    instance_id TEXT REFERENCES browser_instances (id) ON DELETE CASCADE,
    sort_order  INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- NULL instance_id values would bypass a plain UNIQUE(instance_id, url)
-- (SQLite treats NULLs as distinct), so uniqueness is enforced on the
-- effective scope key instead.
CREATE UNIQUE INDEX idx_bookmarks_scope_url
    ON bookmarks (IFNULL(instance_id, ''), url);

CREATE INDEX idx_bookmarks_instance_order
    ON bookmarks (instance_id, sort_order);
