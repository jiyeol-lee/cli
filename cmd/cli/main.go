package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jiyeol-lee/cli/internal/database"
	"github.com/jiyeol-lee/cli/internal/gcal"
	"github.com/jiyeol-lee/cli/internal/voca"
	"github.com/jiyeol-lee/cli/internal/xdg"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "cli:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	return runWithDependencies(ctx, args, dependencies{
		newCalendar: newGoogleCalendar,
		stdout:      os.Stdout,
	})
}

type dependencies struct {
	newCalendar func(context.Context, xdg.Dirs) (gcal.Calendar, error)
	stdout      io.Writer
}

func runWithDependencies(ctx context.Context, args []string, deps dependencies) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cli <voca|gcal> ...")
	}
	var calendarCommand gcal.Command
	switch args[0] {
	case "voca":
		if err := voca.ValidateCommand(args[1:]); err != nil {
			return err
		}
	case "gcal":
		command, err := gcal.ParseCommand(args[1:])
		if err != nil {
			return err
		}
		if command.Help {
			_, err := fmt.Fprintln(deps.stdout, gcal.Usage)
			return err
		}
		calendarCommand = command
	default:
		return fmt.Errorf("unknown app %q", args[0])
	}
	dirs, err := xdg.Resolve()
	if err != nil {
		return err
	}
	switch args[0] {
	case "voca":
		db, err := database.OpenPermanent(dirs.PermanentDB())
		if err != nil {
			return err
		}
		defer db.Close()
		if err := voca.Migrate(ctx, db); err != nil {
			return err
		}
		repo := voca.NewRepository(db)
		app := voca.App{
			Repo:  repo,
			AI:    voca.AIClient{HTTP: &http.Client{}, APIKey: os.Getenv("OPENCODE_GO_API_KEY")},
			News:  voca.News{Scraper: voca.APScraper{HTTP: &http.Client{Timeout: 15 * time.Second}}, Pager: voca.TerminalPager{}},
			Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		}
		return app.Run(ctx, args[1:])
	case "gcal":
		cal, err := deps.newCalendar(ctx, dirs)
		if err != nil {
			return err
		}
		return (gcal.App{Calendar: cal, Stdout: deps.stdout}).Run(ctx, calendarCommand)
	}
	return nil
}

func newGoogleCalendar(ctx context.Context, dirs xdg.Dirs) (gcal.Calendar, error) {
	oauth := gcal.OAuth{TokenPath: dirs.GoogleToken(), Stdout: os.Stderr}
	client, err := oauth.Client(ctx)
	if err != nil {
		return gcal.Calendar{}, err
	}
	service, err := calendar.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return gcal.Calendar{}, fmt.Errorf("create Google Calendar client: %w", err)
	}
	return gcal.Calendar{Source: gcal.GoogleSource{Service: service}, CalendarID: os.Getenv("GCAL_CALENDAR_ID")}, nil
}
