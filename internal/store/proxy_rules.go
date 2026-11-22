package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/onbeee/DeBrowser/internal/model"
)

// This file is the repository for the editable Stage 6C proxy
// rules and settings (migration 0017): proxy_rule_policies
// (per-ruleset policy overrides), proxy_custom_rules (the user's
// top rules) and proxy_settings (key/value core settings).

// --- Built-in ruleset policy overrides ---

// ListRulePolicies returns every stored policy override. Rulesets
// without a row keep their built-in default policy — the absence
// of a row IS the default state.
func (s *Store) ListRulePolicies(ctx context.Context) ([]model.ProxyRulePolicy, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT provider_name, policy, updated_at FROM proxy_rule_policies ORDER BY provider_name`)
	if err != nil {
		return nil, fmt.Errorf("store: list rule policies: %w", err)
	}
	defer rows.Close()
	var out []model.ProxyRulePolicy
	for rows.Next() {
		var p model.ProxyRulePolicy
		if err := rows.Scan(&p.ProviderName, &p.Policy, &p.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: list rule policies: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpsertRulePolicy stores one ruleset's policy override.
func (s *Store) UpsertRulePolicy(ctx context.Context, providerName, policy string) error {
	if _, err := s.q.ExecContext(ctx,
		`INSERT INTO proxy_rule_policies (provider_name, policy)
		 VALUES (?, ?)
		 ON CONFLICT (provider_name) DO UPDATE SET
		   policy = excluded.policy,
		   updated_at = datetime('now')`,
		providerName, policy); err != nil {
		return fmt.Errorf("store: upsert rule policy: %w", err)
	}
	return nil
}

// DeleteAllRulePolicies removes every override ("restore
// defaults").
func (s *Store) DeleteAllRulePolicies(ctx context.Context) error {
	if _, err := s.q.ExecContext(ctx, `DELETE FROM proxy_rule_policies`); err != nil {
		return fmt.Errorf("store: delete rule policies: %w", err)
	}
	return nil
}

// --- Custom top rules ---

const customRuleColumns = `id, rule_type, value, policy, enabled, sort_order, created_at, updated_at`

func scanCustomRule(row interface{ Scan(...any) error }) (model.ProxyCustomRule, error) {
	var r model.ProxyCustomRule
	var enabled int
	err := row.Scan(&r.ID, &r.RuleType, &r.Value, &r.Policy, &enabled,
		&r.SortOrder, &r.CreatedAt, &r.UpdatedAt)
	r.Enabled = enabled != 0
	return r, err
}

// ListCustomRules returns all custom rules in evaluation order.
func (s *Store) ListCustomRules(ctx context.Context) ([]model.ProxyCustomRule, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT `+customRuleColumns+` FROM proxy_custom_rules ORDER BY sort_order, created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list custom rules: %w", err)
	}
	defer rows.Close()
	var out []model.ProxyCustomRule
	for rows.Next() {
		r, err := scanCustomRule(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list custom rules: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetCustomRule returns one custom rule (ErrNotFound when absent).
func (s *Store) GetCustomRule(ctx context.Context, id string) (model.ProxyCustomRule, error) {
	r, err := scanCustomRule(s.q.QueryRowContext(ctx,
		`SELECT `+customRuleColumns+` FROM proxy_custom_rules WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.ProxyCustomRule{}, ErrNotFound
	}
	if err != nil {
		return model.ProxyCustomRule{}, fmt.Errorf("store: get custom rule: %w", err)
	}
	return r, nil
}

// CreateCustomRule inserts one custom rule.
func (s *Store) CreateCustomRule(ctx context.Context, r model.ProxyCustomRule) (model.ProxyCustomRule, error) {
	enabled := 0
	if r.Enabled {
		enabled = 1
	}
	if _, err := s.q.ExecContext(ctx,
		`INSERT INTO proxy_custom_rules (id, rule_type, value, policy, enabled, sort_order)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		r.ID, r.RuleType, r.Value, r.Policy, enabled, r.SortOrder); err != nil {
		return model.ProxyCustomRule{}, fmt.Errorf("store: create custom rule: %w", err)
	}
	return s.GetCustomRule(ctx, r.ID)
}

// UpdateCustomRule rewrites one custom rule's editable fields
// (ErrNotFound when absent). Sort order is NOT changed here —
// use SetCustomRuleOrder.
func (s *Store) UpdateCustomRule(ctx context.Context, r model.ProxyCustomRule) error {
	enabled := 0
	if r.Enabled {
		enabled = 1
	}
	res, err := s.q.ExecContext(ctx,
		`UPDATE proxy_custom_rules
		 SET rule_type = ?, value = ?, policy = ?, enabled = ?,
		     updated_at = datetime('now')
		 WHERE id = ?`,
		r.RuleType, r.Value, r.Policy, enabled, r.ID)
	if err != nil {
		return fmt.Errorf("store: update custom rule: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: update custom rule: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteCustomRule removes one custom rule (ErrNotFound when
// absent).
func (s *Store) DeleteCustomRule(ctx context.Context, id string) error {
	res, err := s.q.ExecContext(ctx,
		`DELETE FROM proxy_custom_rules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete custom rule: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete custom rule: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetCustomRuleOrder rewrites sort_order from the given ID
// sequence (position = order). The caller validates that the
// sequence covers exactly the stored rules; unknown IDs are
// rejected here as well so a partial rewrite can never happen
// silently.
func (s *Store) SetCustomRuleOrder(ctx context.Context, ids []string) error {
	return s.WithTx(ctx, func(tx *Store) error {
		for i, id := range ids {
			res, err := tx.q.ExecContext(ctx,
				`UPDATE proxy_custom_rules
				 SET sort_order = ?, updated_at = datetime('now')
				 WHERE id = ?`, i, id)
			if err != nil {
				return fmt.Errorf("store: reorder custom rules: %w", err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("store: reorder custom rules: %w", err)
			}
			if n == 0 {
				return ErrNotFound
			}
		}
		return nil
	})
}

// --- Core settings (key/value) ---

// ProxySettingsEntries returns the raw stored settings map.
// Missing keys are simply absent — assembling defaults is the
// caller's job.
func (s *Store) ProxySettingsEntries(ctx context.Context) (map[string]string, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT key, value FROM proxy_settings`)
	if err != nil {
		return nil, fmt.Errorf("store: read proxy settings: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("store: read proxy settings: %w", err)
		}
		out[k] = v
	}
	return out, rows.Err()
}

// SetProxySettingsEntries writes the given entries atomically.
func (s *Store) SetProxySettingsEntries(ctx context.Context, entries map[string]string) error {
	return s.WithTx(ctx, func(tx *Store) error {
		for k, v := range entries {
			if _, err := tx.q.ExecContext(ctx,
				`INSERT INTO proxy_settings (key, value) VALUES (?, ?)
				 ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
				k, v); err != nil {
				return fmt.Errorf("store: write proxy settings: %w", err)
			}
		}
		return nil
	})
}
