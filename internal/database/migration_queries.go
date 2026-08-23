package database

const (
	beginImmediateSQL        = `BEGIN IMMEDIATE`
	commitSQL                = `COMMIT`
	rollbackSQL              = `ROLLBACK`
	beginMigrationSQL        = `SAVEPOINT cli_migration`
	commitMigrationSQL       = `RELEASE SAVEPOINT cli_migration`
	rollbackMigrationSQL     = `ROLLBACK TO SAVEPOINT cli_migration`
	createMigrationLedgerSQL = `CREATE TABLE IF NOT EXISTS cli__schema_migrations (
		app TEXT NOT NULL,
		version INTEGER NOT NULL,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL,
		PRIMARY KEY(app, version)
	)`
	selectAppliedMigrationsSQL = `SELECT version, name FROM cli__schema_migrations WHERE app = ? ORDER BY version`
	selectAppMigrationCountSQL = `SELECT COUNT(*) FROM cli__schema_migrations WHERE app = ?`
	insertMigrationSQL         = `INSERT INTO cli__schema_migrations(app, version, name, applied_at) VALUES (?, ?, ?, ?)`
)
