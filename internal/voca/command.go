package voca

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

type Generator interface {
	Generate(context.Context, string, io.Writer) error
}
type NewsRunner interface {
	Run(context.Context, io.Reader, io.Writer, io.Writer) error
}

type App struct {
	Repo   *Repository
	AI     Generator
	News   NewsRunner
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

func (a App) Run(ctx context.Context, args []string) error {
	if err := ValidateCommand(args); err != nil {
		return err
	}
	switch args[0] {
	case "add", "delete":
		phrase := strings.Join(args[1:], " ")
		if args[0] == "add" {
			return a.Repo.Add(ctx, phrase)
		}
		return a.Repo.Delete(ctx, phrase)
	case "list":
		words, err := a.Repo.List(ctx)
		if err != nil {
			return err
		}
		return renderVocabularyTable(a.Stdout, words)
	case "study":
		if a.AI == nil {
			return fmt.Errorf("AI client is not configured")
		}
		if len(args) > 1 {
			phrase := strings.TrimSpace(strings.Join(args[1:], " "))
			if phrase == "" {
				return fmt.Errorf("phrase cannot be empty")
			}
			return a.AI.Generate(ctx, studyPrompt(phrase), a.Stdout)
		}
		word, err := a.Repo.LeastReadRandom(ctx)
		if err != nil {
			return err
		}
		if err := a.AI.Generate(ctx, studyPrompt(word.Word), a.Stdout); err != nil {
			return err
		}
		return a.Repo.Increment(ctx, []int64{word.ID})
	case "story":
		if a.AI == nil {
			return fmt.Errorf("AI client is not configured")
		}
		words, err := a.Repo.Random(ctx, 10)
		if err != nil {
			return err
		}
		names := make([]string, len(words))
		ids := make([]int64, len(words))
		for i, word := range words {
			names[i], ids[i] = word.Word, word.ID
		}
		if err := a.AI.Generate(ctx, storyPrompt(names), a.Stdout); err != nil {
			return err
		}
		return a.Repo.Increment(ctx, ids)
	case "news":
		if a.News == nil {
			return fmt.Errorf("news client is not configured")
		}
		return a.News.Run(ctx, a.Stdin, a.Stdout, a.Stderr)
	}
	return nil
}

func ValidateCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cli voca <add|delete|list|study|story|news>")
	}
	switch args[0] {
	case "add", "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: cli voca %s <phrase>", args[0])
		}
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("usage: cli voca list")
		}
	case "study":
	case "story":
		if len(args) != 1 {
			return fmt.Errorf("usage: cli voca story")
		}
	case "news":
		if len(args) != 1 {
			return fmt.Errorf("usage: cli voca news")
		}
	default:
		return fmt.Errorf("unknown voca command %q", args[0])
	}
	return nil
}

func renderVocabularyTable(output io.Writer, words []Word) error {
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "Word\tRead Count"); err != nil {
		return err
	}
	for _, word := range words {
		displayWord := strings.Join(strings.Fields(word.Word), " ")
		if _, err := fmt.Fprintf(writer, "%s\t%d\n", displayWord, word.ReadCount); err != nil {
			return err
		}
	}
	return writer.Flush()
}

func studyPrompt(phrase string) string {
	return fmt.Sprintf("Teach the English phrase %q to a Korean speaker. Explain its meaning and nuance in Korean, then give natural English examples with Korean translations. Keep the lesson focused and practical.", phrase)
}

func storyPrompt(words []string) string {
	return fmt.Sprintf("Write one coherent short English story that naturally uses every item in this list: %s. Bold each listed item, then provide a fluent Korean translation. Do not omit any item.", strings.Join(words, ", "))
}
