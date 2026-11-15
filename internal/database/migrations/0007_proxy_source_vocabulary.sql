-- Stage 6C: one explicit source vocabulary for every proxy row.
--
-- The unified proxy configuration module manages three sources —
-- subscription nodes, nodes imported from a local file, and
-- custom (manually created) nodes. The Stage 4A vocabulary
-- ('manual', 'file_import', 'subscription') predates that model;
-- this migration renames it to ('custom', 'file', 'subscription'),
-- matching mihomo_configs.source_type and the unified UI.
--
-- Changing the CHECK constraint on proxies.source requires a
-- table rebuild. The rebuild follows the link-rescue pattern
-- proven by migration 0005: the migration runner executes with
-- PRAGMA foreign_keys=1 and FK actions fire immediately, so
-- before the old proxies table is dropped, every incoming link
-- is rescued —
--
--   * browser_instances.proxy_id (ON DELETE SET NULL) is copied
--     aside and nulled, then restored afterwards;
--   * mihomo_proxy_group_members rows of kind 'proxy' (their
--     proxy_id is ON DELETE CASCADE, and their CHECK forbids a
--     NULL proxy_id, so they cannot just be detached) are copied
--     aside in full, deleted, and re-inserted against the rebuilt
--     table, preserving group, member order and names exactly.
--
-- No row, ID, credential reference or relation changes apart from
-- the source value mapping:
--   'manual' -> 'custom', 'file_import' -> 'file',
--   'subscription' -> 'subscription'.
-- Any failure rolls the whole migration back; the runner's
-- pre-migration backup is a second net.

-- 1. Rescue instance -> proxy bindings.
CREATE TABLE mig0007_instance_proxy (
    instance_id TEXT PRIMARY KEY,
    proxy_id    TEXT NOT NULL
);
INSERT INTO mig0007_instance_proxy (instance_id, proxy_id)
SELECT id, proxy_id FROM browser_instances WHERE proxy_id IS NOT NULL;
UPDATE browser_instances SET proxy_id = NULL WHERE proxy_id IS NOT NULL;

-- 2. Rescue proxy-kind group members in full.
CREATE TABLE mig0007_group_members (
    group_id        TEXT NOT NULL,
    sort_order      INTEGER NOT NULL,
    member_kind     TEXT NOT NULL,
    proxy_id        TEXT,
    target_group_id TEXT,
    member_name     TEXT NOT NULL
);
INSERT INTO mig0007_group_members (
    group_id, sort_order, member_kind, proxy_id, target_group_id, member_name
)
SELECT group_id, sort_order, member_kind, proxy_id, target_group_id, member_name
FROM mihomo_proxy_group_members
WHERE proxy_id IS NOT NULL;
DELETE FROM mihomo_proxy_group_members WHERE proxy_id IS NOT NULL;

-- 3. Rebuild proxies with the unified source vocabulary.
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
    source          TEXT NOT NULL DEFAULT 'custom'
                    CHECK (source IN ('custom', 'file', 'subscription')),
    subscription_id TEXT REFERENCES subscriptions (id) ON DELETE CASCADE,
    dedup_key       TEXT NOT NULL DEFAULT '',
    last_latency_ms INTEGER
                    CHECK (last_latency_ms IS NULL OR last_latency_ms >= 0),
    last_test_ok    INTEGER
                    CHECK (last_test_ok IS NULL OR last_test_ok IN (0, 1)),
    last_tested_at  TEXT,
    config_id       TEXT REFERENCES mihomo_configs (id) ON DELETE SET NULL,
    source_node_name TEXT NOT NULL DEFAULT ''
);

INSERT INTO proxies_new (
    id, name, protocol, address, port, config_json, secret_ref, enabled,
    created_at, updated_at, sort_order, source, subscription_id, dedup_key,
    last_latency_ms, last_test_ok, last_tested_at, config_id, source_node_name
)
SELECT
    id, name, protocol, address, port, config_json, secret_ref, enabled,
    created_at, updated_at, sort_order,
    CASE source
        WHEN 'manual' THEN 'custom'
        WHEN 'file_import' THEN 'file'
        ELSE source
    END,
    subscription_id, dedup_key,
    last_latency_ms, last_test_ok, last_tested_at, config_id, source_node_name
FROM proxies;

DROP TABLE proxies;

ALTER TABLE proxies_new RENAME TO proxies;

CREATE INDEX idx_proxies_protocol ON proxies (protocol);
CREATE INDEX idx_proxies_enabled ON proxies (enabled);
CREATE UNIQUE INDEX idx_proxies_dedup ON proxies (dedup_key) WHERE dedup_key <> '';
CREATE INDEX idx_proxies_subscription ON proxies (subscription_id);
CREATE INDEX idx_proxies_config ON proxies (config_id);

-- 4. Restore the rescued instance bindings.
UPDATE browser_instances
SET proxy_id = (
    SELECT proxy_id FROM mig0007_instance_proxy
    WHERE mig0007_instance_proxy.instance_id = browser_instances.id
)
WHERE id IN (SELECT instance_id FROM mig0007_instance_proxy);
DROP TABLE mig0007_instance_proxy;

-- 5. Restore the rescued group members.
INSERT INTO mihomo_proxy_group_members (
    group_id, sort_order, member_kind, proxy_id, target_group_id, member_name
)
SELECT group_id, sort_order, member_kind, proxy_id, target_group_id, member_name
FROM mig0007_group_members;
DROP TABLE mig0007_group_members;
