package task

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/dethmasque/ackn4th/internal/event"
)

// g27 starts a controller over a temporary data dir and returns a client
// pointed at it, exactly as a client on another machine would be. Everything
// below goes through the real gRPC stack on a real socket: a codec bug or a
// missing method shows up here, not in a mock.
func g27(t *testing.T) (*GRPC, *Store) {
	t.Helper()
	dir := t.TempDir()
	local := NewStore(dir)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveAckn(ctx, ln, local, func(req Request) Response {
			return Response{OK: true, Message: "did " + string(req.Action) + " on " + req.Task}
		}, func() string { return "providers:\n  openai:\n    model: gpt-test\n" })
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("controller: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the controller did not stop")
		}
	})
	g, err := OpenGRPC("ackn://"+ln.Addr().String(), t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	return g, local
}

// The whole record surface is one RPC per Backend method; if any of them is
// wired wrong the task lifecycle a CLI drives breaks somewhere.
func TestGRPCTaskLifecycleOverTheWire(t *testing.T) {
	g, _ := g27(t)
	tk := &Task{ID: NewID(), Prompt: "grpc", Name: "wire-test", Status: Pending,
		Workspace: "/srv/ws", Agents: []AgentSpec{{Role: "backend_dev", Provider: "openai", Model: "m"}}}
	if err := g.Create(tk); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := g.Load(tk.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Prompt != "grpc" || len(got.Agents) != 1 || got.Agents[0].Model != "m" {
		t.Fatalf("load returned %+v", got)
	}
	// A prefix finds the task, as it does on the file backend: the client
	// passes a reference through and the daemon resolves it against the
	// records it owns, rather than the client guessing.
	byPrefix, err := g.Find(tk.ID[:3])
	if err != nil || byPrefix.ID != tk.ID {
		t.Fatalf("find by prefix: %+v, %v", byPrefix, err)
	}
	got.Status = Pending
	if err := g.Save(got); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := g.Rename(got, "renamed"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	// Rename changes the record it is given; a client that did not read that
	// back would show the old name and undo the rename on its next Save.
	if got.Name != "renamed" {
		t.Fatalf("after rename the client still calls it %q", got.Name)
	}
	list, err := g.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %d tasks, %v", len(list), err)
	}
	if list[0].Status != Pending {
		t.Fatalf("list returned %+v", list[0])
	}
	if locked, err := g.Locked(tk.ID); err != nil || locked {
		t.Fatalf("locked = %v, %v (nothing holds it)", locked, err)
	}
	// Reconciliation is the daemon's rule, applied where the state is: a task
	// left "running" by a runner that died reads as failed to a client, so the
	// CLI offers a resume instead of showing a task that is not running.
	stale := &Task{ID: NewID(), Prompt: "stale", Status: Pending}
	if err := g.Create(stale); err != nil {
		t.Fatalf("create stale: %v", err)
	}
	stale.Status = Running
	if err := g.Save(stale); err != nil {
		t.Fatalf("save stale: %v", err)
	}
	if reloaded, err := g.Load(stale.ID); err != nil || reloaded.Status != Failed {
		t.Fatalf("a dead run still reads as %v (%v), want failed", reloaded.Status, err)
	}
	if err := g.Delete(tk.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := g.Load(tk.ID); err == nil {
		t.Fatal("a deleted task still loads")
	}
}

// Events, the cursor and the live preview: what the renderer reads.
func TestGRPCEventsAndLivePreview(t *testing.T) {
	g, _ := g27(t)
	id := NewID()
	if err := g.Create(&Task{ID: id, Prompt: "events", Status: Pending}); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := g.AppendEvent(id, event.Event{Kind: event.Message, Text: "m", Round: i}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	evs, cursor, err := g.ReadEvents(id, 0)
	if err != nil || len(evs) != 3 {
		t.Fatalf("read events: %d, %v", len(evs), err)
	}
	// The cursor is opaque but must advance, or a follower re-reads for ever.
	if more, _, err := g.ReadEvents(id, cursor); err != nil || len(more) != 0 {
		t.Fatalf("re-read after the cursor returned %d events (%v)", len(more), err)
	}
	if err := g.WriteLive(id, 0, Live{Phase: "replying", Text: "half a wor"}); err != nil {
		t.Fatalf("write live: %v", err)
	}
	if l := g.ReadLive(id, 0); l == nil || l.Text != "half a wor" {
		t.Fatalf("read live: %+v", l)
	}
	if err := g.ClearLive(id, 0); err != nil {
		t.Fatalf("clear live: %v", err)
	}
	if l := g.ReadLive(id, 0); l != nil {
		t.Fatalf("the preview survived a clear: %+v", l)
	}
}

// Watch is what keeps the UI live: it replays what the client missed, then
// streams what happens next, without the client polling.
func TestGRPCWatchReplaysThenStreams(t *testing.T) {
	g, _ := g27(t)
	id := NewID()
	if err := g.Create(&Task{ID: id, Prompt: "watch", Status: Pending}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := g.AppendEvent(id, event.Event{Kind: event.Message, Text: "before"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, stop, err := g.Subscribe(ctx, id, 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer stop()

	select {
	case e := <-ch:
		if e.Text != "before" {
			t.Fatalf("replay gave %q", e.Text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no replay of the event that was already there")
	}
	if err := g.AppendEvent(id, event.Event{Kind: event.Message, Text: "after"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	select {
	case e := <-ch:
		if e.Text != "after" {
			t.Fatalf("live gave %q", e.Text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an appended event never reached a subscriber: the UI would freeze")
	}
}

// Control is the start/stop path: one RPC, one answer, no queue to poll.
func TestGRPCControlAnswersTheDaemon(t *testing.T) {
	g, _ := g27(t)
	id := NewID()
	if err := g.Create(&Task{ID: id, Prompt: "control", Status: Pending}); err != nil {
		t.Fatalf("create: %v", err)
	}
	resp, err := g.Send(context.Background(), Request{ID: NewID(), Action: ActionStart, Task: id}, 5*time.Second)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !resp.OK || !strings.Contains(resp.Message, id) {
		t.Fatalf("response: %+v", resp)
	}
	if !g.ControllerAlive() {
		t.Fatal("ControllerAlive is false with the controller answering")
	}
}

// A start against a host with no controller is a fact to report, not a hang
// and not a raw transport error: the CLI turns ErrNoController into "start a
// controller".
func TestGRPCSendWithNoController(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listening: connection refused

	g, err := OpenGRPC("ackn://"+addr, t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer g.Close()
	g.timeout = 500 * time.Millisecond
	if g.ControllerAlive() {
		t.Fatal("ControllerAlive is true with nothing listening")
	}
	_, err = g.Send(context.Background(), Request{ID: NewID(), Action: ActionStart, Task: NewID()}, time.Second)
	if !errors.Is(err, ErrNoController) {
		t.Fatalf("send: %v, want ErrNoController", err)
	}
}

// A server that accepts the connection and then says nothing must fail the
// command in bounded time. An unbounded call froze every command for ever on
// exactly this, which is what the person met as "it got stuck".
func TestGRPCDoesNotHangOnASilentServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			buf := make([]byte, 4096)
			for {
				if _, err := c.Read(buf); err != nil {
					return
				}
			}
		}
	}()
	g, err := OpenGRPC("ackn://"+ln.Addr().String(), t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer g.Close()
	g.timeout = 300 * time.Millisecond

	done := make(chan error, 1)
	go func() { _, err := g.List(); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("List succeeded against a server that never answered")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a command against a silent server never returned: the deadline is not applied")
	}
}

// The daemon's own settings travel to the client, so it offers only what the
// tasks can actually run.
func TestGRPCConfigComesFromTheDaemon(t *testing.T) {
	g, _ := g27(t)
	text, found, err := g.SharedConfig()
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if !found || !strings.Contains(text, "gpt-test") {
		t.Fatalf("config = %q, found = %v", text, found)
	}
}

// The URL is the whole configuration: a typo must be refused with words that
// name the fix, not dialled for ever.
func TestGRPCEndpointParsing(t *testing.T) {
	for _, c := range []struct{ in, want, err string }{
		{"ackn://host:1234", "host:1234", ""},
		{"ackn://host", "host:7687", ""},
		{"ackn://", DefaultGRPCAddr, ""},
		{"127.0.0.1:9000", "127.0.0.1:9000", ""},
		{"127.0.0.1", "", "needs a port"},
		{"ackn://host:1/db", "", "without a path"},
		{"http://host:1", "", "unsupported scheme"},
	} {
		addr, err := parseAcknAddr(c.in)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("parseAcknAddr(%q) = %q, %v; want an error containing %q", c.in, addr, err, c.err)
			}
			continue
		}
		if err != nil || addr != c.want {
			t.Errorf("parseAcknAddr(%q) = %q, %v; want %q", c.in, addr, err, c.want)
		}
	}
}

// OpenBackend is what every command goes through: the scheme has to pick the
// backend, and the local data dir has to stay the default.
func TestOpenBackendSelectsByScheme(t *testing.T) {
	t.Setenv(EnvBackendURL, "ackn://127.0.0.1:1")
	b, err := OpenBackend(t.TempDir())
	if err != nil {
		t.Fatalf("open ackn: %v", err)
	}
	defer b.Close()
	if _, ok := b.(*GRPC); !ok {
		t.Fatalf("ackn:// gave %T", b)
	}
	if !b.(Shared).Shared() {
		t.Fatal("the gRPC backend reports itself as local")
	}
	t.Setenv(EnvBackendURL, "http://nope")
	if _, err := OpenBackend(t.TempDir()); err == nil {
		t.Fatal("an unsupported scheme was accepted")
	}
	t.Setenv(EnvBackendURL, "")
	b2, err := OpenBackend(t.TempDir())
	if err != nil {
		t.Fatalf("open local: %v", err)
	}
	defer b2.Close()
	if _, ok := b2.(*GRPC); !ok {
		t.Fatalf("no endpoint gave %T, want the wire backend served in this process", b2)
	}
	if b2.(Shared).Shared() {
		t.Fatal("the default backend reports itself as shared: this machine's tasks would be treated as another host's")
	}
}

// The record travels as the JSON the file backend writes, so a round trip
// cannot lose a field that nobody remembered to add to a schema.
func TestGRPCRecordRoundTripKeepsEveryField(t *testing.T) {
	g, local := g27(t)
	tk := &Task{ID: NewID(), Prompt: "fields", Status: Running, Round: 3,
		Todos:  []Todo{{ID: 1, Text: "do it", By: "qa"}},
		Agents: []AgentSpec{{Role: "qa", Provider: "openai", Model: "m", Turns: 2}}}
	if err := g.Create(tk); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := g.Load(tk.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// The record the daemon stores is the truth (it normalises status,
	// timestamps and defaults on create); the wire must deliver exactly it.
	want, err := local.Load(tk.ID)
	if err != nil {
		t.Fatalf("the daemon's own record: %v", err)
	}
	a, _ := json.Marshal(want)
	b, _ := json.Marshal(got)
	if string(a) != string(b) {
		t.Fatalf("the record changed crossing the wire:\n daemon %s\n client %s", a, b)
	}
	if len(got.Agents) != 1 || got.Agents[0].Turns != 2 || len(got.Todos) != 1 {
		t.Fatalf("fields were lost on the way: %+v", got)
	}
}

// A stopping controller must not wait for a Watch stream, which ends only when
// the client leaves: Ctrl-C, `systemctl stop` and an upgrade used to hang for
// as long as anybody had `watch-task` or the dashboard open, and a supervisor
// then killed the process mid-request.
func TestGRPCStopsWithAWatchOpen(t *testing.T) {
	old := serveStopGrace
	serveStopGrace = 100 * time.Millisecond
	defer func() { serveStopGrace = old }()

	local := NewStore(t.TempDir())
	id := NewID()
	if err := local.Create(&Task{ID: id, Prompt: "watch", Agents: []AgentSpec{{Role: "qa", Provider: "mock", Model: "m"}}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveAckn(ctx, ln, local, func(Request) Response { return Response{OK: true} }, func() string { return "" })
	}()

	// A follower: the stream stays open, as `watch-task` holds it.
	w, err := OpenGRPC("ackn://"+ln.Addr().String(), t.TempDir())
	if err != nil {
		t.Fatalf("open client: %v", err)
	}
	defer w.Close()
	wctx, wcancel := context.WithCancel(context.Background())
	defer wcancel()
	if _, stop, err := w.Subscribe(wctx, id, 0); err != nil {
		t.Fatalf("subscribe: %v", err)
	} else {
		defer stop()
	}

	cancel() // as Ctrl-C and SIGTERM do
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("controller: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the controller is still running 10s after being asked to stop, because a client is watching")
	}
}
