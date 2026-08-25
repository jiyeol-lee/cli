package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type Entry struct {
	ID               int64
	Scope            Scope
	ProjectDirectory string
	Category         Category
	Memory           string
	UpdatedAt        string
}

type Repository struct{ db *sql.DB }

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

func (r *Repository) Write(ctx context.Context, text string, category Category, scope Scope, projectDirectory string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("memory cannot be empty")
	}
	var directory any
	if scope == ScopeProject {
		directory = projectDirectory
	}
	if _, err := r.db.ExecContext(ctx, insertMemorySQL, scope, directory, category, text); err != nil {
		return fmt.Errorf("write memory: %w", err)
	}
	return nil
}

func (r *Repository) Read(ctx context.Context, scope Scope, projectDirectory string) ([]Entry, error) {
	var (
		rows *sql.Rows
		err  error
	)
	switch scope {
	case ScopeProject:
		rows, err = r.db.QueryContext(ctx, selectProjectMemoriesSQL, projectDirectory)
	case ScopeGlobal:
		rows, err = r.db.QueryContext(ctx, selectGlobalMemoriesSQL)
	default:
		rows, err = r.db.QueryContext(ctx, selectAllMemoriesSQL, projectDirectory)
	}
	if err != nil {
		return nil, fmt.Errorf("read memories: %w", err)
	}
	defer rows.Close()

	var entries []Entry
	for rows.Next() {
		var entry Entry
		var directory sql.NullString
		if err := rows.Scan(&entry.ID, &entry.Scope, &directory, &entry.Category, &entry.Memory, &entry.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan memory: %w", err)
		}
		entry.ProjectDirectory = directory.String
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read memories: %w", err)
	}
	return entries, nil
}

func (r *Repository) Archive(ctx context.Context, id int64, scope Scope, projectDirectory string) error {
	var (
		result sql.Result
		err    error
	)
	if scope == ScopeProject {
		result, err = r.db.ExecContext(ctx, archiveProjectMemorySQL, id, projectDirectory)
	} else {
		result, err = r.db.ExecContext(ctx, archiveGlobalMemorySQL, id)
	}
	if err != nil {
		return fmt.Errorf("archive memory %d: %w", id, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("archive memory %d: %w", id, err)
	}
	if count == 0 {
		return fmt.Errorf("active %s memory %d not found", scope, id)
	}
	return nil
}
