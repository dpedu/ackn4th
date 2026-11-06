package arch_test

// The person's requirement for this round, in one line: *all* CLI and TUI
// communication with task state goes through the interface that wraps the gRPC
// client, local runs included. The reason is bugs, not tidiness - a local-only
// code path is a behaviour the remote path never exercises, so it can rot
// silently while every remote test stays green.
//
// This guard is the mechanical half of that rule. `a.store` and `m.opt.Store`
// still exist, and legitimately: they are this host's own files (run logs,
// clones, snapshots, the run lock, path helpers). What must not come back is a
// *state* call on them, because that is a read or write of the task record, the
// event log or the live preview that skipped the interface.
//
// The rule had already been broken once when this test was written:
// `internal/cli/tasks.go`'s `__run` loaded the task record from `a.store`
// directly. Nothing caught it - it compiled, it worked locally, and it was
// invisible on any machine where a controller happened to serve the same
// directory. So: scan the sources, name the receiver and the line, and let the
// allowlist below be the only place a host-file call is blessed.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// backendStateMethods are `task.Backend`'s methods: the ones that read or write
// task state. Kept as a literal list on purpose - if Backend gains a method,
// this test should need a matching edit, or the new method would be unguarded.
var backendStateMethods = map[string]bool{
	"Create": true, "Save": true, "Load": true, "Find": true, "List": true,
	"Delete": true, "Rename": true, "Switch": true, "Reconcile": true,
	"Locked": true, "BeginRun": true, "EndRun": true, "Heartbeat": true,
	"AppendEvent": true, "ReadEvents": true, "ReadLive": true,
	"WriteLive": true, "ClearLive": true, "Close": true,
}

// hostFileMethods are the deliberately-local calls on a store: this host's own
// files and paths, not task state. Each one is here because the CLI must name a
// path or a pid on *this* machine, which the interface cannot express.
//
// Note the asymmetry that makes `Artifact` safe: state is a record the daemon
// owns, an artifact is a file this host wrote. A client asks the backend for
// artifacts (the `Artifacts` capability) and falls back to its own store, which
// is why the direct call is correct there.
var hostFileMethods = map[string]bool{
	"Spawn": true, "RunnerPID": true, "DataDir": true, "SummaryPath": true,
	"GitHubPath": true, "LogPath": true, "LivePath": true, "Artifact": true,
	"Dir": true, "Lock": true, "Endpoint": true, "HostStore": true,
}

// storeCallRE matches `<receiver>.store.<Method>(` and `<receiver>.Store.<Method>(`
// at word boundaries, so `a.store.Load(`, `m.opt.Store.List(` and `be.Store.Save(`
// are all caught, while `opts.stores` or a local variable named `stored` is not.
var storeCallRE = regexp.MustCompile(`\b\w+\.(?:store|Store)\.([A-Za-z_][A-Za-z0-9_]*)\s*\(`)

// The only route to task state above internal/task is task.Backend.
func TestTheCLIReadsTaskStateOnlyThroughTheBackend(t *testing.T) {
	root := repoRoot(t)
	var offenders []string
	scanned, stateCalls := 0, 0

	for _, dir := range []string{"internal/cli", "internal/dash"} {
		files, err := filepath.Glob(filepath.Join(root, dir, "*.go"))
		if err != nil {
			t.Fatalf("list %s: %v", dir, err)
		}
		if len(files) == 0 {
			t.Fatalf("%s has no Go files: the guard is looking in the wrong place", dir)
		}
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			text := string(b)
			scanned++
			for _, m := range storeCallRE.FindAllStringSubmatchIndex(text, -1) {
				method := text[m[2]:m[3]]
				where := dir + "/" + filepath.Base(path) + ":" + strconv.Itoa(1+strings.Count(text[:m[0]], "\n"))
				if backendStateMethods[method] {
					stateCalls++
					offenders = append(offenders, where+" calls .store."+method)
					continue
				}
				if !hostFileMethods[method] {
					// An unrecognised method is not a failure by itself, but it
					// means this allowlist has fallen behind the store's API and
					// is no longer checking what it claims.
					t.Errorf("%s calls .store.%s, which is neither a Backend method nor a known host-file method: add it to hostFileMethods with a reason, or (if it touches task state) route it through task.Backend", where, method)
				}
			}
		}
	}

	if scanned == 0 {
		t.Fatal("scanned no files: this guard examines nothing")
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("task state is read or written off a concrete store instead of the backend:\n\t%s\n\tevery state call goes through task.Backend so a local run exercises the same path a remote one does; a host-file call (a log path, a pid) is the only thing that may touch the store directly",
			strings.Join(offenders, "\n\t"))
	}
	// The guard is only meaningful while the CLI still has a backend to go
	// through: if nobody calls a Backend method here, either the code stopped
	// reading state or this scan stopped matching it.
	if stateCalls == 0 && len(offenders) == 0 {
		if !anyFileContains(t, root, "internal/cli", "a.backend().") && !anyFileContains(t, root, "internal/dash", "opt.state().") {
			t.Fatal("no Backend state call found in internal/cli or internal/dash: this guard has stopped matching the code it guards")
		}
	}
}

// anyFileContains reports whether any non-test Go file under dir contains want.
func anyFileContains(t *testing.T, root, dir, want string) bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, dir, "*.go"))
	if err != nil {
		t.Fatalf("list %s: %v", dir, err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(b), want) {
			return true
		}
	}
	return false
}

// The local default must be the wire implementation, not a shortcut. This is the
// other half of "all communication goes through the interface": if the default
// backend were the concrete store, every local command would bypass the gRPC
// client and only REMOTE runs would exercise it - the exact inversion of the
// requirement, and invisible in a suite that mostly runs locally.
func TestTheLocalDefaultIsTheWireImplementation(t *testing.T) {
	root := repoRoot(t)
	src, err := os.ReadFile(filepath.Join(root, "internal/task/select.go"))
	if err != nil {
		t.Fatalf("read internal/task/select.go: %v", err)
	}
	text := string(src)

	// The default row is the first one in the table and it has no scheme.
	firstRow := regexp.MustCompile(`(?s)var backends = \[\]backendRow\{\s*\{[^}]*?kind:\s*(\w+)`)
	if m := firstRow.FindStringSubmatch(text); len(m) == 2 {
		if m[1] != "KindLocal" {
			t.Errorf("the selection table's first row is %s, want KindLocal: the default is read off this row", m[1])
		}
	} else {
		t.Fatalf("internal/task/select.go: cannot find the first row of the selection table")
	}

	// A row that opened the bare store would be a local path around the wire.
	if regexp.MustCompile(`open:\s*func\(dataDir, _ string\) \(Backend, error\) \{ return NewStore\(dataDir\), nil \}`).MatchString(text) {
		t.Error("a selection row opens NewStore(dataDir) directly: local runs must go through the gRPC client (task.Local) like every other run, or the local path is one the remote path never exercises")
	}
	if !strings.Contains(text, "return Local(dataDir)") {
		t.Error("internal/task/select.go's default row does not use Local(dataDir): local runs must reach state through the wire implementation served in-process")
	}
}
