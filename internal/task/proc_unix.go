//go:build unix

package task

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
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
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("task is already running")
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
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

// Spawn starts a detached background runner for the task: it survives the
// terminal closing, and its output goes to runner.log.
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
	cmd := exec.Command(exe, "__run", id)
	// The runner works on this host's copy of the task - the directory it
	// lives in - so it must not inherit a client's endpoint. It used to:
	// nothing sets Env, so `__run` re-read the controller's own
	// ACKN4TH_BACKEND_URL and dialled the process that started it, which made
	// restarting the controller abort every task in flight with "runner exited
	// unexpectedly" and lose that round's events. Everything else is passed on:
	// the credentials, ACKN4TH_HOME and the rest are what the run needs.
	cmd.Env = withoutBackendURL(os.Environ())
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	return pid, cmd.Process.Release()
}

// Signal asks a runner to stop gracefully.
func Signal(pid int) error {
	if pid <= 0 {
		return errors.New("no runner process recorded")
	}
	return syscall.Kill(pid, syscall.SIGTERM)
}

// StopContext is cancelled when the process is interrupted or asked to
// stop (Signal).
func StopContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
}
