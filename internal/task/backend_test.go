package task

import (
	"strings"
	"testing"
	"time"

	"github.com/dethmasque/ackn4th/internal/event"
)

// testBackend exercises the behaviour both backends must share.
func testBackend(t *testing.T, b Backend) {
	t.Helper()
	tk := &Task{
		ID:        NewID(),
		Prompt:    "add rate limiting",
		Workspace: "/tmp/ws",
		Agents:    []AgentSpec{{Role: "backend_dev", Provider: "openai", Model: "m"}},
		MaxRounds: 3,
	}
	if err := b.Create(tk); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if tk.Status != Pending || tk.Round != 1 || len(tk.Started) != 1 || tk.Started[0] {
		t.Fatalf("Create defaults: status=%q round=%d started=%v", tk.Status, tk.Round, tk.Started)
	}

	got, err := b.Load(tk.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Prompt != tk.Prompt || got.Agents[0].Role != "backend_dev" {
		t.Fatalf("Load returned %+v", got)
	}

	// Rename is stored apart from the record and Find sees it.
	if err := b.Rename(got, "  The   Thing "); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got.Name != "The Thing" {
		t.Fatalf("Rename set name %q", got.Name)
	}
	if byName, err := b.Find("the thing"); err != nil || byName.ID != tk.ID {
		t.Fatalf("Find by name: got %v, err %v", byName, err)
	}
	if byPrefix, err := b.Find(strings.ToLower(tk.ID[:3])); err != nil || byPrefix.ID != tk.ID {
		t.Fatalf("Find by prefix: got %v, err %v", byPrefix, err)
	}
	if _, err := b.Find("nothing like this"); err == nil {
		t.Fatal("Find of a missing task should fail")
	}

	got.Handoff = "over to you"
	if err := b.Save(got); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := b.Load(tk.ID)
	if err != nil || reloaded.Handoff != "over to you" {
		t.Fatalf("Save did not persist handoff: %+v, %v", reloaded, err)
	}

	list, err := b.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, x := range list {
		if x.ID == tk.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("List does not contain %s", tk.ID)
	}

	// The cursor is opaque but monotonic within a backend.
	for _, text := range []string{"first", "second"} {
		if err := b.AppendEvent(tk.ID, event.Event{Time: time.Now().UTC(), Kind: event.Message, Text: text}); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
	first, cur, err := b.ReadEvents(tk.ID, 0)
	if err != nil || len(first) != 2 || first[0].Text != "first" {
		t.Fatalf("ReadEvents(0): %v, cursor %d, err %v", first, cur, err)
	}
	if err := b.AppendEvent(tk.ID, event.Event{Time: time.Now().UTC(), Kind: event.Message, Text: "third"}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	more, cur2, err := b.ReadEvents(tk.ID, cur)
	if err != nil || len(more) != 1 || more[0].Text != "third" {
		t.Fatalf("ReadEvents(cursor): %v, cursor %d, err %v", more, cur2, err)
	}
	if cur2 == cur {
		t.Fatalf("cursor did not advance: %d", cur2)
	}
	if none, _, _ := b.ReadEvents(tk.ID, cur2); len(none) != 0 {
		t.Fatalf("ReadEvents past the end returned %d events", len(none))
	}

	if err := b.WriteLive(tk.ID, 0, Live{Phase: "replying", Text: "hi"}); err != nil {
		t.Fatalf("WriteLive: %v", err)
	}
	if l := b.ReadLive(tk.ID, 0); l == nil || l.Text != "hi" || l.Phase != "replying" {
		t.Fatalf("ReadLive: %+v", l)
	}
	if l := b.ReadLive(tk.ID, 3); l != nil {
		t.Fatalf("ReadLive for an idle agent: %+v", l)
	}

	if err := b.Delete(tk.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := b.Load(tk.ID); err == nil {
		t.Fatal("Load after Delete should fail")
	}
}

func TestFileBackend(t *testing.T) {
	testBackend(t, NewStore(t.TempDir()))
}

// Without ACKN4TH_BACKEND_URL this machine's data dir is the backend, served by
// this process over the wire: reading it as "remote" would route every local
// start and stop to a controller that cannot exist, and reading it through the
// store would be a second code path the remote one never exercises.
func TestOpenBackendLocalByDefault(t *testing.T) {
	t.Setenv(EnvBackendURL, "")
	dir := t.TempDir()
	b, err := OpenBackend(dir)
	if err != nil {
		t.Fatalf("OpenBackend: %v", err)
	}
	defer b.Close()
	g, ok := b.(*GRPC)
	if !ok {
		t.Fatalf("OpenBackend returned %T, want *GRPC: local and remote reach state the same way", b)
	}
	if !g.selfHosted {
		t.Error("the default backend dials something it does not serve")
	}
	if b.(Shared).Shared() {
		t.Error("the default backend reports itself as shared: this machine's tasks would be treated as another host's")
	}
	if b.(Endpointer).Endpoint() != dir {
		t.Errorf("endpoint = %q, want the data dir %q", b.(Endpointer).Endpoint(), dir)
	}
}

// An ackn:// URL selects the controller, and anything else is refused by name
// rather than silently treated as a host name.
func TestOpenBackendSelectsTheController(t *testing.T) {
	for _, tc := range []struct {
		url      string
		wantGRPC bool
	}{
		{"ackn://127.0.0.1:7687", true},
		{"127.0.0.1:7687", true},
		{"grpc://127.0.0.1:7687", false},
		{"http://127.0.0.1:7687", false},
	} {
		t.Run(tc.url, func(t *testing.T) {
			t.Setenv(EnvBackendURL, tc.url)
			b, err := OpenBackend(t.TempDir())
			if tc.wantGRPC {
				if err != nil {
					t.Fatalf("OpenBackend(%q): %v", tc.url, err)
				}
				g, ok := b.(*GRPC)
				if !ok {
					t.Fatalf("OpenBackend(%q) = %T, want *GRPC", tc.url, b)
				}
				g.Close()
				return
			}
			if err == nil {
				if b != nil {
					b.Close()
				}
				t.Fatalf("OpenBackend(%q) was accepted: a URL this build cannot speak names no backend", tc.url)
			}
			if !strings.Contains(err.Error(), EnvBackendURL) {
				t.Errorf("the refusal does not name the variable: %v", err)
			}
		})
	}
}
