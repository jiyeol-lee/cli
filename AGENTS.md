# Agent notes

Personal CLI. One binary, multiple apps.

## Layout

- `cmd/cli`: process entry only. Signals, XDG, open DB, wire deps, dispatch app.
- `internal/<app>`: all app logic. New code starts here.
- Shared code: extract to `internal/<concern>` only when a second app needs it. No `pkg/`, no `internal/shared`.
- `internal/database`: SQLite open, ledger, migrate, baseline. App-agnostic.
- `internal/xdg`: data/runtime paths only.
- New app: `internal/<name>` plus a `case` in `cmd/cli/main.go`.

## Files

- Package names: short, lowercase (`voca`, `gcal`, `xdg`).
- Go files: lowercase, no underscores (`command.go`, `repository.go`).
- Runtime SQL constants: `queries.go`.
- Migration / pragma SQL: `migration_queries.go`.
- Schema: `internal/<app>/migrations/NNNN_snake_name.sql` (4-digit version, `[a-z0-9_]+`).
- Fixtures: `testdata/`.
- Tests: same package, `_test.go`.

## CLI

- No flag frameworks. Hand-parse `args`.
- `cmd/cli` constructs deps. `internal/<app>.App` runs commands.
- Usage errors: `fmt.Errorf("usage: cli voca ...")`.
- Join leftover args into one phrase: `strings.Join(args[1:], " ")`.
- Inject I/O (`Stdin`/`Stdout`/`Stderr`) and collaborators on `App`. Do not read/write `os.Stdout` from library packages except `cmd/cli`.

## Style

- `go fmt`. No aliases. Stdlib import group, then one group for module + third party.
- Small consumer interfaces (`Generator`, `NewsRunner`, `EventSource`). Concrete `*Repository`, no repo interface.
- Sparse comments. Doc comment only on exported migrate/baseline entry points.
- No logger, ORM, DI container, assertion library, or generated SQL.

## Errors

- Return errors. `main` prints `cli: <err>` to stderr and exits 1.
- Wrap with `fmt.Errorf("verb noun: %w", err)` at package boundaries.
- User-facing messages: `fmt.Errorf`, not `errors.New`.
- Sentinels only when callers need `errors.Is` (`voca.ErrEmpty`). No custom error types.

## Database

- `database/sql` + handwritten SQL. No query builder.
- App tables: `<app>__<name>` (`voca__vocabulary`). Ledger: `cli__schema_migrations`.
- Embed migrations: `//go:embed migrations/*.sql`. Call `database.AdoptBaseline` then `database.Migrate` from the app package.
- Filenames are identity. Do not rename or edit an already-applied file. Do not reuse a version.
- `word` is stored `ToLower(TrimSpace(...))` and is unique. Match add/delete on that value.
- Permanent DB: `$XDG_DATA_HOME/cli/cli.sqlite3`. Unset or relative `$XDG_DATA_HOME` is an error.
- Dirs `0700`, DB and OAuth token `0600`. WAL + foreign keys + busy timeout via `database.OpenPermanent`.
- Prefer a real migration file over one-off SQL unless the user asks otherwise.

## Tests

- Std `testing`. Helpers: `t.Helper()`, `t.Cleanup()`, `t.TempDir()`.
- Assert with `t.Fatal` / `t.Fatalf`.
- Table tests: anonymous struct + `t.Run`.
- Real SQLite in temp dirs. HTTP: `httptest`. Output: `bytes.Buffer`. Fakes for small interfaces.
- Cover migrate idempotency when touching schema.

## Verify

```
go test ./...
go build ./cmd/cli
```
