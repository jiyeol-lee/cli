package workmux

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Tmux struct {
	Runner  Runner
	Getenv  func(string) string
	TempDir string
}

func (tmux Tmux) AgentPath(ctx context.Context) (string, error) {
	out, err := tmux.run(ctx, "show-environment", "-g", "PATH")
	if err != nil {
		return "", err
	}
	path, ok := strings.CutPrefix(strings.TrimSuffix(string(out), "\n"), "PATH=")
	if !ok {
		return "", nil
	}
	return path, nil
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
	if exited, err := tmuxServerExited(w.ServerPID); err != nil || exited {
		return exited, err
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
		if id == w.Window {
			return id, nil
		}
		if window == "" || windowNumber(id) < windowNumber(window) {
			window = id
		}
	}
	if window == "" && w.ServerPID == 0 && (w.Window != "" || w.Socket != "") {
		return "", uncertainTmuxServer()
	}
	return window, nil
}

func windowNumber(id string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(id, "@"))
	return n
}

func (tmux Tmux) CurrentWindow(ctx context.Context, w Workspace) (string, error) {
	getenv := tmux.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	pane := getenv("TMUX_PANE")
	if !tmuxID(pane, '%') {
		return "", fmt.Errorf("close without a name requires a current tmux pane")
	}
	out, err := tmux.runWorkspace(ctx, w, "display-message", "-p", "-t", pane, "#{window_id}")
	if err != nil {
		return "", err
	}
	window := strings.TrimSpace(string(out))
	if err := tmux.owned(ctx, window, w); err != nil {
		return "", err
	}
	return window, nil
}

func (tmux Tmux) Adopt(ctx context.Context, w Workspace) (string, error) {
	out, err := tmux.run(ctx, "list-windows", "-a", "-F", "#{window_id}\t#{window_name}\t#{pane_current_path}\t#{@cli_workmux_id}\t#{@workmux_token}")
	if err != nil {
		if tmuxAbsent(err) {
			return "", nil
		}
		return "", err
	}
	var candidates []string
	for line := range strings.SplitSeq(strings.TrimSuffix(string(out), "\n"), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 5 || fields[1] != "wm-"+w.Handle || fields[3] != "" && fields[3] != w.ID {
			continue
		}
		cwd, err := canonicalDirectory(fields[2])
		if err != nil || cwd != w.Path {
			continue
		}
		if !tmuxID(fields[0], '@') {
			return "", fmt.Errorf("invalid adoption window ID")
		}
		candidates = append(candidates, fields[0])
	}
	sort.Slice(candidates, func(i, j int) bool { return windowNumber(candidates[i]) < windowNumber(candidates[j]) })
	for _, id := range candidates {
		// Re-read cwd and name before claiming a window, never use its name alone.
		out, err := tmux.run(ctx, "display-message", "-p", "-t", id, "#{window_name}\t#{pane_current_path}\t#{@cli_workmux_id}")
		if err != nil {
			return "", err
		}
		fields := strings.Split(strings.TrimSuffix(string(out), "\n"), "\t")
		if len(fields) != 3 || fields[0] != "wm-"+w.Handle || fields[1] != w.Path || fields[2] != "" && fields[2] != w.ID {
			return "", fmt.Errorf("tmux window changed during adoption")
		}
		if _, err := tmux.run(ctx, "set-option", "-w", "-t", id, "@cli_workmux_id", w.ID); err != nil {
			return "", err
		}
	}
	if len(candidates) == 0 {
		return "", nil
	}
	return candidates[0], nil
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

func (tmux Tmux) Create(ctx context.Context, session string, w Workspace, panes []Pane, commands [][]string) (string, error) {
	if !tmuxID(session, '$') || !validID(w.ID) || len(commands) != len(panes) {
		return "", fmt.Errorf("invalid tmux workspace or pane configuration")
	}
	if err := validatePanes(panes); err != nil {
		return "", err
	}
	if found, err := tmux.Find(ctx, w); err != nil {
		return "", err
	} else if found != "" && !w.NewWindow {
		return found, fmt.Errorf("workspace already has a tmux window")
	}
	// The first pane starts normally. Only a configured command needs a respawn.
	out, err := tmux.run(ctx, "new-window", "-a", "-d", "-P", "-F", "#{window_id}\t#{pane_id}",
		"-t", session+":", "-n", tmuxLiteral("wm-"+w.Handle), "-c", tmuxLiteral(w.Path))
	if err != nil {
		return "", fmt.Errorf("create tmux window: %w", err)
	}
	window, first, ok := strings.Cut(strings.TrimSuffix(string(out), "\n"), "\t")
	if !ok || !tmuxID(window, '@') || !tmuxID(first, '%') {
		return "", fmt.Errorf("tmux returned invalid window or pane IDs")
	}
	for _, args := range [][]string{
		{"set-option", "-w", "-t", window, "@cli_workmux_id", w.ID},
	} {
		if _, err := tmux.run(ctx, args...); err != nil {
			return window, err
		}
	}
	out, err = tmux.run(ctx, "show-options", "-A", "-v", "-t", session, "default-shell")
	if err != nil {
		return window, fmt.Errorf("find tmux default shell: %w", err)
	}
	shell := strings.TrimSuffix(string(out), "\n")
	if !filepath.IsAbs(shell) || strings.ContainsAny(shell, "\x00\r\n") {
		return window, fmt.Errorf("tmux default-shell must be an absolute executable path")
	}
	ids := []string{first}
	handshakes := make([]string, len(panes))
	defer func() {
		for _, channel := range handshakes {
			if channel != "" {
				tmux.releaseHandshake(ctx, w, channel)
			}
		}
	}()
	for i, pane := range panes {
		if i > 0 {
			direction := "-h"
			if pane.Split == "vertical" {
				direction = "-v"
			} else if pane.Split != "" && pane.Split != "horizontal" {
				return window, fmt.Errorf("invalid pane split %q", pane.Split)
			}
			target := i - 1
			if pane.Target != nil {
				target = *pane.Target
			}
			if target < 0 || target >= i {
				return window, fmt.Errorf("pane target must refer to a previous pane")
			}
			args := []string{"split-window", "-d", "-P", "-F", "#{pane_id}", "-t", ids[target], direction, "-c", tmuxLiteral(w.Path)}
			if pane.SizeSpecified() {
				args = append(args, "-l", strconv.Itoa(pane.Size))
			} else if pane.Percentage != 0 {
				args = append(args, "-l", strconv.Itoa(pane.Percentage)+"%")
			}
			if len(commands[i]) != 0 {
				channel, script, err := tmux.prepareHandshake(ctx, w, shell)
				if err != nil {
					return window, err
				}
				handshakes[i] = channel
				args = append(args, "--", "exec /bin/bash -c "+shellQuote(script, shell))
			}
			out, err := tmux.run(ctx, args...)
			if err != nil {
				return window, fmt.Errorf("split tmux pane: %w", err)
			}
			id := strings.TrimSuffix(string(out), "\n")
			if !tmuxID(id, '%') {
				return window, fmt.Errorf("tmux returned an invalid split pane ID")
			}
			ids = append(ids, id)
		}
	}
	focus := first
	for i, pane := range panes {
		if pane.Focus || pane.Zoom {
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
	for i := range panes {
		id := ids[i]
		if len(commands[i]) == 0 {
			continue
		}
		if i == 0 {
			if err := tmux.startPane(ctx, w, id, shell, commands[i]); err != nil {
				return window, err
			}
			continue
		}
		if err := tmux.waitHandshake(ctx, w, handshakes[i]); err != nil {
			return window, err
		}
		handshakes[i] = ""
		if err := tmux.sendPaneCommand(ctx, w, id, shell, commands[i]); err != nil {
			return window, err
		}
	}
	return window, nil
}

func (tmux Tmux) startPane(ctx context.Context, w Workspace, pane, shell string, command []string) error {
	channel, script, err := tmux.prepareHandshake(ctx, w, shell)
	if err != nil {
		return err
	}
	defer func() {
		if channel != "" {
			tmux.releaseHandshake(ctx, w, channel)
		}
	}()
	if _, err := tmux.runWorkspace(ctx, w, "respawn-pane", "-k", "-t", pane, "-c", tmuxLiteral(w.Path), "--", "exec /bin/bash -c "+shellQuote(script, shell)); err != nil {
		return fmt.Errorf("start tmux shell: %w", err)
	}
	if err := tmux.waitHandshake(ctx, w, channel); err != nil {
		return err
	}
	channel = ""
	return tmux.sendPaneCommand(ctx, w, pane, shell, command)
}

func (tmux Tmux) prepareHandshake(ctx context.Context, w Workspace, shell string) (string, string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", "", err
	}
	channel := "cli-workmux-" + hex.EncodeToString(random[:])
	if _, err := tmux.runWorkspace(ctx, w, "wait-for", "-L", channel); err != nil {
		return "", "", err
	}
	unlock := "tmux "
	if w.Socket != "" {
		unlock += "-S " + shellQuote(w.Socket, "sh") + " "
	}
	unlock += "wait-for -U " + channel
	script := "stty -echo; " + unlock + "; stty echo; exec " + shellQuote(shell, "sh") + " -l"
	return channel, script, nil
}

func (tmux Tmux) releaseHandshake(ctx context.Context, w Workspace, channel string) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	_, _ = tmux.runWorkspace(cleanup, w, "wait-for", "-U", channel)
}

func (tmux Tmux) waitHandshake(ctx context.Context, w Workspace, channel string) error {
	readyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := tmux.runWorkspace(readyCtx, w, "wait-for", "-L", channel); err != nil {
		return fmt.Errorf("wait for tmux shell: %w", err)
	}
	if _, err := tmux.runWorkspace(ctx, w, "wait-for", "-U", channel); err != nil {
		return err
	}
	return nil
}

func (tmux Tmux) sendPaneCommand(ctx context.Context, w Workspace, pane, shell string, command []string) (resultErr error) {
	if len(command) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	line := command[0]
	if len(command) > 1 {
		launch, err := preparePaneLaunch(tmux.TempDir, command)
		if err != nil {
			return err
		}
		defer func() {
			if resultErr != nil {
				resultErr = errors.Join(resultErr, launch.remove())
			}
		}()
		line = nativeShellCommand([]string{"/bin/bash", launch.path}, shell)
		if len(line) > 1024 {
			return fmt.Errorf("private pane launcher path is too long for terminal handoff")
		}
	}
	if _, err := tmux.runWorkspace(ctx, w, "send-keys", "-t", pane, "-l", "--", line); err != nil {
		return err
	}
	_, err := tmux.runWorkspace(ctx, w, "send-keys", "-t", pane, "Enter")
	return err
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

func (tmux Tmux) CaptureAll(ctx context.Context, w Workspace, token string) (CleanupWindow, error) {
	primary, err := tmux.Capture(ctx, w, token)
	if err != nil || primary.ID == "" {
		return primary, err
	}
	out, err := tmux.runWorkspace(ctx, w, "list-windows", "-a", "-F", "#{window_id}\t#{@cli_workmux_id}")
	if err != nil {
		return primary, err
	}
	seen := map[string]bool{primary.ID: true}
	var ids []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		id, owner, ok := strings.Cut(line, "\t")
		if ok && owner == w.ID && !seen[id] {
			if !tmuxID(id, '@') {
				return primary, fmt.Errorf("invalid owned window ID")
			}
			seen[id] = true
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return windowNumber(ids[i]) < windowNumber(ids[j]) })
	for _, id := range ids {
		w.Window = id
		captured, err := tmux.Capture(ctx, w, token)
		if err != nil {
			return primary, err
		}
		primary.Caller = primary.Caller || captured.Caller
		primary.Others = append(primary.Others, captured)
	}
	primary.AllOwned = true
	return primary, nil
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
	othersPresent := false
	for _, other := range window.Others {
		present, err := tmux.CapturedExists(ctx, w, other)
		if err != nil {
			return false, err
		}
		othersPresent = othersPresent || present
	}
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
	if exited, err := tmuxServerExited(window.ServerPID); err != nil || exited {
		return othersPresent, err
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
		} else if window.AllOwned && fields[1] == w.ID {
			known := false
			for _, other := range window.Others {
				if other.ID == fields[0] {
					known = true
				}
			}
			if !known {
				return false, fmt.Errorf("a new owned tmux window appeared during cleanup")
			}
		}
	}
	return present || othersPresent, nil
}

func (tmux Tmux) CloseCaptured(ctx context.Context, w Workspace, window CleanupWindow) error {
	if _, err := tmux.CapturedExists(ctx, w, window); err != nil {
		return err
	}
	for _, other := range window.Others {
		if err := tmux.CloseCaptured(ctx, w, other); err != nil {
			return err
		}
	}
	window.Others = nil
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
