# QA test plan: how the CLI reaches a daemon

Scope: the `task.Backend` interface (`internal/task/backend.go`) and its one
implementation, the gRPC client (`internal/task/grpc.go`, `internal/task/local.go`,
`internal/task/ackn.proto`, `internal/task/acknpb`), which either dials a
controller or serves this machine's data dir in-process via the file store
(`internal/task/task.go`); the runner that writes through it
(`internal/task/runner.go`); and the CLI/dashboard commands that read or write
task state (`internal/cli/{tasks,control,dash,github,root}.go`,
`internal/dash`).

The design in one line: task state is reached through one interface and one
implementation — the daemon-side store is served over `ackn://host:port` to a
client anywhere, and the same client reaches the same store in-process when
nothing is configured. `docs/grpc-backend.md` is the reader-facing version of
this.

## How to run

```sh
export GOTOOLCHAIN=auto
go test ./internal/task/ ./internal/cli/ ./internal/dash/ ./internal/workspace/ -count=1   # the suites
go test -race ./internal/task/ ./internal/cli/ ./internal/dash/ -count=1                   # concurrency
go test ./internal/task/ ./internal/cli/ -run '^TestQA' -count=1 -v                        # bug-report invariants
go test ./test/docs/ -count=1                                                              # links, anchors, env vars
```

No test needs a provider key, a terminal, or a network beyond loopback: the
dashboard and watch tests drive the real `app`/`dash.Options` wiring with output
captured in a buffer, and the controller tests host a real gRPC server on
`127.0.0.1:0`. A test that needs a controller picks its port by listening on
`:0`, closing, and retrying if `Serve` loses the race — never by hard-coding a
port, and never with a sleep in place of waiting for `ControllerAlive()`.

**A failing `TestQA*` test is a bug report, not a broken test.** Do not make one
green by loosening it: fix the production code, or argue the expectation is
wrong and change this plan in the same breath.

## Acceptance criteria → tests

| AC | What must hold | Test |
| --- | --- | --- |
| AC1 | One interface, one implementation reached two ways: the local endpoint (this process serving its own data dir over the wire) and the controller answer every `task.Backend` call through the same client | `task.TestFileBackend`, `task.TestBothBackendsAreEndpoints`, `task.TestStoreEndpointIsTheDataDir`, `task.TestLocalIsTheWireImplementation` |
| AC2 | The default stays local: with no `ACKN4TH_BACKEND_URL` every command uses this machine's data dir, served in-process through the same client the controller is reached with | `task.TestOpenBackendLocalByDefault`, `cli.TestDefaultBackendIsLocal` |
| AC3 | Endpoint parsing accepts `ackn://host:port` and a bare `host:port` (filling in the port), and refuses anything else with a message naming the flag | `task.TestGRPCEndpointParsing`, `task.TestOpenBackendSelectsByScheme`, `cli.TestAnUnsupportedBackendURLIsRefused` |
| AC4 | An endpoint makes the backend remote, so commands stop acting on local processes | `task.TestOpenBackendSelectsTheController`, `cli.TestControllerURLMakesTheBackendRemote`, `cli.TestControllerRequiresASharedBackend` |
| AC5 | A task id is one path segment on disk and one key over the wire; every call rejects empty, `.`, `..`, spaces, `:` and NUL | `task.TestStoreRefusesIDsThatAreNotOnePathSegment`, `task.TestNewIDsAreValid` |
| AC6 | A record crosses the wire without losing a field, and the lifecycle works through the client alone | `task.TestGRPCRecordRoundTripKeepsEveryField`, `task.TestGRPCTaskLifecycleOverTheWire` |
| AC7 | Events are append-only and replayable from a cursor, and a live view streams new ones | `task.TestGRPCEventsAndLivePreview`, `task.TestGRPCWatchReplaysThenStreams`, `task.TestLiveFeedShowsAFileBeingWritten` |
| AC8 | An unreachable or silent controller fails fast with a message a person can act on; nothing waits for ever | `task.TestGRPCDoesNotHangOnASilentServer`, `task.TestGRPCSendWithNoController`, `cli.TestRemoteStartWithoutAControllerFails` |
| AC9 | A client runs tasks under the **daemon's** settings: its workspace roots, providers and models, adopted over the interface | `task.TestGRPCConfigComesFromTheDaemon`, `cli.TestRemoteWorkspaceComesFromTheDaemon`, `cli.TestRemoteWorkspaceWithoutADefaultRootSaysSo` |
| AC10 | A start from a client is carried out by the daemon, which clones what the task needs before it spawns anything | `cli.TestRemoteStartGoesThroughTheController`, `cli.TestTheRecordCarriesTheRepository`, `cli.TestRepoPathIsWhereTheClientSaysTheTaskWillWork` |
| AC11 | A client never signals a pid it did not start; the controller stops a run only when the record's pid is the one holding that task's lock | `cli.TestRemoteStopDoesNotSignalLocally`, `cli.TestRemoteStopWithoutAControllerFails`, `task.TestStoreRunnerPIDNeedsBothTheLockAndThePid`, `task.TestGRPCStopsWithAWatchOpen` |
| AC12 | The daemon's run files reach a client as **text**: an allowlisted name, a validated id, a path asserted to be one file in the task's own directory, and no host path in any refusal | `task.TestStoreArtifactServesOnlyTheAllowlist`, `task.TestGRPCArtifactsComeFromTheDaemon`, `task.TestTheMissingArtifactTextIsSaidOnce`, `cli.TestTheSummaryComesFromTheController` |
| AC13 | The GitHub context travels with the record, so it lands where the runner will read it | `task.TestCreateWithBriefStoresTheGitHubContext`, `cli.TestTheGitHubContextTravelsWithTheRecord` |
| AC14 | A watch started during a controller restart waits for it, while a task the daemon says does not exist is reported at once | `cli.TestWatchWaitsForTheControllerItStartedUnder` |
| AC15 | The local endpoint stays first-class: with no endpoint configured, every command still works on this machine's data dir | `task.TestFileBackend`, `task.TestLocalWritesThroughTheWireIntoTheDataDir` plus the whole `internal/cli` suite, which runs with no controller |
| AC16 | The docs say what the code does: every relative link and `#anchor` resolves, and every documented `ACKN4TH_*` variable exists in the code | `docs.TestRelativeLinksResolve`, `docs.TestDocumentedEnvVarsExist` |
| AC17 | Every state call in the dashboard goes through the backend it was given (`opt.state()`), and no test of that is allowed to rot: the guard reads the sources, so a receiver that no test happens to drive is still caught | `dash.TestTheDashReadsStateThroughItsBackend`, `dash.TestAnUnreachableBackendIsSaidNotShownAsNoTasks` |
| AC18 | Pluggability is structural, not merely documented: the CLI and dashboard reach task state only as `task.Backend`, nothing above the interface imports the gRPC runtime or the generated protobuf package, both implementations are compile-time asserted against the interface, and the CLI really opens the backend it is configured with | `arch.TestOnlyTheBackendImportsTheWire`, `arch.TestBothBackendsAreAssertedAndChosen`, `arch.TestTheCLIOpensTheConfiguredBackend` |
| AC19 | The backend is chosen in **one** place and held in **one** field: the selector is a table of rows rather than a condition chain over `ACKN4TH_BACKEND_URL`, both rows are selectable, the **local** row is the default *and it is the wire implementation served in-process*, and each holder's accessor is exactly one `return` of that field — no `if`/`switch` fallback behind it | `arch.TestTheSelectorIsATable`, `arch.TestTheCLIBackendAccessorDoesNotChoose`, `arch.TestTheDashStateAccessorDoesNotChoose`, `arch.TestEachHolderHasOneBackendField`, `task.TestSelectPicksTheImplementationByURL`, `task.TestEveryRowIsSelectable`, `task.TestTheDefaultRowIsLocal` |
| AC20 | **Every** read or write of task state in the CLI and dashboard goes through the `task.Backend` interface, local runs included: no `<recv>.store.<state method>` call survives above `internal/task`, and the local default really is the gRPC client (`OpenBackend("")` returns `*GRPC` serving this process), so a local-only code path cannot drift from the remote one | `arch.TestTheCLIReadsTaskStateOnlyThroughTheBackend`, `task.TestTheDefaultRowIsLocal`, `cli.TestDefaultBackendIsLocal` |
| AC21 | A local backend depends on **nothing outside the data dir it serves**: no writable temp dir, no home, no port, no socket path. With `TMPDIR` uncreatable it still opens and serves (create, read back through the transport and off the disk), it creates no filesystem object for the transport at all — the client dials an in-memory pipe, so there is no path to exceed the kernel's 104-byte `sun_path` limit and nothing for another local user to reach — and `Close` leaves nothing behind in the data dir — because `load()` opens one for *every* command, so otherwise a read-only `/tmp` breaks `version`, `status` and everything else. `Endpoint()` names the data dir, which is what `status` prints | `task.TestQALocalServesWithoutAWritableTempDir`, `task.TestTheLocalEndpointNamesTheDataDir`, `cli.TestQACLIWorksWithoutAWritableTempDir` |

### One implementation, two locations

This build no longer has a "file backend" and a "gRPC backend" as two things
that could behave differently. There is **one** implementation — the gRPC
client, `*task.GRPC` — reached two ways: it either dials a controller on another
host, or it serves this machine's data dir in-process (`task.Local`) and dials
that. `task.Backend` is still the interface, and every read and write of task
state goes through it, local runs included.

That is deliberate about bugs, not elegance: the person's requirement was that
nothing above `internal/task` may take a local shortcut, because a local-only
path is a behaviour the remote path never exercises. `AC20`'s guard is what
keeps it true, and its mutation is re-adding `a.store.Load(...)` in `__run` —
the one place this rule had already been broken once.

`a.store`/`m.opt.Store` are **not** a second route to state. They are this
host's own files — run logs, clones, snapshots, the run lock, `GitHubPath`,
`SummaryPath`, `RunnerPID`, `Spawn`, `DataDir` — reached through
`task.HostStore` so the CLI writes to the same directory the backend reads
back. See the state/artifact split below for which calls are deliberately local.

### Guarding "one place chooses": why the accessor tests assert an exact body

`arch.TestTheCLIBackendAccessorDoesNotChoose` and its dash twin require the
accessor's body to be **exactly** `return a.be` / `return o.State`. An earlier
version only rejected a literal `if ` and stopped reading at the first `\n}`.
That made it blind to a body with a block in it: a `switch { case a.be != nil:
return a.be; default: return a.store }` truncated to a string that still
contained `return a.be`, so the guard passed on the very second selection it
names. Three mutations now fail it — `if`-shaped, `switch`-shaped, and a bare
`return a.store` — and the dash guard fails on the `switch` shape too.

The lesson generalises and is why this section exists: **mutation-test every
structural guard in `test/arch` before trusting it.** A source-scanning guard
that never fails on the shape it names is worse than none, because it advertises
coverage. When a guard's shape changes, change the mutation with it.

### Why a local backend may not need a temp dir (AC21)

The local endpoint is opened by `load()`, so **every** command opens one. That
makes anything `task.Local` needs a dependency of the whole CLI: when it took its
socket directory from `os.MkdirTemp("")`, a read-only `/tmp` turned `ackn4th
version` into `✗ local backend: mkdir /tmp/ackn4th-…: operation not permitted`.
Read-only `/tmp` is not exotic — `read_only: true` containers, most CI images and
this very sandbox have it (`env -u TMPDIR ./bin/ackn4th list-tasks` reproduces).

The fix is not a better place for a socket, it is **no socket**: the local client
dials an in-memory pipe (`bufconn`) served in the same process, so the transport
creates no filesystem object, needs no path and has no permissions to get wrong.
That kills the temp-dir dependency, the 104-byte `sun_path` limit (a data dir
under a long home cannot hold a unix socket) and the "who may read this socket"
question in one move: nothing outside the process can reach it — no path to
guess, no port to scan, no file to `stat`.

AC21's tests make `TMPDIR` point under a directory that **does not exist**, so
any attempt to create a file there fails for root too, and then assert the
backend serves — create, read back through the transport, and find the record in
the data dir. An "it returned an error saying data dir" assertion would pass on
broken code and is exactly the shape of test this round was about; do not loosen
it back. `task.TestQALocalServesWithoutAWritableTempDir` additionally asserts the
endpoint's address is not a filesystem path and that no `sock-*`/`ackn4th-*`
entry survives `Close`, which is what keeps a socket from creeping back in.

### The state/artifact split, and where a direct `store` call is correct

`a.store` and `m.opt.Store` are **local artifacts**: run logs, clones,
snapshots, the run lock, `GitHubPath`, `SummaryPath`, `RunnerPID`, `Spawn`.
State — the record, the event log, the live preview, deletes, switches —
belongs to `a.backend()` / `opt.state()`, which is the controller when one is
configured. Four direct store calls are deliberate and must not be "fixed":

- `internal/cli/tasks.go` `artifact`/`summaryOf`: behind `a.remote()`, which is
  the local branch of the two.
- `internal/cli/tasks.go` `internalRunCmd`: the detached runner runs on the host
  that holds the clone, so it loads the record from that host on purpose.
- `internal/cli/dash.go` `firstRun`: nil-checked local store, and it lists
  through `a.backend()`.

Changing any of them to a backend call makes a remote task read the wrong
machine; the guard in `dash` would not catch a CLI-side slip, so read this
before touching `a.store`.

## Where the evidence for the findings lives

This branch was built against a review, and every finding that shaped it has a
test. The ones still worth reading as evidence (rather than as coverage) are kept
with the review notes, outside the repository: the shipped tests cited above are
what CI runs, and a path covered only by evidence is a gap to close with a test
here, not a pass. Nothing in this plan needs those files to run.

## What a passing gate looks like

1. `git apply` both patches on a fresh `git archive <base>` extraction that has
   been `git init`ed — `git apply` outside a repository is a silent no-op that
   exits 0, so assert the tracked file count moved.
2. No `.rej`, no `.orig`, and no patch line that carries a timestamp.
3. **The patched tree equals the tree that was verified**, file for file
   (`diff -r` with only the excluded paths). Two cuts of this patch shipped
   with a file that had moved on after the cut — `internal/dash/backend_test.go`
   twice — so "it applied cleanly" is not the check; identity is.
4. **The patch reverts nothing it did not mean to.** Every path the patch
   *deletes* must be a path the base tree no longer needs; a patch cut from a
   copy of the tree that predates a base commit silently undoes that commit.
   The one that shipped deleted both README screenshots from `ddc5371`.
   `git diff --stat base..<branch>` should show no deletion of a file that
   exists in `base` unless the change is about deleting it.
5. `go build ./internal/... ./cmd/...`, `go vet`, `gofmt -l` all clean.
6. `GOOS=windows` and `GOOS=plan9 GOARCH=amd64` builds of `internal/task`.
7. The nine packages above plus `./test/docs/` green, then `-race`.
8. Any extra review tests you add: no unexpected failure. A failure here is a
   finding with a test, not a flake.
9. The tree leaves nothing behind: no controller process, no temp data dir.
