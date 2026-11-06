package task

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// A shared backend separates the CLI from the machine that runs
// the tasks, so the CLI cannot start or signal a process there. The
// control plane closes that gap: the CLI queues a request, and a
// controller on the daemon host carries it out locally.

// Action is what a controller should do with a task.
type Action string

const (
	// ActionStart starts a runner for a task on the daemon host. It covers
	// both a first start and a resume: the runner works out which it is.
	ActionStart Action = "start"
	// ActionStop asks the task's runner to stop, gracefully.
	ActionStop Action = "stop"
)

func (a Action) valid() bool { return a == ActionStart || a == ActionStop }

// Request is one queued instruction for the controller.
type Request struct {
	// ID identifies the request, so its answer can be routed back.
	ID     string    `json:"id"`
	Action Action    `json:"action"`
	Task   string    `json:"task"`
	At     time.Time `json:"at"`
}

// Response is the controller's answer.
type Response struct {
	OK bool `json:"ok"`
	// Message says what happened ("started (pid 4242)").
	Message string `json:"message,omitempty"`
	// Error says why it did not happen.
	Error string `json:"error,omitempty"`
}

// ErrNoController means nothing was watching the queue: the daemon host
// has no controller running, so the request would never be carried out.
var ErrNoController = errors.New("no ackn4th controller is running for this backend")

// ErrNoReply means the controller took the request but did not answer in
// time. It may still carry it out, so callers must not assume failure.
var ErrNoReply = errors.New("the controller did not answer in time")

// controlStaleAfter is how old a queued request may be before a controller
// refuses it. A controller pops within a second, so a request still waiting
// this long later was left behind by a controller that died, and the CLI
// that sent it has already given up: acting on it now would start or stop a
// task nobody is waiting for. A request without a timestamp is not stale
// (Send stamps it after this check), and neither is one from a host whose
// clock runs ahead — clocks differ, and a future request is not an old one.
const controlStaleAfter = 2 * time.Minute

// Controller is implemented by a backend that can relay a request to the
// host that runs the tasks. Send waits up to wait for the answer.
type Controller interface {
	Send(ctx context.Context, req Request, wait time.Duration) (Response, error)
	// ControllerAlive reports whether a controller is watching, so a
	// command can explain the problem before queueing anything.
	ControllerAlive() bool
}

// ConfigSharer is implemented by a shared backend that can carry the daemon
// host's configuration to clients: the tasks run under that host's providers,
// models, limits and workspace roots, so a client reads them rather than
// applying its own file to somebody else's run.
type ConfigSharer interface {
	// ShareConfig publishes this host's settings. A backend where the
	// controller is the source keeps them itself, and does nothing.
	ShareConfig(text string) error
	// SharedConfig returns the published settings, and whether the daemon
	// has any to offer.
	SharedConfig() (text string, ok bool, err error)
}

// ControlDaemon is the other end: it serves requests on the daemon host.
// handle runs locally, as the user would have run the command there.
type ControlDaemon interface {
	Serve(ctx context.Context, handle func(Request) Response) error
}

// RunnerPIDer is implemented by a backend that can name the process a
// run's heartbeat belongs to. A controller signals only a pid that a live
// run claims, never one left in a stale record.
type RunnerPIDer interface {
	RunnerPID(id string) (pid int, ok bool)
}

// CheckRequest rejects a request a controller must not act on.
func CheckRequest(req Request) error {
	if req.ID == "" {
		return errors.New("control request: no id")
	}
	if !req.Action.valid() {
		return fmt.Errorf("control request: unknown action %q", req.Action)
	}
	if err := checkID(req.Task); err != nil {
		return err
	}
	if !req.At.IsZero() && time.Since(req.At) > controlStaleAfter {
		return fmt.Errorf("control request: queued %s ago, stale after %s; it was not carried out when it was sent, so it is refused now",
			time.Since(req.At).Round(time.Second), controlStaleAfter)
	}
	return nil
}
