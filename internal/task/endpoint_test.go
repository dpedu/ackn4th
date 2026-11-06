package task

import (
	"testing"
)

// Endpoint is printed by `ackn4th status`, so it must name what is being
// driven without carrying anything secret: the file store answers with its
// data dir, and the gRPC client with the controller's address.
func TestStoreEndpointIsTheDataDir(t *testing.T) {
	dir := t.TempDir()
	if got := NewStore(dir).Endpoint(); got != dir {
		t.Errorf("Store.Endpoint() = %q, want the data dir %q", got, dir)
	}
}

// Both backends can say where they keep state, so a command that shows the
// live one never has to type-switch.
func TestBothBackendsAreEndpoints(t *testing.T) {
	for _, b := range []Backend{NewStore(t.TempDir()), (*GRPC)(nil)} {
		if _, ok := b.(Endpointer); !ok {
			t.Errorf("%T is not an Endpointer", b)
		}
	}
}
