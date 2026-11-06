package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/dethmasque/ackn4th/internal/config"
	"github.com/dethmasque/ackn4th/internal/task"
	"github.com/spf13/cobra"
)

// startWait is how long a start may take to be answered. A start is not a
// signal: the daemon may clone a repository first, and that is allowed ten
// minutes. Waiting fifteen seconds and reporting "it may still carry out the
// request" would make every first start of a big repository look like a
// failure while it is working correctly.
const startWait = 10 * time.Minute

// controlWait is how long a command waits for the controller to answer.
// The work it does (spawn a process, signal one) is immediate.
const controlWait = 15 * time.Second

// localRunner reports whether a runner should start in this process, on
// this machine: always for a local data dir, and on the daemon host when
// someone says so explicitly.
func (a *app) localRunner() bool {
	return !a.remote() || os.Getenv(task.EnvAllowLocalRunner) == "1"
}

// control sends one request to the controller and turns the common
// failures into something a person can act on.
func (a *app) control(ctx context.Context, action task.Action, id string) (task.Response, error) {
	wait := controlWait
	if action == task.ActionStart {
		wait = startWait
	}
	// A single-value assertion on purpose. `_ Controller = (*GRPC)(nil)` in
	// grpc.go (beside the `_ Backend` assertions in backend.go) is the place a
	// new implementation registers its capabilities, so this cannot fail for
	// anything this build ships, and a "no control plane" branch here would be
	// dead code that reads like coverage (finding 23). A second implementation
	// must add its assertion there before it can reach a command - if one ever
	// arrives without it, this panics at the call site rather than returning a
	// wrong answer, which is the failure mode to prefer, and the assertion
	// list is where the fix belongs.
	c := a.backend().(task.Controller)
	req := task.Request{ID: task.NewID(), Action: action, Task: id, At: time.Now().UTC()}
	resp, err := c.Send(ctx, req, wait)
	if errors.Is(err, task.ErrNoController) {
		return resp, fmt.Errorf("%w: nothing would carry out the request; start one on the host that runs the tasks with `ackn4th controller`", err)
	}
	if errors.Is(err, task.ErrNoReply) {
		return resp, fmt.Errorf("the controller did not answer within %s; it may still carry out the request (check `ackn4th list-tasks`)", wait)
	}
	return resp, err
}

// spawnRunner starts a background runner for the task where its state
// lives: here, or on the daemon host through the controller.
func (a *app) spawnRunner(ctx context.Context, id string) error {
	if a.localRunner() {
		_, err := a.store.Spawn(id)
		return err
	}
	resp, err := a.control(ctx, task.ActionStart, id)
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	return nil
}

// stopRunner asks the task's runner to stop. The pid recorded in a shared
// backend belongs to the daemon host, so it is never signalled from here.
func (a *app) stopRunner(ctx context.Context, tk *task.Task) error {
	if a.localRunner() {
		if err := task.Signal(tk.PID); err != nil {
			return fmt.Errorf("signal runner (pid %d): %w", tk.PID, err)
		}
		return nil
	}
	resp, err := a.control(ctx, task.ActionStop, tk.ID)
	if err != nil {
		return err
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	return nil
}

// controllerCmd is the daemon host's control loop: it watches the queue
// and starts or stops the task's runner here, where the processes are.
func controllerCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "controller",
		GroupID: "session",
		Short:   "Run the control loop that starts and stops tasks for remote CLIs",
		Long: `Run the control loop on the host that runs the tasks. With a shared
backend the CLI cannot start or signal a process on
another machine, so it queues the request instead and this loop carries it
out: starting a runner, resuming one, or stopping one.

Run it where the workspaces and the provider credentials are, next to the
same data directory the runners use:

  ACKN4TH_BACKEND_URL=ackn://0.0.0.0:7687 ackn4th controller

It serves the task state in this host's data dir over gRPC, so a client on
another machine can list tasks, follow their events live and start or stop
them here, where the workspaces and the provider keys are.

The port has no authentication and no encryption: anyone who can reach it can
read every task, follow its events, and start or stop its runner. Bind it to a
private interface - a VPN address, or 127.0.0.1 behind an ` + "`ssh -L`" + ` tunnel - and
never to a public one. The wildcard address is shown because a workspace of
several machines needs it, not because it is safe.

It runs until interrupted. Keep it alive with systemd, tmux or nohup; a CLI
that cannot reach it says so rather than leaving a task silently unstarted.`,
		Example: `  ACKN4TH_BACKEND_URL=ackn://0.0.0.0:7687 ackn4th controller
  ACKN4TH_BACKEND_URL=ackn://tasks.internal:7687 ackn4th start-task -d "Add rate limiting"`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// A local endpoint is a ControlDaemon too, since it is the same
			// implementation - but it serves only this process, over a unix
			// socket nothing else can dial. Serving "here" needs an address
			// another machine can reach, so the endpoint must be configured:
			// asking the type would let `ackn4th controller` with no URL
			// announce itself as a daemon and serve nothing.
			if !a.remote() {
				return fmt.Errorf("ackn4th controller needs an endpoint to serve: start it with %s=ackn://host:port (see docs/grpc-backend.md)", task.EnvBackendURL)
			}
			// Configured, so this is the controller client - the one
			// implementation, asserted to implement ControlDaemon at compile
			// time in backend.go. No second guard: it could not fail.
			d := a.backend().(task.ControlDaemon)
			// Clients must run tasks under this host's providers, models and
			// workspace roots, so the Config RPC answers from the file that is
			// actually in effect here. Read per call, so an edit and a restart
			// are the only steps needed.
			if s, ok := a.backend().(configSetter); ok {
				s.SetSharedConfig(configFileText)
			}
			t := a.term
			where := "for requests"
			if ep, ok := a.backend().(task.Endpointer); ok {
				where = "on " + ep.Endpoint()
			}
			t.Println(t.Green("✓ "), "controller serving ", t.Lime(where), t.Gray("  (Ctrl-C to stop)"))
			err := d.Serve(cmd.Context(), a.handleControl)
			if errors.Is(err, context.Canceled) {
				t.Println()
				t.Println(t.Gray("controller stopped"))
				return nil
			}
			return err
		},
	}
	return cmd
}

// handleControl carries out one request on this host.
func (a *app) handleControl(req task.Request) (resp task.Response) {
	defer func() {
		if p := recover(); p != nil {
			resp = task.Response{Error: fmt.Sprintf("controller: %v", p)}
		}
	}()
	tk, err := a.backend().Load(req.Task)
	if err != nil {
		return task.Response{Error: err.Error()}
	}
	a.backend().Reconcile(tk)
	switch req.Action {
	case task.ActionStart:
		switch tk.Status {
		case task.Running:
			return task.Response{Error: fmt.Sprintf("task %s is already running", tk.ID)}
		case task.Completed:
			return task.Response{Error: fmt.Sprintf("task %s is already completed", tk.ID)}
		}
		// The clone belongs here: this is the host with the disk and the gh
		// login the runner will use. Named by the client, made by the daemon.
		if err := a.prepareWorkspace(context.Background(), tk); err != nil {
			return task.Response{Error: err.Error()}
		}
		pid, err := a.store.Spawn(tk.ID)
		if err != nil {
			return task.Response{Error: err.Error()}
		}
		a.term.Println(a.term.Gray("  → started " + tk.ID + " (pid " + fmt.Sprint(pid) + ")"))
		return task.Response{OK: true, Message: fmt.Sprintf("started (pid %d)", pid)}
	case task.ActionStop:
		if tk.Status != task.Running {
			return task.Response{Error: fmt.Sprintf("task %s is not running: %s", tk.ID, tk.Status)}
		}
		// Signal only a pid a live run still claims. A record left behind
		// by a crash names a pid the OS may since have given to something
		// else, and a run begun on another host names a number that means
		// something else here: RunnerPID refuses both, and no proof of a
		// live run is not permission to signal its recorded pid.
		if p, ok := a.liveRunnerPID(tk.ID); !ok {
			return task.Response{Error: fmt.Sprintf("task %s: no live run on this host claims pid %d; not signalling (it has exited, or another host runs it)", tk.ID, tk.PID)}
		} else if p != tk.PID {
			return task.Response{Error: fmt.Sprintf("task %s: the live run is pid %d, not the recorded %d; not signalling", tk.ID, p, tk.PID)}
		}
		if err := task.Signal(tk.PID); err != nil {
			return task.Response{Error: fmt.Sprintf("signal runner (pid %d): %v", tk.PID, err)}
		}
		a.term.Println(a.term.Gray("  → stopped " + tk.ID + " (pid " + fmt.Sprint(tk.PID) + ")"))
		return task.Response{OK: true, Message: fmt.Sprintf("stopped (pid %d)", tk.PID)}
	}
	return task.Response{Error: fmt.Sprintf("unknown action %q", req.Action)}
}

// liveRunnerPID is the pid a live run on this host claims.
//
// The backend answers first and must: a shared backend knows which host minted
// a run token, and a pid only names a process on the machine that recorded it.
// The local store is the fallback, and on a gRPC controller it is the only one
// that can answer - the client object in front of it is not a RunnerPIDer, so
// asking only the backend left stop-task unable to stop anything there, with
// the refusal claiming another host owned the run.
func (a *app) liveRunnerPID(id string) (int, bool) {
	if r, ok := a.backend().(task.RunnerPIDer); ok {
		if pid, ok := r.RunnerPID(id); ok {
			return pid, true
		}
	}
	if a.store != nil {
		return a.store.RunnerPID(id)
	}
	return 0, false
}

// configSetter lets the controller give its served Config RPC the settings
// this host's tasks run under.
type configSetter interface {
	SetSharedConfig(func() string)
}

// configFileText is this host's config file as written, or "" when there is
// none: what a client is told it runs under here.
func configFileText() string {
	path, err := config.Path()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}
