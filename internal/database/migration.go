package database

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var migrationFilenamePattern = regexp.MustCompile(`^([0-9]{4})_[a-z0-9]+(?:_[a-z0-9]+)*\.sql$`)

type migration struct {
	version int
	name    string
	sql     string
}

// Migrate applies an application's embedded SQL migrations in version order.
func Migrate(ctx context.Context, db *sql.DB, app string, migrationFS fs.FS, dir string) error {
	if strings.TrimSpace(app) == "" {
		return fmt.Errorf("migration app cannot be empty")
	}

	migrations, err := loadMigrations(migrationFS, dir)
	if err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get migration connection for %s: %w", app, err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, beginImmediateSQL); err != nil {
		return fmt.Errorf("begin migrations for %s: %w", app, err)
	}
	defer conn.ExecContext(context.Background(), rollbackSQL)

	if _, err := conn.ExecContext(ctx, createMigrationLedgerSQL); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}

	applied, err := appliedMigrations(ctx, conn, app)
	if err != nil {
		return err
	}
	if err := validateMigrationHistory(app, migrations, applied); err != nil {
		return err
	}
	for _, migration := range migrations[len(applied):] {
		canCommit, err := applyMigration(ctx, conn, app, migration)
		if err != nil {
			if canCommit {
				if _, commitErr := conn.ExecContext(context.Background(), commitSQL); commitErr != nil {
					return fmt.Errorf("%w; commit completed migrations: %v", err, commitErr)
				}
			}
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, commitSQL); err != nil {
		return fmt.Errorf("commit migrations for %s: %w", app, err)
	}
	return nil
}

func loadMigrations(migrationFS fs.FS, dir string) ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}

	versions := make(map[int]string, len(entries))
	migrations := make([]migration, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		matches := migrationFilenamePattern.FindStringSubmatch(name)
		if entry.IsDir() || matches == nil {
			return nil, fmt.Errorf("invalid migration filename %q", name)
		}
		version, err := strconv.Atoi(matches[1])
		if err != nil || version < 1 {
			return nil, fmt.Errorf("invalid migration version in %q", name)
		}
		if previous, ok := versions[version]; ok {
			return nil, fmt.Errorf("duplicate migration version %d in %q and %q", version, previous, name)
		}
		contents, err := fs.ReadFile(migrationFS, path.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", name, err)
		}
		sqlText := strings.TrimSpace(string(contents))
		if sqlText == "" {
			return nil, fmt.Errorf("migration %q is empty", name)
		}
		versions[version] = name
		migrations = append(migrations, migration{version: version, name: name, sql: sqlText})
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].version < migrations[j].version
	})
	return migrations, nil
}

func appliedMigrations(ctx context.Context, conn *sql.Conn, app string) ([]migration, error) {
	rows, err := conn.QueryContext(ctx, selectAppliedMigrationsSQL, app)
	if err != nil {
		return nil, fmt.Errorf("read applied migrations for %s: %w", app, err)
	}
	defer rows.Close()

	var applied []migration
	for rows.Next() {
		var migration migration
		if err := rows.Scan(&migration.version, &migration.name); err != nil {
			return nil, fmt.Errorf("scan applied migration for %s: %w", app, err)
		}
		applied = append(applied, migration)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read applied migrations for %s: %w", app, err)
	}
	return applied, nil
}

func validateMigrationHistory(app string, loaded, applied []migration) error {
	if len(applied) > len(loaded) {
		return fmt.Errorf("migration history for %s contains unknown version %d", app, applied[len(loaded)].version)
	}
	for i, recorded := range applied {
		expected := loaded[i]
		if recorded.version != expected.version {
			return fmt.Errorf("migration history for %s has version %d at position %d, want %d", app, recorded.version, i+1, expected.version)
		}
		if recorded.name != expected.name {
			return fmt.Errorf("migration %s version %d is recorded as %q, want %q", app, recorded.version, recorded.name, expected.name)
		}
	}
	return nil
}

func applyMigration(ctx context.Context, conn *sql.Conn, app string, migration migration) (bool, error) {
	if _, err := conn.ExecContext(ctx, beginMigrationSQL); err != nil {
		return true, fmt.Errorf("begin migration %s %d: %w", app, migration.version, err)
	}
	if _, err := conn.ExecContext(ctx, migration.sql); err != nil {
		migrationErr := fmt.Errorf("apply migration %s %d (%s): %w", app, migration.version, migration.name, err)
		return rollbackMigration(conn, migrationErr)
	}
	if _, err := conn.ExecContext(ctx, insertMigrationSQL, app, migration.version, migration.name, migrationTimestamp()); err != nil {
		migrationErr := fmt.Errorf("record migration %s %d: %w", app, migration.version, err)
		return rollbackMigration(conn, migrationErr)
	}
	if _, err := conn.ExecContext(ctx, commitMigrationSQL); err != nil {
		return false, fmt.Errorf("commit migration %s %d: %w", app, migration.version, err)
	}
	return true, nil
}

func rollbackMigration(conn *sql.Conn, migrationErr error) (bool, error) {
	if _, err := conn.ExecContext(context.Background(), rollbackMigrationSQL); err != nil {
		return false, fmt.Errorf("%w; roll back migration: %v", migrationErr, err)
	}
	if _, err := conn.ExecContext(context.Background(), commitMigrationSQL); err != nil {
		return false, fmt.Errorf("%w; close migration savepoint: %v", migrationErr, err)
	}
	return true, migrationErr
}
