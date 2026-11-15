-- Stage 9A (product adjustment): manual browser registration is
-- now a PURE PATH REGISTRATION.
--
-- Migration 0011 stored the outcome of identity, version and
-- Authenticode publisher verification for hand-added browsers.
-- The product no longer performs ANY identity or security check
-- on a user-chosen executable: the user picks a program, and
-- DeBrowser records only its path and a display name. This
-- migration rebuilds external_browsers without the verification
-- columns and their CHECK constraints.
--
-- Existing registrations are preserved (id, name, executable
-- path and timestamps are copied verbatim): a registration made
-- under the old rules remains a valid path registration under
-- the new ones. Nothing else in the database is touched.

CREATE TABLE external_browsers_new (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    executable_path TEXT NOT NULL,
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

INSERT INTO external_browsers_new (id, name, executable_path, created_at, updated_at)
    SELECT id, name, executable_path, created_at, updated_at
    FROM external_browsers;

DROP TABLE external_browsers;

ALTER TABLE external_browsers_new RENAME TO external_browsers;

-- One registration per executable path (unchanged rule).
CREATE UNIQUE INDEX idx_external_browsers_path
    ON external_browsers (executable_path);
