-- Stage 9A: DeBrowser-managed standalone Chromium kernels.
--
-- Only kernels that DeBrowser itself downloaded and installed are
-- persisted here. System browsers (an installed Google Chrome or
-- Brave) are detected live at runtime and are NEVER written to this
-- table: DeBrowser does not own them, must not modify or delete them,
-- and their records would go stale the moment the user upgrades or
-- uninstalls them outside DeBrowser.
--
-- Paths are stored RELATIVE to the portable application root
-- (for example "kernels/142.0.7444.162"), so the whole portable
-- folder can move machines unchanged; the service layer resolves
-- and re-validates the real path on every use.

CREATE TABLE kernels (
    id              TEXT PRIMARY KEY,
    version         TEXT NOT NULL,
    channel         TEXT NOT NULL DEFAULT 'stable',
    platform        TEXT NOT NULL DEFAULT 'win64',
    install_path    TEXT NOT NULL,
    executable_path TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'ready'
                    CHECK (status IN ('ready', 'verification_failed', 'missing')),
    created_at      TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

-- One registration per version and platform: installing the same
-- Chrome for Testing build twice is an error, not a duplicate row.
CREATE UNIQUE INDEX idx_kernels_version_platform
    ON kernels (version, platform);
