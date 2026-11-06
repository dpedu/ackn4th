package cli

// Tests for the control plane: a CLI on one machine asking the controller on
// the daemon host to start or stop a task.
//
// The stub is a shared backend built on the file store, not a second backend:
// what is being tested here is the client's routing - does a start in remote
// mode become a request, or does it spawn a runner on this machine - and that
// is the same question whatever carries the request. The wire itself is
// covered in internal/task (the client/server pair over a real socket) and by
// QA's live-stop test, which drives a real child process through a real
// controller.

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/dethmasque/ackn4th/internal/config"
	"github.com/dethmasque/ackn4th/internal/task"
	"github.com/dethmasque/ackn4th/internal/ui"
)

// remoteApp wires an app whose task state lives on a controller.
func remoteApp(t *testing.T, be task.Backend) *app {
	t.Helper()
	a := &app{
		cfg:       config.Default(),
		store:     task.NewStore(t.TempDir()),
		be:        be,
		term:      ui.NewTerm(io.Discard, false),
		cfgSource: cfgFromController,
	}
	if !a.remote() {
		t.Fatal("the app is not in remote mode, so these tests prove nothing")
	}
	return a
}

// recordingController is a shared backend that records what the CLI asked the
// daemon host to do, so the routing can be checked without a runner. The
// embedded store is the *client's* data dir; nothing remote may write it.
type recordingController struct {
	*task.Store
	mu   sync.Mutex
	sent []task.Request
	resp task.Response
	err  error
}

func newController(t *testing.T) *recordingController {
	t.Helper()
	c := &recordingController{Store: task.NewStore(t.TempDir())}
	if !task.Shared(c).Shared() {
		t.Fatal("the stub is not shared: the client would act locally")
	}
	return c
}

// Shared is the whole point of the stub: a backend another machine owns.
func (c *recordingController) Shared() bool { return true }

// Reconcile is a no-op on a client: the daemon reconciles its own records.
func (c *recordingController) Reconcile(*task.Task) {}

func (c *recordingController) Send(ctx context.Context, req task.Request, wait time.Duration) (task.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, req)
	return c.resp, c.err
}

func (c *recordingController) ControllerAlive() bool { return true }

func (c *recordingController) took(t *testing.T, action task.Action) task.Request {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sent) != 1 {
		t.Fatalf("the CLI sent %d requests, want 1", len(c.sent))
	}
	if c.sent[0].Action != action {
		t.Fatalf("the CLI sent %q, want %q", c.sent[0].Action, action)
	}
	return c.sent[0]
}

// Starting a task in remote mode must go to the daemon host, not spawn a
// runner on this machine.
func TestRemoteStartGoesThroughTheController(t *testing.T) {
	ctrl := newController(t)
	ctrl.resp = task.Response{OK: true, Message: "started (pid 42)"}
	a := remoteApp(t, ctrl)

	tk := &task.Task{ID: task.NewID(), Prompt: "p", Agents: []task.AgentSpec{{Role: "backend_dev"}}}
	if err := ctrl.Create(tk); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := a.spawnRunner(context.Background(), tk.ID); err != nil {
		t.Fatalf("spawnRunner: %v", err)
	}
	if req := ctrl.took(t, task.ActionStart); req.Task != tk.ID {
		t.Fatalf("asked to start %q, want %q", req.Task, tk.ID)
	}
	// Nothing was started here.
	if locked, err := a.store.Locked(tk.ID); err != nil || locked {
		t.Fatalf("a local runner was spawned: locked=%v err=%v", locked, err)
	}
}

// Stopping a remote task must not signal the pid the daemon recorded: that pid
// belongs to another host, where it is somebody else's process.
func TestRemoteStopDoesNotSignalLocally(t *testing.T) {
	ctrl := newController(t)
	ctrl.resp = task.Response{OK: true, Message: "stopped (pid 42)"}
	a := remoteApp(t, ctrl)

	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = child.Wait(); close(done) }()
	defer func() { _ = child.Process.Kill(); <-done }()

	tk := &task.Task{ID: task.NewID(), Prompt: "p", Status: task.Running, PID: child.Process.Pid,
		Agents: []task.AgentSpec{{Role: "backend_dev"}}}
	if err := ctrl.Create(tk); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := ctrl.Save(tk); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := a.stopRunner(context.Background(), tk); err != nil {
		t.Fatalf("stopRunner: %v", err)
	}
	ctrl.took(t, task.ActionStop)
	select {
	case <-done:
		t.Fatalf("the CLI signalled local pid %d, which is the daemon host's", child.Process.Pid)
	case <-time.After(300 * time.Millisecond):
	}
}

// With nobody watching, a start must fail loudly rather than look queued.
func TestRemoteStartWithoutAControllerFails(t *testing.T) {
	ctrl := newController(t)
	ctrl.err = task.ErrNoController
	a := remoteApp(t, ctrl)

	tk := &task.Task{ID: task.NewID(), Prompt: "p", Agents: []task.AgentSpec{{Role: "backend_dev"}}}
	if err := ctrl.Create(tk); err != nil {
		t.Fatalf("Create: %v", err)
	}

	err := a.spawnRunner(context.Background(), tk.ID)
	if !errors.Is(err, task.ErrNoController) {
		t.Fatalf("spawnRunner without a controller = %v, want ErrNoController", err)
	}
}

// A controller keeps running when its terminal's environment lacks the
// backend: it must say so instead of pretending to watch.
func TestControllerRequiresASharedBackend(t *testing.T) {
	st := task.NewStore(t.TempDir())
	a := &app{cfg: config.Default(), be: st, store: st, term: ui.NewTerm(io.Discard, false)}
	cmd := controllerCmd(a)
	cmd.SetArgs(nil)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	if err := cmd.Execute(); err == nil {
		t.Fatal("the controller started without a shared backend")
	}
}

// The stop path must not fall back to a local signal when the daemon is
// unreachable: the pid is not this host's, and "I could not ask" is not "it
// stopped".
func TestRemoteStopWithoutAControllerFails(t *testing.T) {
	ctrl := newController(t)
	ctrl.err = task.ErrNoController
	a := remoteApp(t, ctrl)

	child := exec.Command("sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = child.Wait(); close(done) }()
	defer func() { _ = child.Process.Kill(); <-done }()

	tk := &task.Task{ID: task.NewID(), Prompt: "p", Status: task.Running, PID: child.Process.Pid}
	if err := ctrl.Create(tk); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := a.stopRunner(context.Background(), tk); !errors.Is(err, task.ErrNoController) {
		t.Fatalf("stopRunner with no controller = %v, want ErrNoController", err)
	}
	select {
	case <-done:
		t.Fatal("the CLI signalled a pid from the daemon host after failing to reach it")
	case <-time.After(200 * time.Millisecond):
	}
}

var _ = os.Getenv // keep os for the helper above on every platform
