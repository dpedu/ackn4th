//go:build !unix && !windows

package task

import (
	"context"
	"errors"
	"os"
	"os/signal"
)

var errUnsupported = errors.New("background tasks are only supported on macOS, Linux and Windows")

// Lock is a no-op on this platform.
func (s *Store) Lock(id string) (func(), error) { return func() {}, checkID(id) }

// Locked always reports false on this platform.
func (s *Store) Locked(id string) (bool, error) { return false, checkID(id) }

// Spawn is unsupported on this platform.
func (s *Store) Spawn(id string) (int, error) { return 0, errUnsupported }

// Signal is unsupported on this platform.
func Signal(pid int) error { return errUnsupported }

// StopContext is cancelled when the user interrupts the process.
func StopContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(ctx, os.Interrupt)
}
