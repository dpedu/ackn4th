package cli

// R79, the `--wait` half. `stop-task --wait` spins on `(*app).runnerGone`
// (`tasks.go`) and hands the person a result: "still stopping" if the run never
// went away, the ✓ line if it did. Nothing shipped tests that predicate, so a
// change that made it answer wrongly would keep every test green and turn
// `stop-task` into a command that either hangs for its full wait or claims a
// stop that did not happen.
//
// runnerGone's contract, which this pins, is stated in its own comment: the
// answer comes from the backend that holds the task (a local lock says nothing
// about a pid on the daemon host), and a runner killed after writing its record
// must still count as gone - which is why Reconcile is called on every poll, not
// once. The child here is a real process holding the task's flock, so the two
// answers are observed rather than simulated.

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/dethmasque/ackn4th/internal/config"
	"github.com/dethmasque/ackn4th/internal/task"
)

const (
	qa35HoldDirEnv = "ACKN4TH_QA_HOLD_DIR"
	qa35HoldIDEnv  = "ACKN4TH_QA_HOLD_ID"
)

// TestStopWaitHelperProcess is the run itself when this binary is re-executed
// as a child: it takes the task's lock, records itself as running, and stays
// alive until it is killed. Not named TestQA: it is a helper, not a test, and
// the QA gate must not count it.
func TestStopWaitHelperProcess(t *testing.T) {
	dir, id := os.Getenv(qa35HoldDirEnv), os.Getenv(qa35HoldIDEnv)
	if dir == "" || id == "" {
		t.Skip("helper process only: re-executed by TestQARunnerGoneFollowsTheRun")
	}
	s := task.NewStore(dir)
	release, err := s.Lock(id)
	if err != nil {
		os.Exit(3)
	}
	tk, err := s.Load(id)
	if err != nil {
		os.Exit(3)
	}
	tk.Status, tk.PID = task.Running, os.Getpid()
	if err := s.Save(tk); err != nil {
		os.Exit(3)
	}
	time.Sleep(time.Minute) // killed by the test; the lock dies with the process
	release()
}

func TestQARunnerGoneFollowsTheRun(t *testing.T) {
	dir := t.TempDir()
	store := task.NewStore(dir)
	id := task.NewID()
	if err := store.Create(&task.Task{ID: id, Prompt: "long run"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	a := &app{cfg: config.Default(), be: store, store: store}

	// Not started yet: nothing is running, so a stop has nothing to wait for.
	if !a.runnerGone(id) {
		t.Error("a task that was never started is not 'gone': stop-task --wait would burn its whole timeout on a task nobody ran")
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestStopWaitHelperProcess$")
	cmd.Env = append(os.Environ(), qa35HoldDirEnv+"="+dir, qa35HoldIDEnv+"="+id)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the run: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	// Alive, holding the lock: not gone.
	deadline := time.Now().Add(30 * time.Second)
	for {
		tk, err := store.Load(id)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if tk.Status == task.Running && tk.PID == cmd.Process.Pid {
			break
		}
		select {
		case werr := <-exited:
			t.Fatalf("the run exited before it was observed (%v)", werr)
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("the run never showed up as running")
		}
	}
	if a.runnerGone(id) {
		t.Error("a live run holding the task's lock is reported gone: stop-task --wait would return before the run stopped")
	}

	// Killed hard, exactly as a crash leaves it: the record still says Running,
	// and only a reconcile against the released lock proves that run dead.
	_ = cmd.Process.Kill()
	select {
	case <-exited:
	case <-time.After(30 * time.Second):
		t.Fatal("the killed run did not exit")
	}
	if !a.runnerGone(id) {
		t.Error("a run killed with its record still saying running is not reported gone: stop-task --wait would never finish for a crashed run")
	}
	tk, err := store.Load(id)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if tk.Status == task.Running {
		t.Error("runnerGone answered 'gone' but left the record running: the next command reads it as live")
	}
}
