package dash

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/dethmasque/ackn4th/internal/task"
)

// unreachableState is a backend that cannot be read: a controller that is down.
type unreachableState struct {
	*task.Store
	err error
}

func (u *unreachableState) List() ([]*task.Task, error) { return nil, u.err }

// A controller that cannot be reached is not "this host has no tasks": the
// first frame has to say so, and the notice has to go away once the backend
// answers again (R67, R63).
func TestAnUnreachableBackendIsSaidNotShownAsNoTasks(t *testing.T) {
	state := &unreachableState{Store: fixture(t), err: errors.New("no controller at 127.0.0.1:7687: not reachable")}
	m := newModel(Options{State: state, Store: state.Store})
	m.w = 200
	if !strings.Contains(m.footer(), "no controller at 127.0.0.1:7687") {
		t.Fatalf("the first frame does not say the backend is unreachable: %q", m.footer())
	}
	// The controller comes back: the next poll fills the list, and the notice
	// must not outlive its cause.
	state.err = nil
	m.poll()
	if m.err != nil {
		t.Errorf("a successful poll left the old error in place: %v", m.err)
	}
	if !strings.Contains(m.footer(), "↑↓") {
		t.Errorf("the key chips did not come back after a successful poll: %q", m.footer())
	}
}

// stateMethods are the Backend calls that carry task state: the record, the
// event log, the live preview. The local file store is where this host's
// artifacts live (run logs, workspaces, snapshots); the backend given in
// Options is where state lives, which is the controller when the CLI talks to
// a daemon.
//
// The distinction is invisible on one machine and wrong on two: a hand-merge
// once re-pointed these very calls at m.opt.Store, and the behavioural test
// above was the only thing that noticed. Reading the source, not a live
// store, catches every remaining receiver, including the ones no test
// happens to drive.
var stateMethods = map[string]bool{
	"List": true, "Find": true, "Load": true, "Save": true, "Delete": true,
	"Reconcile": true, "ReadEvents": true, "AppendEvent": true,
	"Create": true, "CreateWithBrief": true, "Switch": true,
	// readLive takes the backend too: passing the file store here reads the
	// live previews off the wrong machine, and it was part of the same
	// regression (Live.readLive(s *task.Store) became Live.readLive(s
	// task.Backend) in this change).
	"readLive": true,
}

var storeCallRE = regexp.MustCompile(`opt\.Store\.([A-Za-z]+)`)

// storeArgRE catches the other shape: the store *passed* to a call that takes
// a backend, as in `l.readLive(m.opt.Store)`.
var storeArgRE = regexp.MustCompile(`readLive\([\w.]*opt\.Store\b`)

// The dashboard reads task state through the backend it was given.
func TestTheDashReadsStateThroughItsBackend(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list package files: %v", err)
	}
	scanned, stateCall := 0, false
	var bad []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		text := string(b)
		scanned++
		if strings.Contains(text, "opt.state()") {
			stateCall = true
		}
		for _, m := range storeCallRE.FindAllStringSubmatchIndex(text, -1) {
			if method := text[m[2]:m[3]]; stateMethods[method] {
				bad = append(bad, name+":"+strconv.Itoa(1+strings.Count(text[:m[0]], "\n"))+" calls opt.Store."+method)
			}
		}
		for _, m := range storeArgRE.FindAllStringIndex(text, -1) {
			bad = append(bad, name+":"+strconv.Itoa(1+strings.Count(text[:m[0]], "\n"))+" passes opt.Store to readLive")
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no files: the guard is looking in the wrong directory")
	}
	if len(bad) > 0 {
		t.Errorf("the dash reads task state from the local file store, not the backend it was given:\n\t%s\n\twith a controller configured this reads the wrong machine's data dir; use opt.state()",
			strings.Join(bad, "\n\t"))
	}
	if !stateCall && len(bad) == 0 {
		t.Fatal("no opt.state() anywhere in the package: either the dash stopped reading state through its backend or this guard no longer matches it")
	}
}
