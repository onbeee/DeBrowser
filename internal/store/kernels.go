package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/onbeee/DeBrowser/internal/model"
)

// Kernel repository (Stage 9A). Only DeBrowser-managed standalone
// Chromium builds are stored; system browsers are detected live and
// never persisted (see migration 0010). Paths in these rows are
// relative to the portable application root.

// CreateKernel inserts one managed kernel. The (version, platform)
// unique index rejects a duplicate registration.
func (s *Store) CreateKernel(ctx context.Context, k model.Kernel) (model.Kernel, error) {
	if k.ID == "" {
		id, err := newID()
		if err != nil {
			return model.Kernel{}, err
		}
		k.ID = id
	}
	if k.Channel == "" {
		k.Channel = "stable"
	}
	if k.Platform == "" {
		k.Platform = "win64"
	}
	if k.Status == "" {
		k.Status = "ready"
	}
	_, err := s.q.ExecContext(ctx,
		`INSERT INTO kernels (id, version, channel, platform, install_path, executable_path, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.Version, k.Channel, k.Platform, k.InstallPath, k.ExecutablePath, k.Status)
	if err != nil {
		return model.Kernel{}, fmt.Errorf("store: create kernel: %w", err)
	}
	return s.GetKernel(ctx, k.ID)
}

// GetKernel returns one managed kernel by ID.
func (s *Store) GetKernel(ctx context.Context, id string) (model.Kernel, error) {
	row := s.q.QueryRowContext(ctx,
		`SELECT id, version, channel, platform, install_path, executable_path, status, created_at, updated_at
		 FROM kernels WHERE id = ?`, id)
	return scanKernel(row)
}

// GetKernelByVersion returns the managed kernel registered for one
// version and platform, or ErrNotFound.
func (s *Store) GetKernelByVersion(ctx context.Context, version, platform string) (model.Kernel, error) {
	row := s.q.QueryRowContext(ctx,
		`SELECT id, version, channel, platform, install_path, executable_path, status, created_at, updated_at
		 FROM kernels WHERE version = ? AND platform = ?`, version, platform)
	return scanKernel(row)
}

// ListKernels returns every managed kernel, newest version first.
func (s *Store) ListKernels(ctx context.Context) ([]model.Kernel, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT id, version, channel, platform, install_path, executable_path, status, created_at, updated_at
		 FROM kernels ORDER BY created_at DESC, version DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: list kernels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []model.Kernel{}
	for rows.Next() {
		k, err := scanKernel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list kernels: %w", err)
	}
	return out, nil
}

// UpdateKernelStatus records the outcome of a verification pass.
func (s *Store) UpdateKernelStatus(ctx context.Context, id, status string) error {
	res, err := s.q.ExecContext(ctx,
		`UPDATE kernels SET status = ?, updated_at = datetime('now') WHERE id = ?`, status, id)
	if err != nil {
		return fmt.Errorf("store: update kernel status: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: update kernel status: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteKernel removes one managed kernel row. Filesystem removal
// is the service layer's job and happens before this call; if this
// call fails the service restores consistency (see BrowserService).
func (s *Store) DeleteKernel(ctx context.Context, id string) error {
	res, err := s.q.ExecContext(ctx, `DELETE FROM kernels WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete kernel: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: delete kernel: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

type kernelScanner interface {
	Scan(dest ...any) error
}

func scanKernel(sc kernelScanner) (model.Kernel, error) {
	var k model.Kernel
	err := sc.Scan(&k.ID, &k.Version, &k.Channel, &k.Platform, &k.InstallPath,
		&k.ExecutablePath, &k.Status, &k.CreatedAt, &k.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Kernel{}, ErrNotFound
	}
	if err != nil {
		return model.Kernel{}, fmt.Errorf("store: scan kernel: %w", err)
	}
	return k, nil
}
