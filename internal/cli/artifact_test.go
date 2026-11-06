package cli

import (
	"io"
	"os"
	"testing"

	"github.com/dethmasque/ackn4th/internal/config"
	"github.com/dethmasque/ackn4th/internal/task"
	"github.com/dethmasque/ackn4th/internal/ui"
)

// daemonBackend is a client's view of a controller: shared, carrying a task's
// GitHub context and answering for its run files, the way *task.GRPC does. The
// embedded store is the *client's* data dir, and nothing remote may write it.
type daemonBackend struct {
	*task.Store
	daemon  *task.Store
	carried string
}

func (d *daemonBackend) Shared() bool         { return true }
func (d *daemonBackend) Reconcile(*task.Task) {}
func (d *daemonBackend) CreateWithBrief(t *task.Task, github string) error {
	d.carried = github
	return d.daemon.CreateWithBrief(t, github)
}
func (d *daemonBackend) Artifact(id, name string) (string, error) {
	return d.daemon.Artifact(id, name)
}

func daemonApp(t *testing.T) (*app, *daemonBackend) {
	t.Helper()
	d := &daemonBackend{Store: task.NewStore(t.TempDir()), daemon: task.NewStore(t.TempDir())}
	a := &app{cfg: config.Default(), be: d, store: d.Store, term: ui.NewTerm(io.Discard, false),
		cfgSource: cfgFromController}
	if !a.remote() {
		t.Fatal("the app is not in remote mode, so these tests prove nothing")
	}
	return a, d
}

// The GitHub context a task's brief starts from belongs next to the task, on the
// host that runs it: a client that wrote its own github.md left the daemon's
// brief empty (R62).
func TestTheGitHubContextTravelsWithTheRecord(t *testing.T) {
	a, d := daemonApp(t)
	tk := &task.Task{ID: task.NewID(), Prompt: "fix #3", Agents: []task.AgentSpec{{Role: "backend_dev", Provider: "openai", Model: "m"}}}
	if err := a.createTask(tk, "### issue 3: broken\n"); err != nil {
		t.Fatalf("createTask: %v", err)
	}
	if d.carried != "### issue 3: broken\n" {
		t.Errorf("the backend carried %q", d.carried)
	}
	if got, err := os.ReadFile(d.daemon.GitHubPath(tk.ID)); err != nil || string(got) != "### issue 3: broken\n" {
		t.Errorf("the daemon's github.md = %q, %v", got, err)
	}
	if _, err := os.Stat(d.GitHubPath(tk.ID)); err == nil {
		t.Error("the client's data dir got a github.md: no runner reads it there")
	}
}

// At the end of a remote run the summary is on the other machine: the client
// shows its text rather than a path it cannot open (R56).
func TestTheSummaryComesFromTheController(t *testing.T) {
	a, d := daemonApp(t)
	tk := &task.Task{ID: task.NewID(), Prompt: "x", Agents: []task.AgentSpec{{Role: "backend_dev", Provider: "openai", Model: "m"}}}
	if err := d.daemon.Create(tk); err != nil {
		t.Fatal(err)
	}
	if v := a.summaryOf(tk.ID); v.path != "" || v.text != "" {
		t.Errorf("before a run wrote one: %+v; want an empty view", v)
	}
	if err := os.WriteFile(d.daemon.SummaryPath(tk.ID), []byte("done: all green\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v := a.summaryOf(tk.ID)
	if v.text != "done: all green" {
		t.Errorf("summary text = %q", v.text)
	}
	if v.path != "" {
		t.Errorf("a remote summary was shown as the client's path %q", v.path)
	}
}
