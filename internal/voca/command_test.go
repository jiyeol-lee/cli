package voca

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type fakeGenerator struct{ err error }

func (f fakeGenerator) Generate(_ context.Context, _ string, out io.Writer) error {
	if f.err != nil {
		return f.err
	}
	_, err := io.WriteString(out, "lesson\n")
	return err
}

type truncatedGenerator struct{}

func (truncatedGenerator) Generate(_ context.Context, _ string, out io.Writer) error {
	_, err := parseSSE(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"), out)
	return err
}

func TestStudyIncrementsOnlyAfterGeneration(t *testing.T) {
	repo := testRepository(t)
	ctx := context.Background()
	if err := repo.Add(ctx, "Word"); err != nil {
		t.Fatal(err)
	}
	app := App{Repo: repo, AI: fakeGenerator{err: errors.New("failed")}, Stdout: &bytes.Buffer{}}
	if err := app.Run(ctx, []string{"study"}); err == nil {
		t.Fatal("expected error")
	}
	words, _ := repo.List(ctx)
	if words[0].ReadCount != 0 {
		t.Fatal("failed generation incremented count")
	}
	app.AI = fakeGenerator{}
	if err := app.Run(ctx, []string{"study"}); err != nil {
		t.Fatal(err)
	}
	words, _ = repo.List(ctx)
	if words[0].ReadCount != 1 {
		t.Fatalf("count = %d", words[0].ReadCount)
	}
}

func TestStudyDoesNotIncrementAfterTruncatedAIStream(t *testing.T) {
	repo := testRepository(t)
	ctx := context.Background()
	if err := repo.Add(ctx, "Word"); err != nil {
		t.Fatal(err)
	}
	app := App{Repo: repo, AI: truncatedGenerator{}, Stdout: &bytes.Buffer{}}
	if err := app.Run(ctx, []string{"study"}); err == nil {
		t.Fatal("expected truncated stream error")
	}
	words, err := repo.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if words[0].ReadCount != 0 {
		t.Fatalf("count = %d", words[0].ReadCount)
	}
}

func TestStudyWithArgumentDoesNotUseRepository(t *testing.T) {
	repo := testRepository(t)
	var out bytes.Buffer
	app := App{Repo: repo, AI: fakeGenerator{}, Stdout: &out}
	if err := app.Run(context.Background(), []string{"study", "Ad Hoc"}); err != nil {
		t.Fatal(err)
	}
	words, err := repo.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(words) != 0 {
		t.Fatalf("words = %#v", words)
	}
}

func TestRenderVocabularyTableAlignsColumns(t *testing.T) {
	words := []Word{
		{Word: "ant", ReadCount: 2},
		{Word: "extraordinary", ReadCount: 17},
	}
	var out bytes.Buffer
	if err := renderVocabularyTable(&out, words); err != nil {
		t.Fatal(err)
	}
	want := "Word           Read Count\n" +
		"ant            2\n" +
		"extraordinary  17\n"
	if out.String() != want {
		t.Fatalf("renderVocabularyTable() = %q, want %q", out.String(), want)
	}
}

func TestRenderVocabularyTableEmpty(t *testing.T) {
	var out bytes.Buffer
	if err := renderVocabularyTable(&out, nil); err != nil {
		t.Fatal(err)
	}
	const want = "Word  Read Count\n"
	if out.String() != want {
		t.Fatalf("renderVocabularyTable() = %q, want %q", out.String(), want)
	}
}

func TestRenderVocabularyTableNormalizesEmbeddedWhitespace(t *testing.T) {
	words := []Word{{Word: "line\tbreak\nphrase", ReadCount: 3}}
	var out bytes.Buffer
	if err := renderVocabularyTable(&out, words); err != nil {
		t.Fatal(err)
	}
	const want = "Word               Read Count\nline break phrase  3\n"
	if out.String() != want {
		t.Fatalf("renderVocabularyTable() = %q, want %q", out.String(), want)
	}
}
