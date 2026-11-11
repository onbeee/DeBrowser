package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/onbeee/DeBrowser/internal/model"
)

const bookmarkColumns = `id, name, url, instance_id, sort_order, created_at, updated_at`

func scanBookmark(row interface{ Scan(dest ...any) error }) (model.Bookmark, error) {
	var (
		b          model.Bookmark
		instanceID sql.NullString
	)
	if err := row.Scan(&b.ID, &b.Name, &b.URL, &instanceID,
		&b.SortOrder, &b.CreatedAt, &b.UpdatedAt); err != nil {
		return model.Bookmark{}, err
	}
	if instanceID.Valid {
		b.InstanceID = &instanceID.String
	}
	return b, nil
}

// CreateBookmark inserts a bookmark, generating its ID when empty.
func (s *Store) CreateBookmark(ctx context.Context, b model.Bookmark) (model.Bookmark, error) {
	if b.ID == "" {
		id, err := newID()
		if err != nil {
			return model.Bookmark{}, err
		}
		b.ID = id
	}
	if _, err := s.q.ExecContext(ctx, `INSERT INTO bookmarks
		(id, name, url, instance_id, sort_order) VALUES (?, ?, ?, ?, ?)`,
		b.ID, b.Name, b.URL, nullStr(b.InstanceID), b.SortOrder); err != nil {
		return model.Bookmark{}, fmt.Errorf("store: create bookmark: %w", err)
	}
	return s.GetBookmark(ctx, b.ID)
}

// GetBookmark returns one bookmark by ID, or ErrNotFound.
func (s *Store) GetBookmark(ctx context.Context, id string) (model.Bookmark, error) {
	b, err := scanBookmark(s.q.QueryRowContext(ctx,
		`SELECT `+bookmarkColumns+` FROM bookmarks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Bookmark{}, ErrNotFound
	}
	if err != nil {
		return model.Bookmark{}, fmt.Errorf("store: get bookmark: %w", err)
	}
	return b, nil
}

// ListBookmarks returns bookmarks in display order: for one instance
// when instanceID is non-nil, or only the global scope when it is nil.
func (s *Store) ListBookmarks(ctx context.Context, instanceID *string) ([]model.Bookmark, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if instanceID == nil {
		rows, err = s.q.QueryContext(ctx,
			`SELECT `+bookmarkColumns+` FROM bookmarks WHERE instance_id IS NULL ORDER BY sort_order, name, id`)
	} else {
		rows, err = s.q.QueryContext(ctx,
			`SELECT `+bookmarkColumns+` FROM bookmarks WHERE instance_id = ? ORDER BY sort_order, name, id`, *instanceID)
	}
	if err != nil {
		return nil, fmt.Errorf("store: list bookmarks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []model.Bookmark
	for rows.Next() {
		b, err := scanBookmark(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list bookmarks: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list bookmarks: %w", err)
	}
	return out, nil
}

// UpdateBookmark replaces the editable fields of a bookmark and
// refreshes its updated_at timestamp.
func (s *Store) UpdateBookmark(ctx context.Context, b model.Bookmark) (model.Bookmark, error) {
	res, err := s.q.ExecContext(ctx, `UPDATE bookmarks SET
		name = ?, url = ?, instance_id = ?, sort_order = ?,
		updated_at = datetime('now') WHERE id = ?`,
		b.Name, b.URL, nullStr(b.InstanceID), b.SortOrder, b.ID)
	if err != nil {
		return model.Bookmark{}, fmt.Errorf("store: update bookmark: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return model.Bookmark{}, fmt.Errorf("store: update bookmark: %w", err)
	} else if n == 0 {
		return model.Bookmark{}, ErrNotFound
	}
	return s.GetBookmark(ctx, b.ID)
}

// DeleteBookmark removes a bookmark.
func (s *Store) DeleteBookmark(ctx context.Context, id string) error {
	res, err := s.q.ExecContext(ctx, `DELETE FROM bookmarks WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete bookmark: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("store: delete bookmark: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}
