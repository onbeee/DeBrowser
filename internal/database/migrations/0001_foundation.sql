-- Stage 1 foundation schema.
-- Application-level key/value settings. Business tables (browser instances,
-- proxies, kernels, bookmarks) are added by later stages in their own
-- migrations; see docs/DATABASE.md.
CREATE TABLE IF NOT EXISTS app_settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
