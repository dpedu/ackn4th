package arch_test

// The claim this package guards is the one that makes the change worth having:
// task state goes through an ordinary Go interface, `task.Backend`, with the
// file store and the gRPC controller as two implementations of it, chosen once
// at the edge. That is what makes a third backend "one file and zero call
// sites" - and it is a claim prose cannot keep true.
//
// The failure mode is not hypothetical: a hand-merge during this very branch
// dropped `task.OpenBackend` from `app.load()` and pointed the dashboard's
// state reads back at the concrete `*task.Store`. Everything still compiled,
// every command still worked on one machine, and the only symptom was that a
// controller was silently ignored. A single struct field
// (`app.comm`, `dash.Options.State`) is all that stands between "pluggable
// backends" and "gRPC wired through the CLI", and no unit test notices when a
// caller starts using the concrete type instead.
//
// So these tests read the import graph and the selector, not behaviour:
//   - nothing above the interface may import the wire (grpc, the generated
//     `acknpb` package) - otherwise the transport leaks into the UI and the
//     interface stops being the design;
//   - the selector must exist and be what the CLI calls, because a backend
//     nobody opens is a backend nobody uses.
//
// They are deliberately structural: cheap, deterministic, and they fail with a
// file and a line rather than a puzzling behaviour change on a machine with a
// controller configured.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// repoRoot walks up from this package to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s has no go.mod: %v", root, err)
	}
	return root
}

// importsIn returns the import path -> first file:line that uses it, for every
// non-test Go file under dir.
func importsIn(t *testing.T, root, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	base := filepath.Join(root, dir)
	fset := token.NewFileSet()
	err := filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		for _, spec := range f.Imports {
			p, uerr := strconv.Unquote(spec.Path.Value)
			if uerr != nil {
				return uerr
			}
			if _, seen := out[p]; seen {
				continue
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				rel = path
			}
			out[p] = rel + ":" + strconv.Itoa(fset.Position(spec.Pos()).Line)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

// wireImports are the packages that only the transport implementation may
// import: the gRPC runtime and the generated protobuf types.
var wireImports = []string{
	"google.golang.org/grpc",
	"github.com/dethmasque/ackn4th/internal/task/acknpb",
}

// hostedAbove are the packages that must see task state only as task.Backend.
var hostedAbove = []string{"internal/cli", "internal/dash"}

// The commands and the dashboard reach task state through the interface, never
// through the transport.
func TestOnlyTheBackendImportsTheWire(t *testing.T) {
	root := repoRoot(t)
	for _, dir := range hostedAbove {
		imports := importsIn(t, root, dir)
		if len(imports) == 0 {
			t.Fatalf("%s imports nothing: the guard is looking in the wrong place", dir)
		}
		var bad []string
		for _, wire := range wireImports {
			if where, ok := imports[wire]; ok {
				bad = append(bad, where+" imports "+wire)
			}
		}
		sort.Strings(bad)
		if len(bad) > 0 {
			t.Errorf("%s reaches the wire directly:\n\t%s\n\tstate must cross this boundary as task.Backend, so a second backend needs no change here; move the call behind internal/task",
				dir, strings.Join(bad, "\n\t"))
		}
	}
}

// The two implementations are asserted against the interface at compile time,
// and the selector is what hands them out. A guard-on-the-guard: if the
// assertions are deleted, "two implementations of one interface" stops being
// checked by anything.
func TestBothBackendsAreAssertedAndChosen(t *testing.T) {
	root := repoRoot(t)

	backend, err := os.ReadFile(filepath.Join(root, "internal/task/backend.go"))
	if err != nil {
		t.Fatalf("read internal/task/backend.go: %v", err)
	}
	for _, want := range []string{"_ Backend", "(*Store)(nil)"} {
		if !strings.Contains(string(backend), want) {
			t.Errorf("internal/task/backend.go no longer asserts %q: the file store would drift from the interface unnoticed", want)
		}
	}
	if !strings.Contains(string(backend), "func OpenBackend(") {
		t.Error("internal/task/backend.go has no OpenBackend: something must choose between the backends, and it must be one place")
	}

	grpc, err := os.ReadFile(filepath.Join(root, "internal/task/grpc.go"))
	if err != nil {
		t.Fatalf("read internal/task/grpc.go: %v", err)
	}
	for _, want := range []string{"_ Backend", "(*GRPC)(nil)"} {
		if !strings.Contains(string(grpc), want) {
			t.Errorf("internal/task/grpc.go no longer asserts %q: the controller would drift from the interface unnoticed", want)
		}
	}
}

// The CLI must actually open the backend it is configured with. This is the
// exact line a bad merge dropped once: without it `ACKN4TH_BACKEND_URL` is
// accepted, ignored, and every command quietly works on the local data dir.
func TestTheCLIOpensTheConfiguredBackend(t *testing.T) {
	root := repoRoot(t)
	cliDir := filepath.Join(root, "internal/cli")
	files, err := filepath.Glob(filepath.Join(cliDir, "*.go"))
	if err != nil {
		t.Fatalf("list internal/cli: %v", err)
	}
	opened := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(b), "task.OpenBackend(") {
			opened++
		}
	}
	if opened == 0 {
		t.Fatal("no command opens the backend: internal/cli calls no task.OpenBackend, so a configured controller is silently ignored and every command works on this machine's data dir instead")
	}
}
