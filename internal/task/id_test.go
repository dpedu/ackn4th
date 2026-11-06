package task

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dethmasque/ackn4th/internal/event"
)

// checkID is the boundary rule, not a CLI formality: the file store joins an
// ID straight onto its root, and the gRPC server passes on whatever a caller
// sent, on a port that is not authenticated. One path segment, no separators,
// nothing that would confuse a shared store's keys.
func TestStoreRefusesIDsThatAreNotOnePathSegment(t *testing.T) {
	bad := []string{"", ".", "..", "a/b", "../victim", "../../victim", "/etc", `a\b`, "a b", "a:b", "a\nb", "a\x00b"}
	s := NewStore(t.TempDir())
	for _, id := range bad {
		t.Run(fmt.Sprintf("%q", id), func(t *testing.T) {
			if err := s.Save(&Task{ID: id}); err == nil {
				t.Error("Save accepted the ID")
			}
			if _, err := s.Load(id); err == nil {
				t.Error("Load accepted the ID")
			}
			if err := s.AppendEvent(id, event.Event{}); err == nil {
				t.Error("AppendEvent accepted the ID")
			}
			if _, _, err := s.ReadEvents(id, 0); err == nil {
				t.Error("ReadEvents accepted the ID")
			}
			if err := s.Delete(id); err == nil {
				t.Error("Delete accepted the ID")
			}
			if _, err := s.Locked(id); err == nil {
				t.Error("Locked accepted the ID")
			}
			if _, err := s.Lock(id); err == nil {
				t.Error("Lock accepted the ID")
			}
		})
	}
}

// Every ID NewID hands out must pass the rule, or nothing could be created.
func TestNewIDsAreValid(t *testing.T) {
	for i := 0; i < 200; i++ {
		if id := NewID(); checkID(id) != nil {
			t.Fatalf("NewID returned %q, which checkID refuses", id)
		}
	}
}

// A refused ID leaves nothing behind, and nothing outside.
func TestDeleteOutsideTheDataDirDoesNothing(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "victim")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(victim, "keep.txt")
	if err := os.WriteFile(keep, []byte("important"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(filepath.Join(root, "data"))
	if err := s.Delete("../victim"); err == nil {
		t.Error("Delete accepted a path out of its root")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("the file outside the data dir is gone: %v", err)
	}
	if err := s.Save(&Task{ID: "../victim"}); err == nil {
		t.Error("Save accepted a path out of its root")
	}
	if _, err := os.Stat(filepath.Join(victim, "task.json")); !os.IsNotExist(err) {
		t.Errorf("Save wrote outside the data dir (%v)", err)
	}
}
