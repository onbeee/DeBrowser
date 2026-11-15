package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/onbeee/DeBrowser/internal/model"
)

// External browser repository (Stage 9A). These rows are PURE
// PATH REGISTRATIONS of programs the user added by hand: the
// store records only the chosen path and a display name, and it
// never touches the registered files themselves. No identity,
// version, signature or publisher information exists here,
// because DeBrowser performs no such checks on user-chosen
// executables.

// CreateExternalBrowser inserts one manual browser registration.
func (s *Store) CreateExternalBrowser(ctx context.Context, b model.ExternalBrowser) (model.ExternalBrowser, error) {
	if b.ID == "" {
		id, err := newID()
		if err != nil {
			return model.ExternalBrowser{}, err
		}
		b.ID = id
	}
	_, err := s.q.ExecContext(ctx,
		`INSERT INTO external_browsers (id, name, executable_path) VALUES (?, ?, ?)`,
		b.ID, b.Name, b.ExecutablePath)
	if err != nil {
		return model.ExternalBrowser{}, fmt.Errorf("store: create external browser: %w", err)
	}
	return s.GetExternalBrowser(ctx, b.ID)
}

// GetExternalBrowser returns one registration by ID.
func (s *Store) GetExternalBrowser(ctx context.Context, id string) (model.ExternalBrowser, error) {
	row := s.q.QueryRowContext(ctx,
		`SELECT id, name, executable_path, created_at, updated_at
		 FROM external_browsers WHERE id = ?`, id)
	return scanExternalBrowser(row)
}

// GetExternalBrowserByPath returns the registration for one
// executable path, or ErrNotFound.
func (s *Store) GetExternalBrowserByPath(ctx context.Context, path string) (model.ExternalBrowser, error) {
	row := s.q.QueryRowContext(ctx,
		`SELECT id, name, executable_path, created_at, updated_at
		 FROM external_browsers WHERE executable_path = ?`, path)
	return scanExternalBrowser(row)
}

// ListExternalBrowsers returns every manual registration, in the
// order the user added them.
func (s *Store) ListExternalBrowsers(ctx context.Context) ([]model.ExternalBrowser, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT id, name, executable_path, created_at, updated_at
		 FROM external_browsers ORDER BY created_at, name`)
	if err != nil {
		return nil, fmt.Errorf("store: list external browsers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []model.ExternalBrowser{}
	for rows.Next() {
		b, err := scanExternalBrowser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list external browsers: %w", err)
	}
	return out, nil
}

// UpdateExternalBrowserPath records a re-selected path (and the
// display name derived from it) for one registration.
func (s *Store) UpdateExternalBrowserPath(ctx context.Context, id, name, path string) error {
	res, err := s.q.ExecContext(ctx,
		`UPDATE external_browsers
		 SET name = ?, executable_path = ?, updated_at = datetime('now')
		 WHERE id = ?`,
		name, path, id)
	if err != nil {
		return fmt.Errorf("store: update external browser: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: update external browser: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteExternalBrowser removes one registration. It deletes only
// the database row — never the file it pointed at.
func (s *Store) DeleteExternalBrowser(ctx context.Context, id string) error {
	res, err := s.q.ExecContext(ctx, `DELETE FROM external_browsers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete external browser: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: delete external browser: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

type externalBrowserScanner interface {
	Scan(dest ...any) error
}

func scanExternalBrowser(sc externalBrowserScanner) (model.ExternalBrowser, error) {
	var b model.ExternalBrowser
	err := sc.Scan(&b.ID, &b.Name, &b.ExecutablePath, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ExternalBrowser{}, ErrNotFound
	}
	if err != nil {
		return model.ExternalBrowser{}, fmt.Errorf("store: scan external browser: %w", err)
	}
	return b, nil
}
