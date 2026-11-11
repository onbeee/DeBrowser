package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/onbeee/DeBrowser/internal/model"
)

const instanceColumns = `id, name, kernel_id, kernel_name, kernel_version, user_data_dir, proxy_id, status, start_url, sort_order, created_at, updated_at`

func scanInstance(row interface{ Scan(dest ...any) error }) (model.Instance, error) {
	var (
		in      model.Instance
		proxyID sql.NullString
		status  string
	)
	if err := row.Scan(&in.ID, &in.Name, &in.KernelID, &in.KernelName,
		&in.KernelVersion, &in.UserDataDir, &proxyID, &status,
		&in.StartURL, &in.SortOrder, &in.CreatedAt, &in.UpdatedAt); err != nil {
		return model.Instance{}, err
	}
	if proxyID.Valid {
		in.ProxyID = &proxyID.String
	}
	in.Status = model.InstanceStatus(status)
	return in, nil
}

// CreateInstance inserts a new browser instance, generating its ID
// when empty. Status defaults to stopped.
func (s *Store) CreateInstance(ctx context.Context, in model.Instance) (model.Instance, error) {
	if in.ID == "" {
		id, err := newID()
		if err != nil {
			return model.Instance{}, err
		}
		in.ID = id
	}
	if in.Status == "" {
		in.Status = model.StatusStopped
	}
	if _, err := s.q.ExecContext(ctx, `INSERT INTO browser_instances
		(id, name, kernel_id, kernel_name, kernel_version, user_data_dir,
		 proxy_id, status, start_url, sort_order)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		in.ID, in.Name, in.KernelID, in.KernelName, in.KernelVersion,
		in.UserDataDir, nullStr(in.ProxyID), string(in.Status),
		in.StartURL, in.SortOrder); err != nil {
		return model.Instance{}, fmt.Errorf("store: create instance: %w", err)
	}
	return s.GetInstance(ctx, in.ID)
}

// GetInstance returns one instance by ID, or ErrNotFound.
func (s *Store) GetInstance(ctx context.Context, id string) (model.Instance, error) {
	in, err := scanInstance(s.q.QueryRowContext(ctx,
		`SELECT `+instanceColumns+` FROM browser_instances WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Instance{}, ErrNotFound
	}
	if err != nil {
		return model.Instance{}, fmt.Errorf("store: get instance: %w", err)
	}
	return in, nil
}

// ListInstances returns all instances in display order.
func (s *Store) ListInstances(ctx context.Context) ([]model.Instance, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT `+instanceColumns+` FROM browser_instances ORDER BY sort_order, name, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list instances: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.Instance
	for rows.Next() {
		in, err := scanInstance(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list instances: %w", err)
		}
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list instances: %w", err)
	}
	return out, nil
}

// UpdateInstance replaces the editable fields of an instance and
// refreshes its updated_at timestamp.
func (s *Store) UpdateInstance(ctx context.Context, in model.Instance) (model.Instance, error) {
	res, err := s.q.ExecContext(ctx, `UPDATE browser_instances SET
		name = ?, kernel_id = ?, kernel_name = ?, kernel_version = ?,
		user_data_dir = ?, proxy_id = ?, status = ?, start_url = ?,
		sort_order = ?, updated_at = datetime('now')
		WHERE id = ?`,
		in.Name, in.KernelID, in.KernelName, in.KernelVersion,
		in.UserDataDir, nullStr(in.ProxyID), string(in.Status),
		in.StartURL, in.SortOrder, in.ID)
	if err != nil {
		return model.Instance{}, fmt.Errorf("store: update instance: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return model.Instance{}, fmt.Errorf("store: update instance: %w", err)
	} else if n == 0 {
		return model.Instance{}, ErrNotFound
	}
	return s.GetInstance(ctx, in.ID)
}

// SetInstanceStatus persists a lifecycle state transition.
func (s *Store) SetInstanceStatus(ctx context.Context, id string, status model.InstanceStatus) error {
	res, err := s.q.ExecContext(ctx, `UPDATE browser_instances SET
		status = ?, updated_at = datetime('now') WHERE id = ?`,
		string(status), id)
	if err != nil {
		return fmt.Errorf("store: set instance status: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: set instance status: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteInstance removes an instance. Its bookmarks are deleted with
// it (schema ON DELETE CASCADE); the profile directory on disk is the
// caller's concern (Stage 3) and is not touched here.
func (s *Store) DeleteInstance(ctx context.Context, id string) error {
	res, err := s.q.ExecContext(ctx, `DELETE FROM browser_instances WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete instance: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: delete instance: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}
