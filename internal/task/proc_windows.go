package task

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

// Lock takes the task's run lock, held until release is called or the
// process exits. It fails if another runner holds it.
func (s *Store) Lock(id string) (release func(), err error) {
	if err := checkID(id); err != nil {
		return nil, err
	}
	// The record may live in a shared backend, so nothing has created this
	// host's copy of the task dir yet (a remote `__run`, a controller).
	if err := os.MkdirAll(s.Dir(id), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.Dir(id), "run.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	h := windows.Handle(f.Fd())
	ol := new(windows.Overlapped)
	if err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol); err != nil {
		f.Close()
		return nil, errors.New("task is already running")
	}
	return func() {
		windows.UnlockFileEx(h, 0, 1, 0, ol)
		f.Close()
	}, nil
}

// Locked reports whether a runner currently holds the task's lock.
func (s *Store) Locked(id string) (bool, error) {
	if err := checkID(id); err != nil {
		return false, err
	}
	release, err := s.Lock(id)
	if err != nil {
		if _, statErr := os.Stat(s.Dir(id)); statErr != nil {
			return false, nil // no such task: certainly not locked
		}
		return true, nil
	}
	release()
	return false, nil
}

// Spawn starts a background runner for the task, with no console, so it
// survives the terminal closing; its output goes to runner.log.
func (s *Store) Spawn(id string) (int, error) {
	if err := checkID(id); err != nil {
		return 0, err
	}
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	// Same reason as Lock: the task dir may be on the controller, not here.
	if err := os.MkdirAll(s.Dir(id), 0o700); err != nil {
		return 0, err
	}
	logf, err := os.OpenFile(s.LogPath(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	start := func(flags uint32) (*exec.Cmd, error) {
		cmd := exec.Command(exe, "__run", id)
		// See proc_unix.go: the runner works on this host's copy of the task,
		// so it must not inherit a client's endpoint and dial its way back to
		// the controller that started it.
		cmd.Env = withoutBackendURL(os.Environ())
		cmd.Stdout, cmd.Stderr = logf, logf
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
		return cmd, cmd.Start()
	}
	flags := uint32(windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP)
	// Leave the terminal's job, if it has one, so closing the terminal
	// doesn't end the task; a job that forbids it keeps the runner.
	cmd, err := start(flags | windows.CREATE_BREAKAWAY_FROM_JOB)
	if err != nil {
		if cmd, err = start(flags); err != nil {
			return 0, err
		}
	}
	pid := cmd.Process.Pid
	return pid, cmd.Process.Release()
}

// stopEvent names the event a runner waits on to stop gracefully.
func stopEvent(pid int) *uint16 {
	n, _ := windows.UTF16PtrFromString(fmt.Sprintf(`Local\ackn4th-stop-%d`, pid))
	return n
}

// Signal asks a runner to stop gracefully.
func Signal(pid int) error {
	if pid <= 0 {
		return errors.New("no runner process recorded")
	}
	ev, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, stopEvent(pid))
	if err != nil {
		return fmt.Errorf("runner %d isn't running: %w", pid, err)
	}
	defer windows.CloseHandle(ev)
	return windows.SetEvent(ev)
}

// StopContext is cancelled when the process is interrupted (Ctrl-C) or
// asked to stop (Signal).
func StopContext(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	ev, err := windows.CreateEvent(nil, 1, 0, stopEvent(os.Getpid()))
	if err != nil {
		return ctx, stop
	}
	done, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(ev)
		return ctx, stop
	}
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		if n, err := windows.WaitForMultipleObjects([]windows.Handle{ev, done}, false, windows.INFINITE); err == nil && n == windows.WAIT_OBJECT_0 {
			cancel()
		}
		windows.CloseHandle(ev)
		windows.CloseHandle(done)
	}()
	return ctx, func() {
		windows.SetEvent(done)
		cancel()
		stop()
	}
}
