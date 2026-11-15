-- Stage 6C review fix: user-added members for native proxy-groups.
--
-- A user may attach their own custom nodes (proxies.source =
-- 'custom') to the native Mihomo proxy-groups of an imported
-- config. Those attachments are NOT part of the imported document
-- and must survive subscription refreshes and re-imports, which
-- rebuild mihomo_proxy_groups / mihomo_proxy_group_members from
-- the source document — so they live in this separate overlay
-- table, keyed by the STABLE triple (config, group NAME, node):
-- the group name is the identity a refresh preserves, because a
-- refresh deletes and recreates the group rows themselves.
--
-- This is a membership overlay, not a DeBrowser grouping system:
-- the groups themselves remain Mihomo's native proxy-groups,
-- defined by the imported document.
--
-- Cleanup is by foreign key only: deleting a config or a custom
-- node removes its overlay rows; nothing cascades INTO proxies,
-- configs or imported members. Whether a proxy is really
-- source='custom' and whether (config_id, group_name) names a
-- group the config actually has are validated by the service
-- layer inside the write transaction (a CHECK cannot express
-- them).

CREATE TABLE mihomo_proxy_group_custom_members (
    config_id  TEXT NOT NULL REFERENCES mihomo_configs (id) ON DELETE CASCADE,
    group_name TEXT NOT NULL,
    proxy_id   TEXT NOT NULL REFERENCES proxies (id) ON DELETE CASCADE,
    sort_order INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (config_id, group_name, proxy_id)
);

CREATE INDEX idx_mpg_custom_members_proxy
    ON mihomo_proxy_group_custom_members (proxy_id);

CREATE INDEX idx_mpg_custom_members_config
    ON mihomo_proxy_group_custom_members (config_id, group_name, sort_order);
