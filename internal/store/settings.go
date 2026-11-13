package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// GetSetting reads one app_settings key/value pair. The app_settings
// table (migration 0001) is the database-level KV store; user-facing
// preferences live in config/settings.json instead and do not use
// this. Missing keys return ErrNotFound.
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var value string
	err := s.q.QueryRowContext(ctx,
		`SELECT value FROM app_settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: get setting: %w", err)
	}
	return value, nil
}

// SetSetting writes one app_settings pair, creating or replacing it.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	if _, err := s.q.ExecContext(ctx, `INSERT INTO app_settings (key, value, updated_at)
		VALUES (?, ?, datetime('now'))
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = datetime('now')`,
		key, value); err != nil {
		return fmt.Errorf("store: set setting: %w", err)
	}
	return nil
}

// DeleteSetting removes one app_settings pair. Removing a missing
// key is not an error.
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	if _, err := s.q.ExecContext(ctx, `DELETE FROM app_settings WHERE key = ?`, key); err != nil {
		return fmt.Errorf("store: delete setting: %w", err)
	}
	return nil
}
