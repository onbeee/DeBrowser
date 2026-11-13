package store

import (
	"context"
	"fmt"
)

// TagCount is one tag label and how many proxies carry it.
type TagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// TagsForProxy returns the tags of one proxy, ordered by tag.
func (s *Store) TagsForProxy(ctx context.Context, proxyID string) ([]string, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT tag FROM proxy_tags WHERE proxy_id = ? ORDER BY tag`, proxyID)
	if err != nil {
		return nil, fmt.Errorf("store: tags for proxy: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var tag string
		if err := rows.Scan(&tag); err != nil {
			return nil, fmt.Errorf("store: tags for proxy: %w", err)
		}
		out = append(out, tag)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: tags for proxy: %w", err)
	}
	return out, nil
}

// TagsForProxies returns the tags of many proxies in one query,
// keyed by proxy ID (proxies without tags are absent).
func (s *Store) TagsForProxies(ctx context.Context, proxyIDs []string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(proxyIDs) == 0 {
		return out, nil
	}
	placeholders := make([]byte, 0, len(proxyIDs)*2)
	args := make([]any, 0, len(proxyIDs))
	for i, id := range proxyIDs {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
		args = append(args, id)
	}
	rows, err := s.q.QueryContext(ctx,
		`SELECT proxy_id, tag FROM proxy_tags WHERE proxy_id IN (`+string(placeholders)+`) ORDER BY proxy_id, tag`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: tags for proxies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var pid, tag string
		if err := rows.Scan(&pid, &tag); err != nil {
			return nil, fmt.Errorf("store: tags for proxies: %w", err)
		}
		out[pid] = append(out[pid], tag)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: tags for proxies: %w", err)
	}
	return out, nil
}

// SetProxyTags replaces the full tag set of one proxy. Callers run
// it inside WithTx together with the proxy write it belongs to.
func (s *Store) SetProxyTags(ctx context.Context, proxyID string, tags []string) error {
	if _, err := s.q.ExecContext(ctx, `DELETE FROM proxy_tags WHERE proxy_id = ?`, proxyID); err != nil {
		return fmt.Errorf("store: clear proxy tags: %w", err)
	}
	for _, tag := range tags {
		if _, err := s.q.ExecContext(ctx,
			`INSERT INTO proxy_tags (proxy_id, tag) VALUES (?, ?)`, proxyID, tag); err != nil {
			return fmt.Errorf("store: add proxy tag: %w", err)
		}
	}
	return nil
}

// ListTagCounts returns every tag in use with its proxy count,
// ordered by tag.
func (s *Store) ListTagCounts(ctx context.Context) ([]TagCount, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT tag, COUNT(1) FROM proxy_tags GROUP BY tag ORDER BY tag`)
	if err != nil {
		return nil, fmt.Errorf("store: list tags: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []TagCount{}
	for rows.Next() {
		var tc TagCount
		if err := rows.Scan(&tc.Tag, &tc.Count); err != nil {
			return nil, fmt.Errorf("store: list tags: %w", err)
		}
		out = append(out, tc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list tags: %w", err)
	}
	return out, nil
}

// DeleteTagEverywhere removes one tag label from every proxy that
// carries it and returns how many associations were removed.
func (s *Store) DeleteTagEverywhere(ctx context.Context, tag string) (int64, error) {
	res, err := s.q.ExecContext(ctx, `DELETE FROM proxy_tags WHERE tag = ?`, tag)
	if err != nil {
		return 0, fmt.Errorf("store: delete tag: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: delete tag: %w", err)
	}
	return n, nil
}

// RenameTagEverywhere renames one tag label on every proxy that
// carries it. A proxy that already carries the new label keeps a
// single association (the duplicate insert is ignored, the old one
// removed). Run inside WithTx.
func (s *Store) RenameTagEverywhere(ctx context.Context, oldTag, newTag string) error {
	if _, err := s.q.ExecContext(ctx,
		`INSERT OR IGNORE INTO proxy_tags (proxy_id, tag)
		 SELECT proxy_id, ? FROM proxy_tags WHERE tag = ?`, newTag, oldTag); err != nil {
		return fmt.Errorf("store: rename tag (copy): %w", err)
	}
	if _, err := s.q.ExecContext(ctx, `DELETE FROM proxy_tags WHERE tag = ?`, oldTag); err != nil {
		return fmt.Errorf("store: rename tag (remove): %w", err)
	}
	return nil
}
