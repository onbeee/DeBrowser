// Package store is DeBrowser's repository layer: it owns all SQL for
// the core models and isolates it from the business logic above.
// Repositories never import each other; cross-model atomic work goes
// through Store.WithTx, which runs several repository calls inside
// one database transaction.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"

	"github.com/onbeee/DeBrowser/internal/database"
)

// querier is the subset of *sql.DB and *sql.Tx the repositories use,
// so the same code runs inside and outside transactions.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Store groups the repositories over one database handle (or one
// transaction, inside WithTx).
type Store struct {
	db *database.DB // nil when bound to a transaction
	q  querier
	tx bool
}

// New returns a Store over db.
func New(db *database.DB) *Store {
	return &Store{db: db, q: db.SQL()}
}

// WithTx runs fn with a transaction-bound Store: every repository call
// made through the Store passed to fn participates in the same
// transaction, which commits when fn returns nil and rolls back
// otherwise. Nested WithTx calls are rejected.
func (s *Store) WithTx(ctx context.Context, fn func(tx *Store) error) error {
	if s.tx {
		return fmt.Errorf("store: nested transactions are not supported")
	}
	return s.db.WithTx(ctx, func(sqlTx *sql.Tx) error {
		return fn(&Store{q: sqlTx, tx: true})
	})
}

// newID returns a random RFC 4122 version 4 UUID. IDs are generated in
// the application so rows can be referenced before they are inserted.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("store: generate id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// ErrNotFound is returned by single-row lookups that match nothing.
var ErrNotFound = fmt.Errorf("store: not found")

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullStr(p *string) sql.NullString {
	if p == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *p, Valid: true}
}

func nullInt(p *int) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(*p), Valid: true}
}
