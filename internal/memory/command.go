package memory

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
)

const Usage = `usage: cli memory <directory|read|write|archive>

  cli memory directory
  cli memory read [--scope all|project|global]
  cli memory write <memory words...> --category preference|convention|note --scope project|global
  cli memory archive <positive-id> --scope project|global`

type Scope string

const (
	ScopeAll     Scope = "all"
	ScopeProject Scope = "project"
	ScopeGlobal  Scope = "global"
)

type Category string

const (
	CategoryPreference Category = "preference"
	CategoryConvention Category = "convention"
	CategoryNote       Category = "note"
)

type CommandKind string

const (
	CommandDirectory CommandKind = "directory"
	CommandRead      CommandKind = "read"
	CommandWrite     CommandKind = "write"
	CommandArchive   CommandKind = "archive"
)

type Command struct {
	Kind     CommandKind
	Help     bool
	Scope    Scope
	Category Category
	Memory   string
	ID       int64
}

func ParseCommand(args []string) (Command, error) {
	if len(args) == 0 {
		return Command{Help: true}, nil
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		return Command{Help: true}, nil
	}
	switch args[0] {
	case "directory":
		if len(args) != 1 {
			return Command{}, fmt.Errorf("usage: cli memory directory")
		}
		return Command{Kind: CommandDirectory}, nil
	case "read":
		return parseRead(args[1:])
	case "write":
		return parseWrite(args[1:])
	case "archive":
		return parseArchive(args[1:])
	default:
		return Command{}, fmt.Errorf("unknown memory command %q", args[0])
	}
}

func parseRead(args []string) (Command, error) {
	command := Command{Kind: CommandRead, Scope: ScopeAll}
	if len(args) == 0 {
		return command, nil
	}
	if len(args) != 2 || args[0] != "--scope" {
		return Command{}, fmt.Errorf("usage: cli memory read [--scope all|project|global]")
	}
	scope, err := parseScope(args[1], true)
	if err != nil {
		return Command{}, err
	}
	command.Scope = scope
	return command, nil
}

func parseWrite(args []string) (Command, error) {
	command := Command{Kind: CommandWrite}
	var words []string
	optionsStarted := false
	seenCategory := false
	seenScope := false
	for i := 0; i < len(args); i++ {
		argument := args[i]
		if !strings.HasPrefix(argument, "--") {
			if optionsStarted {
				return Command{}, fmt.Errorf("memory words must precede options")
			}
			words = append(words, argument)
			continue
		}
		optionsStarted = true
		if i+1 >= len(args) {
			return Command{}, fmt.Errorf("missing value for %s", argument)
		}
		switch argument {
		case "--category":
			if seenCategory {
				return Command{}, fmt.Errorf("duplicate --category")
			}
			category, err := parseCategory(args[i+1])
			if err != nil {
				return Command{}, err
			}
			command.Category = category
			seenCategory = true
		case "--scope":
			if seenScope {
				return Command{}, fmt.Errorf("duplicate --scope")
			}
			scope, err := parseScope(args[i+1], false)
			if err != nil {
				return Command{}, err
			}
			command.Scope = scope
			seenScope = true
		default:
			return Command{}, fmt.Errorf("unknown memory option %q", argument)
		}
		i++
	}
	command.Memory = strings.Join(words, " ")
	if strings.TrimSpace(command.Memory) == "" || !seenCategory || !seenScope {
		return Command{}, fmt.Errorf("usage: cli memory write <memory words...> --category preference|convention|note --scope project|global")
	}
	return command, nil
}

func parseArchive(args []string) (Command, error) {
	if len(args) != 3 || args[1] != "--scope" {
		return Command{}, fmt.Errorf("usage: cli memory archive <positive-id> --scope project|global")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || id < 1 {
		return Command{}, fmt.Errorf("memory id must be a positive integer")
	}
	scope, err := parseScope(args[2], false)
	if err != nil {
		return Command{}, err
	}
	return Command{Kind: CommandArchive, ID: id, Scope: scope}, nil
}

func parseScope(value string, allowAll bool) (Scope, error) {
	scope := Scope(value)
	if scope == ScopeProject || scope == ScopeGlobal || allowAll && scope == ScopeAll {
		return scope, nil
	}
	return "", fmt.Errorf("invalid memory scope %q", value)
}

func parseCategory(value string) (Category, error) {
	category := Category(value)
	if category == CategoryPreference || category == CategoryConvention || category == CategoryNote {
		return category, nil
	}
	return "", fmt.Errorf("invalid memory category %q", value)
}

type DirectoryResolver interface {
	Resolve(context.Context) (string, error)
}

type App struct {
	Repo     *Repository
	Resolver DirectoryResolver
	Stdout   io.Writer
}

func (a App) Run(ctx context.Context, command Command) error {
	if command.Help {
		_, err := fmt.Fprintln(a.Stdout, Usage)
		return err
	}
	if command.Kind == CommandDirectory {
		directory, err := a.Resolver.Resolve(ctx)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(a.Stdout, directory)
		return err
	}
	projectDirectory := ""
	if command.Scope != ScopeGlobal {
		var err error
		projectDirectory, err = a.Resolver.Resolve(ctx)
		if err != nil {
			return err
		}
	}
	switch command.Kind {
	case CommandRead:
		entries, err := a.Repo.Read(ctx, command.Scope, projectDirectory)
		if err != nil {
			return err
		}
		return renderTable(a.Stdout, entries)
	case CommandWrite:
		return a.Repo.Write(ctx, command.Memory, command.Category, command.Scope, projectDirectory)
	case CommandArchive:
		return a.Repo.Archive(ctx, command.ID, command.Scope, projectDirectory)
	}
	return nil
}

func renderTable(output io.Writer, entries []Entry) error {
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(writer, "id\tscope\tproject_directory\tcategory\tmemory\tupdated_at"); err != nil {
		return err
	}
	for _, entry := range entries {
		memory := strings.Join(strings.Fields(entry.Memory), " ")
		if _, err := fmt.Fprintf(writer, "%d\t%s\t%s\t%s\t%s\t%s\n", entry.ID, entry.Scope, entry.ProjectDirectory, entry.Category, memory, entry.UpdatedAt); err != nil {
			return err
		}
	}
	return writer.Flush()
}
