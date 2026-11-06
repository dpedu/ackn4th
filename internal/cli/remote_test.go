package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dethmasque/ackn4th/internal/config"
	"github.com/dethmasque/ackn4th/internal/event"
	"github.com/dethmasque/ackn4th/internal/task"
	"github.com/dethmasque/ackn4th/internal/ui"
)

// remoteApp is a client of a controller that is not there: enough to make
// a.remote() true without a socket.
func deadCommApp(t *testing.T, cfg config.Config) *app {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() // refused from here on
	g, err := task.OpenGRPC("ackn://"+addr, t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	a := &app{cfg: cfg, be: g, store: task.NewStore(t.TempDir()), term: ui.NewTerm(io.Discard, false),
		cfgSource: cfgFromController}
	if !a.remote() {
		t.Fatal("the app is not in remote mode, so these tests prove nothing")
	}
	return a
}

// A client's working directory is not a place on the controller. Without a
// --workspace the task must land where the daemon works, which it publishes as
// default_workspace_root: sending this machine's cwd made the daemon's runner
// refuse the task with "not a trusted directory (trust it with ackn4th trust
// /Users/you/…)" - advice for the wrong machine - and a same-named directory
// there would have made it work somewhere nobody asked for.
func TestRemoteWorkspaceComesFromTheDaemon(t *testing.T) {
	cfg := config.Default()
	cfg.WorkspaceRoots = []string{"/srv/work"}
	cfg.DefaultWorkspaceRoot = "/srv/work"
	a := deadCommApp(t, cfg)

	for _, tc := range []struct {
		name  string
		flags runFlags
		want  string
	}{
		{"no workspace", runFlags{}, "/srv/work"},
		{"a relative one", runFlags{workspace: "proj"}, "/srv/work/proj"},
		{"an absolute one", runFlags{workspace: "/srv/other/thing"}, "/srv/other/thing"},
		{"scratch", runFlags{scratch: true}, "/srv/work/k3f9qz"},
		{"repo", runFlags{repo: "dethmasque/ackn4th"}, "/srv/work/ackn4th"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := a.taskWorkspace(context.Background(), &tc.flags, "k3f9qz")
			if err != nil {
				t.Fatalf("taskWorkspace: %v", err)
			}
			if got != tc.want {
				t.Errorf("workspace = %q, want %q", got, tc.want)
			}
		})
	}
}

// With no default root published, the client cannot guess and must say so
// rather than send its own directory.
func TestRemoteWorkspaceWithoutADefaultRootSaysSo(t *testing.T) {
	cfg := config.Default()
	cfg.WorkspaceRoots = []string{"/srv/work"}
	cfg.DefaultWorkspaceRoot = ""
	a := deadCommApp(t, cfg)
	if got, err := a.taskWorkspace(context.Background(), &runFlags{}, "k3f9qz"); err == nil {
		t.Fatalf("no workspace and no default root resolved to %q, which can only be this machine's", got)
	}
	if got, err := a.taskWorkspace(context.Background(), &runFlags{workspace: "proj"}, "k3f9qz"); err == nil {
		t.Fatalf("a relative workspace with no default root resolved to %q", got)
	}
}

// away is a backend whose reads fail while a controller is restarting. It
// embeds the file store for everything else, and its Reconcile is a no-op like
// task.GRPC's: a client that cannot see the daemon's locks must not decide a
// task has died.
type away struct {
	*task.Store
	down atomic.Bool
}

func (b *away) Reconcile(*task.Task) {}

func (b *away) ReadEvents(id string, off int64) ([]event.Event, int64, error) {
	if b.down.Load() {
		return nil, off, errAway
	}
	return b.Store.ReadEvents(id, off)
}

func (b *away) Load(id string) (*task.Task, error) {
	if b.down.Load() {
		return nil, errAway
	}
	return b.Store.Load(id)
}

// errAway has the shape rpcErrAt really produces for an unreachable server, so
// a retry scoped to that sentinel is what this observes.
var errAway = fmt.Errorf("%w at %s: %s", task.ErrNoController, "127.0.0.1:1", "not reachable")

func TestWatchWaitsForTheControllerItStartedUnder(t *testing.T) {
	store := task.NewStore(t.TempDir())
	id := task.NewID()
	if err := store.Create(&task.Task{ID: id, Prompt: "watch", Workspace: t.TempDir(),
		Agents: []task.AgentSpec{{Role: "qa", Provider: "mock", Model: "m"}}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	tk, err := store.Load(id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	tk.Status = task.Completed // so the watcher returns once it can read
	if err := store.Save(tk); err != nil {
		t.Fatalf("save: %v", err)
	}

	be := &away{Store: store}
	be.down.Store(true)
	a := &app{cfg: config.Default(), store: store, be: be, term: ui.NewTerm(io.Discard, false)}

	done := make(chan error, 1)
	go func() { done <- a.watch(context.Background(), id, true) }()
	select {
	case err := <-done:
		t.Fatalf("watch-task gave up before the controller answered once: %v", err)
	case <-time.After(300 * time.Millisecond):
		// still waiting. correct.
	}
	be.down.Store(false)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("watch-task failed after the controller came back: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watch-task never picked the task up once the controller was back")
	}
}

// The repository has to travel: the clone happens on the host that runs the
// task, which is the host with the disk and the gh login. It travels in the
// record, so the record must carry it.
func TestTheRecordCarriesTheRepository(t *testing.T) {
	b, err := json.Marshal(&task.Task{ID: "k3f9qz", Prompt: "p", Repo: "dethmasque/ackn4th"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back task.Task
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Repo != "dethmasque/ackn4th" {
		t.Fatalf("the repository did not survive the wire: %s", b)
	}
}

// The client says where the work will happen; the controller decides where to
// clone. They agree only if both ask the same function.
func TestRepoPathIsWhereTheClientSaysTheTaskWillWork(t *testing.T) {
	cfg := config.Default()
	cfg.WorkspaceRoots = []string{"/srv/work"}
	cfg.DefaultWorkspaceRoot = "/srv/work"
	a := deadCommApp(t, cfg)

	ws, err := a.taskWorkspace(context.Background(), &runFlags{repo: "dethmasque/ackn4th"}, "k3f9qz")
	if err != nil {
		t.Fatalf("--repo in remote mode: %v", err)
	}
	clone, err := a.repoPath("dethmasque/ackn4th", []string{"/srv/work"})
	if err != nil {
		t.Fatalf("repoPath: %v", err)
	}
	if ws != clone {
		t.Errorf("the client names %q and the controller would clone into %q", ws, clone)
	}
	if _, err := a.repoPath("./not-a-repo", []string{"/srv/work"}); err == nil {
		t.Error("repoPath accepted something that is not owner/name")
	}
	// The name becomes a directory inside the root, so a name that is only dots
	// names the root or its parent. Since the daemon computes this path and
	// records it, accepting one would put the task outside its workspace root.
	for _, repo := range []string{"owner/.", "owner/..", "owner/..git", "owner/...", "owner/../etc"} {
		if dir, err := a.repoPath(repo, []string{"/srv/work"}); err == nil {
			t.Errorf("repoPath(%q) = %q, which is not a directory inside the root", repo, dir)
		}
	}
	if _, err := a.repoPath("owner/name", nil); err == nil {
		t.Error("repoPath named a path with no workspace root to put it in")
	}
}

// A task with no repository must not make the controller clone anything: every
// start goes through prepareWorkspace.
func TestPrepareWorkspaceDoesNothingWithoutARepository(t *testing.T) {
	store := task.NewStore(t.TempDir())
	id := task.NewID()
	tk := &task.Task{ID: id, Prompt: "p", Workspace: "/srv/work/thing",
		Agents: []task.AgentSpec{{Role: "qa", Provider: "mock", Model: "m"}}}
	if err := store.Create(tk); err != nil {
		t.Fatalf("create: %v", err)
	}
	a := &app{cfg: config.Default(), be: store, store: store, term: ui.NewTerm(io.Discard, false)}
	if err := a.prepareWorkspace(context.Background(), tk); err != nil {
		t.Fatalf("prepareWorkspace with no repository: %v", err)
	}
	if tk.Workspace != "/srv/work/thing" {
		t.Errorf("prepareWorkspace moved a task that named no repository to %q", tk.Workspace)
	}
}
