package task

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/dethmasque/ackn4th/internal/event"
)

// EnvAllowLocalRunner lets this machine run a task whose state lives on a
// controller. Set it on the host that runs the daemons; leave it unset on a
// client, where a local runner would race the daemon.
const EnvAllowLocalRunner = "ACKN4TH_ALLOW_LOCAL_RUNNER"

// Backend is the communication layer between a running task (the daemon
// side, which writes) and the CLI (the client side, which reads and
// nudges the task by saving it). A Backend holds only the state that
// flows between the two: task records, the event log and the live
// streaming preview. Artifacts that never leave the machine the runner
// runs on (scratch dirs, agent conversations, workspace snapshots, logs)
// stay on the file Store.
//
// Backends must be safe for concurrent use.
//
// ReadEvents' offset is an opaque cursor owned by the backend, which is why
// callers pass back the cursor the same backend returned and never interpret
// it.
type Backend interface {
	Create(t *Task) error
	Save(t *Task) error
	Load(id string) (*Task, error)
	Find(ref string) (*Task, error)
	List() ([]*Task, error)
	Delete(id string) error
	Rename(t *Task, name string) error
	Switch(t *Task, n int, provider, model, effort string) error
	// Reconcile fixes up a task whose runner died without recording why.
	// It must only act on proof, never on a failed liveness check: a
	// backend it cannot reach says "unknown", not "dead".
	Reconcile(t *Task)
	// Locked reports whether a runner currently holds the task, and an
	// error when that cannot be determined (the backend is unreachable).
	Locked(id string) (bool, error)
	// BeginRun records that a runner has started, and EndRun that it has
	// stopped. The file backend relies on its flock and does nothing.
	BeginRun(id string) error
	EndRun(id string) error
	// Heartbeat records that a runner is alive for the task.
	Heartbeat(id string) error
	AppendEvent(id string, e event.Event) error
	ReadEvents(id string, off int64) ([]event.Event, int64, error)
	ReadLive(id string, n int) *Live
	WriteLive(id string, n int, l Live) error
	// ClearLive drops agent n's streaming preview, once its reply is done.
	ClearLive(id string, n int) error
	// Close releases the backend's resources.
	Close() error
}

// ErrRunLost means another host has taken the run over: this runner no
// longer owns the task and must stop, or two runners work one task at
// once. A transport error is not this: not knowing is not losing.
var ErrRunLost = errors.New("run taken over by another host")

// ErrNoRun means this client has not claimed a run for the task: it never
// called BeginRun, or its run already ended. A heartbeat must not create
// liveness out of nothing — anyone able to reach the backend could then keep
// a dead task looking alive, and every reader that trusts the heartbeat
// (Reconcile, delete-task, stop-task, the controller's pid check) would be
// lied to.
var ErrNoRun = errors.New("no run claimed by this client")

// Shared is implemented by a Backend whose state more than one machine can
// reach, which means the process running a task may be somewhere else. The
// CLI uses it to decide whether to act here or ask a controller to act there;
// the file backend is never shared, so its answers are local.
type Shared interface {
	Shared() bool
}

// Endpointer is implemented by a Backend that can say where it keeps task
// state, in a form safe to print: a command that shows it must never echo a
// password, since that output ends up in shells, logs and screenshots.
type Endpointer interface {
	Endpoint() string
}

// Streamer is implemented by a Backend that can push events as they are
// appended, so a remote client can follow a task without polling.
// Subscribers first receive events after the given cursor, then live ones.
type Streamer interface {
	Subscribe(ctx context.Context, id string, after int64) (<-chan event.Event, func(), error)
}

// Briefed is implemented by a Backend that can take a task's GitHub context
// with its record, so github.md lands beside the task its runner reads rather
// than in the client's data dir. The file store has it, and the controller
// answers it from its own store. A backend without it gets a plain Create and
// the text goes to the local data dir, which is right for a backend whose
// runners run on the machine that created the task.
type Briefed interface {
	CreateWithBrief(t *Task, github string) error
}

// Artifacts is implemented by a Backend that keeps a task's run files where it
// can read them back: the summary a finished run wrote, and the context its
// brief was built from. The file store reads its own directory; a client reads
// them from the controller, the host that has them.
type Artifacts interface {
	Artifact(id, name string) (string, error)
}

// HostStore is implemented by a Backend that can hand back the Store this
// machine keeps its own files in: the local backend's served store, and the
// store a remote client keeps for the run files that never leave it. The CLI
// keeps that store for logs, scratch dirs, clones and this host's run lock, so
// what it writes is what the backend reads back - and never as a second route
// to task state, which goes through the Backend and nothing else.
type HostStore interface {
	HostStore() *Store
}

// HostStore returns the store itself: a Store is where a local process's state
// and files already are.
func (s *Store) HostStore() *Store { return s }

// HostStore returns the store the served endpoint answers from, which on a
// client is this machine's own data dir for artifacts.
func (g *GRPC) HostStore() *Store { return g.local }

// withoutBackendURL is the environment a spawned runner gets: everything the
// process that started it had, minus the endpoint. The runner works on the
// task's own directory here, and inheriting a client's endpoint would make it
// a remote client of its own controller (see Store.Spawn).
func withoutBackendURL(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if name == EnvBackendURL {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// OpenBackend returns the backend this process is configured to use: whatever
// ACKN4TH_BACKEND_URL names, else the file store on the local data dir. It is
// the only place configuration becomes a Backend; the implementations
// themselves are listed in select.go.
func OpenBackend(dataDir string) (Backend, error) {
	return Select(os.Getenv(EnvBackendURL), dataDir)
}

var (
	_ Backend     = (*Store)(nil)
	_ RunnerPIDer = (*Store)(nil)
	_ Briefed     = (*Store)(nil)
	_ Briefed     = (*GRPC)(nil)
	_ Artifacts   = (*Store)(nil)
	_ Artifacts   = (*GRPC)(nil)
	_ Shared      = (*Store)(nil)
	_ Endpointer  = (*Store)(nil)
	_ HostStore   = (*Store)(nil)
	_ HostStore   = (*GRPC)(nil)
)

// The file backend keeps liveness in the flock, so the run markers and
// the heartbeat are no-ops: Lock, held for as long as the runner lives,
// is the proof.
func (s *Store) BeginRun(id string) error  { _ = id; return nil }
func (s *Store) EndRun(id string) error    { _ = id; return nil }
func (s *Store) Heartbeat(id string) error { _ = id; return nil }

// Close releases nothing: the file store holds no resources.
func (s *Store) Close() error { return nil }

// Shared is false: this store only exists on the machine that holds the
// data dir.
func (s *Store) Shared() bool { return false }

// Endpoint names the data dir this store keeps its tasks under: the one it
// was opened with, not a level derived from Root.
func (s *Store) Endpoint() string { return s.data }

// RunnerPID names the process a live run belongs to, from this host's own
// proof: a running task holds its run lock (the flock, or the file lock on
// Windows) and its runner records the pid as it starts. Neither alone would do
// - the record outlives the process, and the lock says nothing about which pid
// - so the answer is "the pid in the record, but only while the lock is held".
//
// Without this, a controller answering over gRPC could not stop anything: the
// stop path asks the backend for the pid, and only the shared backends used to
// answer.
func (s *Store) RunnerPID(id string) (int, bool) {
	locked, err := s.Locked(id)
	if err != nil || !locked {
		return 0, false
	}
	t, err := s.Load(id)
	if err != nil || t.PID <= 0 {
		return 0, false
	}
	return t.PID, true
}

// ClearLive drops agent n's (0-based) streaming output.
func (s *Store) ClearLive(id string, n int) error {
	if err := checkID(id); err != nil {
		return err
	}
	if err := os.Remove(s.LivePath(id, n)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// WriteLive records agent n's (0-based) streaming output.
func (s *Store) WriteLive(id string, n int, l Live) error {
	if err := checkID(id); err != nil {
		return err
	}
	l.Updated = time.Now().UTC()
	data, err := json.Marshal(l)
	if err != nil {
		return err
	}
	return writeAtomic(s.LivePath(id, n), data)
}
