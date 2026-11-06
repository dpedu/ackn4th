package arch_test

// Where the build decides which backend a process uses has to be one place.
//
// The shape this guards against is not "a wrong answer" but "a second answer":
// a holder that keeps two backends and picks between them with a nil check.
// That is how a configured controller once stayed ignored while everything
// still compiled and every test passed - the selection had been copied into an
// accessor instead of living in the selector.
//
// So: the selector is a table (internal/task/select.go), the holders are
// accessors with no branch in them, and each holder has exactly one backend
// field to hold.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// accessorInner returns the text between the braces of the named method, found
// by matching braces rather than by looking for a line ending.
//
// Matching braces is the point. An earlier version of this helper stopped at
// the first "\n}", which made every guard below blind to any body with a block
// in it: a `switch { case a.be != nil: return a.be; default: return a.store }`
// - the exact second selection these tests exist to catch - truncated to a
// string that still contained `return a.be`, and the guard passed. A guard is
// only worth having if it fails on the shape it names.
func accessorInner(t *testing.T, path, signature string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(b)
	i := strings.Index(src, signature)
	if i < 0 {
		t.Fatalf("%s has no %q: the holder must stay a named accessor so this guard, and a reader, can find it", path, signature)
	}
	rest := src[i+len(signature):]
	open := strings.Index(rest, "{")
	if open < 0 {
		t.Fatalf("%s: %q has no body", path, signature)
	}
	depth := 0
	for j := open; j < len(rest); j++ {
		switch rest[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return strings.TrimSpace(rest[open+1 : j])
			}
		}
	}
	t.Fatalf("%s: %q has an unclosed body", path, signature)
	return ""
}

// assertAccessorIsOneReturn is the whole contract of these accessors: one
// statement, returning the one field, so there is nowhere for a second
// selection to hide. An exact match is deliberate - `if`, `switch`, `case`,
// `default` and a bare `return a.store` are all just "not the one return", and
// each of them is a backend being chosen twice.
func assertAccessorIsOneReturn(t *testing.T, path, signature, want, why string) {
	t.Helper()
	inner := accessorInner(t, path, signature)
	if inner == want {
		return
	}
	hint := "it must be exactly one statement, `" + want + "`"
	for _, branch := range []string{"if ", "switch", "case ", "default:", "for "} {
		if strings.Contains(inner, branch) {
			hint = "this is a second selection: " + strings.TrimSpace(branch) + " means the accessor decides again, so a configured controller can be silently ignored behind it"
			break
		}
	}
	t.Errorf("%s: the accessor is not a single return of the one field:\n\tgot:  %s\n\twant: %s\n\t%s\n\t%s", path, inner, want, hint, why)
}

// The CLI picks a backend once, in load, through task.OpenBackend; backend() is
// an accessor. Anything but `return a.be` is a second, silent selection.
func TestTheCLIBackendAccessorDoesNotChoose(t *testing.T) {
	assertAccessorIsOneReturn(t,
		filepath.Join(repoRoot(t), "internal/cli/root.go"),
		"func (a *app) backend() task.Backend",
		"return a.be",
		"one place chooses the backend (task.OpenBackend in load); the app holds one backend, chosen once")
}

// The dashboard is handed the backend the CLI already chose. Its state()
// accessor must not re-decide either; a caller that left State out gets it
// filled once, in newModel, not behind every read.
func TestTheDashStateAccessorDoesNotChoose(t *testing.T) {
	assertAccessorIsOneReturn(t,
		filepath.Join(repoRoot(t), "internal/dash/model.go"),
		"func (o *Options) state() task.Backend",
		"return o.State",
		"the dashboard reads state through the backend it was given; defaulting belongs in newModel, once")
}

// Each holder has one field for the backend. Two fields is what makes a
// fallback necessary, and the fallback is the bug.
func TestEachHolderHasOneBackendField(t *testing.T) {
	root := repoRoot(t)
	for _, tc := range []struct{ path, want string }{
		{"internal/cli/root.go", "be task.Backend"},
		{"internal/dash/model.go", "State task.Backend"},
	} {
		b, err := os.ReadFile(filepath.Join(root, tc.path))
		if err != nil {
			t.Fatalf("read %s: %v", tc.path, err)
		}
		if !strings.Contains(string(b), tc.want) {
			t.Errorf("%s has no %q field: the backend a process uses is held once, not reconstructed per call", tc.path, tc.want)
		}
	}
	// The CLI must not keep a second channel under another name: that is the
	// pair of fields the nil-check existed to reconcile.
	src, err := os.ReadFile(filepath.Join(root, "internal/cli/root.go"))
	if err != nil {
		t.Fatalf("read internal/cli/root.go: %v", err)
	}
	if regexp.MustCompile(`(?m)^\s*comm\s+task\.Backend`).MatchString(string(src)) {
		t.Error("internal/cli/root.go declares a second task.Backend field (comm): one backend per process, chosen in load")
	}
}

// The selector itself is a table: rows of locations, looked up by scheme, so
// adding one is adding a row rather than editing a condition chain.
func TestTheSelectorIsATable(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "internal/task/select.go"))
	if err != nil {
		t.Fatalf("read internal/task/select.go: %v", err)
	}
	src := string(b)
	for _, want := range []string{"var backends = []backendRow{", "func Select(", "scheme"} {
		if !strings.Contains(src, want) {
			t.Errorf("internal/task/select.go no longer has %q: the set of locations is a table, and the table is the only place that knows it", want)
		}
	}
	// Both rows must be here, or one of them is being chosen somewhere else.
	// They are two *locations* of one implementation now, not two types: the
	// point of the table is that nothing above it can tell which row it got.
	for _, kind := range []string{"KindLocal", "KindController"} {
		// gofmt aligns the struct literals, so allow the padding.
		row := regexp.MustCompile(`kind:\s+` + kind + `\b`)
		if !row.MatchString(src) {
			t.Errorf("internal/task/select.go has no row for %s", kind)
		}
	}
	// A row whose kind is the old "file" would mean a second implementation
	// survived alongside the wire, which is exactly what this round removed.
	if regexp.MustCompile(`KindFile\b`).MatchString(src) {
		t.Error("internal/task/select.go still defines KindFile: this build ships one implementation reached two ways, so a \"file\" kind is a second one")
	}
}
