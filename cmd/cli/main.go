package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/jiyeol-lee/cli/internal/database"
	"github.com/jiyeol-lee/cli/internal/gcal"
	"github.com/jiyeol-lee/cli/internal/memory"
	"github.com/jiyeol-lee/cli/internal/voca"
	"github.com/jiyeol-lee/cli/internal/workmux"
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
		newWorkmux: func() (workmux.App, error) {
			return newWorkmuxApp(os.Stdin, os.Stdout, os.Stderr)
		},
		newCleanup: func(paths workmux.CleanupPaths) workmux.App {
			return workmuxApp(paths, nil, io.Discard, io.Discard)
		},
		directoryResolver: memory.Resolver{Runner: memory.GitWorktreeRunner{}},
		stdout:            os.Stdout,
	})
}

type dependencies struct {
	newCalendar       func(context.Context, xdg.Dirs) (gcal.Calendar, error)
	newWorkmux        func() (workmux.App, error)
	newCleanup        func(workmux.CleanupPaths) workmux.App
	directoryResolver memory.DirectoryResolver
	stdout            io.Writer
}

func runWithDependencies(ctx context.Context, args []string, deps dependencies) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cli <voca|gcal|memory|workmux> ...")
	}
	var calendarCommand gcal.Command
	var memoryCommand memory.Command
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
	case "memory":
		command, err := memory.ParseCommand(args[1:])
		if err != nil {
			return err
		}
		if command.Help {
			_, err := fmt.Fprintln(deps.stdout, memory.Usage)
			return err
		}
		memoryCommand = command
		if command.Kind == memory.CommandDirectory {
			return (memory.App{Resolver: deps.directoryResolver, Stdout: deps.stdout}).Run(ctx, command)
		}
	case "workmux":
		if len(args) > 1 && args[1] == "_cleanup" {
			command, err := workmux.ParseCleanupCommand(args[1:])
			if err != nil {
				return err
			}
			if deps.newCleanup == nil {
				return fmt.Errorf("workmux cleanup dependencies are not configured")
			}
			return workmux.RunCleanupProcess(ctx, command, deps.newCleanup)
		}
		command, err := workmux.ParseCommand(args[1:])
		if err != nil {
			return err
		}
		if command.Help {
			_, err := io.WriteString(deps.stdout, workmux.Usage)
			return err
		}
		if deps.newWorkmux == nil {
			return fmt.Errorf("workmux dependencies are not configured")
		}
		app, err := deps.newWorkmux()
		if err != nil {
			return err
		}
		return app.Run(ctx, command)
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
			News:  voca.News{Scraper: voca.APScraper{HTTP: voca.NewAPHTTPClient()}, Pager: voca.TerminalPager{}},
			Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		}
		return app.Run(ctx, args[1:])
	case "gcal":
		cal, err := deps.newCalendar(ctx, dirs)
		if err != nil {
			return err
		}
		return (gcal.App{Calendar: cal, Stdout: deps.stdout}).Run(ctx, calendarCommand)
	case "memory":
		db, err := database.OpenPermanent(dirs.PermanentDB())
		if err != nil {
			return err
		}
		defer db.Close()
		if err := memory.Migrate(ctx, db); err != nil {
			return err
		}
		app := memory.App{
			Repo:     memory.NewRepository(db),
			Resolver: deps.directoryResolver,
			Stdout:   deps.stdout,
		}
		return app.Run(ctx, memoryCommand)
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

func newWorkmuxApp(stdin io.Reader, stdout, stderr io.Writer) (workmux.App, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return workmux.App{}, fmt.Errorf("resolve workmux home: %w", err)
	}
	if !filepath.IsAbs(home) {
		return workmux.App{}, fmt.Errorf("workmux home directory must be absolute")
	}
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		stateHome = filepath.Join(home, ".local", "state")
	}
	if !filepath.IsAbs(stateHome) {
		return workmux.App{}, fmt.Errorf("XDG_STATE_HOME must be absolute")
	}
	stateDir := filepath.Join(stateHome, "cli", "workmux")
	return workmuxApp(workmux.CleanupPaths{HomeDir: home, StateDir: stateDir, ConfigDir: filepath.Join(home, ".config", "cli", "workmux")}, stdin, stdout, stderr), nil
}

func workmuxApp(paths workmux.CleanupPaths, stdin io.Reader, stdout, stderr io.Writer) workmux.App {
	runner := workmux.ExecRunner{}
	return workmux.App{
		Runner:  runner,
		Mux:     workmux.Tmux{Runner: runner},
		Spawner: workmux.ExecCleanupSpawner{},
		Sandbox: &workmux.Containers{
			Runner: runner, HomeDir: paths.HomeDir, StateDir: paths.StateDir,
		},
		Stdin: stdin, Stdout: stdout, Stderr: stderr,
		HomeDir: paths.HomeDir, StateDir: paths.StateDir,
		ConfigDir: paths.ConfigDir,
	}
}
