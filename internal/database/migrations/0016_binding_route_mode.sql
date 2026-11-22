-- Stage 6C: per-instance routing mode for proxy bindings.
--
-- route_mode decides how one bound instance's listener traffic
-- is routed inside the Mihomo core:
--   rule   — the instance's sub-rules over the shared
--            rule-providers decide per request, with the bound
--            node as the final fallback (MATCH);
--   global — every request goes to the bound node;
--   direct — every request goes direct, the bound node is unused
--            until the mode changes.
-- The mode is part of the binding record: the generated runtime
-- configuration derives the instance's listener rule and
-- sub-rules from it. Existing bindings take the default 'rule'.
ALTER TABLE instance_proxy_bindings
    ADD COLUMN route_mode TEXT NOT NULL DEFAULT 'rule'
        CHECK (route_mode IN ('rule', 'global', 'direct'));
