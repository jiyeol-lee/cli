package voca

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/jiyeol-lee/cli/internal/database"
)

func testRepository(t *testing.T) *Repository {
	t.Helper()
	db, err := database.OpenPermanent(filepath.Join(t.TempDir(), "cli.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal("migration is not idempotent:", err)
	}
	return NewRepository(db)
}

func TestRepositoryNormalizesAndStoresLowercase(t *testing.T) {
	repo := testRepository(t)
	ctx := context.Background()
	if err := repo.Add(ctx, "  Take Off  "); err != nil {
		t.Fatal(err)
	}
	if err := repo.Add(ctx, "take off"); err == nil {
		t.Fatal("expected normalized duplicate error")
	}
	words, err := repo.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(words) != 1 || words[0].Word != "take off" {
		t.Fatalf("words = %#v", words)
	}
	if err := repo.Delete(ctx, " TAKE OFF "); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.LeastReadRandom(ctx); !errors.Is(err, ErrEmpty) {
		t.Fatalf("error = %v", err)
	}
}

func TestRepositoryIncrement(t *testing.T) {
	repo := testRepository(t)
	ctx := context.Background()
	for _, word := range []string{"beta", "Alpha"} {
		if err := repo.Add(ctx, word); err != nil {
			t.Fatal(err)
		}
	}
	words, err := repo.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if words[0].Word != "alpha" {
		t.Fatalf("not alphabetic: %#v", words)
	}
	if err := repo.Increment(ctx, []int64{words[0].ID, words[1].ID}); err != nil {
		t.Fatal(err)
	}
	words, _ = repo.List(ctx)
	if words[0].ReadCount != 1 || words[1].ReadCount != 1 {
		t.Fatalf("counts = %#v", words)
	}
}
