package gcal

import (
	"context"
	"fmt"
	"io"
	"time"
)

type App struct {
	Calendar Calendar
	Stdout   io.Writer
}

type Command struct {
	Name string
	Text bool
	Help bool
}

const Usage = "usage: cli gcal <list|soon|in-progress> [--text]"

func ParseCommand(args []string) (Command, error) {
	if len(args) == 0 {
		return Command{}, fmt.Errorf("%s", Usage)
	}
	if len(args) == 1 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help") {
		return Command{Help: true}, nil
	}
	if args[0] != "list" && args[0] != "soon" && args[0] != "in-progress" {
		return Command{}, fmt.Errorf("unknown gcal command %q", args[0])
	}
	if len(args) == 2 && (args[1] == "-h" || args[1] == "--help") {
		return Command{Help: true}, nil
	}
	if len(args) > 2 || (len(args) == 2 && args[1] != "--text") {
		return Command{}, fmt.Errorf("usage: cli gcal %s [--text]", args[0])
	}
	return Command{Name: args[0], Text: len(args) == 2}, nil
}

func (a App) Run(ctx context.Context, command Command) error {
	events, err := a.Calendar.Today(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	if a.Calendar.Now != nil {
		now = a.Calendar.Now()
	}
	if a.Calendar.Location != nil {
		now = now.In(a.Calendar.Location)
	}
	switch command.Name {
	case "soon":
		events = Soon(events, now)
	case "in-progress":
		events = InProgress(events, now)
	}
	return WriteEvents(a.Stdout, events, command.Text)
}
