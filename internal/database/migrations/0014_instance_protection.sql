-- Stage 9B (review round 3): instance_protection is the fallback
-- protection journal for an instance whose browser may still be
-- running while its full instance_runtime record could not be
-- written (a database failure at exactly that moment). It carries
-- the same process identity fields as instance_runtime so startup
-- reconciliation can re-verify the process with the same strict
-- standard: confirmed processes are promoted to a real runtime
-- record and re-adopted, proven-gone processes clear the journal,
-- and anything unverifiable keeps the instance frozen at
-- "unknown" across restarts. A row here is never a reason to call
-- an instance stopped. The table never stores browser profile
-- content of any kind.
CREATE TABLE instance_protection (
    instance_id        TEXT PRIMARY KEY REFERENCES browser_instances(id) ON DELETE CASCADE,
    pid                INTEGER NOT NULL,
    process_created_at INTEGER NOT NULL DEFAULT 0,
    platform           TEXT NOT NULL DEFAULT '',
    executable_path    TEXT NOT NULL DEFAULT '',
    user_data_dir      TEXT NOT NULL DEFAULT '',
    marked_at          TEXT NOT NULL DEFAULT ''
);
