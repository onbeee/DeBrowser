package store

import (
	"context"
	"fmt"

	"github.com/onbeee/DeBrowser/internal/model"
)

// Custom group-member overlay (Stage 6C): user attachments of
// custom nodes to native proxy-groups. These repository methods
// only move overlay rows; whether a write is legitimate (config
// and group exist, node is source='custom', no duplicate) is
// validated by the service layer inside the same transaction.

// AddCustomGroupMember inserts one overlay row. The composite
// primary key rejects duplicates at the database level as well.
func (s *Store) AddCustomGroupMember(ctx context.Context, m model.CustomGroupMember) error {
	if _, err := s.q.ExecContext(ctx, `INSERT INTO mihomo_proxy_group_custom_members
		(config_id, group_name, proxy_id, sort_order)
		VALUES (?, ?, ?, ?)`,
		m.ConfigID, m.GroupName, m.ProxyID, m.SortOrder); err != nil {
		return fmt.Errorf("store: add custom group member: %w", err)
	}
	return nil
}

// RemoveCustomGroupMember deletes one overlay row; removing a
// row that does not exist is an error the service surfaces.
func (s *Store) RemoveCustomGroupMember(ctx context.Context, configID, groupName, proxyID string) error {
	res, err := s.q.ExecContext(ctx, `DELETE FROM mihomo_proxy_group_custom_members
		WHERE config_id = ? AND group_name = ? AND proxy_id = ?`,
		configID, groupName, proxyID)
	if err != nil {
		return fmt.Errorf("store: remove custom group member: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListCustomGroupMembers returns every overlay row of one config,
// ordered by group name and the user's member order.
func (s *Store) ListCustomGroupMembers(ctx context.Context, configID string) ([]model.CustomGroupMember, error) {
	return s.queryCustomMembers(ctx, `SELECT config_id, group_name, proxy_id, sort_order,
		created_at, updated_at FROM mihomo_proxy_group_custom_members
		WHERE config_id = ? ORDER BY group_name, sort_order, proxy_id`, configID)
}

// ListCustomMembershipsByProxy returns every overlay row that
// references one proxy, ordered by config and group name.
func (s *Store) ListCustomMembershipsByProxy(ctx context.Context, proxyID string) ([]model.CustomGroupMember, error) {
	return s.queryCustomMembers(ctx, `SELECT config_id, group_name, proxy_id, sort_order,
		created_at, updated_at FROM mihomo_proxy_group_custom_members
		WHERE proxy_id = ? ORDER BY config_id, group_name`, proxyID)
}

// HasCustomGroupMember reports whether the exact overlay row
// exists.
func (s *Store) HasCustomGroupMember(ctx context.Context, configID, groupName, proxyID string) (bool, error) {
	var n int
	err := s.q.QueryRowContext(ctx, `SELECT COUNT(1) FROM mihomo_proxy_group_custom_members
		WHERE config_id = ? AND group_name = ? AND proxy_id = ?`,
		configID, groupName, proxyID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: check custom group member: %w", err)
	}
	return n > 0, nil
}

// CountCustomGroupMembers counts the overlay rows of one group,
// used to assign the next sort_order.
func (s *Store) CountCustomGroupMembers(ctx context.Context, configID, groupName string) (int, error) {
	var n int
	err := s.q.QueryRowContext(ctx, `SELECT COUNT(1) FROM mihomo_proxy_group_custom_members
		WHERE config_id = ? AND group_name = ?`, configID, groupName).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count custom group members: %w", err)
	}
	return n, nil
}

func (s *Store) queryCustomMembers(ctx context.Context, query string, arg any) ([]model.CustomGroupMember, error) {
	rows, err := s.q.QueryContext(ctx, query, arg)
	if err != nil {
		return nil, fmt.Errorf("store: list custom group members: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []model.CustomGroupMember{}
	for rows.Next() {
		var m model.CustomGroupMember
		if err := rows.Scan(&m.ConfigID, &m.GroupName, &m.ProxyID, &m.SortOrder,
			&m.CreatedAt, &m.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan custom group member: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list custom group members: %w", err)
	}
	return out, nil
}
