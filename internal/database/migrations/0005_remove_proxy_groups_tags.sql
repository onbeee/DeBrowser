-- Stage 4A correction: remove DeBrowser's own proxy groups and tags.
--
-- Product decision (2026-11-13): DeBrowser does not offer its own
-- proxy grouping/tagging feature. (Mihomo's native proxy-groups are a
-- different concept: they arrive with airport config imports in a
-- later stage and are unaffected by this migration.)
--
-- Removed: tables proxy_groups and proxy_tags, columns
-- proxies.group_id and subscriptions.group_id, index idx_proxies_group.
-- Kept: proxies.sort_order (plain list ordering, not a group feature),
-- proxies.subscription_id / dedup_key / source, every credential
-- reference, and all browser_instances bindings.
--
-- Migration shape — table rebuild, designed against facts verified
-- empirically on this project's SQLite build (see stage5_test.go):
--
--   * A bare ALTER TABLE ... DROP COLUMN was rejected for the two
--     group_id columns: both carry a foreign key to proxy_groups and
--     proxies.group_id is additionally covered by idx_proxies_group.
--   * The migration runner executes each migration inside one
--     transaction with PRAGMA foreign_keys=1, and on this SQLite
--     build foreign key ACTIONS (ON DELETE SET NULL / CASCADE) fire
--     immediately — PRAGMA defer_foreign_keys defers constraint
--     checks, not actions. So the naive rebuild (create new table,
--     copy, DROP the old parent) silently unbinds every instance
--     (SET NULL) and cascades away subscription-owned proxies.
--   * This migration therefore rebuilds with explicit link rescue:
--     before a referenced parent table is dropped, the incoming
--     links are copied to a scratch table and nulled; after the
--     rebuilt table is swapped in under the same name, the links are
--     restored from the scratch table. Dropping a parent whose
--     incoming links are all NULL fires no FK action, and the
--     restore UPDATEs are themselves FK-checked against the rebuilt
--     table inside the same transaction.
--   * Any failure rolls the whole migration back (scratch tables
--     included); the runner's pre-migration backup is a second net.
--     The v4 -> v5 upgrade test verifies data preservation plus
--     PRAGMA foreign_key_check and PRAGMA integrity_check.

-- 1. Rescue instance -> proxy bindings, then detach them so dropping
--    the old proxies table cannot SET NULL them away.
CREATE TABLE mig0005_instance_proxy (
    instance_id TEXT PRIMARY KEY,
    proxy_id    TEXT NOT NULL
);
INSERT INTO mig0005_instance_proxy (instance_id, proxy_id)
SELECT id, proxy_id FROM browser_instances WHERE proxy_id IS NOT NULL;
UPDATE browser_instances SET proxy_id = NULL WHERE proxy_id IS NOT NULL;

-- 2. proxy_tags references proxies and is itself being removed;
--    dropping the child table first is always safe.
DROP TABLE proxy_tags;

-- 3. Rebuild proxies without group_id.
CREATE TABLE proxies_new (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    protocol        TEXT NOT NULL CHECK (protocol IN (
                        'http', 'https', 'socks5',
                        'vmess', 'vless', 'trojan', 'shadowsocks',
                        'hysteria2', 'tuic', 'wireguard', 'direct'
                    )),
    address         TEXT NOT NULL DEFAULT '',
    port            INTEGER CHECK (port IS NULL OR (port >= 0 AND port <= 65535)),
    config_json     TEXT NOT NULL DEFAULT '{}',
    secret_ref      TEXT NOT NULL DEFAULT '',
    enabled         INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT NOT NULL DEFAULT (datetime('now')),
    sort_order      INTEGER NOT NULL DEFAULT 0,
    source          TEXT NOT NULL DEFAULT 'manual'
                    CHECK (source IN ('manual', 'file_import', 'subscription')),
    subscription_id TEXT REFERENCES subscriptions (id) ON DELETE CASCADE,
    dedup_key       TEXT NOT NULL DEFAULT '',
    last_latency_ms INTEGER
                    CHECK (last_latency_ms IS NULL OR last_latency_ms >= 0),
    last_test_ok    INTEGER
                    CHECK (last_test_ok IS NULL OR last_test_ok IN (0, 1)),
    last_tested_at  TEXT
);

INSERT INTO proxies_new (
    id, name, protocol, address, port, config_json, secret_ref, enabled,
    created_at, updated_at, sort_order, source, subscription_id, dedup_key,
    last_latency_ms, last_test_ok, last_tested_at
)
SELECT
    id, name, protocol, address, port, config_json, secret_ref, enabled,
    created_at, updated_at, sort_order, source, subscription_id, dedup_key,
    last_latency_ms, last_test_ok, last_tested_at
FROM proxies;

DROP TABLE proxies;

ALTER TABLE proxies_new RENAME TO proxies;

CREATE INDEX idx_proxies_protocol ON proxies (protocol);
CREATE INDEX idx_proxies_enabled ON proxies (enabled);
CREATE UNIQUE INDEX idx_proxies_dedup ON proxies (dedup_key) WHERE dedup_key <> '';
CREATE INDEX idx_proxies_subscription ON proxies (subscription_id);

-- 4. Restore the instance bindings rescued in step 1.
UPDATE browser_instances
SET proxy_id = (
    SELECT proxy_id FROM mig0005_instance_proxy
    WHERE mig0005_instance_proxy.instance_id = browser_instances.id
)
WHERE id IN (SELECT instance_id FROM mig0005_instance_proxy);
DROP TABLE mig0005_instance_proxy;

-- 5. Rescue proxy -> subscription links, then detach them so
--    dropping the old subscriptions table cannot CASCADE them away.
CREATE TABLE mig0005_proxy_subscription (
    proxy_id        TEXT PRIMARY KEY,
    subscription_id TEXT NOT NULL
);
INSERT INTO mig0005_proxy_subscription (proxy_id, subscription_id)
SELECT id, subscription_id FROM proxies WHERE subscription_id IS NOT NULL;
UPDATE proxies SET subscription_id = NULL WHERE subscription_id IS NOT NULL;

-- 6. Rebuild subscriptions without group_id.
CREATE TABLE subscriptions_new (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL,
    url_hint            TEXT NOT NULL DEFAULT '',
    secret_ref          TEXT NOT NULL DEFAULT '',
    allow_lan           INTEGER NOT NULL DEFAULT 0 CHECK (allow_lan IN (0, 1)),
    auto_update_minutes INTEGER NOT NULL DEFAULT 0 CHECK (auto_update_minutes >= 0),
    last_updated_at     TEXT,
    last_status         TEXT NOT NULL DEFAULT '',
    last_error          TEXT NOT NULL DEFAULT '',
    created_at          TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at          TEXT NOT NULL DEFAULT (datetime('now'))
);

INSERT INTO subscriptions_new (
    id, name, url_hint, secret_ref, allow_lan, auto_update_minutes,
    last_updated_at, last_status, last_error, created_at, updated_at
)
SELECT
    id, name, url_hint, secret_ref, allow_lan, auto_update_minutes,
    last_updated_at, last_status, last_error, created_at, updated_at
FROM subscriptions;

DROP TABLE subscriptions;

ALTER TABLE subscriptions_new RENAME TO subscriptions;

-- 7. Restore the subscription links rescued in step 5.
UPDATE proxies
SET subscription_id = (
    SELECT subscription_id FROM mig0005_proxy_subscription
    WHERE mig0005_proxy_subscription.proxy_id = proxies.id
)
WHERE id IN (SELECT proxy_id FROM mig0005_proxy_subscription);
DROP TABLE mig0005_proxy_subscription;

-- 8. Nothing references proxy_groups any more (both group_id columns
--    are gone with the rebuilt tables above); drop it.
DROP TABLE proxy_groups;
