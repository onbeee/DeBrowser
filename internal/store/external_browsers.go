package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/onbeee/DeBrowser/internal/model"
)

// External browser repository (Stage 9A, revised). These rows are
// registrations of browsers the user added by hand after full
// verification; the store never touches the browser files
// themselves.

// CreateExternalBrowser inserts one manual browser registration.
func (s *Store) CreateExternalBrowser(ctx context.Context, b model.ExternalBrowser) (model.ExternalBrowser, error) {
	if b.ID == "" {
		id, err := newID()
		if err != nil {
			return model.ExternalBrowser{}, err
		}
		b.ID = id
	}
	if b.Status == "" {
		b.Status = "verified"
	}
	_, err := s.q.ExecContext(ctx,
		`INSERT INTO external_browsers
		   (id, name, browser_type, version, executable_path, signature_status, signature_publisher, status, last_verified_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.ID, b.Name, b.BrowserType, b.Version, b.ExecutablePath,
		b.SignatureStatus, b.SignaturePublisher, b.Status, b.LastVerifiedAt)
	if err != nil {
		return model.ExternalBrowser{}, fmt.Errorf("store: create external browser: %w", err)
	}
	return s.GetExternalBrowser(ctx, b.ID)
}

// GetExternalBrowser returns one registration by ID.
func (s *Store) GetExternalBrowser(ctx context.Context, id string) (model.ExternalBrowser, error) {
	row := s.q.QueryRowContext(ctx,
		`SELECT id, name, browser_type, version, executable_path, signature_status, signature_publisher, status, created_at, updated_at, last_verified_at
		 FROM external_browsers WHERE id = ?`, id)
	return scanExternalBrowser(row)
}

// GetExternalBrowserByPath returns the registration for one
// executable path, or ErrNotFound.
func (s *Store) GetExternalBrowserByPath(ctx context.Context, path string) (model.ExternalBrowser, error) {
	row := s.q.QueryRowContext(ctx,
		`SELECT id, name, browser_type, version, executable_path, signature_status, signature_publisher, status, created_at, updated_at, last_verified_at
		 FROM external_browsers WHERE executable_path = ?`, path)
	return scanExternalBrowser(row)
}

// ListExternalBrowsers returns every manual registration, in the
// order the user added them.
func (s *Store) ListExternalBrowsers(ctx context.Context) ([]model.ExternalBrowser, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT id, name, browser_type, version, executable_path, signature_status, signature_publisher, status, created_at, updated_at, last_verified_at
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

// UpdateExternalBrowserVerification records the outcome of a
// re-verification pass: fresh identity fields, status and the
// verification time.
func (s *Store) UpdateExternalBrowserVerification(ctx context.Context, b model.ExternalBrowser) error {
	res, err := s.q.ExecContext(ctx,
		`UPDATE external_browsers
		 SET name = ?, version = ?, signature_status = ?, signature_publisher = ?,
		     status = ?, last_verified_at = ?, updated_at = datetime('now')
		 WHERE id = ?`,
		b.Name, b.Version, b.SignatureStatus, b.SignaturePublisher, b.Status, b.LastVerifiedAt, b.ID)
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
// the database row — never the browser file it pointed at.
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
	err := sc.Scan(&b.ID, &b.Name, &b.BrowserType, &b.Version, &b.ExecutablePath,
		&b.SignatureStatus, &b.SignaturePublisher, &b.Status,
		&b.CreatedAt, &b.UpdatedAt, &b.LastVerifiedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ExternalBrowser{}, ErrNotFound
	}
	if err != nil {
		return model.ExternalBrowser{}, fmt.Errorf("store: scan external browser: %w", err)
	}
	return b, nil
}
