package memory

import (
	"context"
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
	return NewRepository(db)
}

func TestRepositoryScopesOrdersAndArchives(t *testing.T) {
	repo := testRepository(t)
	ctx := context.Background()
	writes := []struct {
		text      string
		category  Category
		scope     Scope
		directory string
	}{
		{text: "  Preserve My Case  ", category: CategoryPreference, scope: ScopeProject, directory: "/project"},
		{text: "Preserve My Case", category: CategoryPreference, scope: ScopeProject, directory: "/project"},
		{text: "other project", category: CategoryNote, scope: ScopeProject, directory: "/other"},
		{text: "global", category: CategoryConvention, scope: ScopeGlobal},
	}
	for _, write := range writes {
		if err := repo.Write(ctx, write.text, write.category, write.scope, write.directory); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := repo.Read(ctx, ScopeAll, "/project")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %#v", entries)
	}
	if entries[0].Scope != ScopeProject || entries[1].Scope != ScopeProject || entries[2].Scope != ScopeGlobal {
		t.Fatalf("scope order = %#v", entries)
	}
	if entries[0].Memory != "Preserve My Case" || entries[0].ID < entries[1].ID {
		t.Fatalf("project order or trimming = %#v", entries[:2])
	}
	if entries[2].ProjectDirectory != "" {
		t.Fatalf("global directory = %q", entries[2].ProjectDirectory)
	}

	if err := repo.Archive(ctx, entries[0].ID, ScopeProject, "/other"); err == nil {
		t.Fatal("archive accepted wrong project")
	}
	if err := repo.Archive(ctx, entries[0].ID, ScopeProject, "/project"); err != nil {
		t.Fatal(err)
	}
	if err := repo.Archive(ctx, entries[0].ID, ScopeProject, "/project"); err == nil {
		t.Fatal("archive accepted an archived record")
	}
	entries, err = repo.Read(ctx, ScopeProject, "/project")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("active project entries = %#v", entries)
	}
}

func TestRepositoryRejectsEmptyMemory(t *testing.T) {
	if err := testRepository(t).Write(context.Background(), " \n ", CategoryNote, ScopeGlobal, ""); err == nil {
		t.Fatal("expected empty memory error")
	}
}
