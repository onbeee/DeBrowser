package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/onbeee/DeBrowser/internal/model"
)

const proxyGroupColumns = `id, name, sort_order, created_at, updated_at`

func scanProxyGroup(row interface{ Scan(dest ...any) error }) (model.ProxyGroup, error) {
	var g model.ProxyGroup
	if err := row.Scan(&g.ID, &g.Name, &g.SortOrder, &g.CreatedAt, &g.UpdatedAt); err != nil {
		return model.ProxyGroup{}, err
	}
	return g, nil
}

// CreateProxyGroup inserts a new proxy group, generating its ID
// when empty. Group names are unique (schema constraint).
func (s *Store) CreateProxyGroup(ctx context.Context, g model.ProxyGroup) (model.ProxyGroup, error) {
	if g.ID == "" {
		id, err := newID()
		if err != nil {
			return model.ProxyGroup{}, err
		}
		g.ID = id
	}
	if _, err := s.q.ExecContext(ctx, `INSERT INTO proxy_groups
		(id, name, sort_order) VALUES (?, ?, ?)`,
		g.ID, g.Name, g.SortOrder); err != nil {
		return model.ProxyGroup{}, fmt.Errorf("store: create proxy group: %w", err)
	}
	return s.GetProxyGroup(ctx, g.ID)
}

// GetProxyGroup returns one group by ID, or ErrNotFound.
func (s *Store) GetProxyGroup(ctx context.Context, id string) (model.ProxyGroup, error) {
	g, err := scanProxyGroup(s.q.QueryRowContext(ctx,
		`SELECT `+proxyGroupColumns+` FROM proxy_groups WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.ProxyGroup{}, ErrNotFound
	}
	if err != nil {
		return model.ProxyGroup{}, fmt.Errorf("store: get proxy group: %w", err)
	}
	return g, nil
}

// ListProxyGroups returns all groups in display order.
func (s *Store) ListProxyGroups(ctx context.Context) ([]model.ProxyGroup, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT `+proxyGroupColumns+` FROM proxy_groups ORDER BY sort_order, name, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list proxy groups: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.ProxyGroup
	for rows.Next() {
		g, err := scanProxyGroup(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list proxy groups: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list proxy groups: %w", err)
	}
	return out, nil
}

// UpdateProxyGroup replaces a group's editable fields.
func (s *Store) UpdateProxyGroup(ctx context.Context, g model.ProxyGroup) (model.ProxyGroup, error) {
	res, err := s.q.ExecContext(ctx, `UPDATE proxy_groups SET
		name = ?, sort_order = ?, updated_at = datetime('now')
		WHERE id = ?`, g.Name, g.SortOrder, g.ID)
	if err != nil {
		return model.ProxyGroup{}, fmt.Errorf("store: update proxy group: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return model.ProxyGroup{}, fmt.Errorf("store: update proxy group: %w", err)
	} else if n == 0 {
		return model.ProxyGroup{}, ErrNotFound
	}
	return s.GetProxyGroup(ctx, g.ID)
}

// DeleteProxyGroup removes a group. Its proxies and subscriptions
// keep existing with group_id set to NULL (schema ON DELETE SET
// NULL).
func (s *Store) DeleteProxyGroup(ctx context.Context, id string) error {
	res, err := s.q.ExecContext(ctx, `DELETE FROM proxy_groups WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete proxy group: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: delete proxy group: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ProxyGroupCounts returns how many proxies belong to each group
// (groups with no proxies are absent from the map).
func (s *Store) ProxyGroupCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT group_id, COUNT(1) FROM proxies WHERE group_id IS NOT NULL GROUP BY group_id`)
	if err != nil {
		return nil, fmt.Errorf("store: proxy group counts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("store: proxy group counts: %w", err)
		}
		out[id] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: proxy group counts: %w", err)
	}
	return out, nil
}
