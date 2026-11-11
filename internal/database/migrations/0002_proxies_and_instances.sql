-- Stage 2 core models, part 1: proxy nodes and browser instances.
--
-- IDs are application-generated UUIDs (TEXT) so rows can be created
-- offline, merged across backups and referenced before insert.
-- Timestamps are UTC ISO-8601 text written by SQLite defaults and by
-- the repository layer on update.
--
-- Credential policy: proxy passwords, keys and tokens are NEVER stored
-- in this database and never logged. Non-secret connection parameters
-- live in config_json; secret_ref is only an opaque reference to a
-- future secure credential store (Stage 4 decides the mechanism).

CREATE TABLE proxies (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    protocol    TEXT NOT NULL CHECK (protocol IN (
                    'http', 'https', 'socks5',
                    'vmess', 'vless', 'trojan', 'shadowsocks',
                    'hysteria2', 'tuic', 'wireguard', 'direct'
                )),
    address     TEXT NOT NULL DEFAULT '',
    port        INTEGER CHECK (port IS NULL OR (port >= 0 AND port <= 65535)),
    config_json TEXT NOT NULL DEFAULT '{}',
    secret_ref  TEXT NOT NULL DEFAULT '',
    enabled     INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_proxies_protocol ON proxies (protocol);
CREATE INDEX idx_proxies_enabled ON proxies (enabled);

CREATE TABLE browser_instances (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL UNIQUE,
    -- Kernel identity is recorded as data; the kernels table and its
    -- foreign key arrive with kernel management in Stage 5.
    kernel_id       TEXT NOT NULL DEFAULT '',
    kernel_name     TEXT NOT NULL DEFAULT '',
    kernel_version  TEXT NOT NULL DEFAULT '',
    -- Path of this instance's Chromium user data directory, relative
    -- to the portable data directory (for example profiles/<id>). It
    -- is unique and never shared between instances. The profile
    -- contents themselves are browser data and are NOT in SQLite.
    user_data_dir   TEXT NOT NULL UNIQUE,
    proxy_id        TEXT REFERENCES proxies (id) ON DELETE SET NULL,
    -- Last persisted lifecycle state. Live runtime state is owned by
    -- the Stage 3 instance manager; this column is its durable record.
    status          TEXT NOT NULL DEFAULT 'stopped'
                    CHECK (status IN ('stopped', 'running', 'error')),
    start_url       TEXT NOT NULL DEFAULT '',
    sort_order      INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_instances_proxy ON browser_instances (proxy_id);
CREATE INDEX idx_instances_status ON browser_instances (status);
