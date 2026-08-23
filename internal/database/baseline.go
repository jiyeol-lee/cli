package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type BaselineQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type BaselineValidator func(context.Context, BaselineQueryer) (bool, error)

// AdoptBaseline records a validated schema that predates the migration ledger.
func AdoptBaseline(ctx context.Context, db *sql.DB, app string, version int, name string, validate BaselineValidator) error {
	if strings.TrimSpace(app) == "" || version < 1 || strings.TrimSpace(name) == "" {
		return fmt.Errorf("invalid migration baseline")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get migration baseline connection for %s: %w", app, err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, beginImmediateSQL); err != nil {
		return fmt.Errorf("begin migration baseline for %s: %w", app, err)
	}
	defer conn.ExecContext(context.Background(), rollbackSQL)

	if _, err := conn.ExecContext(ctx, createMigrationLedgerSQL); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}
	var applied int
	if err := conn.QueryRowContext(ctx, selectAppMigrationCountSQL, app).Scan(&applied); err != nil {
		return fmt.Errorf("read migration baseline for %s: %w", app, err)
	}
	if applied > 0 {
		if _, err := conn.ExecContext(ctx, commitSQL); err != nil {
			return fmt.Errorf("commit migration baseline for %s: %w", app, err)
		}
		return nil
	}

	exists, err := validate(ctx, conn)
	if err != nil {
		return fmt.Errorf("validate migration baseline for %s: %w", app, err)
	}
	if exists {
		if _, err := conn.ExecContext(ctx, insertMigrationSQL, app, version, name, migrationTimestamp()); err != nil {
			return fmt.Errorf("record migration baseline for %s: %w", app, err)
		}
	}
	if _, err := conn.ExecContext(ctx, commitSQL); err != nil {
		return fmt.Errorf("commit migration baseline for %s: %w", app, err)
	}
	return nil
}

func migrationTimestamp() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
