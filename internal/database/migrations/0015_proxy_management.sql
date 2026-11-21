-- Stage 6C: Mihomo proxy management.
--
-- Two new tables carry DeBrowser's own proxy business data. They
-- deliberately do NOT model proxy nodes, proxy groups or rules:
-- those are Mihomo core concepts, parsed and executed by the core
-- from the generated runtime configuration. DeBrowser stores only
-- what the core cannot know:
--
--   proxy_sources: the subscription / local-file sources the user
--   imported. A source maps 1:1 to one Mihomo proxy-provider in the
--   generated runtime configuration (provider_name). Subscription
--   URLs are credentials: they live in the credential vault and
--   only the opaque secret_ref is stored here, never the URL
--   itself. Local YAML sources are copied under data/mihomo/
--   sources/ and file_name is that copy's basename; the user's
--   original file is never modified.
--
--   instance_proxy_bindings: which node (identified by source +
--   the node's name inside that source) each browser instance
--   uses, and the loopback listener port the generated
--   configuration assigns to the instance. The binding
--   is DeBrowser's data: it is never delegated to the core's
--   cache.db. Deleting an instance removes its binding; a source
--   cannot be deleted while bindings reference it (the service
--   reports the conflict first, the foreign key is the backstop).
--
-- The historical proxies / subscriptions / mihomo_configs tables
-- (migrations 0001-0009) stay untouched as a data-compatibility
-- layer; nothing here reads or writes them.

CREATE TABLE proxy_sources (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    kind          TEXT NOT NULL CHECK (kind IN ('url', 'file')),
    secret_ref    TEXT NOT NULL DEFAULT '',
    file_name     TEXT NOT NULL DEFAULT '',
    provider_name TEXT NOT NULL UNIQUE,
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE instance_proxy_bindings (
    instance_id TEXT PRIMARY KEY REFERENCES browser_instances (id) ON DELETE CASCADE,
    source_id   TEXT NOT NULL REFERENCES proxy_sources (id),
    node_name   TEXT NOT NULL,
    port        INTEGER NOT NULL UNIQUE,
    created_at  TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX idx_instance_proxy_bindings_source
    ON instance_proxy_bindings (source_id);
