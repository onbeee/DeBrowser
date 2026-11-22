package store

// Persistence for the redesigned proxy module: sources, their
// parsed nodes, and the one authoritative instance -> node
// binding. Plain SQL over three small tables (migration 0015);
// no state beyond what the user created.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/onbeee/DeBrowser/internal/model"
)

// --- Sources ---

func scanProxySource(row interface{ Scan(...any) error }) (model.ProxySource, error) {
	var src model.ProxySource
	err := row.Scan(&src.ID, &src.Name, &src.Kind, &src.SecretRef, &src.FileName, &src.CreatedAt, &src.UpdatedAt)
	return src, err
}

const proxySourceColumns = `id, name, kind, secret_ref, file_name, created_at, updated_at`

func (s *Store) CreateProxySource(ctx context.Context, src model.ProxySource) error {
	_, err := s.q.ExecContext(ctx,
		`INSERT INTO proxy_sources (id, name, kind, secret_ref, file_name) VALUES (?, ?, ?, ?, ?)`,
		src.ID, src.Name, src.Kind, src.SecretRef, src.FileName)
	if err != nil {
		return fmt.Errorf("store: create proxy source: %w", err)
	}
	return nil
}

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

// DeleteProxySource removes the source row; its nodes and any
// bindings pointing at them go with it (ON DELETE CASCADE).
func (s *Store) DeleteProxySource(ctx context.Context, id string) error {
	res, err := s.q.ExecContext(ctx, `DELETE FROM proxy_sources WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete proxy source: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- Nodes ---

func scanProxyNode(row interface{ Scan(...any) error }) (model.ProxyNode, error) {
	var n model.ProxyNode
	err := row.Scan(&n.ID, &n.SourceID, &n.Name, &n.ConfigJSON, &n.CreatedAt)
	return n, err
}

const proxyNodeColumns = `id, source_id, name, config_json, created_at`

func (s *Store) CreateProxyNode(ctx context.Context, n model.ProxyNode) error {
	_, err := s.q.ExecContext(ctx,
		`INSERT INTO proxy_nodes (id, source_id, name, config_json) VALUES (?, ?, ?, ?)`,
		n.ID, n.SourceID, n.Name, n.ConfigJSON)
	if err != nil {
		return fmt.Errorf("store: create proxy node: %w", err)
	}
	return nil
}

func (s *Store) GetProxyNode(ctx context.Context, id string) (model.ProxyNode, error) {
	n, err := scanProxyNode(s.q.QueryRowContext(ctx,
		`SELECT `+proxyNodeColumns+` FROM proxy_nodes WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.ProxyNode{}, ErrNotFound
	}
	if err != nil {
		return model.ProxyNode{}, fmt.Errorf("store: get proxy node: %w", err)
	}
	return n, nil
}

func (s *Store) ListProxyNodes(ctx context.Context) ([]model.ProxyNode, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT `+proxyNodeColumns+` FROM proxy_nodes ORDER BY source_id, name`)
	if err != nil {
		return nil, fmt.Errorf("store: list proxy nodes: %w", err)
	}
	defer rows.Close()
	out := []model.ProxyNode{}
	for rows.Next() {
		n, err := scanProxyNode(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list proxy nodes: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ReplaceProxyNodes swaps a source's whole node set in the
// caller's transaction (used by subscription refresh): bindings
// to nodes that no longer exist are removed by CASCADE.
func (s *Store) DeleteProxyNodesOfSource(ctx context.Context, sourceID string) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM proxy_nodes WHERE source_id = ?`, sourceID)
	if err != nil {
		return fmt.Errorf("store: delete proxy nodes of source: %w", err)
	}
	return nil
}

// --- Bindings ---

func scanProxyBinding(row interface{ Scan(...any) error }) (model.InstanceProxyBinding, error) {
	var b model.InstanceProxyBinding
	err := row.Scan(&b.InstanceID, &b.NodeID, &b.ListenPort, &b.CreatedAt, &b.UpdatedAt)
	return b, err
}

const proxyBindingColumns = `instance_id, node_id, listen_port, created_at, updated_at`

func (s *Store) GetProxyBinding(ctx context.Context, instanceID string) (model.InstanceProxyBinding, error) {
	b, err := scanProxyBinding(s.q.QueryRowContext(ctx,
		`SELECT `+proxyBindingColumns+` FROM instance_proxy_bindings WHERE instance_id = ?`, instanceID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.InstanceProxyBinding{}, ErrNotFound
	}
	if err != nil {
		return model.InstanceProxyBinding{}, fmt.Errorf("store: get proxy binding: %w", err)
	}
	return b, nil
}

func (s *Store) ListProxyBindings(ctx context.Context) ([]model.InstanceProxyBinding, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT `+proxyBindingColumns+` FROM instance_proxy_bindings ORDER BY listen_port`)
	if err != nil {
		return nil, fmt.Errorf("store: list proxy bindings: %w", err)
	}
	defer rows.Close()
	out := []model.InstanceProxyBinding{}
	for rows.Next() {
		b, err := scanProxyBinding(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list proxy bindings: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// UpsertProxyBinding creates the binding or points it at a new
// node. The caller decides the port: a new binding gets a fresh
// allocation; an existing binding keeps its recorded port.
func (s *Store) UpsertProxyBinding(ctx context.Context, b model.InstanceProxyBinding) error {
	_, err := s.q.ExecContext(ctx,
		`INSERT INTO instance_proxy_bindings (instance_id, node_id, listen_port) VALUES (?, ?, ?)
		 ON CONFLICT(instance_id) DO UPDATE SET node_id = excluded.node_id, updated_at = datetime('now')`,
		b.InstanceID, b.NodeID, b.ListenPort)
	if err != nil {
		return fmt.Errorf("store: upsert proxy binding: %w", err)
	}
	return nil
}

func (s *Store) DeleteProxyBinding(ctx context.Context, instanceID string) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM instance_proxy_bindings WHERE instance_id = ?`, instanceID)
	if err != nil {
		return fmt.Errorf("store: delete proxy binding: %w", err)
	}
	return nil
}

// UsedProxyPorts returns every listener port currently held by
// a binding.
func (s *Store) UsedProxyPorts(ctx context.Context) (map[int]bool, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT listen_port FROM instance_proxy_bindings`)
	if err != nil {
		return nil, fmt.Errorf("store: used proxy ports: %w", err)
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("store: used proxy ports: %w", err)
		}
		out[p] = true
	}
	return out, rows.Err()
}
