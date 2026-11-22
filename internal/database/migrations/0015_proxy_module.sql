-- Proxy module (redesign): the complete data model of the new
-- proxy feature — nothing more.
--
-- proxy_sources: one imported configuration source, either a
-- subscription (its URL lives in the credential vault; the row
-- holds only the vault reference) or a local file (DeBrowser
-- keeps its own copy under data/mihomo/sources/; the row holds
-- the copy's file name).
--
-- proxy_nodes: the proxy nodes parsed out of a source at import
-- time. The node's full definition is stored verbatim as JSON
-- (config_json) so the core configuration can be generated
-- without re-reading the source. Node identity is the row: two
-- sources may carry nodes with the same name and stay distinct.
--
-- instance_proxy_bindings: THE one authoritative instance ->
-- node binding, shared by the proxy page and the instance
-- pages. listen_port is allocated once when the binding is
-- created and kept for the binding's lifetime. Deleting an
-- instance or a node removes the binding with it (CASCADE).
--
-- The historical proxy tables of migrations 0002-0008 (proxies,
-- subscriptions, mihomo_configs, ...) and the legacy
-- browser_instances.proxy_id column are intentionally left
-- untouched: they hold dormant compatibility data of the
-- pre-redesign era and are neither read nor deleted here.
CREATE TABLE proxy_sources (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('subscription', 'file')),
    secret_ref TEXT NOT NULL DEFAULT '',
    file_name  TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE proxy_nodes (
    id          TEXT PRIMARY KEY,
    source_id   TEXT NOT NULL REFERENCES proxy_sources (id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    config_json TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (source_id, name)
);

CREATE INDEX idx_proxy_nodes_source ON proxy_nodes (source_id, name);

CREATE TABLE instance_proxy_bindings (
    instance_id TEXT PRIMARY KEY REFERENCES browser_instances (id) ON DELETE CASCADE,
    node_id     TEXT NOT NULL REFERENCES proxy_nodes (id) ON DELETE CASCADE,
    listen_port INTEGER NOT NULL UNIQUE CHECK (listen_port BETWEEN 1024 AND 65535),
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);
