-- Stage 6C: editable diversion rules and editable core settings.
--
-- proxy_rule_policies holds per-built-in-ruleset policy OVERRIDES
-- only: a ruleset with no row keeps the built-in default policy
-- compiled into internal/mihomo. policy is one of
--   proxy    — matching traffic goes to the instance's bound node;
--   direct   — matching traffic goes direct;
--   reject   — matching traffic is rejected;
--   disabled — the ruleset is neither generated as a rule-provider
--              nor referenced by any sub-rule.
--
-- proxy_custom_rules holds the user's own top rules, evaluated in
-- sort_order ahead of every built-in RULE-SET line in each
-- rule-mode instance's sub-rules.
--
-- proxy_settings is a small key/value store for the editable core
-- settings (delay-test URL/timeout, provider update interval, DoH
-- upstreams, core log level, listener port range start). Missing
-- keys mean the built-in defaults in internal/model.
CREATE TABLE proxy_rule_policies (
    provider_name TEXT PRIMARY KEY,
    policy TEXT NOT NULL
        CHECK (policy IN ('proxy', 'direct', 'reject', 'disabled')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE proxy_custom_rules (
    id TEXT PRIMARY KEY,
    rule_type TEXT NOT NULL
        CHECK (rule_type IN ('DOMAIN-SUFFIX', 'DOMAIN-KEYWORD', 'IP-CIDR')),
    value TEXT NOT NULL,
    policy TEXT NOT NULL
        CHECK (policy IN ('proxy', 'direct', 'reject')),
    enabled INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    sort_order INTEGER NOT NULL,
    created_at TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE TABLE proxy_settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
