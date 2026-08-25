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
	Name    string
	Text    bool
	Join    string
	JoinSet bool
	Help    bool
}

const Usage = "usage: cli gcal <list|soon|in-progress> [--text] [--join separator]"

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
	usage := fmt.Sprintf("usage: cli gcal %s [--text] [--join separator]", args[0])
	command := Command{Name: args[0]}
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--text":
			if command.Text {
				return Command{}, fmt.Errorf("duplicate --text\n%s", usage)
			}
			command.Text = true
		case "--join":
			if command.JoinSet {
				return Command{}, fmt.Errorf("duplicate --join\n%s", usage)
			}
			if i+1 == len(args) {
				return Command{}, fmt.Errorf("missing separator for --join\n%s", usage)
			}
			i++
			command.Join = args[i]
			command.JoinSet = true
		default:
			return Command{}, fmt.Errorf("%s", usage)
		}
	}
	if command.JoinSet && !command.Text {
		return Command{}, fmt.Errorf("--join requires --text\n%s", usage)
	}
	return command, nil
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
	separator := "\n"
	if command.JoinSet {
		separator = command.Join
	}
	return WriteEvents(a.Stdout, events, command.Text, separator)
}
