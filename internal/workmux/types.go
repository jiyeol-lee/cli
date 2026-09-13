package workmux

import (
	"context"
	"io"
	"os"
)

type Workspace struct {
	ID            string `json:"id"`
	RepoID        string `json:"repo_id"`
	Root          string `json:"root"`
	CommonDir     string `json:"common_dir"`
	Path          string `json:"path"`
	Branch        string `json:"branch"`
	Handle        string `json:"handle"`
	Layout        string `json:"layout,omitempty"`
	Container     string `json:"container,omitempty"`
	Window        string `json:"window,omitempty"`
	Socket        string `json:"socket,omitempty"`
	ServerPID     int    `json:"server_pid,omitempty"`
	SocketDevice  uint64 `json:"socket_device,omitempty"`
	SocketInode   uint64 `json:"socket_inode,omitempty"`
	Stage         string `json:"stage"`
	CreatedBranch bool   `json:"created_branch,omitempty"`
	MergeTarget   string `json:"merge_target,omitempty"`
	MergedCommit  string `json:"merged_commit,omitempty"`
	Config        Config `json:"config"`
}

type Sandbox interface {
	Check(context.Context, SandboxConfig) error
	Ensure(context.Context, Workspace) error
	Exec(context.Context, Workspace, string, []string, io.Reader, io.Writer, io.Writer) error
	PaneCommand(Workspace, string) []string
	Stop(context.Context, Workspace) error
	Remove(context.Context, Workspace) error
}

type Multiplexer interface {
	Session(context.Context) (string, error)
	Server(context.Context) (string, error)
	Find(context.Context, Workspace) (string, error)
	Create(context.Context, string, Workspace, []Pane, [][]string) (string, error)
	Focus(context.Context, Workspace, string) error
	Close(context.Context, Workspace) error
	Capture(context.Context, Workspace, string) (CleanupWindow, error)
	CloseCaptured(context.Context, Workspace, CleanupWindow) error
	CapturedExists(context.Context, Workspace, CleanupWindow) (bool, error)
}

type CleanupWindow struct {
	ID           string `json:"id"`
	Token        string `json:"token"`
	Socket       string `json:"socket"`
	Caller       bool   `json:"caller"`
	ServerPID    int    `json:"server_pid,omitempty"`
	SocketDevice uint64 `json:"socket_device,omitempty"`
	SocketInode  uint64 `json:"socket_inode,omitempty"`
}

type CleanupCommand struct {
	RepoID, ID, Token string
}

type CleanupPaths struct {
	HomeDir, StateDir, ConfigDir string
}

type CleanupLaunch struct {
	Command CleanupCommand
	Paths   CleanupPaths
	Root    string
	Stderr  *os.File
}

type CleanupSpawner interface {
	Start(context.Context, CleanupLaunch) error
}
