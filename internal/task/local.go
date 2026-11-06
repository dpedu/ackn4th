package task

// The local backend: this machine's data dir, served over the same wire the
// controller speaks.
//
// There is one way for the CLI and the dashboard to talk to task state - the
// Backend interface, whose shipped implementation is the gRPC client - and it
// is the way used whether the state is on this machine or on another one. The
// alternative, reading the local data dir through the store directly, is a
// second implementation reached by a different call path, so every local
// command would exercise code no remote command runs and every remote command
// would exercise code no test on one machine ever touches. Two paths, one of
// them rarely run, is exactly where backend-specific bugs live.
//
// So with no endpoint configured this process serves its own data dir and dials
// itself. The CLI sees one implementation, `*GRPC`, always; what differs between
// "local" and "remote" is only where the server is, which is a fact about
// machines rather than a branch in a caller.
//
// The transport is in memory - the client dials a pipe, not a socket - and that
// is a security decision rather than a style one, twice over.
//
// First, reach. What crosses it is the whole of what an agent worked on: records,
// prompts, tool output, previews, run artifacts. A dashboard keeps the backend
// open for hours. A loopback TCP port is not a boundary on a shared machine (any
// local user can enumerate it with `lsof -i`), and a unix socket only moves the
// problem to file permissions - the data dir these records live in was 0700, so
// the answer is to create no filesystem object at all. Nothing outside this
// process can reach an in-memory pipe: there is no path to guess, no port to
// scan, no file to read.
//
// Second, dependencies. A unix socket needs a directory we can write, and unix
// socket paths are capped at 104 bytes, so a data dir under a long home could
// not hold one; a temp dir is not always writable (read_only containers, most CI
// images, hardened systemd units), and `load()` opens a local backend for every
// command - so a socket in $TMPDIR would take down `ackn4th version` on exactly
// the hosts where nothing else works either. A pipe needs neither. The local
// endpoint's only dependency is the data dir it serves.
//
// What this deliberately does not do is make a local run depend on a network:
// nothing leaves the machine, and the daemon's authority is still the file Store
// the transport serves.

import (
	"context"
	"fmt"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/dethmasque/ackn4th/internal/task/acknpb"
)

// localConnBuf is the pipe's buffer: an RPC and its reply fit, so a call
// crossing it does not need both goroutines to run in lockstep. Streams
// (Watch) read continuously, so a larger buffer buys nothing.
const localConnBuf = 1 << 20

// Local returns the backend for a process with no endpoint configured: the gRPC
// implementation, serving this process's data dir over an in-memory pipe,
// answering from dataDir.
//
// The returned backend reports Shared() false and Endpoint() the data dir, so
// everything that asks "do the tasks run somewhere else?" - signing in,
// workspace trust, whether to start a runner here - keeps answering for this
// machine. Opening it is also a liveness check: the server must answer a Ping
// before Local returns, so a local process cannot get a backend that never
// serves.
func Local(dataDir string) (*GRPC, error) {
	ln := bufconn.Listen(localConnBuf)
	conn, err := grpc.NewClient("passthrough:///local",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return ln.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		ln.Close()
		return nil, fmt.Errorf("local backend: %w", err)
	}
	g := &GRPC{
		addr:       "local",
		url:        "local",
		timeout:    grpcTimeout,
		conn:       conn,
		client:     acknpb.NewAcknClient(conn),
		local:      NewStore(dataDir),
		selfHosted: true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	g.stop = cancel
	g.done = make(chan struct{})
	go func() {
		defer close(g.done)
		// No control handler: starting and stopping a runner on this machine
		// is a local process action, not state communication, and the CLI does
		// it here as the daemon does. A request through this transport is
		// refused with the same words a controller-less daemon uses.
		g.serveErr = serveAckn(ctx, ln, g.local, nil, g.sharedYAML)
	}()
	if !g.ControllerAlive() {
		g.Close()
		if g.serveErr != nil {
			return nil, fmt.Errorf("local backend: %w", g.serveErr)
		}
		return nil, fmt.Errorf("local backend: the server accepted a connection but did not answer")
	}
	return g, nil
}
