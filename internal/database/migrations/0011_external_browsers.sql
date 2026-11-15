-- Stage 9A (revised): browsers the user added BY HAND.
--
-- DeBrowser does not detect browsers automatically (no registry
-- or disk scanning, by product decision). When the user picks an
-- installed Chrome or Brave executable in the file picker and it
-- passes identity, version and Authenticode publisher
-- verification, the registration is persisted here so it survives
-- restarts. Only the reference is stored: the browser binary is
-- never copied into DeBrowser's folders, its install directory is
-- never modified, and removing a registration deletes only this
-- row — never the browser itself.
--
-- executable_path is the absolute path the user chose; it is
-- re-verified before every use, and a browser that was moved or
-- uninstalled by the user shows as status 'invalid' instead of
-- being silently re-located.

CREATE TABLE external_browsers (
    id                  TEXT PRIMARY KEY,
    name                TEXT NOT NULL,
    browser_type        TEXT NOT NULL
                        CHECK (browser_type IN ('chrome', 'brave')),
    version             TEXT NOT NULL DEFAULT '',
    executable_path     TEXT NOT NULL,
    signature_status    TEXT NOT NULL DEFAULT '',
    signature_publisher TEXT NOT NULL DEFAULT '',
    status              TEXT NOT NULL DEFAULT 'verified'
                        CHECK (status IN ('verified', 'invalid')),
    created_at          TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at          TEXT NOT NULL DEFAULT (datetime('now')),
    last_verified_at    TEXT NOT NULL DEFAULT ''
);

-- One registration per executable path: adding the same browser
-- twice refreshes its verification instead of duplicating it.
CREATE UNIQUE INDEX idx_external_browsers_path
    ON external_browsers (executable_path);
