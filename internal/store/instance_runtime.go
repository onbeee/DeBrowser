package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/onbeee/DeBrowser/internal/model"
)

// UpsertInstanceRuntime records (or replaces) the process identity
// of an instance's launched browser main process.
func (s *Store) UpsertInstanceRuntime(ctx context.Context, rt model.InstanceRuntime) error {
	_, err := s.q.ExecContext(ctx, `INSERT INTO instance_runtime
		(instance_id, pid, process_created_at, platform, executable_path, user_data_dir, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(instance_id) DO UPDATE SET
			pid = excluded.pid,
			process_created_at = excluded.process_created_at,
			platform = excluded.platform,
			executable_path = excluded.executable_path,
			user_data_dir = excluded.user_data_dir,
			started_at = excluded.started_at`,
		rt.InstanceID, rt.PID, rt.ProcessCreatedAt, rt.Platform,
		rt.ExecutablePath, rt.UserDataDir, rt.StartedAt)
	if err != nil {
		return fmt.Errorf("store: upsert instance runtime: %w", err)
	}
	return nil
}

func scanRuntime(row interface{ Scan(...any) error }) (model.InstanceRuntime, error) {
	var rt model.InstanceRuntime
	err := row.Scan(&rt.InstanceID, &rt.PID, &rt.ProcessCreatedAt, &rt.Platform,
		&rt.ExecutablePath, &rt.UserDataDir, &rt.StartedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return model.InstanceRuntime{}, ErrNotFound
	}
	if err != nil {
		return model.InstanceRuntime{}, fmt.Errorf("store: scan instance runtime: %w", err)
	}
	return rt, nil
}

// GetInstanceRuntime returns the runtime record of one instance,
// or ErrNotFound when the instance has none.
func (s *Store) GetInstanceRuntime(ctx context.Context, instanceID string) (model.InstanceRuntime, error) {
	return scanRuntime(s.q.QueryRowContext(ctx, `SELECT instance_id, pid, process_created_at,
		platform, executable_path, user_data_dir, started_at
		FROM instance_runtime WHERE instance_id = ?`, instanceID))
}

// ListInstanceRuntimes returns every runtime record (startup
// reconciliation works from this list).
func (s *Store) ListInstanceRuntimes(ctx context.Context) ([]model.InstanceRuntime, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT instance_id, pid, process_created_at,
		platform, executable_path, user_data_dir, started_at FROM instance_runtime`)
	if err != nil {
		return nil, fmt.Errorf("store: list instance runtimes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.InstanceRuntime
	for rows.Next() {
		rt, err := scanRuntime(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list instance runtimes: %w", err)
	}
	return out, nil
}

// DeleteInstanceRuntime removes an instance's runtime record.
// Removing a record that does not exist is not an error: the
// record's absence is the desired end state.
func (s *Store) DeleteInstanceRuntime(ctx context.Context, instanceID string) error {
	if _, err := s.q.ExecContext(ctx, `DELETE FROM instance_runtime WHERE instance_id = ?`, instanceID); err != nil {
		return fmt.Errorf("store: delete instance runtime: %w", err)
	}
	return nil
}

// NextProfileSeq atomically consumes and returns the next default
// instance-file sequence number. The counter only moves forward,
// so a number once handed out is never handed out again — not
// after the instance is deleted, and not after its file is
// deleted.
func (s *Store) NextProfileSeq(ctx context.Context) (int, error) {
	var seq int
	err := s.WithTx(ctx, func(tx *Store) error {
		if err := tx.q.QueryRowContext(ctx, `SELECT next_seq FROM profile_counter WHERE id = 1`).Scan(&seq); err != nil {
			return fmt.Errorf("store: read profile counter: %w", err)
		}
		if _, err := tx.q.ExecContext(ctx, `UPDATE profile_counter SET next_seq = ? WHERE id = 1`, seq+1); err != nil {
			return fmt.Errorf("store: advance profile counter: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// CountInstancesByName counts instances carrying the given display
// name (ASCII case-insensitive), optionally excluding one instance
// (the one being edited). Instance names are unique product-wide.
func (s *Store) CountInstancesByName(ctx context.Context, name, excludeID string) (int, error) {
	var n int
	err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM browser_instances
		WHERE name = ? COLLATE NOCASE AND id <> ?`, name, excludeID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count instances by name: %w", err)
	}
	return n, nil
}

// ListInstanceUserDataDirs returns the stored user_data_dir of
// every instance (the binding set for instance files).
func (s *Store) ListInstanceUserDataDirs(ctx context.Context) ([]string, error) {
	rows, err := s.q.QueryContext(ctx, `SELECT user_data_dir FROM browser_instances`)
	if err != nil {
		return nil, fmt.Errorf("store: list instance user data dirs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var dir string
		if err := rows.Scan(&dir); err != nil {
			return nil, fmt.Errorf("store: list instance user data dirs: %w", err)
		}
		out = append(out, dir)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list instance user data dirs: %w", err)
	}
	return out, nil
}
