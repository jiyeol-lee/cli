package voca

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"strings"

	"github.com/jiyeol-lee/cli/internal/database"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

func Migrate(ctx context.Context, db *sql.DB) error {
	if err := database.AdoptBaseline(ctx, db, "voca", 1, "0001_create_vocabulary.sql", validateLegacyVocabulary); err != nil {
		return err
	}
	return database.Migrate(ctx, db, "voca", migrationFS, "migrations")
}

type vocabularyColumn struct {
	name         string
	dataType     string
	notNull      int
	defaultValue sql.NullString
	primaryKey   int
}

func validateLegacyVocabulary(ctx context.Context, tx database.BaselineQueryer) (bool, error) {
	var exists int
	if err := tx.QueryRowContext(ctx, vocabularyTableExistsSQL).Scan(&exists); err != nil {
		return false, err
	}
	if exists == 0 {
		return false, nil
	}

	columns, err := readVocabularyColumns(ctx, tx)
	if err != nil {
		return true, err
	}
	if err := validateVocabularyColumns(columns); err != nil {
		return true, err
	}
	unique, err := hasUniqueWordIndex(ctx, tx)
	if err != nil {
		return true, err
	}
	if !unique {
		return true, fmt.Errorf("word must have a single-column unique index")
	}
	return true, nil
}

func readVocabularyColumns(ctx context.Context, tx database.BaselineQueryer) ([]vocabularyColumn, error) {
	rows, err := tx.QueryContext(ctx, vocabularyTableInfoSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var columns []vocabularyColumn
	for rows.Next() {
		var column vocabularyColumn
		if err := rows.Scan(&column.name, &column.dataType, &column.notNull, &column.defaultValue, &column.primaryKey); err != nil {
			return nil, err
		}
		columns = append(columns, column)
	}
	return columns, rows.Err()
}

func validateVocabularyColumns(columns []vocabularyColumn) error {
	expected := []vocabularyColumn{
		{name: "id", dataType: "INTEGER", primaryKey: 1},
		{name: "word", dataType: "TEXT", notNull: 1},
		{name: "read_count", dataType: "INTEGER", notNull: 1, defaultValue: sql.NullString{String: "0", Valid: true}},
		{name: "created_at", dataType: "TEXT", notNull: 1},
		{name: "updated_at", dataType: "TEXT", notNull: 1},
	}
	if len(columns) != len(expected) {
		return fmt.Errorf("voca__vocabulary has %d columns, want %d", len(columns), len(expected))
	}
	for i, want := range expected {
		got := columns[i]
		if got.name != want.name || !strings.EqualFold(got.dataType, want.dataType) || got.notNull != want.notNull || got.defaultValue != want.defaultValue || got.primaryKey != want.primaryKey {
			return fmt.Errorf("voca__vocabulary column %d does not match %q", i+1, want.name)
		}
	}
	return nil
}

func hasUniqueWordIndex(ctx context.Context, tx database.BaselineQueryer) (bool, error) {
	rows, err := tx.QueryContext(ctx, vocabularyUniqueIndexesSQL)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	indexes := make(map[string][]string)
	for rows.Next() {
		var index, column string
		if err := rows.Scan(&index, &column); err != nil {
			return false, err
		}
		indexes[index] = append(indexes[index], column)
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, columns := range indexes {
		if len(columns) == 1 && columns[0] == "word" {
			return true, nil
		}
	}
	return false, nil
}
