-- Stage 9B: browser instance runtime.
--
-- instance_runtime records, for each instance DeBrowser has
-- launched and not yet seen exit, the identity of the browser's
-- main process: PID plus the process creation time (a PID alone
-- can be recycled by the OS; PID + creation time + executable
-- path + command line is what re-identification after a DeBrowser
-- restart verifies). Rows are written at successful launch and
-- removed when the process exit is observed or the record is
-- proven stale during startup reconciliation. The table never
-- stores browser profile content of any kind.
CREATE TABLE instance_runtime (
    instance_id        TEXT PRIMARY KEY REFERENCES browser_instances(id) ON DELETE CASCADE,
    pid                INTEGER NOT NULL,
    process_created_at INTEGER NOT NULL,
    platform           TEXT NOT NULL DEFAULT '',
    executable_path    TEXT NOT NULL DEFAULT '',
    user_data_dir      TEXT NOT NULL DEFAULT '',
    started_at         TEXT NOT NULL DEFAULT ''
);

-- profile_counter is the persistent, monotonically increasing
-- sequence behind the default instance-file names P-1, P-2, ...
-- It only ever moves forward: deleting an instance or its file
-- never frees its number for reuse. It starts at 1: the P-<n>
-- naming scheme itself is new in Stage 9B, so no P-<n> folder can
-- predate the counter — and the allocator additionally skips any
-- candidate that is bound or present on disk, so even a foreign
-- P-<n> folder placed there by hand is never handed out or
-- overwritten.
CREATE TABLE profile_counter (
    id       INTEGER PRIMARY KEY CHECK (id = 1),
    next_seq INTEGER NOT NULL
);

INSERT INTO profile_counter (id, next_seq) VALUES (1, 1);

-- The one-to-one binding between instances and instance files
-- needs no new constraint here: browser_instances.user_data_dir
-- has been UNIQUE since migration 0002, and the service enforces
-- the binding in its checks as well. (browser_instances itself is
-- deliberately NOT rebuilt in this migration: bookmarks hold a
-- foreign key to it, and the new runtime states are carried by
-- instance_runtime plus the persisted 'running' status — see
-- internal/service/instance_runtime.go.)
