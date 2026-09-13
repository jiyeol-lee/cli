package workmux

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

type Tmux struct {
	Runner Runner
	Getenv func(string) string
}

func (tmux Tmux) run(ctx context.Context, args ...string) ([]byte, error) {
	runner := tmux.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	literal := make([]string, len(args))
	for i, arg := range args {
		// tmux parses a trailing semicolon as a command separator even in an argv element.
		if prefix, ok := strings.CutSuffix(arg, ";"); ok {
			arg = prefix + "\\;"
		}
		literal[i] = arg
	}
	return runner.Run(ctx, Process{Name: "tmux", Args: literal, Dir: "/"})
}

func tmuxLiteral(value string) string {
	return strings.ReplaceAll(value, "#", "##")
}

func (tmux Tmux) runWorkspace(ctx context.Context, w Workspace, args ...string) ([]byte, error) {
	if w.Socket != "" {
		if !filepath.IsAbs(w.Socket) || strings.ContainsAny(w.Socket, "\x00\r\n") {
			return nil, fmt.Errorf("invalid workspace tmux socket")
		}
		args = append([]string{"-S", w.Socket}, args...)
	}
	return tmux.run(ctx, args...)
}

func tmuxID(value string, prefix byte) bool {
	if len(value) < 2 || value[0] != prefix {
		return false
	}
	for _, char := range value[1:] {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func (tmux Tmux) Session(ctx context.Context) (string, error) {
	getenv := tmux.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	pane := getenv("TMUX_PANE")
	if getenv("TMUX") == "" || !tmuxID(pane, '%') {
		return "", fmt.Errorf("add and open require a live tmux pane ($TMUX and $TMUX_PANE)")
	}
	version, err := tmux.run(ctx, "-V")
	if err != nil {
		return "", err
	}
	if !supportedTmux(strings.TrimSpace(string(version))) {
		return "", fmt.Errorf("tmux 3.2 or newer is required for direct pane commands")
	}
	out, err := tmux.run(ctx, "display-message", "-p", "-t", pane, "#{session_id}\t#{pane_id}")
	if err != nil {
		return "", fmt.Errorf("find current tmux session: %w", err)
	}
	session, actual, ok := strings.Cut(strings.TrimSuffix(string(out), "\n"), "\t")
	if !ok || actual != pane || !tmuxID(session, '$') {
		return "", fmt.Errorf("$TMUX_PANE does not identify a live tmux pane")
	}
	return session, nil
}

func (tmux Tmux) Server(ctx context.Context) (string, error) {
	out, err := tmux.run(ctx, "display-message", "-p", "#{socket_path}")
	if err != nil {
		return "", err
	}
	path := strings.TrimSuffix(string(out), "\n")
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
		return "", fmt.Errorf("tmux returned an invalid socket path")
	}
	return path, nil
}

func (tmux Tmux) CaptureServer(ctx context.Context, socket string) (CleanupWindow, error) {
	if !filepath.IsAbs(socket) || strings.ContainsAny(socket, "\x00\r\n") {
		return CleanupWindow{}, fmt.Errorf("invalid tmux socket path")
	}
	before, err := tmuxSocketIdentity(socket)
	if err != nil {
		return CleanupWindow{}, err
	}
	out, err := tmux.run(ctx, "-S", socket, "display-message", "-p", "#{pid}\t#{socket_path}")
	if err != nil {
		return CleanupWindow{}, err
	}
	fields := strings.Split(strings.TrimSuffix(string(out), "\n"), "\t")
	if len(fields) != 2 || fields[1] != socket {
		return CleanupWindow{}, fmt.Errorf("tmux server socket identity changed")
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 1 {
		return CleanupWindow{}, fmt.Errorf("invalid tmux server PID")
	}
	after, err := tmuxSocketIdentity(socket)
	if err != nil {
		return CleanupWindow{}, err
	}
	if before != after {
		return CleanupWindow{}, fmt.Errorf("tmux socket changed during server capture")
	}
	return CleanupWindow{Socket: socket, ServerPID: pid, SocketDevice: before.Device, SocketInode: before.Inode}, nil
}

func workspaceServer(w Workspace) CleanupWindow {
	return CleanupWindow{Socket: w.Socket, ServerPID: w.ServerPID, SocketDevice: w.SocketDevice, SocketInode: w.SocketInode}
}

func uncertainTmuxServer() error {
	return fmt.Errorf("cannot prove the recorded tmux window/server is gone; restore its original socket connection or close the original server, then retry")
}

func (tmux Tmux) workspaceServerGone(ctx context.Context, w Workspace) (bool, error) {
	if w.ServerPID == 0 {
		return false, nil
	}
	baseline := workspaceServer(w)
	missing, err := capturedSocketMissing(baseline)
	if err != nil {
		return false, fmt.Errorf("%w: %v", uncertainTmuxServer(), err)
	}
	if !missing {
		current, err := tmux.CaptureServer(ctx, w.Socket)
		if err == nil {
			if current.ServerPID != baseline.ServerPID || current.SocketDevice != baseline.SocketDevice || current.SocketInode != baseline.SocketInode {
				return false, fmt.Errorf("%w: server identity changed", uncertainTmuxServer())
			}
			return false, nil
		}
		if !tmuxAbsent(err) {
			return false, err
		}
		if _, err := capturedSocketMissing(baseline); err != nil {
			return false, fmt.Errorf("%w: %v", uncertainTmuxServer(), err)
		}
	}
	exited, err := tmuxServerExited(w.ServerPID)
	if err != nil {
		return false, err
	}
	if !exited {
		return false, uncertainTmuxServer()
	}
	return true, nil
}

func supportedTmux(version string) bool {
	version = strings.TrimPrefix(version, "tmux ")
	major, rest, ok := strings.Cut(version, ".")
	if !ok {
		return false
	}
	n, err := strconv.Atoi(major)
	if err != nil {
		return false
	}
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	minor, err := strconv.Atoi(rest[:end])
	return err == nil && (n > 3 || n == 3 && minor >= 2)
}

func (tmux Tmux) Find(ctx context.Context, w Workspace) (string, error) {
	if !validID(w.ID) {
		return "", fmt.Errorf("invalid workspace identity for tmux")
	}
	if gone, err := tmux.workspaceServerGone(ctx, w); err != nil || gone {
		return "", err
	}
	out, err := tmux.runWorkspace(ctx, w, "list-windows", "-a", "-F", "#{window_id}\t#{@cli_workmux_id}")
	if err != nil {
		if tmuxAbsent(err) {
			if w.ServerPID != 0 {
				gone, checkErr := tmux.workspaceServerGone(ctx, w)
				if checkErr != nil || gone {
					return "", checkErr
				}
			}
			if w.Window != "" || w.Socket != "" {
				return "", uncertainTmuxServer()
			}
			return "", nil
		}
		return "", fmt.Errorf("find owned tmux window: %w", err)
	}
	if gone, err := tmux.workspaceServerGone(ctx, w); err != nil || gone {
		return "", err
	}
	window := ""
	for line := range strings.SplitSeq(strings.TrimSuffix(string(out), "\n"), "\n") {
		id, owner, ok := strings.Cut(line, "\t")
		if ok && w.ServerPID != 0 && id == w.Window && owner != w.ID {
			return "", fmt.Errorf("recorded tmux window ownership changed; restore ownership or close that window before retrying")
		}
		if !ok || owner != w.ID {
			continue
		}
		if !tmuxID(id, '@') {
			return "", fmt.Errorf("invalid owned tmux window ID")
		}
		if window != "" && window != id {
			return "", fmt.Errorf("multiple tmux windows claim this workspace; refusing to guess")
		}
		window = id
	}
	if window == "" && w.ServerPID == 0 && (w.Window != "" || w.Socket != "") {
		return "", uncertainTmuxServer()
	}
	return window, nil
}

func (tmux Tmux) owned(ctx context.Context, window string, w Workspace) error {
	if !tmuxID(window, '@') || !validID(w.ID) {
		return fmt.Errorf("invalid tmux ownership identity")
	}
	out, err := tmux.runWorkspace(ctx, w, "display-message", "-p", "-t", window, "#{window_id}\t#{@cli_workmux_id}")
	if err != nil {
		return err
	}
	if strings.TrimSuffix(string(out), "\n") != window+"\t"+w.ID {
		return fmt.Errorf("tmux window ownership changed; refusing to act")
	}
	return nil
}

func directPaneCommand(command []string) []string {
	if len(command) == 1 {
		return []string{"env", command[0]}
	}
	return command
}

func (tmux Tmux) Create(ctx context.Context, session string, w Workspace, panes []Pane, commands [][]string) (string, error) {
	if !tmuxID(session, '$') || !validID(w.ID) || len(panes) == 0 || len(commands) != len(panes) {
		return "", fmt.Errorf("invalid tmux workspace or pane configuration")
	}
	if found, err := tmux.Find(ctx, w); err != nil {
		return "", err
	} else if found != "" {
		return found, fmt.Errorf("workspace already has a tmux window")
	}
	var shell []string
	for _, command := range commands {
		if len(command) == 0 {
			out, err := tmux.run(ctx, "show-options", "-A", "-v", "-t", session, "default-shell")
			if err != nil {
				return "", fmt.Errorf("find tmux default shell: %w", err)
			}
			path := strings.TrimSuffix(string(out), "\n")
			if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
				return "", fmt.Errorf("tmux default-shell must be an absolute executable path")
			}
			shell = []string{path, "-l"}
			break
		}
	}
	// Keep the first pane alive until ownership and remain-on-exit are installed.
	out, err := tmux.run(ctx, "new-window", "-d", "-P", "-F", "#{window_id}\t#{pane_id}",
		"-t", session+":", "-n", tmuxLiteral(w.Handle), "-c", tmuxLiteral(w.Path), "--", "sleep", "2147483647")
	if err != nil {
		return "", fmt.Errorf("create tmux window: %w", err)
	}
	window, first, ok := strings.Cut(strings.TrimSuffix(string(out), "\n"), "\t")
	if !ok || !tmuxID(window, '@') || !tmuxID(first, '%') {
		return "", fmt.Errorf("tmux returned invalid window or pane IDs")
	}
	for _, args := range [][]string{
		{"set-option", "-w", "-t", window, "@cli_workmux_id", w.ID},
		{"set-option", "-w", "-t", window, "remain-on-exit", "on"},
	} {
		if _, err := tmux.run(ctx, args...); err != nil {
			return window, err
		}
	}
	ids := []string{first}
	for i, pane := range panes {
		id := first
		if i > 0 {
			direction := "-v"
			if pane.Split == "vertical" {
				direction = "-h"
			} else if pane.Split != "" && pane.Split != "horizontal" {
				return window, fmt.Errorf("invalid pane split %q", pane.Split)
			}
			args := []string{"split-window", "-d", "-P", "-F", "#{pane_id}", "-t", ids[i-1], direction, "-c", tmuxLiteral(w.Path)}
			if pane.Size != 0 {
				args = append(args, "-l", strconv.Itoa(pane.Size))
			} else if pane.Percentage != 0 {
				args = append(args, "-l", strconv.Itoa(pane.Percentage)+"%")
			}
			args = append(args, "--", "")
			out, err := tmux.run(ctx, args...)
			if err != nil {
				return window, fmt.Errorf("split tmux pane: %w", err)
			}
			id = strings.TrimSuffix(string(out), "\n")
			if !tmuxID(id, '%') {
				return window, fmt.Errorf("tmux returned an invalid split pane ID")
			}
			ids = append(ids, id)
		}
		args := []string{"respawn-pane", "-k", "-t", id, "-c", tmuxLiteral(w.Path)}
		command := commands[i]
		if len(command) == 0 {
			command = shell
		}
		args = append(args, "--")
		args = append(args, directPaneCommand(command)...)
		if _, err := tmux.run(ctx, args...); err != nil {
			return window, fmt.Errorf("start tmux pane: %w", err)
		}
	}
	focus := first
	for i, pane := range panes {
		if pane.Focus {
			focus = ids[i]
		}
	}
	if _, err := tmux.run(ctx, "select-pane", "-t", focus); err != nil {
		return window, err
	}
	for i, pane := range panes {
		if pane.Zoom {
			if _, err := tmux.run(ctx, "resize-pane", "-Z", "-t", ids[i]); err != nil {
				return window, err
			}
		}
	}
	return window, nil
}

func (tmux Tmux) Focus(ctx context.Context, w Workspace, window string) error {
	if !tmuxID(window, '@') {
		return fmt.Errorf("invalid tmux window ID")
	}
	if _, err := tmux.Session(ctx); err != nil {
		return err
	}
	if w.Socket != "" {
		server, err := tmux.Server(ctx)
		if err != nil {
			return err
		}
		if server != w.Socket {
			return fmt.Errorf("workspace window belongs to another tmux server; open it from that server")
		}
	}
	if err := tmux.owned(ctx, window, w); err != nil {
		return err
	}
	out, err := tmux.run(ctx, "display-message", "-p", "-t", window, "#{window_id}")
	if err != nil {
		return err
	}
	if strings.TrimSuffix(string(out), "\n") != window {
		return fmt.Errorf("tmux window is no longer present")
	}
	_, err = tmux.run(ctx, "switch-client", "-t", window)
	return err
}

func (tmux Tmux) Close(ctx context.Context, w Workspace) error {
	window, err := tmux.Find(ctx, w)
	if err != nil || window == "" {
		return err
	}
	if err := tmux.owned(ctx, window, w); err != nil {
		return err
	}
	_, err = tmux.runWorkspace(ctx, w, "kill-window", "-t", window)
	return err
}

func tmuxAbsent(err error) bool {
	message := err.Error()
	if strings.Contains(message, "Permission denied") || strings.Contains(message, "permission denied") || strings.Contains(message, "Connection refused") {
		return false
	}
	return strings.Contains(message, "no server running on ") || strings.HasSuffix(message, "no sessions") ||
		strings.Contains(message, "error connecting to ") && strings.Contains(message, "No such file or directory")
}

func (tmux Tmux) Capture(ctx context.Context, w Workspace, token string) (CleanupWindow, error) {
	captured := workspaceServer(w)
	captured.Token = token
	if !validID(token) {
		return captured, fmt.Errorf("invalid cleanup window token")
	}
	window, err := tmux.Find(ctx, w)
	if err != nil {
		return captured, err
	}
	if window == "" {
		if w.Window != "" && w.Socket == "" {
			return captured, fmt.Errorf("recorded tmux socket is missing; refusing ambiguous cleanup")
		}
		return captured, err
	}
	out, err := tmux.runWorkspace(ctx, w, "display-message", "-p", "-t", window, "#{window_id}\t#{@cli_workmux_id}\t#{socket_path}\t#{pid}")
	if err != nil {
		return captured, err
	}
	fields := strings.Split(strings.TrimSuffix(string(out), "\n"), "\t")
	if len(fields) != 4 || fields[0] != window || fields[1] != w.ID || !filepath.IsAbs(fields[2]) || strings.ContainsAny(fields[2], "\x00\r\n") {
		return captured, fmt.Errorf("tmux window or socket identity changed")
	}
	captured.ID, captured.Socket = window, fields[2]
	captured.ServerPID, err = strconv.Atoi(fields[3])
	if err != nil || captured.ServerPID <= 1 {
		return captured, fmt.Errorf("tmux returned an invalid server PID")
	}
	socket, err := tmuxSocketIdentity(captured.Socket)
	if err != nil {
		return captured, err
	}
	captured.SocketDevice, captured.SocketInode = socket.Device, socket.Inode
	if w.ServerPID != 0 && (captured.ServerPID != w.ServerPID || captured.SocketDevice != w.SocketDevice || captured.SocketInode != w.SocketInode) {
		return captured, uncertainTmuxServer()
	}
	getenv := tmux.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if pane := getenv("TMUX_PANE"); getenv("TMUX") != "" && tmuxID(pane, '%') {
		out, err := tmux.run(ctx, "display-message", "-p", "-t", pane, "#{pane_id}\t#{window_id}\t#{socket_path}")
		if err != nil {
			return captured, err
		}
		fields := strings.Split(strings.TrimSuffix(string(out), "\n"), "\t")
		if len(fields) != 3 || fields[0] != pane || !tmuxID(fields[1], '@') || !filepath.IsAbs(fields[2]) {
			return captured, fmt.Errorf("caller tmux pane identity changed")
		}
		captured.Caller = fields[1] == window && fields[2] == captured.Socket
	}
	guard := fmt.Sprintf("#{&&:#{==:#{pid},%d},#{&&:#{==:#{window_id},%s},#{==:#{@cli_workmux_id},%s}}}", captured.ServerPID, window, w.ID)
	out, err = tmux.runWorkspace(ctx, w, "if-shell", "-F", "-t", window, guard,
		"set-option -w -t "+window+" @cli_workmux_cleanup "+token, "display-message -p cli-workmux-capture-refused")
	if err != nil {
		return captured, err
	}
	if strings.TrimSpace(string(out)) != "" {
		return captured, fmt.Errorf("tmux refused window ownership during capture")
	}
	if present, err := tmux.CapturedExists(ctx, w, captured); err != nil || !present {
		if err == nil {
			err = fmt.Errorf("tmux server or window disappeared during capture")
		}
		return captured, err
	}
	return captured, nil
}

func tmuxSocketIdentity(path string) (fileIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return fileIdentity{}, err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fileIdentity{}, fmt.Errorf("captured tmux socket is not a socket")
	}
	stat := info.Sys().(*syscall.Stat_t)
	return fileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}, nil
}

func capturedSocketMissing(window CleanupWindow) (bool, error) {
	if window.ServerPID <= 1 || window.SocketInode == 0 {
		return false, fmt.Errorf("captured tmux server identity is missing; refusing ambiguous cleanup")
	}
	id, err := tmuxSocketIdentity(window.Socket)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if id.Device != window.SocketDevice || id.Inode != window.SocketInode {
		return false, fmt.Errorf("captured tmux socket was replaced or rebound to another server")
	}
	return false, nil
}

func tmuxServerExited(pid int) (bool, error) {
	if pid <= 1 {
		return false, fmt.Errorf("invalid captured tmux server PID")
	}
	err := syscall.Kill(pid, 0)
	if err == syscall.ESRCH {
		return true, nil
	}
	if err != nil && err != syscall.EPERM {
		return false, fmt.Errorf("check captured tmux server: %w", err)
	}
	if runtime.GOOS == "linux" {
		// An exited server can await init's reaper. Never mistake a live/reused PID for exit.
		data, readErr := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if readErr == nil {
			prefix, rest, ok := strings.Cut(string(data), ") ")
			if ok && strings.HasPrefix(prefix, strconv.Itoa(pid)+" (") && (strings.HasPrefix(rest, "Z ") || strings.HasPrefix(rest, "X ")) {
				return true, nil
			}
		}
	}
	return false, nil
}

func (tmux Tmux) CapturedExists(ctx context.Context, w Workspace, window CleanupWindow) (bool, error) {
	if !validID(w.ID) || !validID(window.Token) {
		return false, fmt.Errorf("invalid captured cleanup ownership")
	}
	if window.ID == "" {
		if window.ServerPID != 0 {
			w.Socket, w.ServerPID = window.Socket, window.ServerPID
			w.SocketDevice, w.SocketInode = window.SocketDevice, window.SocketInode
		}
		found, err := tmux.Find(ctx, w)
		if err != nil {
			return false, err
		}
		if found != "" {
			return false, fmt.Errorf("a new owned tmux window appeared during cleanup")
		}
		return false, nil
	}
	if !tmuxID(window.ID, '@') || !filepath.IsAbs(window.Socket) || strings.ContainsAny(window.Socket, "\x00\r\n") {
		return false, fmt.Errorf("invalid captured tmux target")
	}
	missing, err := capturedSocketMissing(window)
	if err != nil {
		return false, err
	}
	if missing {
		exited, err := tmuxServerExited(window.ServerPID)
		return !exited, err
	}
	out, err := tmux.run(ctx, "-S", window.Socket, "list-windows", "-a", "-F", "#{window_id}\t#{@cli_workmux_id}\t#{@cli_workmux_cleanup}\t#{pid}")
	if err != nil {
		if tmuxAbsent(err) {
			if _, checkErr := capturedSocketMissing(window); checkErr != nil {
				return false, checkErr
			}
			exited, checkErr := tmuxServerExited(window.ServerPID)
			return !exited, checkErr
		}
		return false, err
	}
	if missing, err := capturedSocketMissing(window); err != nil || missing {
		if err == nil {
			err = fmt.Errorf("captured tmux socket disappeared while inspecting its windows")
		}
		return false, err
	}
	present := false
	for line := range strings.SplitSeq(strings.TrimSuffix(string(out), "\n"), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 4 || fields[3] != strconv.Itoa(window.ServerPID) {
			return false, fmt.Errorf("invalid tmux ownership listing during cleanup")
		}
		if fields[0] == window.ID {
			if fields[1] != w.ID || fields[2] != window.Token {
				return false, fmt.Errorf("captured tmux ID was reused or its ownership token changed")
			}
			present = true
		} else if fields[1] == w.ID {
			return false, fmt.Errorf("a replacement owned tmux window appeared during cleanup")
		}
	}
	return present, nil
}

func (tmux Tmux) CloseCaptured(ctx context.Context, w Workspace, window CleanupWindow) error {
	present, err := tmux.CapturedExists(ctx, w, window)
	if err != nil || !present {
		return err
	}
	guard := fmt.Sprintf("#{&&:#{==:#{pid},%d},#{&&:#{==:#{window_id},%s},#{&&:#{==:#{@cli_workmux_id},%s},#{==:#{@cli_workmux_cleanup},%s}}}}",
		window.ServerPID, window.ID, w.ID, window.Token)
	// -F evaluates and selects the guarded kill in the server command queue, without a shell.
	out, err := tmux.run(ctx, "-S", window.Socket, "if-shell", "-F", "-t", window.ID, guard,
		"kill-window -t "+window.ID, "display-message -p cli-workmux-close-refused")
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(out))) != 0 {
		return fmt.Errorf("tmux server refused captured window ownership; no cleanup was authorized")
	}
	return nil
}
