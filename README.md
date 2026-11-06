# `ackn4th`: a hobbyist's alternative to A2A automation

Wrangle AI agents across providers, models, and effort levels, sending them 
off to tackle a task of your choosing. They can take turns and work serially: 

```
   ▓▒░  a c k n 4 t h  ░▒▓   ::::   taking turns on a task within your project
  ╭──▶ product_manager ──▶ ( ✎ ) ──▶ backend_dev ──▶ ( ✎ ) ──▶ frontend_dev ──╮
  │                                                                           │
  ▲                                                                         ( ✎ )
  │                                                                           ▼
  ╰───── ( ✓?) ◀───── qa_engineer ◀───── ( ✎ ) ◀───── security_engineer ◀─────╯
```

Or in parallel, collaborating until they all agree the task is done:

```
          ▓▒░  a c k n 4 t h  ░▒▓ 
 tackling a task in parallel within your project
       ╭──▶ product_manager ────▶ ( ✎ ) ──╮
       ├──▶ backend_dev ────────▶ ( ✎ ) ──┤
  ╭──▶ ┼──▶ frontend_dev ───────▶ ( ✎ ) ──┼──╮
  │    ├──▶ security_engineer ──▶ ( ✎ ) ──┤  │
  ▲    ╰──▶ qa_engineer ────────▶ ( ✎ ) ──╯  │
  │                                          ▼
  ╰───────────────── ( ✓?) ◀─────────────────╯
```

Tasks can be kicked off using CLI commands:  

```sh
cd ~/code/my-project
ackn4th start-task --parallel --roles backend_dev,qa_engineer,code_reviewer \
  "Complete every requirement in https://github.com/example/example/issues/1"
```

Or via the dashboard, opened when running the binary directly without 
commands (`./ackn4th` from the install directory).

<img width="1882" height="1118" alt="Screenshot 2026-11-04 at 18 58 16" src="https://github.com/user-attachments/assets/3b4312a3-e366-4e6c-9d2e-a867b653f381" />

Agents run on your existing plans through their CLIs (**Claude Code**,
**Codex**, **Cursor**, **Copilot**) or on API keys (**Claude**, **Gemini**,
**OpenAI**). 

> **A hobby project, for hobbyists.** `ackn4th` is for quickly putting together
> a team of software-development agents that iterates on a prompt until the
> result is ready to demo to you. It is not a product, a hosted service or
> something to run in production (...yet? *wink*), and it comes with no support 
> or warranty. It runs on your own accounts and API keys: what you send, what it 
> costs and following each provider's terms are up to you. [Spending caps](#keeping-costs-in-check)
> help keep that in check.

## What you get: one loop, and a choice of where it runs

`ackn4th` gives you a small team of software-development roles — a product
manager, one or more developers, a reviewer, QA — that works a task in your
checkout until they agree it is done:

1. **You describe the task.** A prompt, a GitHub issue, or a form in the
   dashboard.
2. **The roles take turns** (or all work at once) in your project: they read
   the code, run commands, edit files and explain themselves as they go.
3. **They review each other**, keep shared notes on the project, and hand work
   between roles until nothing is left to fix.
4. **You review the result**: every turn, file change and handoff is on
   record in the dashboard, and the task stops resumably whenever you want.

That loop is the product. What is new is **where it can run**: the same loop,
with the same commands and the same dashboard, on **your machine** — or on an
**AI server you own**, with the controller serving task state over gRPC so your
laptop can follow, start and stop the work from anywhere. Nothing about the loop
changes between the two; only where its state lives.

## Is it for you?

**What it's for**

- Building an MVP from a skeleton, or from nothing but an idea.
- Tackling a well-defined GitHub issue with a team of roles: it reads the
  issue, works on a branch and opens a draft pull request for you to review.
- Bug smashing on a repository you know well enough to judge the fixes.
- Anyone happy to review AI-drafted pull requests before anything merges.

**Who it's for**

- Developers who want a team of agents on a side project.
- People who live in GUIs and are curious about the command line: the
  dashboard does everything with a form and arrow keys, and can read itself
  aloud.
- Product managers who want to see an idea working before asking anyone to
  build it.
- Anyone with an idea and some tokens.

**Who it's not for**

- Professionals automating their whole development workflow.
- People who don't like reviewing AI output, code included. Everything
  `ackn4th` makes is a draft for you to check.
- Those without ideas or tokens.

**What it's not for**

- Deploying to production.
- Commercial applications or services.

## Install

ackn4th is one binary for macOS and Linux, on Intel/AMD (amd64) and ARM
(arm64). Every release is on the [releases page](https://github.com/dethmasque/ackn4th/releases), 
with `checksums.txt` to verify the downloads.
**Windows is not officially supported yet**: its sandbox is still being
verified, and without a working sandbox agents can't run commands (see the
[security model](internal/sandbox/README.md)). 

**macOS and Linux**

```sh
os=$(uname -s | tr A-Z a-z); arch=$(uname -m)
case $arch in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
url=https://github.com/dethmasque/ackn4th/releases/latest/download
curl -fsSLO "$url/ackn4th_${os}_${arch}.tar.gz"
curl -fsSL "$url/checksums.txt" | shasum -a 256 --ignore-missing -c   # or sha256sum --ignore-missing -c
tar -xzf "ackn4th_${os}_${arch}.tar.gz" ackn4th
sudo mv ackn4th /usr/local/bin/      # or any directory on your PATH
ackn4th version
```

The binaries aren't signed or notarized. Downloaded with `curl` as above,
macOS runs them as they are; if you download the archive in a browser
instead, clear the quarantine flag once: `xattr -d com.apple.quarantine ackn4th`.

**Debian, Ubuntu, Fedora, Alpine:** packages put `ackn4th` in `/usr/bin`.

```sh
url=https://github.com/dethmasque/ackn4th/releases/latest/download
arch=$(uname -m); case $arch in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
curl -fsSLO "$url/ackn4th_linux_$arch.deb" && sudo apt install "./ackn4th_linux_$arch.deb"    # Debian, Ubuntu
curl -fsSLO "$url/ackn4th_linux_$arch.rpm" && sudo dnf install "./ackn4th_linux_$arch.rpm"    # Fedora, RHEL
curl -fsSLO "$url/ackn4th_linux_$arch.apk" && sudo apk add --allow-untrusted "./ackn4th_linux_$arch.apk"   # Alpine
```

**With Go** (1.27.3 or later), on any OS:

```sh
go install github.com/dethmasque/ackn4th/cmd/ackn4th@v0.1.0
```

Agents' commands run in the OS's own sandbox
([security model](internal/sandbox/README.md)):

- **macOS:** built in.
- **Linux:** install [bubblewrap](https://github.com/containers/bubblewrap)
  (`apt install bubblewrap`).

Optional: the [GitHub CLI](https://cli.github.com) (`gh`), for
[GitHub issues, repositories and draft pull requests](internal/github/README.md).

On macOS or Linux you can keep ackn4th in a container instead, with Docker
([container/README.md](container/README.md)):

```sh
git clone https://github.com/dethmasque/ackn4th ~/ackn4th
~/ackn4th/container/ackn4th-container --build      # builds the image, once (a few minutes)
alias ackn4th="$HOME/ackn4th/container/ackn4th-container"   # add this to your shell profile
```

## Providers

You only need the ones you use. A provider's CLI on your plan is usually the
better choice: no API key to manage and no API rate limits. When you don't
name a provider and its CLI is signed in, `ackn4th` uses the CLI.

| Provider | Runs on |
| --- | --- |
| **Claude Code** (`claude-code`) | your Claude Pro/Max plan |
| **Codex** (`codex`) | your ChatGPT plan |
| **Copilot** (`copilot`) | your GitHub Copilot plan |
| **Cursor** (`cursor`) | your Cursor plan |
| **Claude** (`claude`) | Anthropic API credits |
| **Gemini** (`gemini`) | Google AI Studio (free tier) |
| **OpenAI** (`openai`) | OpenAI API credits |

How to sign in to each, and how to choose models and efforts per agent:
[internal/provider/README.md](internal/provider/README.md).

## First run

<img width="941" height="1115" alt="Screenshot 2026-11-04 at 18 57 06" src="https://github.com/user-attachments/assets/b1fca6bb-0552-4d7f-81a7-54d3cbc52448" />

Agents work in the current directory. The first time they work in a
directory, `ackn4th` asks you to **trust** it, since agents read it and send
its contents to the model provider. `ackn4th` has no accounts of its own:
agents run on the AI providers you sign in to, and they stay signed in
until you run `ackn4th logout`.

```sh
cd ~/code/my-project
ackn4th
```

`ackn4th` on its own opens the [dashboard](internal/dash/README.md):

1. **Configuration.** It opens first, showing how ackn4th is set up:
   spending caps, round and tool-call limits, each provider's model and
   effort, whether agents' commands may use the network, and the voice.
   `↑↓` choose, `⏎` changes; each change is saved as you make it. **`C`**
   brings it back any time.
2. **A walkthrough** shows a tooltip at a time. `t` (or `ackn4th tour`)
   replays it whenever you like.
3. **Set up providers.** It lists each one as ready, needing sign-in, or not
   installed (with the install command). Pick some with `space`, press `⏎` to
   sign in. `p` brings this back, and `x` there signs out.
4. **Accessibility.** Press **`v`** to have the dashboard read aloud by a
   natural-sounding voice that runs on your computer
   ([details](internal/voice/README.md)). `ackn4th --voice` opens it
   speaking.
5. **Start a task.** Press **`n`**. Write the prompt, name the task, pick
   the workspace (or a GitHub repository and branch), set its rounds and
   spending cap (**CAP $**), then add agents: a role, provider, model and
   effort each. **`f1`** explains every field; **`ctrl+s`** launches it in
   the background.
6. **Roles.** `R` lists the roles agents can play; add your own, edit
   any, or import role files ([roles](roles/README.md)).
7. **Watch.** Each agent works live. `?` lists every key. Quitting the
   dashboard never stops a task.

Tasks run from commands too. A command that's missing an argument, value
or flag asks for it, unless running in non-interactive
(`ACKN4TH_NO_INPUT`) mode.

```sh
ackn4th login                      # pick providers from a numbered list
ackn4th start-task --name "Rate limiting" --roles backend_dev,code_reviewer --max-cost 5 \
  "Add rate limiting to the API, with tests"
ackn4th watch-task "Rate limiting" # follow a task by name (or ID); ctrl+c detaches
ackn4th logout --provider openai   # sign out of one provider (or all, without --provider)
```

Every command, flag and the scripting (CI) mode:
[internal/cli/README.md](internal/cli/README.md).

## Keeping costs in check

Agents on an API key (Claude, Gemini, OpenAI) cost money with every
request; agents on your plan through a CLI (Claude Code, Codex, Cursor,
Copilot) use its limits instead. `ackn4th` estimates what each request 
costs and can stop a task before it spends more than you allow:

- **For one task:** the **CAP $** field in the new-task form, or
  `start-task --max-cost 5`.
- **For every task, a provider, a day or a month:** **`C`** in the
  dashboard (*Spending caps*), or `spend:` in the
  [config](internal/config/README.md).

A task that reaches a cap stops, resumably, and says which cap it hit;
`ackn4th resume-task <task> --max-cost 2` lets it spend $2 more.
`ackn4th spend` shows today's and this month's totals, and the dashboard
shows each task's (`≈$1.23 of $5.00`). How it works:
[spending caps](internal/task/README.md#spending-caps).

## Choosing where task state lives

Everything a task needs to be followed and driven — its record, its event log,
its run markers, the streaming previews — goes through one interface,
[`task.Backend`](internal/task/backend.go). There is **one implementation** of
it — the gRPC client — reached two ways: **local**, where this process serves
its own data dir over a private unix socket, and **controller**, where it dials
a daemon on another machine. So every command takes the same code path for task
state whichever one is live, and the two cannot drift apart. **You choose where
state lives when you install it.** Both run the same agent loop with the same
commands and the same dashboard.

| Where | Chosen by | State lives in | Reach for it when |
| --- | --- | --- | --- |
| **local** — the default | nothing to set | this machine's data dir, served in-process | you work on one machine; no setup, nothing to expose |
| **controller** | `ACKN4TH_BACKEND_URL` | the controller's data dir, on that host | the CLI should follow and control tasks running on another machine |

**Local is the default, and it stays the default.** With no configuration at
all, task state is files under the data dir, exactly as it always has been — the
socket that carries it is a unix socket in a private directory, so nothing is
exposed to the network and nothing local is readable by another user.

**The controller is the same model, in a shared place.** The daemon keeps the
same files a local run uses — `task.json`, the event log, `summary.md`, the live
previews — under its own data dir, and serves them over a small gRPC interface.
What that buys you is the thing files cannot do: **a CLI on another machine
reading and driving the same task**, including the files a run leaves behind.

```sh
# the default: same machine, files under the data dir
ackn4th start-task -d "Add rate limiting"

# or: point at the controller so any machine sees the same tasks
export ACKN4TH_BACKEND_URL=ackn://tasks.internal:7687
ackn4th list-tasks
```

`ackn4th status` shows which backend is live and where its state actually is —
this machine's data dir, or the controller's address.

## Running the daemon on another host

The tasks run on a server with the checkouts and the provider credentials, and
your laptop watches and steers them.

```sh
# on the host with the workspaces and the keys
ACKN4TH_BACKEND_URL=ackn://0.0.0.0:7687 ackn4th controller
```

It carries out `start-task`, `resume-task` and `stop-task` from anywhere, so
nothing signals a process on the wrong machine. Everything that never leaves the
runner's host — agent conversations, snapshots, scratch dirs, run logs, the spend
ledger — stays there. What the interface is, what runs where, streaming, and the
trust boundary (the port is unauthenticated):
[the controller's gRPC interface](docs/grpc-backend.md).

## Documentation

Each topic lives next to the code it describes:

| Topic | Where | Code |
| --- | --- | --- |
| Commands, flags, scripts and CI | [internal/cli/README.md](internal/cli/README.md) | `internal/cli/` |
| Running the daemon on another host | [docs/grpc-backend.md](docs/grpc-backend.md) | `internal/task/` (`backend.go`, `grpc.go`) |
| Providers, sign-in, choosing models | [internal/provider/README.md](internal/provider/README.md) | `internal/provider/`, `internal/auth/` |
| How a task runs: turns, parallel, shared context, onboarding | [internal/task/README.md](internal/task/README.md) | `internal/task/`, `internal/agent/`, `internal/onboard/` |
| The dashboard | [internal/dash/README.md](internal/dash/README.md) | `internal/dash/` |
| The dashboard's voice (accessibility) | [internal/voice/README.md](internal/voice/README.md) | `internal/voice/` |
| GitHub: issues, repositories, branches, draft pull requests | [internal/github/README.md](internal/github/README.md) | `internal/github/` |
| Security model: sign-ins, the workspace jail, OS sandboxes | [internal/sandbox/README.md](internal/sandbox/README.md) | `internal/sandbox/`, `internal/workspace/`, `internal/auth/` |
| Roles: built-in, and adding, editing and importing your own | [roles/README.md](roles/README.md) | `roles/`, `internal/role/` |
| Configuration and where files live | [internal/config/README.md](internal/config/README.md) | `internal/config/` |
| Spending caps and cost estimates | [internal/task/README.md](internal/task/README.md#spending-caps) | `internal/spend/` |
| Running in a container | [container/README.md](container/README.md) | `container/` |
| Developing: build, test, the dev sandbox | [sandbox/README.md](sandbox/README.md) | `justfile`, `sandbox/` |
| Packaging and releasing | [packaging/README.md](packaging/README.md) | `packaging/` |

The rest of the tree:

```
cmd/ackn4th/          main package
internal/agent/       one agent's turn loop and system prompt
internal/auth/        OAuth grants (ant / gcloud), keychain API keys (or a tmpfs dir)
internal/notes/       shared per-project notes
internal/onboard/     project brief, workspace snapshots, change digests with diffs
internal/ratelimit/   shared per-key pacing from rate-limit headers
internal/spend/       cost estimates per model, the spending ledger and its caps
internal/tips/        first-run tips and the walkthrough (seen-state, opt-out)
internal/tools/       agent tools (list/read/search/write/edit/delete/run_command/…)
internal/ui/          ASCII art: banner, boxes, tables, gauges, animations
internal/workspace/   os.Root confinement, write globs, secret filtering
```

## License

[MIT](LICENSE): use, change and share it freely, with no warranty. The
license covers `ackn4th`'s own code only. The models and agent CLIs it drives
come with their providers' terms, which you agree to when you sign in. The
optional voice downloads sherpa-onnx and Kokoro (both Apache-2.0) from their
own releases.
