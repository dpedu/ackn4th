package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/dethmasque/ackn4th/internal/event"
	"github.com/dethmasque/ackn4th/internal/task/acknpb"
)

// EnvBackendURL names the controller to talk to:
//
//	ackn://tasks.internal:7687     the controller's gRPC endpoint
//
// Unset, ackn4th uses the local data dir exactly as before. Any other scheme
// is refused by name: this build speaks one endpoint and does not guess.
const EnvBackendURL = "ACKN4TH_BACKEND_URL"

// DefaultGRPCAddr is where `ackn4th controller` listens when the URL gives no
// port, and what a client falls back to.
const DefaultGRPCAddr = "127.0.0.1:7687"

// defaultControlWait is how long Send waits for the daemon's answer when the
// caller gives no better idea: a start may need to clone a repository first.
const defaultControlWait = 60 * time.Second

// grpcTimeout bounds one request: a server that accepts the connection and
// then stops answering must fail, not freeze the command. It is a variable so
// tests can shorten it.
var grpcTimeout = 5 * time.Second

// serveStopGrace is how long a stopping controller waits for open calls before
// it drops them. Watches do not end on their own, so it must be short.
var serveStopGrace = 2 * time.Second

// GRPC is the backend a client uses when the tasks run on a controller
// elsewhere: every Backend method is one RPC to it, and the records stay in
// that host's data dir. It is also the daemon side - Serve hosts the same
// interface over its own file Store - so the two ends cannot drift apart.
type GRPC struct {
	addr    string // host:port
	url     string // as configured, for Endpoint
	timeout time.Duration

	conn   *grpc.ClientConn
	client acknpb.AcknClient

	// serving counts Serve calls in flight: this process is the daemon while
	// it is above zero, and a client otherwise.
	serving atomic.Int32

	// selfHosted is set by Local: this object serves its own server, so the
	// state is on this machine even though every call goes over the wire.
	selfHosted bool
	// stop and done belong to a self-hosted server: cancel it, wait for it.
	stop context.CancelFunc
	done chan struct{}
	// serveErr is what the self-hosted server returned. Written before done
	// is closed and read only after receiving from it.
	serveErr error

	// local is the daemon's own task state. It is what Serve answers from:
	// the controller keeps the on-disk model, and clients ask it.
	local *Store

	cfgMu   sync.RWMutex
	cfgYAML func() string // the daemon's published settings, when serving
}

var (
	_ Backend       = (*GRPC)(nil)
	_ Shared        = (*GRPC)(nil)
	_ Endpointer    = (*GRPC)(nil)
	_ Streamer      = (*GRPC)(nil)
	_ Controller    = (*GRPC)(nil)
	_ ControlDaemon = (*GRPC)(nil)
	_ ConfigSharer  = (*GRPC)(nil)
)

// OpenGRPC connects to an ackn:// endpoint. The connection is lazy, so a
// controller that is briefly down does not stop local commands from running.
func OpenGRPC(raw, dataDir string) (*GRPC, error) {
	addr, err := parseAcknAddr(raw)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("ackn %s: %w", addr, err)
	}
	return &GRPC{
		addr:    addr,
		url:     "ackn://" + addr,
		timeout: grpcTimeout,
		conn:    conn,
		client:  acknpb.NewAcknClient(conn),
		local:   NewStore(dataDir),
	}, nil
}

// parseAcknAddr accepts ackn://host:port, a bare host:port, and ackn:// (the
// default), and refuses what it cannot reach.
func parseAcknAddr(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("ackn: empty endpoint")
	}
	if !strings.Contains(raw, "://") {
		if !strings.Contains(raw, ":") {
			return "", fmt.Errorf("ackn: %q needs a port (for example %s)", raw, DefaultGRPCAddr)
		}
		return raw, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("ackn: %w", err)
	}
	if u.Scheme != "ackn" {
		return "", fmt.Errorf("ackn: unsupported scheme %q (use ackn://)", u.Scheme)
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("ackn: %q: the endpoint is host:port, without a path", raw)
	}
	addr := u.Host
	if addr == "" {
		addr = DefaultGRPCAddr
	}
	if u.Port() == "" && u.Hostname() != "" {
		addr = net.JoinHostPort(u.Hostname(), "7687")
	}
	return addr, nil
}

// rpcCtx bounds one call. Every method goes through it: an RPC with no
// deadline is a command that can hang for ever on a server that stopped
// answering.
func (g *GRPC) rpcCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {} // the caller's own patience wins
	}
	return context.WithTimeout(ctx, g.timeout)
}

// artifactMissing is "the daemon has no such artifact yet": the words are its
// own, and errors.Is(err, ErrNoArtifact) is true even though the sentinel is
// not spelled out in the text. One sentence, one meaning.
type artifactMissing struct{ msg string }

func (e artifactMissing) Error() string        { return e.msg }
func (e artifactMissing) Is(target error) bool { return target == ErrNoArtifact }

// rpcErr keeps the words the server sent and drops gRPC's prefix, so a
// message reads like the file backend's ("no task \"abc\" …") instead of
// "rpc error: code = NotFound desc = …".
//
// A transport failure is not the server talking, though: it means nothing is
// listening, or the network went away. That is ErrNoController and the address
// it tried, because it is a fact a person can act on ("start a controller",
// "your tunnel dropped") rather than a gRPC sentence. Send already did this;
// the reads - `list-tasks`, `watch-task` - are where people meet it most.
func (g *GRPC) rpcErr(err error) error { return rpcErrAt(g.addr, err) }

func rpcErrAt(addr string, err error) error {
	if err == nil {
		return nil
	}
	if unreachable(err) {
		st, _ := status.FromError(err)
		why := "not reachable"
		switch st.Code() {
		case codes.DeadlineExceeded:
			why = "did not answer in time"
		case codes.Canceled:
			why = "the request was cancelled"
		case codes.Unauthenticated, codes.PermissionDenied:
			why = "refused the request"
		}
		return fmt.Errorf("%w at %s: %s", ErrNoController, addr, why)
	}
	if st, ok := status.FromError(err); ok {
		return errors.New(st.Message())
	}
	return err
}

// unreachable reports a transport failure: nothing answered, so nothing is
// known. The distinction matters - "no controller" is a fact to report, while
// a dropped connection is not proof that a task died.
func unreachable(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return true
	}
	switch st.Code() {
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.Unauthenticated, codes.PermissionDenied:
		return true
	}
	return false
}

func jmarshal(v any) ([]byte, error) { return json.Marshal(v) }

func (g *GRPC) Create(t *Task) error { return g.CreateWithBrief(t, "") }

// CreateWithBrief creates the task and hands the daemon the GitHub context for
// its brief. The client is usually the machine with a signed-in gh, but the
// file belongs where the task runs, so it travels with the record.
func (g *GRPC) CreateWithBrief(t *Task, github string) error {
	data, err := jmarshal(t)
	if err != nil {
		return err
	}
	ctx, cancel := g.rpcCtx(context.Background())
	defer cancel()
	_, err = g.client.Create(ctx, &acknpb.CreateRequest{Json: data, Github: github})
	return g.rpcErr(err)
}

// Artifact reads one of the task's run files from the controller. A file no
// run has written yet comes back as ErrNoArtifact, which a caller treats as
// "nothing to show" rather than a failure: the daemon answers it as NotFound.
func (g *GRPC) Artifact(id, name string) (string, error) {
	ctx, cancel := g.rpcCtx(context.Background())
	defer cancel()
	b, err := g.client.Artifact(ctx, &acknpb.ArtifactRequest{Task: id, Name: name})
	if err != nil {
		// "That file is not written yet" is an answer, not a failure: a caller
		// shows nothing for it. Anything else is the controller talking (or not
		// answering), and is reported as it is.
		if st, ok := status.FromError(err); ok && !unreachable(err) && st.Code() == codes.NotFound {
			// The daemon's message already reads "no such task artifact: …":
			// it wrapped the sentinel into the text it sent. Wrapping it again
			// with %w put the sentence there twice ("no such task artifact: no
			// such task artifact: …"), so the *code* carries the sentinel and
			// the text is passed through as it arrived.
			return "", artifactMissing{msg: st.Message()}
		}
		return "", g.rpcErr(err)
	}
	return string(b.Json), nil
}

func (g *GRPC) Save(t *Task) error {
	data, err := jmarshal(t)
	if err != nil {
		return err
	}
	ctx, cancel := g.rpcCtx(context.Background())
	defer cancel()
	_, err = g.client.Save(ctx, &acknpb.Blob{Json: data})
	return g.rpcErr(err)
}

func (g *GRPC) Load(id string) (*Task, error) {
	ctx, cancel := g.rpcCtx(context.Background())
	defer cancel()
	b, err := g.client.Load(ctx, &acknpb.Id{Id: id})
	if err != nil {
		return nil, g.rpcErr(err)
	}
	return decodeTask(b.Json)
}

func (g *GRPC) Find(ref string) (*Task, error) {
	ctx, cancel := g.rpcCtx(context.Background())
	defer cancel()
	b, err := g.client.Find(ctx, &acknpb.Id{Id: ref})
	if err != nil {
		return nil, g.rpcErr(err)
	}
	return decodeTask(b.Json)
}

func (g *GRPC) List() ([]*Task, error) {
	ctx, cancel := g.rpcCtx(context.Background())
	defer cancel()
	rep, err := g.client.List(ctx, &acknpb.Empty{})
	if err != nil {
		return nil, g.rpcErr(err)
	}
	out := make([]*Task, 0, len(rep.Tasks))
	for _, data := range rep.Tasks {
		t, err := decodeTask(data)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func (g *GRPC) Delete(id string) error {
	ctx, cancel := g.rpcCtx(context.Background())
	defer cancel()
	_, err := g.client.Delete(ctx, &acknpb.Id{Id: id})
	return g.rpcErr(err)
}

func (g *GRPC) Rename(t *Task, name string) error {
	data, err := jmarshal(t)
	if err != nil {
		return err
	}
	ctx, cancel := g.rpcCtx(context.Background())
	defer cancel()
	rep, err := g.client.Rename(ctx, &acknpb.RenameRequest{Json: data, Name: name})
	if err != nil {
		return g.rpcErr(err)
	}
	if err := applyRecord(&acknpb.Blob{Json: rep.Json}, t); err != nil {
		return err
	}
	if t != nil {
		t.Name = rep.Name
	}
	return nil
}

func (g *GRPC) Switch(t *Task, n int, provider, model, effort string) error {
	data, err := jmarshal(t)
	if err != nil {
		return err
	}
	ctx, cancel := g.rpcCtx(context.Background())
	defer cancel()
	b, err := g.client.Switch(ctx, &acknpb.SwitchRequest{
		Json: data, Agent: int32(n), Provider: provider, Model: model, Effort: effort,
	})
	if err != nil {
		return g.rpcErr(err)
	}
	return applyRecord(b, t)
}

// applyRecord copies a record the daemon returned back into the caller's.
// Rename and Switch change the task they are handed, so a client that kept its
// own copy would show the old name or model and undo the change on its next
// Save.
func applyRecord(b *acknpb.Blob, t *Task) error {
	if t == nil || b == nil || len(b.Json) == 0 {
		return nil
	}
	got, err := decodeTask(b.Json)
	if err != nil {
		return err
	}
	*t = *got
	return nil
}

// Reconcile asks nothing: the daemon owns the record and reconciles it while
// answering, so a client that cannot reach it leaves the task alone - which is
// the contract (not knowing is not proof of death).
func (g *GRPC) Reconcile(t *Task) {}

func (g *GRPC) Locked(id string) (bool, error) {
	ctx, cancel := g.rpcCtx(context.Background())
	defer cancel()
	rep, err := g.client.Locked(ctx, &acknpb.Id{Id: id})
	if err != nil {
		return false, g.rpcErr(err)
	}
	return rep.Value, nil
}

func (g *GRPC) BeginRun(id string) error {
	ctx, cancel := g.rpcCtx(context.Background())
	defer cancel()
	_, err := g.client.BeginRun(ctx, &acknpb.Id{Id: id})
	return g.rpcErr(err)
}

func (g *GRPC) EndRun(id string) error {
	ctx, cancel := g.rpcCtx(context.Background())
	defer cancel()
	_, err := g.client.EndRun(ctx, &acknpb.Id{Id: id})
	return g.rpcErr(err)
}

func (g *GRPC) Heartbeat(id string) error {
	ctx, cancel := g.rpcCtx(context.Background())
	defer cancel()
	_, err := g.client.Heartbeat(ctx, &acknpb.Id{Id: id})
	return g.rpcErr(err)
}

func (g *GRPC) appendEvent(id string, e event.Event) error {
	data, err := jmarshal(e)
	if err != nil {
		return err
	}
	ctx, cancel := g.rpcCtx(context.Background())
	defer cancel()
	_, err = g.client.AppendEvent(ctx, &acknpb.EventRequest{Task: id, Json: data})
	return g.rpcErr(err)
}

func (g *GRPC) AppendEvent(id string, e event.Event) error { return g.appendEvent(id, e) }

func (g *GRPC) ReadEvents(id string, off int64) ([]event.Event, int64, error) {
	ctx, cancel := g.rpcCtx(context.Background())
	defer cancel()
	rep, err := g.client.ReadEvents(ctx, &acknpb.EventsRequest{Task: id, After: off})
	if err != nil {
		return nil, off, g.rpcErr(err)
	}
	out := make([]event.Event, 0, len(rep.Events))
	for _, data := range rep.Events {
		var e event.Event
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, off, fmt.Errorf("event: %w", err)
		}
		out = append(out, e)
	}
	return out, rep.Cursor, nil
}

func (g *GRPC) ReadLive(id string, n int) *Live {
	ctx, cancel := g.rpcCtx(context.Background())
	defer cancel()
	rep, err := g.client.ReadLive(ctx, &acknpb.LiveRequest{Task: id, Agent: int32(n)})
	if err != nil || len(rep.Json) == 0 {
		return nil
	}
	var l Live
	if json.Unmarshal(rep.Json, &l) != nil {
		return nil
	}
	return &l
}

func (g *GRPC) WriteLive(id string, n int, l Live) error {
	data, err := jmarshal(l)
	if err != nil {
		return err
	}
	ctx, cancel := g.rpcCtx(context.Background())
	defer cancel()
	_, err = g.client.WriteLive(ctx, &acknpb.LiveWrite{Task: id, Agent: int32(n), Json: data})
	return g.rpcErr(err)
}

func (g *GRPC) ClearLive(id string, n int) error {
	ctx, cancel := g.rpcCtx(context.Background())
	defer cancel()
	_, err := g.client.ClearLive(ctx, &acknpb.LiveRequest{Task: id, Agent: int32(n)})
	return g.rpcErr(err)
}

// Subscribe follows a task's events: the server replays those after the
// cursor, then streams new ones until stop is called or ctx ends.
func (g *GRPC) Subscribe(ctx context.Context, id string, after int64) (<-chan event.Event, func(), error) {
	ctx, cancel := context.WithCancel(ctx)
	stream, err := g.client.Watch(ctx, &acknpb.EventsRequest{Task: id, After: after})
	if err != nil {
		cancel()
		return nil, nil, g.rpcErr(err)
	}
	out := make(chan event.Event, 64)
	go func() {
		defer close(out)
		for {
			b, err := stream.Recv()
			if err != nil {
				return // ctx ended, or the controller went away
			}
			var e event.Event
			if json.Unmarshal(b.Json, &e) != nil {
				continue
			}
			select {
			case out <- e:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, cancel, nil
}

// Send relays a start or stop to the daemon and waits up to wait for its
// answer. With no controller listening this is ErrNoController, not a
// transport error: the person needs to know nothing is watching.
func (g *GRPC) Send(ctx context.Context, req Request, wait time.Duration) (Response, error) {
	if err := CheckRequest(req); err != nil {
		return Response{}, err
	}
	if req.At.IsZero() {
		req.At = time.Now().UTC()
	}
	if wait <= 0 {
		wait = defaultControlWait
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	rep, err := g.client.Control(ctx, &acknpb.ControlRequest{
		Id: req.ID, Action: string(req.Action), Task: req.Task, AtUnixNano: req.At.UnixNano(),
	})
	if err != nil {
		return Response{}, g.rpcErr(err)
	}
	resp := Response{OK: rep.Ok, Message: rep.Message, Error: rep.Error}
	if !resp.OK && resp.Error != "" && strings.Contains(resp.Error, "controller") {
		return Response{}, fmt.Errorf("%w (%s)", ErrNoController, g.addr)
	}
	return resp, nil
}

// ControllerAlive reports whether a controller answers, so a command can say
// what is wrong before queueing work.
func (g *GRPC) ControllerAlive() bool {
	ctx, cancel := g.rpcCtx(context.Background())
	defer cancel()
	_, err := g.client.Ping(ctx, &acknpb.Empty{})
	return err == nil
}

// Shared reports whether the tasks run somewhere else, which on a controller
// host they do not: there the process *is* the one that runs them, and it
// keeps their state in a local directory.
//
// The distinction is not cosmetic. Everything remote mode turns off - this
// machine's workspace policy, provider sign-in and preflight - is correct for a
// client and wrong for the daemon: with Shared() true on both, a runner started
// by the controller saw itself as a client, could not serve its own task over
// gRPC, and applied a client's trust rules to the daemon's own directories. A
// runner spawned here gets no endpoint in its environment, so it opens the
// local store and answers false; a client that dialled in has a connection to
// something it did not open, and answers true.
//
// A self-hosted backend (see Local) answers false for the same reason the
// daemon does: the state it serves is this machine's, so nothing about remote
// mode applies to it.
func (g *GRPC) Shared() bool { return !g.selfHosted && g.serving.Load() == 0 }

// Endpoint is where task state lives, in words safe to print: this machine's
// data dir when this process is the one serving it, the controller's address
// when it dialled out to another.
func (g *GRPC) Endpoint() string {
	if g.selfHosted {
		return endpointOf(g.local)
	}
	return g.url
}

// Close releases the connection, and stops the server when this backend
// serves its own: a process that opened a local backend must leave no server
// running, and its callers are done by the time it closes.
func (g *GRPC) Close() error {
	if g.stop != nil {
		g.stop()
		g.stop = nil
	}
	if g.done != nil {
		<-g.done
		g.done = nil
	}
	if g.conn == nil {
		return nil
	}
	return g.conn.Close()
}

// SetSharedConfig gives the served Config RPC its answer: the daemon's own
// settings, read where they live rather than copied to a key that expires.
func (g *GRPC) SetSharedConfig(f func() string) {
	g.cfgMu.Lock()
	defer g.cfgMu.Unlock()
	g.cfgYAML = f
}

// SharedConfig asks the controller what its tasks run under.
func (g *GRPC) SharedConfig() (string, bool, error) {
	ctx, cancel := g.rpcCtx(context.Background())
	defer cancel()
	rep, err := g.client.Config(ctx, &acknpb.Empty{})
	if err != nil {
		return "", false, g.rpcErr(err)
	}
	if !rep.Found {
		return "", false, nil
	}
	return rep.Yaml, true, nil
}

// ShareConfig is a no-op: the controller *is* the source of its settings, so
// there is nothing to publish to it.
func (g *GRPC) ShareConfig(string) error { return nil }

// Serve hosts the interface on the daemon side, answering from this host's
// data dir, until ctx is cancelled. handle carries out a start or stop here,
// as the person would have run it here.
func (g *GRPC) Serve(ctx context.Context, handle func(Request) Response) error {
	g.serving.Add(1)
	defer g.serving.Add(-1)
	ln, err := net.Listen("tcp", g.addr)
	if err != nil {
		return fmt.Errorf("ackn controller: %w", err)
	}
	// Say it on the terminal too, not only in --help: the person who typed the
	// bind address is the one who needs to know it is unauthenticated.
	if host, _, herr := net.SplitHostPort(g.addr); herr == nil && !loopbackHost(host) {
		warnUnprotected(g.addr)
	}
	return serveAckn(ctx, ln, g.local, handle, g.sharedYAML)
}

func (g *GRPC) sharedYAML() string {
	g.cfgMu.RLock()
	defer g.cfgMu.RUnlock()
	if g.cfgYAML == nil {
		return ""
	}
	return g.cfgYAML()
}

// warnUnprotected is what a controller says when it binds beyond loopback.
// It is a variable so tests can watch for it.
var warnUnprotected = func(addr string) {
	fmt.Fprintf(os.Stderr, "! %s has no authentication and no encryption: anyone who can reach it can read and control every task. Use a private interface or an ssh tunnel.\n", addr)
}

// loopbackHost reports whether host is only reachable from this machine.
func loopbackHost(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// serveAckn runs a gRPC server over be until ctx is cancelled. It is separate
// from Serve so a test can host one on an ephemeral port without guessing it.
func serveAckn(ctx context.Context, ln net.Listener, be Backend, handle func(Request) Response, cfgYAML func() string) error {
	srv := grpc.NewServer()
	acknpb.RegisterAcknServer(srv, &acknServer{be: be, handle: handle, cfgYAML: cfgYAML})
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// A graceful stop waits for every open RPC, and Watch stays open
			// until the *client* leaves: without a bound, Ctrl-C, `systemctl
			// stop` and an upgrade hang for as long as anyone has `watch-task`
			// or the dashboard open, and then get killed mid-request. Give the
			// short calls a moment, then end the streams - a follower now
			// reconnects instead of dying (see the CLI's watch).
			stopped := make(chan struct{})
			go func() { srv.GracefulStop(); close(stopped) }()
			select {
			case <-stopped:
			case <-time.After(serveStopGrace):
				srv.Stop()
				<-stopped
			}
		case <-done:
		}
	}()
	err := srv.Serve(ln)
	close(done)
	if err != nil && !errors.Is(err, grpc.ErrServerStopped) && ctx.Err() == nil {
		return err
	}
	return nil
}

// acknServer answers RPCs from this host's backend. Every method is the same
// call the CLI would make locally, so the daemon's rules (locking, liveness,
// reconciliation, the workspace boundary) are applied once, here.
type acknServer struct {
	acknpb.UnimplementedAcknServer
	be      Backend
	handle  func(Request) Response
	cfgYAML func() string
}

func (s *acknServer) Ping(context.Context, *acknpb.Empty) (*acknpb.Pong, error) {
	return &acknpb.Pong{Endpoint: endpointOf(s.be), Shared: true}, nil
}

func endpointOf(be Backend) string {
	if ep, ok := be.(Endpointer); ok {
		return ep.Endpoint()
	}
	return ""
}

func (s *acknServer) Create(_ context.Context, r *acknpb.CreateRequest) (*acknpb.Empty, error) {
	t, err := decodeTask(r.Json)
	if err != nil {
		return nil, err
	}
	br, ok := s.be.(Briefed)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "this endpoint cannot store a task's GitHub context")
	}
	if err := br.CreateWithBrief(t, r.Github); err != nil {
		return nil, rpcStatus(err)
	}
	return &acknpb.Empty{}, nil
}

// Artifact serves one of the task's run files from this host's store. The name
// is allowlisted by the store (ArtifactPath), not here: the rule belongs with
// the files it protects.
func (s *acknServer) Artifact(_ context.Context, r *acknpb.ArtifactRequest) (*acknpb.Blob, error) {
	as, ok := s.be.(Artifacts)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "this endpoint keeps no task files")
	}
	text, err := as.Artifact(r.Task, r.Name)
	if err != nil {
		return nil, rpcStatus(err)
	}
	return &acknpb.Blob{Json: []byte(text)}, nil
}

func (s *acknServer) Save(_ context.Context, b *acknpb.Blob) (*acknpb.Empty, error) {
	t, err := decodeTask(b.Json)
	if err != nil {
		return nil, err
	}
	if err := s.be.Save(t); err != nil {
		return nil, rpcStatus(err)
	}
	return &acknpb.Empty{}, nil
}

// load answers a full id or a prefix/name reference, and reconciles the record
// first: a client that asks for a task should see what this host knows, not a
// stale "running" left by a runner that died.
func (s *acknServer) load(id string) (*Task, error) {
	t, err := s.be.Load(id)
	if err != nil {
		t, err = s.be.Find(id)
	}
	if err != nil {
		return nil, err
	}
	s.be.Reconcile(t)
	return t, nil
}

func (s *acknServer) Load(_ context.Context, id *acknpb.Id) (*acknpb.Blob, error) {
	t, err := s.load(id.Id)
	if err != nil {
		return nil, rpcStatus(err)
	}
	data, err := jmarshal(t)
	if err != nil {
		return nil, rpcStatus(err)
	}
	return &acknpb.Blob{Json: data}, nil
}

func (s *acknServer) Find(_ context.Context, id *acknpb.Id) (*acknpb.Blob, error) {
	return s.Load(context.Background(), id)
}

func (s *acknServer) List(context.Context, *acknpb.Empty) (*acknpb.ListReply, error) {
	tasks, err := s.be.List()
	if err != nil {
		return nil, rpcStatus(err)
	}
	rep := &acknpb.ListReply{}
	for _, t := range tasks {
		s.be.Reconcile(t)
		data, err := jmarshal(t)
		if err != nil {
			return nil, rpcStatus(err)
		}
		rep.Tasks = append(rep.Tasks, data)
	}
	return rep, nil
}

func (s *acknServer) Delete(_ context.Context, id *acknpb.Id) (*acknpb.Empty, error) {
	if err := s.be.Delete(id.Id); err != nil {
		return nil, rpcStatus(err)
	}
	return &acknpb.Empty{}, nil
}

func (s *acknServer) Rename(_ context.Context, r *acknpb.RenameRequest) (*acknpb.RenameReply, error) {
	t, err := decodeTask(r.Json)
	if err != nil {
		return nil, err
	}
	if err := s.be.Rename(t, r.Name); err != nil {
		return nil, rpcStatus(err)
	}
	return &acknpb.RenameReply{Json: mustJSON(t), Name: t.Name}, nil
}

func (s *acknServer) Switch(_ context.Context, r *acknpb.SwitchRequest) (*acknpb.Blob, error) {
	t, err := decodeTask(r.Json)
	if err != nil {
		return nil, err
	}
	if err := s.be.Switch(t, int(r.Agent), r.Provider, r.Model, r.Effort); err != nil {
		return nil, rpcStatus(err)
	}
	return blobOf(t)
}

func blobOf(t *Task) (*acknpb.Blob, error) {
	return &acknpb.Blob{Json: mustJSON(t)}, nil
}

func (s *acknServer) Locked(_ context.Context, id *acknpb.Id) (*acknpb.BoolReply, error) {
	locked, err := s.be.Locked(id.Id)
	if err != nil {
		return nil, rpcStatus(err)
	}
	return &acknpb.BoolReply{Value: locked}, nil
}

func (s *acknServer) BeginRun(_ context.Context, id *acknpb.Id) (*acknpb.Empty, error) {
	if err := s.be.BeginRun(id.Id); err != nil {
		return nil, rpcStatus(err)
	}
	return &acknpb.Empty{}, nil
}

func (s *acknServer) EndRun(_ context.Context, id *acknpb.Id) (*acknpb.Empty, error) {
	if err := s.be.EndRun(id.Id); err != nil {
		return nil, rpcStatus(err)
	}
	return &acknpb.Empty{}, nil
}

func (s *acknServer) Heartbeat(_ context.Context, id *acknpb.Id) (*acknpb.Empty, error) {
	if err := s.be.Heartbeat(id.Id); err != nil {
		return nil, rpcStatus(err)
	}
	return &acknpb.Empty{}, nil
}

func (s *acknServer) AppendEvent(_ context.Context, r *acknpb.EventRequest) (*acknpb.Empty, error) {
	var e event.Event
	if err := json.Unmarshal(r.Json, &e); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "event: %v", err)
	}
	if err := s.be.AppendEvent(r.Task, e); err != nil {
		return nil, rpcStatus(err)
	}
	return &acknpb.Empty{}, nil
}

func (s *acknServer) ReadEvents(_ context.Context, r *acknpb.EventsRequest) (*acknpb.EventsReply, error) {
	evs, cursor, err := s.be.ReadEvents(r.Task, r.After)
	if err != nil {
		return nil, rpcStatus(err)
	}
	rep := &acknpb.EventsReply{Cursor: cursor}
	for _, e := range evs {
		data, err := jmarshal(e)
		if err != nil {
			return nil, rpcStatus(err)
		}
		rep.Events = append(rep.Events, data)
	}
	return rep, nil
}

func (s *acknServer) ReadLive(_ context.Context, r *acknpb.LiveRequest) (*acknpb.LiveReply, error) {
	l := s.be.ReadLive(r.Task, int(r.Agent))
	if l == nil {
		return &acknpb.LiveReply{}, nil
	}
	data, err := jmarshal(l)
	if err != nil {
		return nil, rpcStatus(err)
	}
	return &acknpb.LiveReply{Json: data}, nil
}

func (s *acknServer) WriteLive(_ context.Context, r *acknpb.LiveWrite) (*acknpb.Empty, error) {
	var l Live
	if err := json.Unmarshal(r.Json, &l); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "live: %v", err)
	}
	if err := s.be.WriteLive(r.Task, int(r.Agent), l); err != nil {
		return nil, rpcStatus(err)
	}
	return &acknpb.Empty{}, nil
}

func (s *acknServer) ClearLive(_ context.Context, r *acknpb.LiveRequest) (*acknpb.Empty, error) {
	if err := s.be.ClearLive(r.Task, int(r.Agent)); err != nil {
		return nil, rpcStatus(err)
	}
	return &acknpb.Empty{}, nil
}

// watchInterval is how often Watch looks for new events. The store appends to
// a file; there is no notification to wait on, and this is a handful of reads
// a second on the host that owns the file.
var watchInterval = 200 * time.Millisecond

// Watch replays the events after the cursor, then streams new ones until the
// client goes away. An idle watch costs one read per interval.
func (s *acknServer) Watch(r *acknpb.EventsRequest, stream acknpb.Ackn_WatchServer) error {
	cursor := r.After
	send := func(evs []event.Event) error {
		for _, e := range evs {
			data, err := jmarshal(e)
			if err != nil {
				return status.Errorf(codes.Internal, "event: %v", err)
			}
			if err := stream.Send(&acknpb.Blob{Json: data}); err != nil {
				return err
			}
		}
		return nil
	}
	for {
		evs, next, err := s.be.ReadEvents(r.Task, cursor)
		if err != nil {
			return rpcStatus(err)
		}
		if len(evs) > 0 {
			if err := send(evs); err != nil {
				return err
			}
		}
		cursor = next
		select {
		case <-stream.Context().Done():
			return nil
		case <-time.After(watchInterval):
		}
	}
}

// Control carries out a start or stop here and answers with what happened,
// synchronously: gRPC has a reply channel, so there is no queue to poll and no
// registration to keep fresh.
func (s *acknServer) Control(ctx context.Context, r *acknpb.ControlRequest) (*acknpb.ControlReply, error) {
	if s.handle == nil {
		return &acknpb.ControlReply{Error: "this endpoint does not control tasks: run `ackn4th controller`"}, nil
	}
	req := Request{ID: r.Id, Action: Action(r.Action), Task: r.Task, At: time.Unix(0, r.AtUnixNano).UTC()}
	resp := s.handle(req)
	return &acknpb.ControlReply{Ok: resp.OK, Message: resp.Message, Error: resp.Error}, nil
}

func (s *acknServer) Config(context.Context, *acknpb.Empty) (*acknpb.ConfigReply, error) {
	if s.cfgYAML == nil {
		return &acknpb.ConfigReply{}, nil
	}
	text := s.cfgYAML()
	if text == "" {
		return &acknpb.ConfigReply{}, nil
	}
	return &acknpb.ConfigReply{Yaml: text, Found: true}, nil
}

// rpcStatus is the daemon's side: it turns a backend error into something the
// client can print. It names no address - the client adds its own - and it
// keeps the two codes a caller acts on: "you no longer own this run", and "that
// file is not there". Everything else keeps its words under Unknown.
func rpcStatus(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, ErrRunLost), errors.Is(err, ErrNoRun):
		return status.Error(codes.FailedPrecondition, err.Error())
	case errors.Is(err, ErrNoArtifact):
		return status.Error(codes.NotFound, err.Error())
	}
	return status.Error(codes.Unknown, err.Error())
}

func mustJSON(v any) []byte {
	data, _ := json.Marshal(v)
	return data
}

func decodeTask(data []byte) (*Task, error) {
	var t Task
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("task: %w", err)
	}
	return &t, nil
}
