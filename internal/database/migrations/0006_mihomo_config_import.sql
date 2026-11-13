-- Stage 6: subscription management and full Mihomo config import.
--
-- Adds the structured storage for imported Mihomo configurations:
-- one mihomo_configs row per imported document (its credential-
-- stripped artifact lives as a file under data/mihomo/configs/;
-- the database holds only the relative path, the SHA-256 and an
-- opaque vault reference for the extracted credential bundle),
-- the native Mihomo proxy-groups of that config, and their member
-- relations resolved to stable IDs (proxy rows, sibling groups, or
-- Mihomo builtins). Credential policy is unchanged: no passwords,
-- UUIDs, keys or full subscription URLs ever enter this database.

CREATE TABLE mihomo_configs (
    id                   TEXT PRIMARY KEY,
    name                 TEXT NOT NULL,
    source_type          TEXT NOT NULL CHECK (source_type IN ('file', 'subscription')),
    subscription_id      TEXT REFERENCES subscriptions (id) ON DELETE CASCADE,
    -- Relative to data/mihomo/ only (for example
    -- "configs/<uuid>.yaml"); absolute paths and ".." are rejected
    -- by the service layer before any row is written.
    file_path            TEXT NOT NULL,
    sha256               TEXT NOT NULL,
    -- Opaque vault reference of the config-level credential bundle
    -- extracted during sanitisation; '' when the document carried
    -- no config-level credentials.
    secret_ref           TEXT NOT NULL DEFAULT '',
    format_version       INTEGER NOT NULL DEFAULT 1,
    -- JSON array of Stage 6 security findings about the original
    -- document (allow-lan, external listeners/controller, missing
    -- controller secret, TUN). Findings only — Stage 7 enforces.
    security_report_json TEXT NOT NULL DEFAULT '[]',
    created_at           TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at           TEXT NOT NULL DEFAULT (datetime('now'))
);

-- A subscription owns at most one current config.
CREATE UNIQUE INDEX idx_mihomo_configs_subscription
    ON mihomo_configs (subscription_id) WHERE subscription_id IS NOT NULL;

CREATE INDEX idx_mihomo_configs_source ON mihomo_configs (source_type);

-- Proxies imported from a config point back at it; deleting a
-- config keeps the proxy rows (they may be re-homed by the service
-- layer, which normally deletes them explicitly in the same
-- transaction) but must never leave a dangling reference.
ALTER TABLE proxies ADD COLUMN config_id TEXT REFERENCES mihomo_configs (id) ON DELETE SET NULL;
-- The node's name inside its source document. Native proxy-group
-- members address nodes by this name; the display name may be
-- edited by the user without breaking the relation.
ALTER TABLE proxies ADD COLUMN source_node_name TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_proxies_config ON proxies (config_id);

-- Mihomo native proxy-groups of one config. These are the config
-- author's groups, preserved structurally — they are NOT
-- DeBrowser's own grouping feature (removed in Stage 4A/0005).
CREATE TABLE mihomo_proxy_groups (
    id           TEXT PRIMARY KEY,
    config_id    TEXT NOT NULL REFERENCES mihomo_configs (id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    type         TEXT NOT NULL,
    -- Non-secret group options (interval, url, strategy, ...);
    -- credential-bearing options are extracted to the vault
    -- bundle before this JSON is written.
    options_json TEXT NOT NULL DEFAULT '{}',
    sort_order   INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at   TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE UNIQUE INDEX idx_mihomo_groups_config_name ON mihomo_proxy_groups (config_id, name);
CREATE INDEX idx_mihomo_groups_config ON mihomo_proxy_groups (config_id);

-- One row per group member, in the document's original order.
-- Exactly one target column is set, per member_kind:
--   proxy   -> proxy_id (a node of the SAME config)
--   group   -> target_group_id (a group of the SAME config)
--   builtin -> neither (DIRECT, REJECT, GLOBAL, ...)
CREATE TABLE mihomo_proxy_group_members (
    group_id        TEXT NOT NULL REFERENCES mihomo_proxy_groups (id) ON DELETE CASCADE,
    sort_order      INTEGER NOT NULL,
    member_name     TEXT NOT NULL,
    member_kind     TEXT NOT NULL CHECK (member_kind IN ('proxy', 'group', 'builtin')),
    proxy_id        TEXT REFERENCES proxies (id) ON DELETE CASCADE,
    target_group_id TEXT REFERENCES mihomo_proxy_groups (id) ON DELETE CASCADE,
    PRIMARY KEY (group_id, sort_order),
    CHECK (
        (member_kind = 'proxy'   AND proxy_id IS NOT NULL AND target_group_id IS NULL) OR
        (member_kind = 'group'   AND target_group_id IS NOT NULL AND proxy_id IS NULL) OR
        (member_kind = 'builtin' AND proxy_id IS NULL AND target_group_id IS NULL)
    )
);

CREATE INDEX idx_mihomo_members_proxy ON mihomo_proxy_group_members (proxy_id);
CREATE INDEX idx_mihomo_members_target ON mihomo_proxy_group_members (target_group_id);
