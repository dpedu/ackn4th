package task

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dethmasque/ackn4th/internal/event"
	"github.com/dethmasque/ackn4th/internal/provider"
	"github.com/dethmasque/ackn4th/internal/role"
	"github.com/dethmasque/ackn4th/internal/spend"
	"github.com/dethmasque/ackn4th/internal/tools"
)

// ProviderFactory builds the provider for one agent, with its model and
// settings.
type ProviderFactory func(spec AgentSpec) (provider.Provider, error)

// Runner drives a task's agents: in turn, or in parallel rounds.
type Runner struct {
	// Store holds the artifacts that stay on this machine: workspaces,
	// snapshots, agent conversations, scratch dirs, the run lock.
	Store *Store
	// Comm is where the state a CLI reads and writes lives: the task
	// record, the event log and the live preview. It is the controller
	// when the daemon talks to a remote CLI, and nil means Store.
	Comm         Backend
	Roles        *role.Registry
	NewProvider  ProviderFactory
	AllowedRoots []string
	MaxSteps     int
	MaxFileBytes int64
	// Authorize is checked before every model request. When it fails, the
	// task stops (resumably) instead of continuing on stale credentials.
	// It gets the agent's provider.
	Authorize func(provider string) error
	// Spend prices model requests, records them in the shared ledger and
	// enforces the spending caps. Nil records and caps nothing.
	Spend *spend.Policy
	// Sink receives every event, including streamed text, for live display.
	Sink event.Sink
	// Commands lets agents run sandboxed shell commands; nil disallows
	// them. Hide lists paths their commands may never read (ackn4th's
	// config and data).
	Commands *tools.CommandPolicy
	Hide     []string
	// Completed runs once a task completes, before its summary is
	// written; it may record results on the task (the draft PR's URL).
	Completed func(*Task)

	emitMu sync.Mutex // agents working in parallel emit concurrently
	live   *liveFeed  // streamed output, for dashboards in other processes
}

// comm is the communication backend: the daemon writes task state through
// it so a CLI anywhere can watch and reach the task.
func (r *Runner) comm() Backend {
	if r.Comm != nil {
		return r.Comm
	}
	return r.Store
}

// ErrRoundLimit is returned when a task stops because it used its rounds.
var ErrRoundLimit = errors.New("round limit reached")

// unauthorized wraps an Authorize failure.
type unauthorized struct{ error }

// Run executes the task until it completes, fails, hits its round limit,
// or ctx is cancelled (stop-task / Ctrl-C), recording the outcome. It
// returns the task's final state.
func (r *Runner) Run(ctx context.Context, id string, background bool) (*Task, error) {
	release, err := r.Store.Lock(id)
	if err != nil {
		return nil, err
	}
	defer release()
	// State lives in comm: announce the run there, keep the heartbeat
	// fresh so a CLI can tell a live runner from a dead one, and clear it
	// once the outcome is saved.
	comm := r.comm()
	// BeginRun also takes over a claim left by a runner that died without
	// ending its run, so a crashed task can still be resumed.
	if err := comm.BeginRun(id); err != nil {
		return nil, fmt.Errorf("record run start: %w", err)
	}
	// The run can be cancelled by this side or by the backend (another
	// host took it over). Both end the run the same way; only the reason
	// differs.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	var lost atomic.Bool
	beat := make(chan struct{})
	beating := make(chan struct{})
	go func() {
		defer close(beating)
		heartbeat(comm, id, beat, func() {
			lost.Store(true)
			cancelRun()
		})
	}()
	defer func() { _ = comm.EndRun(id) }()
	// Stop the heartbeat, and wait for it to stop, before EndRun: a
	// refresh landing after it would recreate the liveness marker and
	// make delete-task/switch-task refuse for another 30s.
	defer func() { close(beat); <-beating }()
	defer func() {
		if t, err := r.comm().Load(id); err == nil && r.live != nil {
			r.live.clear(id, len(t.Agents))
		}
	}()

	t, err := r.comm().Load(id)
	if err != nil {
		return nil, err
	}
	if t.Status == Completed {
		return t, fmt.Errorf("task %s is already completed", id)
	}
	resumed := t.Status != Pending
	t.Status, t.Reason, t.PID, t.Background = Running, "", os.Getpid(), background
	// Someone started it again: a stuck agent gets a fresh chance.
	for i := range t.Agents {
		t.Agents[i].SameHandoffs, t.Agents[i].Idle = 0, 0
	}
	t.SplitRounds = 0
	if err := r.comm().Save(t); err != nil {
		return t, err
	}
	how := "start"
	if resumed {
		how = "resume"
	}
	r.emit(t, event.Event{Kind: event.TaskStarted, Text: how})
	if t.FollowUpPending() {
		r.emit(t, event.Event{Kind: event.FollowUp, Text: t.FollowUps[len(t.FollowUps)-1].Prompt})
	}

	var runErr error
	if rn, err := r.setup(t); err != nil {
		runErr = err
	} else {
		defer rn.close()
		if t.Parallel() {
			runErr = rn.parallel(runCtx)
		} else {
			runErr = rn.sequential(runCtx)
		}
	}

	t.PID = 0
	if lost.Load() {
		// Another host took the run over: the record, the event log and
		// the task's budget are the winner's now. Report and stop, rather
		// than racing it with a second workspace.
		t.Status = Stopped
		t.Reason = "run taken over by another host"
		return t, fmt.Errorf("task %s: %w", id, ErrRunLost)
	}
	switch {
	case runErr == nil:
		t.Status = Completed
		r.emit(t, event.Event{Kind: event.TaskCompleted, Text: t.Reason})
	case errors.As(runErr, new(unauthorized)):
		t.Status = Stopped
		t.Reason = runErr.Error() + "; once signed in again, resume the task"
		r.emit(t, event.Event{Kind: event.TaskStopped, Text: t.Reason})
	case errors.As(runErr, new(*spend.OverError)):
		t.Status = Stopped
		t.Reason = runErr.Error()
		r.emit(t, event.Event{Kind: event.TaskStopped, Text: t.Reason})
	case errors.As(runErr, new(*StuckError)):
		t.Status = Stopped
		t.Reason = runErr.Error()
		r.emit(t, event.Event{Kind: event.TaskStopped, Text: t.Reason})
	case errors.Is(runErr, ErrRoundLimit):
		t.Status = Stopped
		t.Reason = fmt.Sprintf("round limit reached (%d); resume with --rounds to continue", t.MaxRounds)
		r.emit(t, event.Event{Kind: event.TaskStopped, Text: t.Reason})
	case runCtx.Err() != nil:
		t.Status = Stopped
		t.Reason = "stopped by user"
		r.emit(t, event.Event{Kind: event.TaskStopped, Text: t.Reason})
	default:
		t.Status = Failed
		t.Reason = runErr.Error()
		r.emit(t, event.Event{Kind: event.TaskFailed, Text: t.Reason, Error: true})
	}
	if err := r.comm().Save(t); err != nil {
		return t, err
	}
	if t.Status == Completed && r.Completed != nil {
		r.Completed(t)
		if err := r.comm().Save(t); err != nil {
			return t, err
		}
	}
	writeAtomic(r.Store.SummaryPath(id), []byte(r.Summary(t)))
	if t.Status == Completed || t.Status == Stopped {
		return t, nil
	}
	return t, runErr
}

func (r *Runner) authorize(provider string) error {
	if r.Authorize == nil {
		return nil
	}
	if err := r.Authorize(provider); err != nil {
		return unauthorized{err}
	}
	return nil
}

func allDone(t *Task) bool {
	for _, a := range t.Agents {
		if !a.Done {
			return false
		}
	}
	return true
}

func summary(t *Task) string {
	if t.Solo() {
		return t.Agents[0].DoneSummary
	}
	var b strings.Builder
	for _, a := range t.Agents {
		fmt.Fprintf(&b, "[%s] %s\n", a.Role, a.DoneSummary)
	}
	return strings.TrimSpace(b.String())
}

// runnerTTL is how long a runner's liveness outlives its last heartbeat.
var runnerTTL = 2 * time.Minute

// heartbeatInterval is how often a runner refreshes its liveness, well
// inside runnerTTL. It is a variable so tests need not wait seconds.
var heartbeatInterval = runnerTTL / 3

// heartbeat keeps the runner's liveness fresh until beat closes, so a CLI
// watching a remote task can tell a live runner from a dead one. The first
// beat is the caller's, taken before the claim can be observed.
//
// A failed refresh is ignored: one blip must not end liveness for the rest
// of the run, or a later read would reconcile a live task as failed. Only
// losing the run to another host ends it, through onLost, so a displaced
// runner stops spending against a task it no longer owns.
//
// ErrNoRun means this client no longer holds a claim (its EndRun already
// ran, so the beat lost a race with the end of the run). There is nothing
// left to keep alive, so the loop stops rather than beating into a refusal
// every interval for the rest of the process's life.
func heartbeat(comm Backend, id string, beat <-chan struct{}, onLost func()) {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-beat:
			return
		case <-t.C:
			err := comm.Heartbeat(id)
			if errors.Is(err, ErrRunLost) {
				onLost()
				return
			}
			if errors.Is(err, ErrNoRun) {
				return
			}
		}
	}
}

func (r *Runner) emit(t *Task, e event.Event) {
	e.Round = t.Round
	r.emitRaw(t.ID, e)
}

func (r *Runner) emitRaw(id string, e event.Event) {
	r.emitMu.Lock()
	defer r.emitMu.Unlock()
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if !e.Ephemeral() {
		r.comm().AppendEvent(id, e)
	}
	if r.live == nil {
		r.live = newLiveFeed(r.comm())
	}
	r.live.Handle(id, e)
	if r.Sink != nil {
		r.Sink(e)
	}
}
