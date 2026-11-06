package cli

// R73, behaviourally: no shipped test ever stopped a live run, which is why
// "stop-task does nothing against a gRPC controller" survived six recuts. The
// other suites pin the *refusals* (a recorded pid with no run behind it, a
// token minted on another host) and only ever start tasks. This one starts a
// run, stops it while it is alive, and asserts the record ends at "stopped" -
// not "completed", and not still "running".
//
// The run is a real child process behaving like the runner in the one way that
// matters: it holds the task's flock for its whole life, writes its pid into
// the record, and answers SIGTERM by recording "stopped". No LLM, no network,
// no sleeps: every wait is on an observed state change, with a bound.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/dethmasque/ackn4th/internal/config"
	"github.com/dethmasque/ackn4th/internal/task"
	"github.com/dethmasque/ackn4th/internal/ui"
)

const (
	qa31DirEnv = "ACKN4TH_QA_FAKE_RUNNER_DIR"
	qa31IDEnv  = "ACKN4TH_QA_FAKE_RUNNER_ID"
)

// TestFakeRunnerHelperProcess is the run itself when this test binary is
// re-executed as a child. It is not named TestQA*: it is not a test, and the
// TestQA gate must not count it.
func TestFakeRunnerHelperProcess(t *testing.T) {
	dir, id := os.Getenv(qa31DirEnv), os.Getenv(qa31IDEnv)
	if dir == "" || id == "" {
		t.Skip("helper process only: re-executed by TestQAStoppingALiveRunStopsIt")
	}
	s := task.NewStore(dir)
	release, err := s.Lock(id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake runner: lock: %v\n", err)
		os.Exit(3)
	}
	tk, err := s.Load(id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fake runner: load: %v\n", err)
		os.Exit(3)
	}
	tk.Status, tk.PID = task.Running, os.Getpid()
	if err := s.Save(tk); err != nil {
		fmt.Fprintf(os.Stderr, "fake runner: save: %v\n", err)
		os.Exit(3)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
	<-sig

	// What the runner does when it is signalled: record the outcome, then let
	// go of the lock.
	tk, err = s.Load(id)
	if err != nil {
		os.Exit(3)
	}
	tk.Status, tk.PID = task.Stopped, 0
	if err := s.Save(tk); err != nil {
		fmt.Fprintf(os.Stderr, "fake runner: save stopped: %v\n", err)
		os.Exit(3)
	}
	release()
	os.Exit(0)
}

func TestQAStoppingALiveRunStopsIt(t *testing.T) {
	dir := t.TempDir()
	store := task.NewStore(dir)
	id := task.NewID()
	if err := store.Create(&task.Task{ID: id, Prompt: "stop me"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	var out bytes.Buffer
	cmd := exec.Command(os.Args[0], "-test.run=^TestFakeRunnerHelperProcess$")
	cmd.Env = append(os.Environ(), qa31DirEnv+"="+dir, qa31IDEnv+"="+id)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the fake run: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	// Wait for the run to be visible: the record says running, with the pid of
	// the process that holds the lock.
	var tk *task.Task
	deadline := time.Now().Add(30 * time.Second)
	for {
		var err error
		tk, err = store.Load(id)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if tk.Status == task.Running && tk.PID == cmd.Process.Pid {
			break
		}
		select {
		case werr := <-exited:
			t.Fatalf("the fake run exited before it was stopped (%v): %s", werr, out.String())
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fake run never showed up as running with pid %d; record: %+v", cmd.Process.Pid, tk)
		}
	}

	// The daemon's view: a gRPC backend this process serves, plus the host's
	// store - the same pair `ackn4th controller` holds.
	a := qa31Controller(t, dir)

	resp := a.handleControl(task.Request{ID: task.NewID(), Action: task.ActionStop, Task: id})
	if !resp.OK {
		t.Fatalf("stopping a live run was refused: %s\n(the run is pid %d on this host and holds the task's lock, so this host does run it)", resp.Error, tk.PID)
	}

	select {
	case werr := <-exited:
		if werr != nil {
			t.Fatalf("the run ended badly after stop was reported as done: %v: %s", werr, out.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("stop was reported as done but pid %d is still alive", tk.PID)
	}

	end, err := store.Load(id)
	if err != nil {
		t.Fatalf("load after stop: %v", err)
	}
	if end.Status != task.Stopped {
		t.Fatalf("after a successful stop the record says %q, want %q: a stopped run must not look completed", end.Status, task.Stopped)
	}
}

// qa31Controller hosts a controller for dir on a loopback port and returns the
// daemon's app. The port is chosen by listen-then-close, so a taken port is
// retried rather than fatal.
func qa31Controller(t *testing.T, dir string) *app {
	t.Helper()
	var last error
	for try := 0; try < 5; try++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		addr := ln.Addr().String()
		ln.Close()

		g, err := task.OpenGRPC("ackn://"+addr, dir)
		if err != nil {
			t.Fatalf("open controller: %v", err)
		}
		a := &app{
			cfg:   config.Default(),
			be:    g,
			store: task.NewStore(dir),
			term:  ui.NewTerm(io.Discard, false),
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- g.Serve(ctx, a.handleControl) }()

		alive := false
		for i := 0; i < 100 && !alive; i++ {
			if g.ControllerAlive() {
				alive = true
				continue
			}
			select {
			case last = <-done:
			case <-time.After(20 * time.Millisecond):
			}
		}
		if !alive {
			cancel()
			<-done
			g.Close()
			continue
		}
		t.Cleanup(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("the controller did not stop")
			}
			g.Close()
		})
		return a
	}
	t.Fatalf("could not host a helper controller after 5 attempts (%v)", last)
	return nil
}
