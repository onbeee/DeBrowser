-- Stage 6C redo: the custom group-member overlay is removed
-- (product decision: no custom nodes / custom membership in the
-- proxy configuration module). Migration 0008, which created the
-- table, stays in the published history untouched — databases
-- that already applied it (the PR #9 acceptance builds ran it)
-- upgrade through this drop, and fresh databases create and then
-- drop the table, keeping one continuous migration history for
-- every install. No other table, row or vault reference is
-- affected: the overlay only ever referenced configs and custom
-- nodes, it never owned data.

DROP TABLE IF EXISTS mihomo_proxy_group_custom_members;
