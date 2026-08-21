package voca

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrEmpty = errors.New("vocabulary is empty")

type Word struct {
	ID        int64
	Word      string
	ReadCount int
}

type Repository struct{ db *sql.DB }

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

func normalize(word string) string { return strings.ToLower(strings.TrimSpace(word)) }

func (r *Repository) Add(ctx context.Context, word string) error {
	word = normalize(word)
	if word == "" {
		return fmt.Errorf("phrase cannot be empty")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := r.db.ExecContext(ctx, insertVocabularySQL, word, now, now)
	if err != nil {
		return fmt.Errorf("add %q: %w", word, err)
	}
	return nil
}

func (r *Repository) Delete(ctx context.Context, word string) error {
	normalized := normalize(word)
	if normalized == "" {
		return fmt.Errorf("phrase cannot be empty")
	}
	result, err := r.db.ExecContext(ctx, deleteVocabularySQL, normalized)
	if err != nil {
		return fmt.Errorf("delete %q: %w", word, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("phrase %q not found", strings.TrimSpace(word))
	}
	return nil
}

func (r *Repository) List(ctx context.Context) ([]Word, error) {
	rows, err := r.db.QueryContext(ctx, listVocabularySQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var words []Word
	for rows.Next() {
		var w Word
		if err := rows.Scan(&w.ID, &w.Word, &w.ReadCount); err != nil {
			return nil, err
		}
		words = append(words, w)
	}
	return words, rows.Err()
}

func (r *Repository) LeastReadRandom(ctx context.Context) (Word, error) {
	var w Word
	err := r.db.QueryRowContext(ctx, leastReadRandomSQL).Scan(&w.ID, &w.Word, &w.ReadCount)
	if errors.Is(err, sql.ErrNoRows) {
		return Word{}, ErrEmpty
	}
	if err != nil {
		return Word{}, err
	}
	return w, nil
}

func (r *Repository) Random(ctx context.Context, limit int) ([]Word, error) {
	rows, err := r.db.QueryContext(ctx, randomLimitSQL, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var words []Word
	for rows.Next() {
		var w Word
		if err := rows.Scan(&w.ID, &w.Word, &w.ReadCount); err != nil {
			return nil, err
		}
		words = append(words, w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(words) == 0 {
		return nil, ErrEmpty
	}
	return words, nil
}

func (r *Repository) Increment(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, incrementSQL, now, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
