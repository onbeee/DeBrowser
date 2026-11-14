package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/onbeee/DeBrowser/internal/model"
)

// Repository for Stage 6 Mihomo config imports: config documents,
// their native proxy-groups and group members, plus the proxy
// lookups the import pipeline needs. All methods work on both the
// root Store and a transaction-bound Store (WithTx).

const mihomoConfigColumns = `id, name, source_type, subscription_id, file_path, sha256,
	secret_ref, format_version, security_report_json, created_at, updated_at`

func scanMihomoConfig(row interface{ Scan(dest ...any) error }) (model.MihomoConfig, error) {
	var (
		c     model.MihomoConfig
		subID sql.NullString
	)
	if err := row.Scan(&c.ID, &c.Name, &c.SourceType, &subID, &c.FilePath, &c.SHA256,
		&c.SecretRef, &c.FormatVersion, &c.SecurityReportJSON, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return model.MihomoConfig{}, err
	}
	if subID.Valid {
		c.SubscriptionID = &subID.String
	}
	return c, nil
}

// CreateMihomoConfig inserts a config row, generating its ID when
// empty.
func (s *Store) CreateMihomoConfig(ctx context.Context, c model.MihomoConfig) (model.MihomoConfig, error) {
	if c.ID == "" {
		id, err := newID()
		if err != nil {
			return model.MihomoConfig{}, err
		}
		c.ID = id
	}
	if c.FormatVersion == 0 {
		c.FormatVersion = 1
	}
	if c.SecurityReportJSON == "" {
		c.SecurityReportJSON = "[]"
	}
	if _, err := s.q.ExecContext(ctx, `INSERT INTO mihomo_configs
		(id, name, source_type, subscription_id, file_path, sha256,
		 secret_ref, format_version, security_report_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.Name, c.SourceType, nullStr(c.SubscriptionID), c.FilePath, c.SHA256,
		c.SecretRef, c.FormatVersion, c.SecurityReportJSON); err != nil {
		return model.MihomoConfig{}, fmt.Errorf("store: create mihomo config: %w", err)
	}
	return s.GetMihomoConfig(ctx, c.ID)
}

// GetMihomoConfig returns one config by ID, or ErrNotFound.
func (s *Store) GetMihomoConfig(ctx context.Context, id string) (model.MihomoConfig, error) {
	c, err := scanMihomoConfig(s.q.QueryRowContext(ctx,
		`SELECT `+mihomoConfigColumns+` FROM mihomo_configs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.MihomoConfig{}, ErrNotFound
	}
	if err != nil {
		return model.MihomoConfig{}, fmt.Errorf("store: get mihomo config: %w", err)
	}
	return c, nil
}

// GetMihomoConfigBySubscription returns the config owned by a
// subscription, or ErrNotFound.
func (s *Store) GetMihomoConfigBySubscription(ctx context.Context, subscriptionID string) (model.MihomoConfig, error) {
	c, err := scanMihomoConfig(s.q.QueryRowContext(ctx,
		`SELECT `+mihomoConfigColumns+` FROM mihomo_configs WHERE subscription_id = ?`, subscriptionID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.MihomoConfig{}, ErrNotFound
	}
	if err != nil {
		return model.MihomoConfig{}, fmt.Errorf("store: get mihomo config by subscription: %w", err)
	}
	return c, nil
}

// ListMihomoConfigs returns all configs, newest first.
func (s *Store) ListMihomoConfigs(ctx context.Context) ([]model.MihomoConfig, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT `+mihomoConfigColumns+` FROM mihomo_configs ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list mihomo configs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.MihomoConfig
	for rows.Next() {
		c, err := scanMihomoConfig(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list mihomo configs: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list mihomo configs: %w", err)
	}
	return out, nil
}

// UpdateMihomoConfig replaces the mutable fields of a config (the
// artifact pointer, hash, bundle reference and security report)
// and refreshes updated_at. Identity fields (source, subscription)
// never change.
func (s *Store) UpdateMihomoConfig(ctx context.Context, c model.MihomoConfig) (model.MihomoConfig, error) {
	res, err := s.q.ExecContext(ctx, `UPDATE mihomo_configs SET
		name = ?, file_path = ?, sha256 = ?, secret_ref = ?,
		format_version = ?, security_report_json = ?,
		updated_at = datetime('now')
		WHERE id = ?`,
		c.Name, c.FilePath, c.SHA256, c.SecretRef,
		c.FormatVersion, c.SecurityReportJSON, c.ID)
	if err != nil {
		return model.MihomoConfig{}, fmt.Errorf("store: update mihomo config: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return model.MihomoConfig{}, fmt.Errorf("store: update mihomo config: %w", err)
	} else if n == 0 {
		return model.MihomoConfig{}, ErrNotFound
	}
	return s.GetMihomoConfig(ctx, c.ID)
}

// DeleteMihomoConfig removes a config; groups and members cascade,
// proxies keep their rows with config_id set NULL by the FK.
func (s *Store) DeleteMihomoConfig(ctx context.Context, id string) error {
	res, err := s.q.ExecContext(ctx, `DELETE FROM mihomo_configs WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete mihomo config: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: delete mihomo config: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ReplaceProxyGroups atomically replaces all native proxy-groups
// of a config. Groups are inserted first (with caller-provided
// IDs), then members, so member rows can reference sibling groups.
// The caller resolves member names to IDs before calling.
func (s *Store) ReplaceProxyGroups(ctx context.Context, configID string, groups []model.MihomoProxyGroup) error {
	if _, err := s.q.ExecContext(ctx, `DELETE FROM mihomo_proxy_groups WHERE config_id = ?`, configID); err != nil {
		return fmt.Errorf("store: clear proxy groups: %w", err)
	}
	// Two phases: insert ALL group rows first, then all member
	// rows. A member may reference a group that appears LATER in
	// the document (real-world configs list their main select
	// group first and the auto groups it references afterwards);
	// inserting members group-by-group would violate the
	// target_group_id foreign key for such forward references.
	for _, g := range groups {
		if g.OptionsJSON == "" {
			g.OptionsJSON = "{}"
		}
		if _, err := s.q.ExecContext(ctx, `INSERT INTO mihomo_proxy_groups
			(id, config_id, name, type, options_json, sort_order)
			VALUES (?, ?, ?, ?, ?, ?)`,
			g.ID, configID, g.Name, g.Type, g.OptionsJSON, g.SortOrder); err != nil {
			return fmt.Errorf("store: create proxy group: %w", err)
		}
	}
	for _, g := range groups {
		for _, m := range g.Members {
			if _, err := s.q.ExecContext(ctx, `INSERT INTO mihomo_proxy_group_members
				(group_id, sort_order, member_name, member_kind, proxy_id, target_group_id)
				VALUES (?, ?, ?, ?, ?, ?)`,
				g.ID, m.SortOrder, m.MemberName, m.Kind,
				nullStr(m.ProxyID), nullStr(m.TargetGroupID)); err != nil {
				return fmt.Errorf("store: create proxy group member: %w", err)
			}
		}
	}
	return nil
}

// ListProxyGroups returns the config's groups in document order,
// each with its members in member order.
func (s *Store) ListProxyGroups(ctx context.Context, configID string) ([]model.MihomoProxyGroup, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT id, config_id, name, type, options_json,
		sort_order, created_at, updated_at FROM mihomo_proxy_groups
		WHERE config_id = ? ORDER BY sort_order, name`, configID)
	if err != nil {
		return nil, fmt.Errorf("store: list proxy groups: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var groups []model.MihomoProxyGroup
	byID := map[string]*model.MihomoProxyGroup{}
	for rows.Next() {
		var g model.MihomoProxyGroup
		if err := rows.Scan(&g.ID, &g.ConfigID, &g.Name, &g.Type, &g.OptionsJSON,
			&g.SortOrder, &g.CreatedAt, &g.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: list proxy groups: %w", err)
		}
		groups = append(groups, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list proxy groups: %w", err)
	}
	for i := range groups {
		byID[groups[i].ID] = &groups[i]
	}
	mrows, err := s.q.QueryContext(ctx, `SELECT m.group_id, m.sort_order, m.member_name,
		m.member_kind, m.proxy_id, m.target_group_id
		FROM mihomo_proxy_group_members m
		JOIN mihomo_proxy_groups g ON g.id = m.group_id
		WHERE g.config_id = ? ORDER BY m.group_id, m.sort_order`, configID)
	if err != nil {
		return nil, fmt.Errorf("store: list proxy group members: %w", err)
	}
	defer func() { _ = mrows.Close() }()
	for mrows.Next() {
		var (
			m       model.MihomoProxyGroupMember
			proxyID sql.NullString
			target  sql.NullString
		)
		if err := mrows.Scan(&m.GroupID, &m.SortOrder, &m.MemberName, &m.Kind, &proxyID, &target); err != nil {
			return nil, fmt.Errorf("store: list proxy group members: %w", err)
		}
		if proxyID.Valid {
			m.ProxyID = &proxyID.String
		}
		if target.Valid {
			m.TargetGroupID = &target.String
		}
		if g := byID[m.GroupID]; g != nil {
			g.Members = append(g.Members, m)
		}
	}
	if err := mrows.Err(); err != nil {
		return nil, fmt.Errorf("store: list proxy group members: %w", err)
	}
	return groups, nil
}

// CountProxyGroups returns how many native groups a config has.
func (s *Store) CountProxyGroups(ctx context.Context, configID string) (int, error) {
	var n int
	if err := s.q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mihomo_proxy_groups WHERE config_id = ?`, configID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count proxy groups: %w", err)
	}
	return n, nil
}

// ListProxiesBySubscription returns the proxies owned by a
// subscription, in display order.
func (s *Store) ListProxiesBySubscription(ctx context.Context, subscriptionID string) ([]model.Proxy, error) {
	return s.listProxiesWhere(ctx, `subscription_id = ?`, subscriptionID)
}

// ListProxiesByConfig returns the proxies that came from one
// imported config, in display order.
func (s *Store) ListProxiesByConfig(ctx context.Context, configID string) ([]model.Proxy, error) {
	return s.listProxiesWhere(ctx, `config_id = ?`, configID)
}

func (s *Store) listProxiesWhere(ctx context.Context, where string, arg any) ([]model.Proxy, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT `+proxyColumns+` FROM proxies WHERE `+where+` ORDER BY sort_order, name, id`, arg)
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

// GetProxyByDedupKey returns the proxy with the given non-empty
// dedup fingerprint, or ErrNotFound.
func (s *Store) GetProxyByDedupKey(ctx context.Context, dedupKey string) (model.Proxy, error) {
	p, err := scanProxy(s.q.QueryRowContext(ctx,
		`SELECT `+proxyColumns+` FROM proxies WHERE dedup_key = ? AND dedup_key <> ''`, dedupKey))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Proxy{}, ErrNotFound
	}
	if err != nil {
		return model.Proxy{}, fmt.Errorf("store: get proxy by dedup key: %w", err)
	}
	return p, nil
}
