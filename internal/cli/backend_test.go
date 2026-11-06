package cli

// Tests for the choice the README presents: the local endpoint by default, the
// controller named by ACKN4TH_BACKEND_URL when there is one. Which one is live
// decides whether a command acts on this machine or asks another one to. Both
// are the same implementation - the gRPC client - so what these tests pin is
// where the server is, not which type was built.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dethmasque/ackn4th/internal/config"
	"github.com/dethmasque/ackn4th/internal/task"
)

// loadApp builds the app the way the binary does, so these tests cover the
// real selection path rather than a hand-wired app.
func loadApp(t *testing.T, backendURL string) *app {
	t.Helper()
	t.Setenv(config.HomeEnv, t.TempDir())
	t.Setenv(task.EnvBackendURL, backendURL)
	a := &app{noInput: true}
	if err := a.load(); err != nil {
		t.Fatalf("load (ACKN4TH_BACKEND_URL=%q): %v", backendURL, err)
	}
	t.Cleanup(a.closeComm)
	return a
}

// With no ACKN4TH_BACKEND_URL the local endpoint is live and commands act here:
// reading it as "remote" would send every local start and stop to a controller
// that cannot exist.
//
// The assertions are behavioural on purpose. Which type the backend is does not
// matter; what matters is that it is not a client of another host (remote(),
// localRunner()) and that asking it to run a task elsewhere is refused rather
// than silently doing nothing - that refusal is what a local endpoint answers
// instead of a control plane.
func TestDefaultBackendIsLocal(t *testing.T) {
	a := loadApp(t, "")
	if a.remote() {
		t.Fatal("remote() is true with no ACKN4TH_BACKEND_URL: local start/stop would be routed to a controller")
	}
	if !a.localRunner() {
		t.Fatal("localRunner() is false with no ACKN4TH_BACKEND_URL: nothing would run a task on this machine")
	}
	if _, err := a.control(context.Background(), task.ActionStart, "no-such-task"); !errors.Is(err, task.ErrNoController) {
		t.Fatalf("asking the local endpoint to start a task = %v, want ErrNoController: there is no control plane here", err)
	}
	// And the state really is this machine's: a task created here is readable
	// from the data dir the endpoint names.
	tk := &task.Task{ID: task.NewID(), Prompt: "local", Status: task.Pending}
	if err := a.backend().Create(tk); err != nil {
		t.Fatalf("create through the local endpoint: %v", err)
	}
	ep, ok := a.backend().(task.Endpointer)
	if !ok {
		t.Fatal("the local endpoint cannot say where state lives, so status cannot show it")
	}
	if _, err := task.NewStore(ep.Endpoint()).Load(tk.ID); err != nil {
		t.Errorf("the task is not in the data dir the endpoint names (%s): %v", ep.Endpoint(), err)
	}
}

// Every command goes through load(), and load() opens a local backend, so the
// local backend must not need a writable temp dir: otherwise a read-only /tmp
// (read_only containers, most CI images, hardened hosts) makes `ackn4th version`
// fail before it prints a line.
//
// TMPDIR points under a directory that does not exist, so MkdirTemp cannot
// succeed whoever runs the test. The assertion is that the command has a working
// backend afterwards - create, then load the record back - because "load
// returned an error" would not tell us whether a real user can run anything.
func TestQACLIWorksWithoutAWritableTempDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TMPDIR", filepath.Join(home, "no", "such", "tmp"))
	t.Setenv(config.HomeEnv, home)
	t.Setenv(task.EnvBackendURL, "")
	a := &app{noInput: true}
	if err := a.load(); err != nil {
		t.Fatalf("load with no writable TMPDIR: %v\n\tEvery command opens the local backend, so this is `ackn4th version` failing too.", err)
	}
	defer a.closeComm()

	tk := &task.Task{ID: task.NewID(), Prompt: "no tmpdir", Status: task.Pending}
	if err := a.backend().Create(tk); err != nil {
		t.Fatalf("create through the backend load() chose: %v", err)
	}
	if _, err := a.backend().Load(tk.ID); err != nil {
		t.Fatalf("load the record back through the backend load() chose: %v", err)
	}
}

// A controller URL makes the state shared, so the runner belongs to the
// daemon host and this process asks it. Nothing here needs a controller to be
// listening: the point is which backend the environment chose.
func TestControllerURLMakesTheBackendRemote(t *testing.T) {
	a := loadApp(t, "ackn://127.0.0.1:7602")
	if !a.remote() {
		t.Fatal("remote() is false with a controller URL: the client would run the task itself")
	}
	if a.localRunner() {
		t.Fatal("localRunner() is true against a controller: two runners could claim one task")
	}
	if _, ok := a.backend().(*task.GRPC); !ok {
		t.Fatalf("backend with a controller URL = %T, want *task.GRPC", a.backend())
	}
	ep, ok := a.backend().(task.Endpointer)
	if !ok {
		t.Fatal("the controller backend cannot say where it points, so status cannot show it")
	}
	if got := ep.Endpoint(); !strings.Contains(got, "127.0.0.1:7602") {
		t.Errorf("endpoint = %q, want the controller's address", got)
	}
}

// A URL this build cannot speak is refused by name at load, not silently
// treated as a host name.
func TestAnUnsupportedBackendURLIsRefused(t *testing.T) {
	t.Setenv(config.HomeEnv, t.TempDir())
	t.Setenv(task.EnvBackendURL, "http://127.0.0.1:7602")
	a := &app{noInput: true}
	err := a.load()
	if err == nil {
		t.Fatal("load accepted http:// as a backend URL")
	}
	if !strings.Contains(err.Error(), task.EnvBackendURL) {
		t.Errorf("the refusal does not name the variable: %v", err)
	}
}
