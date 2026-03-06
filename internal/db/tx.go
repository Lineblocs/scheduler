package db

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// DBTX is the common interface between *sqlx.DB and *sqlx.Tx.
// All repository methods should accept this so they work inside or outside transactions.
type DBTX interface {
	sqlx.QueryerContext
	sqlx.ExecerContext
	GetContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
	SelectContext(ctx context.Context, dest interface{}, query string, args ...interface{}) error
	PrepareContext(ctx context.Context, query string) (*sql.Stmt, error)
}

// RunInTx executes fn inside a database transaction.
// If fn returns an error, the transaction is rolled back; otherwise it is committed.
func RunInTx(ctx context.Context, db *sqlx.DB, opts *sql.TxOptions, fn func(tx *sqlx.Tx) error) error {
	tx, err := db.BeginTxx(ctx, opts)
	if err != nil {
		return fmt.Errorf("db: begin tx: %w", err)
	}

	if err := fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return fmt.Errorf("db: rollback failed: %v (original: %w)", rbErr, err)
		}
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("db: commit: %w", err)
	}

	return nil
}
