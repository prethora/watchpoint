// Package db provides PostgreSQL-backed repository implementations for the
// WatchPoint platform. All repositories accept a DBTX interface that is
// satisfied by both *pgxpool.Pool (for normal queries) and pgx.Tx (for
// transactional execution), enabling clean transaction support.
package db

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DBTX is the minimal interface shared by *pgxpool.Pool and pgx.Tx.
// Repositories accept this so the same code works inside or outside a
// transaction.
type DBTX interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}
