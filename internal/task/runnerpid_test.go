package task

import "testing"

// A controller stopping a task asks its backend which process the run belongs
// to, and signals only a pid a live run on this host claims. The file store is
// the backend a gRPC controller serves from, so it has to be able to answer:
// with only the shared backends implementing RunnerPIDer, stop-task refused
// every stop on a controller with "no live run on this host claims pid N" while
// the run was alive, and the task went on to finish.
//
// The proof is on this host's disk: a run holds the task's lock for its whole
// life, and its runner records the pid.
func TestStoreRunnerPIDNeedsBothTheLockAndThePid(t *testing.T) {
	s := NewStore(t.TempDir())
	id := NewID()
	if err := s.Create(&Task{ID: id, Prompt: "p"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	tk, err := s.Load(id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	tk.Status, tk.PID = Running, 4242
	if err := s.Save(tk); err != nil {
		t.Fatalf("save: %v", err)
	}

	// A record that says "running, pid 4242" outlives the process it names, so
	// on its own it proves nothing.
	if pid, ok := s.RunnerPID(id); ok {
		t.Errorf("RunnerPID reported pid %d with nothing holding the lock", pid)
	}

	release, err := s.Lock(id)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	if pid, ok := s.RunnerPID(id); !ok || pid != 4242 {
		t.Errorf("RunnerPID = %d, %v with a live run holding the lock; want 4242, true", pid, ok)
	}
	release()
	if pid, ok := s.RunnerPID(id); ok {
		t.Errorf("RunnerPID still reports pid %d after the run released the lock", pid)
	}

	// A run that has taken the lock but not yet written its pid is not a pid.
	tk.PID = 0
	if err := s.Save(tk); err != nil {
		t.Fatalf("save: %v", err)
	}
	release, err = s.Lock(id)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	defer release()
	if pid, ok := s.RunnerPID(id); ok {
		t.Errorf("RunnerPID reported pid %d for a run that has recorded none", pid)
	}

	if _, ok := s.RunnerPID("../../etc"); ok {
		t.Error("RunnerPID answered for an ID that is not one path segment")
	}
}
