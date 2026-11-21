package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/onbeee/DeBrowser/internal/model"
)

// This file is the repository for Stage 6C proxy management:
// proxy_sources (subscription / local-file node sources) and
// instance_proxy_bindings (per-instance node bindings). It is a
// NEW feature store, unrelated to the historical proxies /
// subscriptions / mihomo_configs compatibility layer in
// proxies.go, which remains read-only.

func scanProxySource(row interface{ Scan(...any) error }) (model.ProxySource, error) {
	var s model.ProxySource
	err := row.Scan(&s.ID, &s.Name, &s.Kind, &s.SecretRef, &s.FileName,
		&s.ProviderName, &s.CreatedAt, &s.UpdatedAt)
	return s, err
}

const proxySourceColumns = `id, name, kind, secret_ref, file_name, provider_name, created_at, updated_at`

// CreateProxySource inserts one source row.
func (s *Store) CreateProxySource(ctx context.Context, src model.ProxySource) (model.ProxySource, error) {
	if _, err := s.q.ExecContext(ctx,
		`INSERT INTO proxy_sources (id, name, kind, secret_ref, file_name, provider_name)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		src.ID, src.Name, src.Kind, src.SecretRef, src.FileName, src.ProviderName); err != nil {
		return model.ProxySource{}, fmt.Errorf("store: create proxy source: %w", err)
	}
	return s.GetProxySource(ctx, src.ID)
}

// GetProxySource returns one source by ID (ErrNotFound when absent).
func (s *Store) GetProxySource(ctx context.Context, id string) (model.ProxySource, error) {
	src, err := scanProxySource(s.q.QueryRowContext(ctx,
		`SELECT `+proxySourceColumns+` FROM proxy_sources WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.ProxySource{}, ErrNotFound
	}
	if err != nil {
		return model.ProxySource{}, fmt.Errorf("store: get proxy source: %w", err)
	}
	return src, nil
}

// ListProxySources returns all sources in creation order.
func (s *Store) ListProxySources(ctx context.Context) ([]model.ProxySource, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT `+proxySourceColumns+` FROM proxy_sources ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list proxy sources: %w", err)
	}
	defer rows.Close()
	out := []model.ProxySource{}
	for rows.Next() {
		src, err := scanProxySource(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list proxy sources: %w", err)
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// DeleteProxySource removes one source row. Bindings referencing
// it block the delete at the database level (foreign key without
// cascade); the service checks first and reports a friendly error.
func (s *Store) DeleteProxySource(ctx context.Context, id string) error {
	res, err := s.q.ExecContext(ctx, `DELETE FROM proxy_sources WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete proxy source: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func scanBinding(row interface{ Scan(...any) error }) (model.InstanceProxyBinding, error) {
	var b model.InstanceProxyBinding
	err := row.Scan(&b.InstanceID, &b.SourceID, &b.NodeName, &b.Mode, &b.Port,
		&b.CreatedAt, &b.UpdatedAt)
	return b, err
}

const bindingColumns = `instance_id, source_id, node_name, mode, port, created_at, updated_at`

// UpsertBinding creates or replaces the binding of one instance.
func (s *Store) UpsertBinding(ctx context.Context, b model.InstanceProxyBinding) (model.InstanceProxyBinding, error) {
	if _, err := s.q.ExecContext(ctx,
		`INSERT INTO instance_proxy_bindings (instance_id, source_id, node_name, mode, port)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT (instance_id) DO UPDATE SET
		   source_id = excluded.source_id,
		   node_name = excluded.node_name,
		   mode = excluded.mode,
		   port = excluded.port,
		   updated_at = datetime('now')`,
		b.InstanceID, b.SourceID, b.NodeName, b.Mode, b.Port); err != nil {
		return model.InstanceProxyBinding{}, fmt.Errorf("store: upsert proxy binding: %w", err)
	}
	return s.GetBinding(ctx, b.InstanceID)
}

// GetBinding returns the binding of one instance (ErrNotFound when
// the instance has no proxy binding).
func (s *Store) GetBinding(ctx context.Context, instanceID string) (model.InstanceProxyBinding, error) {
	b, err := scanBinding(s.q.QueryRowContext(ctx,
		`SELECT `+bindingColumns+` FROM instance_proxy_bindings WHERE instance_id = ?`, instanceID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.InstanceProxyBinding{}, ErrNotFound
	}
	if err != nil {
		return model.InstanceProxyBinding{}, fmt.Errorf("store: get proxy binding: %w", err)
	}
	return b, nil
}

// ListBindings returns all bindings ordered by port.
func (s *Store) ListBindings(ctx context.Context) ([]model.InstanceProxyBinding, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT `+bindingColumns+` FROM instance_proxy_bindings ORDER BY port`)
	if err != nil {
		return nil, fmt.Errorf("store: list proxy bindings: %w", err)
	}
	defer rows.Close()
	out := []model.InstanceProxyBinding{}
	for rows.Next() {
		b, err := scanBinding(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list proxy bindings: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// DeleteBinding removes one instance's binding (no error when the
// instance had none).
func (s *Store) DeleteBinding(ctx context.Context, instanceID string) error {
	if _, err := s.q.ExecContext(ctx,
		`DELETE FROM instance_proxy_bindings WHERE instance_id = ?`, instanceID); err != nil {
		return fmt.Errorf("store: delete proxy binding: %w", err)
	}
	return nil
}

// CountBindingsForSource reports how many instances are bound to
// nodes of the given source.
func (s *Store) CountBindingsForSource(ctx context.Context, sourceID string) (int, error) {
	var n int
	if err := s.q.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM instance_proxy_bindings WHERE source_id = ?`,
		sourceID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count bindings for source: %w", err)
	}
	return n, nil
}

// UsedBindingPorts returns every listener port currently assigned
// to a binding.
func (s *Store) UsedBindingPorts(ctx context.Context) ([]int, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT port FROM instance_proxy_bindings ORDER BY port`)
	if err != nil {
		return nil, fmt.Errorf("store: used binding ports: %w", err)
	}
	defer rows.Close()
	out := []int{}
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("store: used binding ports: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
