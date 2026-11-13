package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/onbeee/DeBrowser/internal/model"
)

const proxyColumns = `id, name, protocol, address, port, config_json, secret_ref, enabled,
	group_id, sort_order, source, subscription_id, dedup_key,
	last_latency_ms, last_test_ok, last_tested_at, created_at, updated_at`

func scanProxy(row interface{ Scan(dest ...any) error }) (model.Proxy, error) {
	var (
		p        model.Proxy
		port     sql.NullInt64
		enable   int
		groupID  sql.NullString
		subID    sql.NullString
		latency  sql.NullInt64
		testOK   sql.NullInt64
		testedAt sql.NullString
	)
	if err := row.Scan(&p.ID, &p.Name, &p.Protocol, &p.Address, &port,
		&p.ConfigJSON, &p.SecretRef, &enable,
		&groupID, &p.SortOrder, &p.Source, &subID, &p.DedupKey,
		&latency, &testOK, &testedAt, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return model.Proxy{}, err
	}
	if port.Valid {
		v := int(port.Int64)
		p.Port = &v
	}
	p.Enabled = enable != 0
	if groupID.Valid {
		p.GroupID = &groupID.String
	}
	if subID.Valid {
		p.SubscriptionID = &subID.String
	}
	if latency.Valid {
		v := int(latency.Int64)
		p.LastLatencyMs = &v
	}
	if testOK.Valid {
		v := testOK.Int64 != 0
		p.LastTestOK = &v
	}
	if testedAt.Valid {
		p.LastTestedAt = &testedAt.String
	}
	return p, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullBool(p *bool) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(boolInt(*p)), Valid: true}
}

// CreateProxy inserts a new proxy node, generating its ID when empty.
func (s *Store) CreateProxy(ctx context.Context, p model.Proxy) (model.Proxy, error) {
	if p.ID == "" {
		id, err := newID()
		if err != nil {
			return model.Proxy{}, err
		}
		p.ID = id
	}
	if p.ConfigJSON == "" {
		p.ConfigJSON = "{}"
	}
	if p.Source == "" {
		p.Source = "manual"
	}
	if _, err := s.q.ExecContext(ctx, `INSERT INTO proxies
		(id, name, protocol, address, port, config_json, secret_ref, enabled,
		 group_id, sort_order, source, subscription_id, dedup_key,
		 last_latency_ms, last_test_ok, last_tested_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Name, p.Protocol, p.Address, nullInt(p.Port),
		p.ConfigJSON, p.SecretRef, boolInt(p.Enabled),
		nullStr(p.GroupID), p.SortOrder, p.Source, nullStr(p.SubscriptionID), p.DedupKey,
		nullInt(p.LastLatencyMs), nullBool(p.LastTestOK), nullStr(p.LastTestedAt)); err != nil {
		return model.Proxy{}, fmt.Errorf("store: create proxy: %w", err)
	}
	return s.GetProxy(ctx, p.ID)
}

// GetProxy returns one proxy by ID, or ErrNotFound.
func (s *Store) GetProxy(ctx context.Context, id string) (model.Proxy, error) {
	p, err := scanProxy(s.q.QueryRowContext(ctx,
		`SELECT `+proxyColumns+` FROM proxies WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Proxy{}, ErrNotFound
	}
	if err != nil {
		return model.Proxy{}, fmt.Errorf("store: get proxy: %w", err)
	}
	return p, nil
}

// ListProxies returns all proxies in display order (sort_order,
// then name).
func (s *Store) ListProxies(ctx context.Context) ([]model.Proxy, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+proxyColumns+` FROM proxies ORDER BY sort_order, name, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list proxies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.Proxy
	for rows.Next() {
		p, err := scanProxy(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list proxies: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list proxies: %w", err)
	}
	return out, nil
}

// UpdateProxy replaces the editable fields of a proxy and refreshes
// its updated_at timestamp. The last-test fields are managed
// separately (UpdateProxyTestResult) and are not touched here.
func (s *Store) UpdateProxy(ctx context.Context, p model.Proxy) (model.Proxy, error) {
	res, err := s.q.ExecContext(ctx, `UPDATE proxies SET
		name = ?, protocol = ?, address = ?, port = ?, config_json = ?,
		secret_ref = ?, enabled = ?, group_id = ?, sort_order = ?,
		source = ?, subscription_id = ?, dedup_key = ?,
		updated_at = datetime('now')
		WHERE id = ?`,
		p.Name, p.Protocol, p.Address, nullInt(p.Port), p.ConfigJSON,
		p.SecretRef, boolInt(p.Enabled), nullStr(p.GroupID), p.SortOrder,
		p.Source, nullStr(p.SubscriptionID), p.DedupKey, p.ID)
	if err != nil {
		return model.Proxy{}, fmt.Errorf("store: update proxy: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return model.Proxy{}, fmt.Errorf("store: update proxy: %w", err)
	} else if n == 0 {
		return model.Proxy{}, ErrNotFound
	}
	return s.GetProxy(ctx, p.ID)
}

// UpdateProxyTestResult records the outcome of one connection test.
// A failed test clears the latency (nil) rather than keeping a
// stale success value.
func (s *Store) UpdateProxyTestResult(ctx context.Context, id string, latencyMs *int, ok bool, testedAt string) error {
	res, err := s.q.ExecContext(ctx, `UPDATE proxies SET
		last_latency_ms = ?, last_test_ok = ?, last_tested_at = ?,
		updated_at = datetime('now')
		WHERE id = ?`,
		nullInt(latencyMs), boolInt(ok), testedAt, id)
	if err != nil {
		return fmt.Errorf("store: update proxy test result: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: update proxy test result: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteProxy removes a proxy. Instances that referenced it keep
// existing with proxy_id set to NULL (schema ON DELETE SET NULL);
// its tag associations are removed (ON DELETE CASCADE).
func (s *Store) DeleteProxy(ctx context.Context, id string) error {
	res, err := s.q.ExecContext(ctx, `DELETE FROM proxies WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete proxy: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: delete proxy: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SecretRefInUse reports whether any database row (proxy or
// subscription) currently references the given credential ref. It
// backs the vault's guarded cleanup: a ref that is still in use
// must never be deleted from the vault.
func (s *Store) SecretRefInUse(ctx context.Context, ref string) (bool, error) {
	if ref == "" {
		return false, nil
	}
	var n int
	if err := s.q.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(1) FROM proxies WHERE secret_ref = ?) +
		        (SELECT COUNT(1) FROM subscriptions WHERE secret_ref = ?)`,
		ref, ref).Scan(&n); err != nil {
		return false, fmt.Errorf("store: secret ref in use: %w", err)
	}
	return n > 0, nil
}

// AllSecretRefs returns the set of credential refs currently
// referenced by any proxy or subscription row — the live set for
// vault garbage collection.
func (s *Store) AllSecretRefs(ctx context.Context) (map[string]bool, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT secret_ref FROM proxies WHERE secret_ref <> ''
		 UNION
		 SELECT secret_ref FROM subscriptions WHERE secret_ref <> ''`)
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
