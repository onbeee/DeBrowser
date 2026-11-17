package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/onbeee/DeBrowser/internal/model"
)

// Bookmark library nodes (Stage 9C). The library is a tree of
// folders and bookmarks stored flat with a parent pointer; ordering
// inside one level is (sort_order, created_at, id) so it stays
// stable even before explicit sort values are assigned.

func scanBookmarkNode(row interface{ Scan(dest ...any) error }) (model.BookmarkNode, error) {
	var (
		n        model.BookmarkNode
		parentID sql.NullString
	)
	err := row.Scan(&n.ID, &n.Kind, &n.Name, &n.URL, &parentID, &n.SortOrder, &n.CreatedAt, &n.UpdatedAt)
	if err != nil {
		return model.BookmarkNode{}, err
	}
	if parentID.Valid {
		p := parentID.String
		n.ParentID = &p
	}
	return n, nil
}

const bookmarkNodeColumns = `id, kind, name, url, parent_id, sort_order, created_at, updated_at`

// CreateBookmarkNode inserts one library node.
func (s *Store) CreateBookmarkNode(ctx context.Context, n model.BookmarkNode) (model.BookmarkNode, error) {
	if n.ID == "" {
		id, err := newID()
		if err != nil {
			return model.BookmarkNode{}, err
		}
		n.ID = id
	}
	_, err := s.q.ExecContext(ctx,
		`INSERT INTO bookmark_nodes (id, kind, name, url, parent_id, sort_order)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		n.ID, n.Kind, n.Name, n.URL, nullStr(n.ParentID), n.SortOrder)
	if err != nil {
		return model.BookmarkNode{}, fmt.Errorf("store: create bookmark node: %w", err)
	}
	return s.GetBookmarkNode(ctx, n.ID)
}

// GetBookmarkNode returns one library node by ID.
func (s *Store) GetBookmarkNode(ctx context.Context, id string) (model.BookmarkNode, error) {
	n, err := scanBookmarkNode(s.q.QueryRowContext(ctx,
		`SELECT `+bookmarkNodeColumns+` FROM bookmark_nodes WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return model.BookmarkNode{}, ErrNotFound
	}
	if err != nil {
		return model.BookmarkNode{}, fmt.Errorf("store: get bookmark node: %w", err)
	}
	return n, nil
}

// ListBookmarkNodes returns every library node in stable tree order
// (grouped by parent, ordered within each level). Callers assemble
// the tree from the parent pointers.
func (s *Store) ListBookmarkNodes(ctx context.Context) ([]model.BookmarkNode, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT `+bookmarkNodeColumns+` FROM bookmark_nodes
		 ORDER BY (parent_id IS NOT NULL), parent_id, sort_order, created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list bookmark nodes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []model.BookmarkNode{}
	for rows.Next() {
		n, err := scanBookmarkNode(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list bookmark nodes: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list bookmark nodes: %w", err)
	}
	return out, nil
}

// UpdateBookmarkNode rewrites a node's editable fields (name, url,
// parent, sort order). Kind and timestamps are managed by the
// database and the caller respectively.
func (s *Store) UpdateBookmarkNode(ctx context.Context, n model.BookmarkNode) (model.BookmarkNode, error) {
	res, err := s.q.ExecContext(ctx,
		`UPDATE bookmark_nodes
		 SET name = ?, url = ?, parent_id = ?, sort_order = ?, updated_at = datetime('now')
		 WHERE id = ?`,
		n.Name, n.URL, nullStr(n.ParentID), n.SortOrder, n.ID)
	if err != nil {
		return model.BookmarkNode{}, fmt.Errorf("store: update bookmark node: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return model.BookmarkNode{}, fmt.Errorf("store: update bookmark node: %w", err)
	}
	if affected == 0 {
		return model.BookmarkNode{}, ErrNotFound
	}
	return s.GetBookmarkNode(ctx, n.ID)
}

// DeleteBookmarkNode removes one node row. Removing a whole subtree
// is the caller's job (children first), so a folder with children
// still present fails on the foreign key instead of orphaning them.
func (s *Store) DeleteBookmarkNode(ctx context.Context, id string) error {
	res, err := s.q.ExecContext(ctx, `DELETE FROM bookmark_nodes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete bookmark node: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: delete bookmark node: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}
