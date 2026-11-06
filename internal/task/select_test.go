package task

import (
	"strings"
	"testing"
)

// Choosing a backend is a table lookup. These tests hold the table to what the
// rest of the build relies on: the default serves this machine's data dir
// in-process, the controller is selected by its scheme (or a bare host:port),
// and anything else is refused with a message that lists what this build does
// speak, generated from the table so it cannot go stale.
func TestSelectPicksTheImplementationByURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		want backendKind
		// wantErr is a substring the refusal must carry.
		wantErr string
	}{
		{name: "unset is the local endpoint", url: "", want: KindLocal},
		{name: "spaces are not a URL", url: "   ", want: KindLocal},
		{name: "ackn selects the controller", url: "ackn://127.0.0.1:1", want: KindController},
		{name: "a bare host:port is a controller", url: "127.0.0.1:1", want: KindController},
		{name: "http is refused, and named", url: "http://127.0.0.1:1", wantErr: "http:// is not a backend"},
		{name: "grpc is refused: the table names one scheme", url: "grpc://127.0.0.1:1", wantErr: "ackn://host:port"},
		{name: "a path is not an address", url: "/tmp/socket", wantErr: EnvBackendURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be, err := Select(tc.url, t.TempDir())
			if tc.wantErr != "" {
				if err == nil {
					if be != nil {
						be.Close()
					}
					t.Fatalf("Select(%q) accepted a URL this build cannot speak", tc.url)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("the refusal does not say %q: %v", tc.wantErr, err)
				}
				if !strings.Contains(err.Error(), EnvBackendURL) {
					t.Errorf("the refusal does not name the variable: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Select(%q): %v", tc.url, err)
			}
			defer be.Close()
			g, ok := be.(*GRPC)
			if !ok {
				t.Fatalf("Select(%q) returned %T: this build ships one implementation, the wire", tc.url, be)
			}
			got := KindController
			if g.selfHosted {
				got = KindLocal
			}
			if got != tc.want {
				t.Errorf("Select(%q) chose %s, want %s", tc.url, got, tc.want)
			}
		})
	}
}

// Every row in the table has to be reachable: a row nobody can select is dead
// code pretending to be a supported backend.
func TestEveryRowIsSelectable(t *testing.T) {
	seen := map[backendKind]bool{}
	for _, r := range backends {
		url := ""
		if r.scheme != "" {
			url = r.scheme + "://127.0.0.1:1"
		}
		be, err := Select(url, t.TempDir())
		if err != nil {
			t.Errorf("%s is in the table but no URL selects it (%q): %v", r.kind, url, err)
			continue
		}
		be.Close()
		seen[r.kind] = true
	}
	for _, want := range []backendKind{KindLocal, KindController} {
		if !seen[want] {
			t.Errorf("%s is not reachable through Select", want)
		}
	}
}

// The local endpoint is the row with no scheme, and it must stay the default: a
// missing URL is the common case, and it has to mean "this machine's data dir",
// served by this process - not a controller, and not a shortcut around the
// interface.
func TestTheDefaultRowIsLocal(t *testing.T) {
	if len(backends) == 0 {
		t.Fatal("the selection table is empty")
	}
	if backends[0].kind != KindLocal || backends[0].scheme != "" {
		t.Fatalf("the first row is %q (scheme %q), want the local endpoint with no scheme: the default is read off this row", backends[0].kind, backends[0].scheme)
	}
	dir := t.TempDir()
	be, err := Select("", dir)
	if err != nil {
		t.Fatalf("Select(\"\"): %v", err)
	}
	defer be.Close()
	g, ok := be.(*GRPC)
	if !ok {
		t.Fatalf("the default backend is %T, want *GRPC: local and remote reach state the same way", be)
	}
	if !g.selfHosted {
		t.Error("the default backend dials something it does not serve: with no URL configured it must serve this machine's data dir itself")
	}
	if g.Shared() {
		t.Error("the default backend reports Shared(): a local process would be treated as a client of a controller that cannot exist")
	}
	if ep, ok := be.(Endpointer); !ok || ep.Endpoint() != dir {
		t.Errorf("the default backend's endpoint = %q, want the data dir %q", g.Endpoint(), dir)
	}
}
