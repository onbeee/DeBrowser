package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/onbeee/DeBrowser/internal/model"
)

const proxyColumns = `id, name, protocol, address, port, config_json, secret_ref, enabled, created_at, updated_at`

func scanProxy(row interface{ Scan(dest ...any) error }) (model.Proxy, error) {
	var (
		p      model.Proxy
		port   sql.NullInt64
		enable int
	)
	if err := row.Scan(&p.ID, &p.Name, &p.Protocol, &p.Address, &port,
		&p.ConfigJSON, &p.SecretRef, &enable, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return model.Proxy{}, err
	}
	if port.Valid {
		v := int(port.Int64)
		p.Port = &v
	}
	p.Enabled = enable != 0
	return p, nil
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
	enabled := 0
	if p.Enabled {
		enabled = 1
	}
	if _, err := s.q.ExecContext(ctx, `INSERT INTO proxies
		(id, name, protocol, address, port, config_json, secret_ref, enabled)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Name, p.Protocol, p.Address, nullInt(p.Port),
		p.ConfigJSON, p.SecretRef, enabled); err != nil {
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

// ListProxies returns all proxies ordered by name.
func (s *Store) ListProxies(ctx context.Context) ([]model.Proxy, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT `+proxyColumns+` FROM proxies ORDER BY name, id`)
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
// its updated_at timestamp.
func (s *Store) UpdateProxy(ctx context.Context, p model.Proxy) (model.Proxy, error) {
	enabled := 0
	if p.Enabled {
		enabled = 1
	}
	res, err := s.q.ExecContext(ctx, `UPDATE proxies SET
		name = ?, protocol = ?, address = ?, port = ?, config_json = ?,
		secret_ref = ?, enabled = ?, updated_at = datetime('now')
		WHERE id = ?`,
		p.Name, p.Protocol, p.Address, nullInt(p.Port), p.ConfigJSON,
		p.SecretRef, enabled, p.ID)
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

// DeleteProxy removes a proxy. Instances that referenced it keep
// existing with proxy_id set to NULL (schema ON DELETE SET NULL).
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
