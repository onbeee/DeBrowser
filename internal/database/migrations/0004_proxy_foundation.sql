-- Stage 4A: proxy management data layer.
--
-- Adds proxy groups, subscription records, proxy tags and the proxy
-- metadata the management UI and later stages need. Credential
-- policy is unchanged and strict: proxy passwords/keys and full
-- subscription URLs are NEVER stored in this database. They live in
-- the credential vault (internal/secrets); rows here hold only the
-- opaque secret_ref and, for subscriptions, a scheme+host url_hint.

CREATE TABLE proxy_groups (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    sort_order INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE subscriptions (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL,
    -- Scheme+host only (for example "https://example.com"). The full
    -- URL may carry credentials and lives exclusively in the vault.
    url_hint            TEXT NOT NULL DEFAULT '',
    secret_ref          TEXT NOT NULL DEFAULT '',
    group_id            TEXT REFERENCES proxy_groups (id) ON DELETE SET NULL,
    -- Subscription fetch policy (Stage 4B consumes these; the
    -- defaults are the reviewed safe values: LAN access off,
    -- automatic updates off).
    allow_lan           INTEGER NOT NULL DEFAULT 0 CHECK (allow_lan IN (0, 1)),
    auto_update_minutes INTEGER NOT NULL DEFAULT 0 CHECK (auto_update_minutes >= 0),
    last_updated_at     TEXT,
    last_status         TEXT NOT NULL DEFAULT '',
    -- Sanitised summary only (status class / error kind): never a
    -- URL, never credential material.
    last_error          TEXT NOT NULL DEFAULT '',
    created_at          TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at          TEXT NOT NULL DEFAULT (datetime('now'))
);

ALTER TABLE proxies ADD COLUMN group_id TEXT REFERENCES proxy_groups (id) ON DELETE SET NULL;
ALTER TABLE proxies ADD COLUMN sort_order INTEGER NOT NULL DEFAULT 0;
ALTER TABLE proxies ADD COLUMN source TEXT NOT NULL DEFAULT 'manual'
    CHECK (source IN ('manual', 'file_import', 'subscription'));
ALTER TABLE proxies ADD COLUMN subscription_id TEXT REFERENCES subscriptions (id) ON DELETE CASCADE;
ALTER TABLE proxies ADD COLUMN dedup_key TEXT NOT NULL DEFAULT '';
ALTER TABLE proxies ADD COLUMN last_latency_ms INTEGER
    CHECK (last_latency_ms IS NULL OR last_latency_ms >= 0);
ALTER TABLE proxies ADD COLUMN last_test_ok INTEGER
    CHECK (last_test_ok IS NULL OR last_test_ok IN (0, 1));
ALTER TABLE proxies ADD COLUMN last_tested_at TEXT;

-- Import deduplication fingerprint: unique among rows that carry
-- one; manually created rows (empty key) never collide.
CREATE UNIQUE INDEX idx_proxies_dedup ON proxies (dedup_key) WHERE dedup_key <> '';
CREATE INDEX idx_proxies_group ON proxies (group_id);
CREATE INDEX idx_proxies_subscription ON proxies (subscription_id);

-- Tag associations. Tags are free-form labels attached to proxies;
-- the association table (rather than a JSON column) keeps tag
-- filtering indexable. Deleting a proxy removes its associations.
CREATE TABLE proxy_tags (
    proxy_id TEXT NOT NULL REFERENCES proxies (id) ON DELETE CASCADE,
    tag      TEXT NOT NULL,
    PRIMARY KEY (proxy_id, tag)
);

CREATE INDEX idx_proxy_tags_tag ON proxy_tags (tag);

-- Per-instance Mihomo listener port (Stage 4C allocates from the
-- app_settings range 'proxy.port_range', default 19000-19499).
-- Unique so two instances can never claim the same listener.
-- (SQLite cannot add a UNIQUE column via ALTER TABLE, so the
-- uniqueness lives in a unique index; NULL ports never collide.)
ALTER TABLE browser_instances ADD COLUMN proxy_listen_port INTEGER
    CHECK (proxy_listen_port IS NULL OR (proxy_listen_port >= 1 AND proxy_listen_port <= 65535));

CREATE UNIQUE INDEX idx_instances_listen_port ON browser_instances (proxy_listen_port);
