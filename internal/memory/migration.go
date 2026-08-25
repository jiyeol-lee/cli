package memory

import (
	"context"
	"database/sql"
	"embed"

	"github.com/jiyeol-lee/cli/internal/database"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migrate applies the memory schema migrations.
func Migrate(ctx context.Context, db *sql.DB) error {
	return database.Migrate(ctx, db, "memory", migrationFS, "migrations")
}
