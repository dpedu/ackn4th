package task

// The local backend is the wire, served in this process. These tests hold the
// two properties the CLI depends on: what it writes lands in this machine's data
// dir (so a runner, a daemon restarted later or a `git`-style look at the files
// sees it), and it still knows it is not a client of a controller - the state is
// local, and everything remote mode turns off must stay on.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A local backend is the gRPC implementation, not a shortcut around it. If this
// ever fails, the CLI is once again exercising a code path a controller never
// runs, which is the whole point of the change.
func TestLocalIsTheWireImplementation(t *testing.T) {
	var be Backend = mustLocal(t, t.TempDir())
	defer be.Close()
	if _, ok := be.(*GRPC); !ok {
		t.Fatalf("Local returned %T, want *GRPC", be)
	}
	g := be.(*GRPC)
	if !g.selfHosted {
		t.Error("the local backend does not know it serves its own endpoint")
	}
	if be.(Shared).Shared() {
		t.Error("the local backend reports Shared(): this machine's state would be treated as another host's")
	}
}

// mustLocal opens a local backend over dir, failing the test if it cannot.
func mustLocal(t *testing.T, dir string) *GRPC {
	t.Helper()
	be, err := Local(dir)
	if err != nil {
		t.Fatalf("Local(%s): %v", dir, err)
	}
	return be
}

// Every write goes over the socket and lands in the data dir the backend was
// opened on: the file on disk is the proof that the RPC reached a real store.
func TestLocalWritesThroughTheWireIntoTheDataDir(t *testing.T) {
	dir := t.TempDir()
	be, err := Local(dir)
	if err != nil {
		t.Fatalf("Local: %v", err)
	}
	defer be.Close()

	tk := &Task{ID: NewID(), Prompt: "local", Name: "wire-local", Status: Pending}
	if err := be.Create(tk); err != nil {
		t.Fatalf("create over the local endpoint: %v", err)
	}
	if _, err := NewStore(dir).Load(tk.ID); err != nil {
		t.Fatalf("the task is not in the data dir the backend serves: %v", err)
	}
	got, err := be.Load(tk.ID)
	if err != nil {
		t.Fatalf("load over the local endpoint: %v", err)
	}
	if got.Prompt != "local" {
		t.Errorf("load returned prompt %q, want the record that was created", got.Prompt)
	}
	if ep := be.Endpoint(); ep != dir {
		t.Errorf("Endpoint = %q, want the data dir %q: status prints this line", ep, dir)
	}
}

// The store the endpoint serves is the store the caller gets back, so a log or a
// summary a run writes is the one the backend can read, not a second view of the
// same directory.
func TestLocalHostStoreIsTheServedStore(t *testing.T) {
	dir := t.TempDir()
	be, err := Local(dir)
	if err != nil {
		t.Fatalf("Local: %v", err)
	}
	defer be.Close()
	s := be.HostStore()
	if s != be.local {
		t.Fatal("HostStore returned a different store from the one the endpoint answers from")
	}
	if s.DataDir() != dir {
		t.Errorf("HostStore data dir = %q, want %q", s.DataDir(), dir)
	}
}

// Closing the backend stops the server it opened: a CLI command that ends must
// not leave a listener behind, and a later call must fail rather than hang.
func TestLocalCloseStopsServing(t *testing.T) {
	be, err := Local(t.TempDir())
	if err != nil {
		t.Fatalf("Local: %v", err)
	}
	addr := be.addr
	// A stopped server refuses the connection at once, so a short deadline
	// keeps this test fast without weakening it.
	be.timeout = 200 * time.Millisecond
	if err := be.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if be.ControllerAlive() {
		t.Errorf("the endpoint on %s still answers after Close", addr)
	}
	if _, err := be.List(); err == nil {
		t.Error("List succeeded after Close: the calls must fail, not reach a stopped server")
	}
}

// A local endpoint serves state; starting and stopping a runner stays a local
// process action, and the endpoint says so in the words the daemon uses instead
// of pretending to have carried a request out.
func TestLocalRefusesToControlTasks(t *testing.T) {
	be, err := Local(t.TempDir())
	if err != nil {
		t.Fatalf("Local: %v", err)
	}
	defer be.Close()
	_, err = be.Send(t.Context(), Request{ID: NewID(), Action: ActionStart, Task: NewID()}, 0)
	if !errors.Is(err, ErrNoController) {
		t.Fatalf("Send through the local endpoint = %v, want ErrNoController", err)
	}
}

// The local backend must need nothing this machine cannot give it: no writable
// temp dir, no port, no socket path, no home. Every command opens one (`load()`
// in internal/cli), so a backend that needs a writable $TMPDIR takes the whole
// tool down - `ackn version` included - on a read-only /tmp, which is exactly
// what read_only containers, most CI images and hardened hosts give you.
//
// The transport is an in-memory pipe (see local.go): no socket file, no port, so
// there is nothing to create outside the data dir in the first place. This test
// pins that, because the alternatives all fail on some real host - a temp dir
// that is not writable, a unix socket path over the kernel's 104-byte limit when
// the data dir is deep, a loopback port any local user can enumerate.
//
// TMPDIR points under a directory that does not exist, so anything that tried to
// create a file there would fail for root too, and the result does not depend on
// who runs the test or on mode bits. What it asserts is that the backend
// *serves* (create, then read the state back through the transport and off the
// disk), not that it returned a nicer error somewhere - an error-only assertion
// would pass on a broken backend, which is the trap.
func TestQALocalServesWithoutAWritableTempDir(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "no", "such", "tmp")
	t.Setenv("TMPDIR", tmp)

	be, err := Local(dir)
	if err != nil {
		t.Fatalf("Local with no writable TMPDIR: %v\n\tA local backend must depend on nothing outside the data dir it serves: every CLI command opens one, so otherwise a read-only /tmp breaks the whole tool.", err)
	}
	defer be.Close()

	tk := &Task{ID: NewID(), Prompt: "no tmpdir", Status: Pending}
	if err := be.Create(tk); err != nil {
		t.Fatalf("create through a local backend opened without a writable TMPDIR: %v", err)
	}
	got, err := be.Load(tk.ID)
	if err != nil {
		t.Fatalf("load through that backend: %v", err)
	}
	if got.Prompt != "no tmpdir" {
		t.Errorf("load returned prompt %q, want the record that was created", got.Prompt)
	}
	// The state is this machine's data dir: serving from a scratch copy would
	// pass the round trip and lose the task.
	if _, err := NewStore(dir).Load(tk.ID); err != nil {
		t.Errorf("the task is not in the data dir the backend serves (%s): %v", dir, err)
	}

	// Nothing named after a socket may exist anywhere, because there is no
	// socket: be.addr is a label for error messages, not a path.
	if strings.ContainsRune(be.addr, os.PathSeparator) {
		t.Errorf("the local endpoint's address is a filesystem path (%q): an in-memory transport needs none, and a path brings the temp dir and 104-byte problems back", be.addr)
	}
	if _, err := os.Stat(be.addr); err == nil {
		t.Errorf("something exists at the local endpoint's address (%q): the transport must create no filesystem object", be.addr)
	}
	if _, err := os.Stat(tmp); err == nil {
		t.Errorf("%s was created: nothing may be written to the temp dir for the local endpoint", tmp)
	}
	// Close leaves the data dir as it found it, apart from the task that was
	// created there.
	if err := be.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the data dir after Close: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "sock-") || strings.HasPrefix(e.Name(), "ackn4th-") {
			t.Errorf("Close left %q in the data dir: the local endpoint must leave no transport files behind", e.Name())
		}
	}
}

// The local endpoint's liveness is this process's, so a command can say where
// state lives without a path that could be wrong: Endpoint() is the data dir,
// and that is the line `ackn4th status` prints. This is the replacement for a
// test that asked the error to name the socket directories it tried - there are
// none now, and "where did it look" is answered by "nowhere: it needs nothing".
func TestTheLocalEndpointNamesTheDataDir(t *testing.T) {
	dir := t.TempDir()
	be, err := Local(dir)
	if err != nil {
		t.Fatalf("Local: %v", err)
	}
	defer be.Close()
	if got := be.Endpoint(); got != dir {
		t.Errorf("Endpoint = %q, want the data dir %q", got, dir)
	}
	if be.Shared() {
		t.Error("the local endpoint reports Shared(): this machine's state would be treated as another host's")
	}
}
