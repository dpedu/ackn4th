# The controller's gRPC interface

Task state stays where the work happens: the controller keeps the same data
directory a local run uses (`<data>/tasks/<id>/task.json`, its event log, its
live preview). What changes is how a CLI on another machine reaches it. In
this mode the CLI is a thin client of the controller:

```sh
# on the host that runs the tasks, where the workspaces and keys are
ACKN4TH_BACKEND_URL=ackn://0.0.0.0:7687 ackn4th controller

# on your laptop
export ACKN4TH_BACKEND_URL=ackn://tasks.internal:7687
ackn4th list-tasks
ackn4th watch-task k3f9qz
ackn4th start-task -d --repo dethmasque/ackn4th "Add rate limiting"
```

`ACKN4TH_BACKEND_URL` names the controller; any other scheme is refused by
name. Unset, the local endpoint is used: this process serves its own data dir
over a private unix socket and reaches it through the same client, so a command
follows one code path whether the state is here or on another machine. Nothing
changes in what it does — same files, same layout, same commands — and nothing
leaves the machine.

## Why one RPC per backend method

`internal/task/ackn.proto` mirrors `task.Backend`: create, save, load, list,
delete, rename, switch, locking, the run markers, events, the live preview,
control, and the daemon's settings. Two calls are not backend methods:
`Create`'s GitHub context and `Artifact`, because the file store has paths the
client has no way to name. That is deliberate. Every defect this
project shipped came from one rule written twice - `load()` pulling settings
the controller had also pulled, the dashboard re-implementing the workspace
policy - and the fix was always to keep one copy.

So the server answers each RPC from *this host's* file store, and the client
translates the same interface. Reconciliation, locking, liveness and the
workspace boundary are applied once, on the daemon, where the state is.

Task records, events and live previews travel as the JSON the file backend
already writes. Mirroring their fields in protobuf would be a second copy of
the model, edited in lockstep for ever; a round-trip test asserts the record
arrives byte for byte as the daemon stored it.

## What runs where

The controller is the daemon: it owns the task state, the workspaces, the
provider credentials and the model calls. A client lists tasks, follows events
and asks for a start or a stop. Everything remote mode switches off on the
client is deliberate - it does not apply this machine's workspace trust rules to
a directory the daemon will work in, it does not require a local API key, and it
does not make a provider request to verify a model:

- **Workspaces.** The daemon resolves `--workspace`, `--scratch` and `--repo`
  against its own roots, clones a repository on its own disk, and enforces the
  trust boundary when it starts the runner. A client that refused a path (or
  cloned it locally) would be judging a filesystem it cannot see. With no
  `--workspace` the task lands in the daemon's `default_workspace_root`, which
  the client reads from the `Config` RPC - never this machine's working
  directory, and never this machine's default root when the daemon published
  none.
- **Clones.** The repository travels on the record (`repo`), and the controller
  clones it when it starts the task: that is the host with the disk and the `gh`
  login the runner will use. `--branch` and `--draft-pr` are still refused from
  a client, because checking a branch out and opening a pull request there is
  not something this build can ask the controller to do yet.
- **Credentials.** Keys stay on the daemon host. Preflight runs there, once,
  where the key and the model are.
- **Settings.** On load a client adopts the daemon's providers, models, limits
  and workspace roots from the `Config` RPC, in memory only: it never writes a
  `config.yaml` it does not own. `ackn4th status` says which machine's settings
  are in effect, and warns when they could not be read.

Two flags a client refuses rather than fakes are `--branch` and `--draft-pr`:
the checkout and the pull request belong to the machine with the clone, and this
build has no request that asks the daemon to do either. Accepting them quietly
would put the agents' commits on whichever branch the clone already had.

A runner is not a client. It is started by the daemon, on the daemon, and works
on the task's own directory: `Store.Spawn` removes the endpoint from its
environment and `app.runner()` hands it the local store. That is what makes a
controller restart safe - the tasks in flight keep running and keep recording
their events. It is also why a runner can never be started on a client.

## The files a run leaves behind

A task's directory also holds what a person reads around the work: `summary.md`,
written when a run ends, `github.md`, the issues and pull requests the prompt
linked to, and `brief.md`, the project brief frozen on the first turn. Those stay
on the daemon - that is the model, not a staging area - and a client asks for
them:

- **`Artifact(task, name)`** returns one of them. The name is checked against a
  short allowlist (`summary.md`, `github.md`, `brief.md`) on the daemon, because
  it arrives from a client over an unauthenticated port and must never name a
  path: the same directory holds the record, the event log and agent
  conversations. A file no run has written yet is answered as "not there", which
  the CLI shows as nothing - `watch-task` simply ends without a summary line, and
  prints the summary text when there is one.
- **`Create` carries `github.md`** when the client could read GitHub, so the
  context lands beside the record on the host that runs the task. The read
  happens where `gh` is signed in - usually the laptop, since that is the
  person's login - but the file belongs to the task, and a copy in the client's
  data dir was one no runner would ever open. A prompt with no GitHub links
  creates no file.

The dashboard's uncommitted-file count follows the same rule: with the workspace
on the controller, `git` on the client cannot see it, so the count is unknown
instead of a `git` process per poll for a path that does not exist here.

## Streaming

`Watch` replays the events after the client's cursor, then streams new ones. It
polls the daemon's own event file a few times a second - the file store has no
notification to wait on - so an idle follower costs one read per interval and
the UI updates in real time without the client polling.

## Rules the interface keeps

- **A silent server must not freeze a command.** Every RPC carries a deadline
  (5s, and the control call the caller's own patience). Without it, a paused
  server, a dropped tunnel or a suspended host freezes every command that
  touches the controller - and in remote mode every command does.
  `TestGRPCDoesNotHangOnASilentServer` pins it.
- **No queue without a reason.** gRPC has a reply channel, so `Control` is one
  synchronous call: no request key, no reply key, no registration that expires,
  no `controller did not answer in time` ambiguity.
- **Not knowing is not losing.** A transport failure is reported as such; it is
  never read as proof that a task died. Only the daemon, which can see its own
  locks and heartbeats, decides a run is over.
- **Settings come from the daemon.** The `Config` RPC answers from the file in
  effect on that host, read per call. There is no published copy to go stale.

## Trust

The interface is unauthenticated and unencrypted: whoever can reach the port
can read every task and start or stop runners. Bind it to a
private interface or reach it through a tunnel, and keep it off public
networks. `ackn4th controller` prints the endpoint it serves.

Provider credentials never cross it: they stay on the daemon host, which is
also the only place a task runs.

An unauthenticated port is why the task ID is checked on the server. Every
backend refuses an ID that is not one path segment (no separator, no `.` or
`..`, nothing that would confuse a key), because the ID arrives from the client
and the file store joins it onto the daemon's tasks directory: `Delete(id)`
removes that directory, so an unvalidated ID would let anyone who reaches the
port delete a directory outside the data dir.
