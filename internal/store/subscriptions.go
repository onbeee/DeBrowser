package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/onbeee/DeBrowser/internal/model"
)

const subscriptionColumns = `id, name, url_hint, secret_ref,
	allow_lan, auto_update_minutes, last_updated_at, last_status, last_error,
	created_at, updated_at`

func scanSubscription(row interface{ Scan(dest ...any) error }) (model.Subscription, error) {
	var (
		sub           model.Subscription
		allowLAN      int
		lastUpdatedAt sql.NullString
	)
	if err := row.Scan(&sub.ID, &sub.Name, &sub.URLHint, &sub.SecretRef,
		&allowLAN, &sub.AutoUpdateMinutes, &lastUpdatedAt, &sub.LastStatus,
		&sub.LastError, &sub.CreatedAt, &sub.UpdatedAt); err != nil {
		return model.Subscription{}, err
	}
	sub.AllowLAN = allowLAN != 0
	if lastUpdatedAt.Valid {
		sub.LastUpdatedAt = &lastUpdatedAt.String
	}
	return sub, nil
}

// CreateSubscription inserts a new subscription record, generating
// its ID when empty. The full URL is never stored here (see the
// schema comment in migration 0004); SecretRef points into the
// credential vault.
func (s *Store) CreateSubscription(ctx context.Context, sub model.Subscription) (model.Subscription, error) {
	if sub.ID == "" {
		id, err := newID()
		if err != nil {
			return model.Subscription{}, err
		}
		sub.ID = id
	}
	if _, err := s.q.ExecContext(ctx, `INSERT INTO subscriptions
		(id, name, url_hint, secret_ref, allow_lan,
		 auto_update_minutes, last_updated_at, last_status, last_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sub.ID, sub.Name, sub.URLHint, sub.SecretRef,
		boolInt(sub.AllowLAN), sub.AutoUpdateMinutes, nullStr(sub.LastUpdatedAt),
		sub.LastStatus, sub.LastError); err != nil {
		return model.Subscription{}, fmt.Errorf("store: create subscription: %w", err)
	}
	return s.GetSubscription(ctx, sub.ID)
}

// GetSubscription returns one subscription by ID, or ErrNotFound.
func (s *Store) GetSubscription(ctx context.Context, id string) (model.Subscription, error) {
	sub, err := scanSubscription(s.q.QueryRowContext(ctx,
		`SELECT `+subscriptionColumns+` FROM subscriptions WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Subscription{}, ErrNotFound
	}
	if err != nil {
		return model.Subscription{}, fmt.Errorf("store: get subscription: %w", err)
	}
	return sub, nil
}

// ListSubscriptions returns all subscriptions ordered by name.
func (s *Store) ListSubscriptions(ctx context.Context) ([]model.Subscription, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT `+subscriptionColumns+` FROM subscriptions ORDER BY name, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list subscriptions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.Subscription
	for rows.Next() {
		sub, err := scanSubscription(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list subscriptions: %w", err)
		}
		out = append(out, sub)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list subscriptions: %w", err)
	}
	return out, nil
}

// UpdateSubscription replaces a subscription's editable fields.
func (s *Store) UpdateSubscription(ctx context.Context, sub model.Subscription) (model.Subscription, error) {
	res, err := s.q.ExecContext(ctx, `UPDATE subscriptions SET
		name = ?, url_hint = ?, secret_ref = ?,
		allow_lan = ?, auto_update_minutes = ?, last_updated_at = ?,
		last_status = ?, last_error = ?, updated_at = datetime('now')
		WHERE id = ?`,
		sub.Name, sub.URLHint, sub.SecretRef,
		boolInt(sub.AllowLAN), sub.AutoUpdateMinutes, nullStr(sub.LastUpdatedAt),
		sub.LastStatus, sub.LastError, sub.ID)
	if err != nil {
		return model.Subscription{}, fmt.Errorf("store: update subscription: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return model.Subscription{}, fmt.Errorf("store: update subscription: %w", err)
	} else if n == 0 {
		return model.Subscription{}, ErrNotFound
	}
	return s.GetSubscription(ctx, sub.ID)
}

// DeleteSubscription removes a subscription. Proxies imported from
// it are removed with it (schema ON DELETE CASCADE on
// proxies.subscription_id).
func (s *Store) DeleteSubscription(ctx context.Context, id string) error {
	res, err := s.q.ExecContext(ctx, `DELETE FROM subscriptions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete subscription: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: delete subscription: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}
