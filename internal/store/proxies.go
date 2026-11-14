package store

import (
	"context"
	"fmt"
)

// This file is the historical-data compatibility layer for the
// proxies / subscriptions / mihomo_configs tables. The proxy,
// subscription and Mihomo-config features were removed in the PR #9
// scope cleanup: no service, page or binding can create, update or
// delete those records any more, so this repository intentionally
// exposes no CRUD at all.
//
// What remains is exactly what data compatibility still needs:
//
//   - SecretRefInUse / AllSecretRefs: the vault's guarded cleanup
//     must keep recognising credential refs held by historical
//     proxy, subscription and imported-config rows, so an in-use
//     credential is never collected.
//   - The tables themselves (created by migrations 0001–0009, which
//     are never modified) keep historical rows readable by SQLite
//     and keep browser_instances.proxy_id foreign keys valid.

// SecretRefInUse reports whether any database row (proxy,
// subscription or imported Mihomo config) currently references
// the given credential ref. It backs the vault's guarded cleanup:
// a ref that is still in use must never be deleted from the vault.
func (s *Store) SecretRefInUse(ctx context.Context, ref string) (bool, error) {
	if ref == "" {
		return false, nil
	}
	var n int
	if err := s.q.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(1) FROM proxies WHERE secret_ref = ?) +
		        (SELECT COUNT(1) FROM subscriptions WHERE secret_ref = ?) +
		        (SELECT COUNT(1) FROM mihomo_configs WHERE secret_ref = ?)`,
		ref, ref, ref).Scan(&n); err != nil {
		return false, fmt.Errorf("store: secret ref in use: %w", err)
	}
	return n > 0, nil
}

// AllSecretRefs returns the set of credential refs currently
// referenced by any proxy, subscription or imported-config row —
// the live set for vault garbage collection.
func (s *Store) AllSecretRefs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT secret_ref FROM proxies WHERE secret_ref <> ''
		 UNION
		 SELECT secret_ref FROM subscriptions WHERE secret_ref <> ''
		 UNION
		 SELECT secret_ref FROM mihomo_configs WHERE secret_ref <> ''`)
	if err != nil {
		return nil, fmt.Errorf("store: all secret refs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, fmt.Errorf("store: all secret refs: %w", err)
		}
		out[ref] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: all secret refs: %w", err)
	}
	return out, nil
}
