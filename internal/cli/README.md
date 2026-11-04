# Commands

[← README](../../README.md)

`ackn4th --help` lists these too, and `ackn4th <command> --help` explains
each with examples. Bare `ackn4th` opens the [dashboard](../dash/README.md)
on a terminal (`ackn4th --voice` reads it aloud) and prints the help
elsewhere.

| Command | What it does |
| --- | --- |
| `login [--provider p]… [--reauth] [--gcp-project id]` | Sign in (a CLI's own login, browser OAuth, or an API key from a prompt or stdin), until you sign out |
| `logout [--provider p]…` | Sign out of every provider, or just the ones named. API keys are deleted, OAuth grants revoked, and running tasks stop; agent CLIs keep their own logins |
| `status` | Signed-in providers, keys, workspace roots, task counts |
| `start-task --roles a,b [--name n] [--parallel] [--provider p \| --mix] [--agent role,k=v…] [--repo o/n] [--branch b] [--draft-pr] [prompt]` | Put a team on a task: in turns or in parallel, any mix of providers, models and efforts |
| `run-agent --role r[,r2…] [--name n] [--provider p] [--model m] [--effort e] [--profile n] [prompt]` | Run one agent, in one or more roles, iterating until done |
| `dash [task] [--voice]` (also `top`) | The dashboard: every task, where each agent is, logs, files, handoffs; start tasks from a form. `--voice` reads it aloud this time |
| `walkthrough [--voice]` (also `tour`) | Open the dashboard with its walkthrough from the start, even if tips are off |
| `watch-task <task>` | Follow a running task live (ctrl+c detaches) |
| `stop-task <task>` | Gracefully stop a running task (foreground or background) |
| `resume-task <task> [--rounds n] [--max-cost dollars] [--note "…"] [--provider p] [-d]` | Continue a stopped/failed task (`--provider` moves its agents, e.g. to `claude-code`; `--max-cost` lets it spend that much more), or attach to a running one |
| `follow-up <task> ["prompt"] [--rounds n] [--max-cost dollars] [-d]` | Give a task that isn't running (completed too) more work: the same agents continue with their conversations and the workspace, every agent gets the prompt, and all must declare done again |
| `list-tasks [--all]` | Table of tasks, by name: the 20 latest, or all, with tokens and estimated cost |
| `rename-task <task> <name>` | Give a task a new name (works while it runs) |
| `show-task <task>` | Replay a task's transcript and its summary |
| `delete-task <task> --yes` | Delete a task's records (never its workspace) |
| `open-pr <task>` | Propose a finished task's changes as a draft GitHub pull request ([GitHub](../github/README.md)) |
| `notes [dir\|task] [--edit] [--clear]` | Show, edit or clear a project's shared notes |
| `brief [dir\|task]` | Preview the onboarding brief agents get |
| `trust [dir]` / `trust --list` / `untrust [dir]` | Manage the directories agents may work in |
| `roles [name]` | List [roles](../../roles/README.md) or show one |
| `roles add <name>` / `roles edit <name>` / `roles import <file\|url>…` / `roles remove <name>` | Write your own roles, edit any (an edited built-in becomes yours), import role files, or remove yours ([roles](../../roles/README.md#your-own-roles)) |
| `config` / `config init` | Show the effective [config](../config/README.md) / write defaults |
| `spend` | Estimated spending today and this month, in all and by provider, beside the [caps](../task/README.md#spending-caps) |
| `voice on\|off\|install\|uninstall\|test\|status\|list` | The dashboard's [voice](../voice/README.md) |
| `disable-tips` / `enable-tips` | Turn first-run tips off, or back on from the start |
| `version` | Print the version |
| `completion bash\|zsh\|fish\|powershell` | Print a shell completion script (`ackn4th completion <shell> --help` says how to load it) |

Shared flags for `start-task` and `run-agent`:

| Flag | Meaning |
| --- | --- |
| `--name n` | The task's name, shown everywhere and usable instead of its ID. Without it, a terminal asks (`⏎` takes a name drawn from the prompt); scripts get that name |
| `-p, --prompt` / `--prompt-file f` | The task (`-` reads stdin); positional args work too |
| `-w, --workspace dir` | Where agents work (default: the current directory) |
| `--scratch` | Work in a new empty directory under `default_workspace_root` |
| `--repo owner/name` | Work in a GitHub repository: cloned with `gh` into `default_workspace_root`, or reused |
| `--branch b` | Work on branch `b`: checked out, fetched from GitHub, or created |
| `--draft-pr` | When the task completes, open a draft pull request with the agents' changes |
| `-r, --rounds n` / `--forever` | Round limit; `0` or `--forever` = until consensus |
| `--max-cost dollars` | Cap the task's spending (default: `spend.per_task`; see [spending caps](../task/README.md#spending-caps)) |
| `-d, --detach` | Run in the background |

Providers: `claude`, `gemini`, `openai`, `claude-code`, `codex`, `cursor`,
`copilot`. Roles without `--use-claude`/`--use-gemini` use `--provider`,
else `default_provider` (claude, through Claude Code when it's signed in);
`--mix` alternates claude, gemini, claude, …

Every task has a **name** you give it and a short random **ID**
(`k3f9qz`). Lists, the dashboard and messages show the name. Commands that
take a task accept either: the name (any case; quote it if it has spaces),
the ID, or the start of an ID that matches only one task
(`ackn4th stop-task "Rate limiting"`, `ackn4th stop-task k3f`). Names
needn't be unique, but a name shared by several tasks has to be given as an
ID instead.

## Missing input is asked for

On a terminal, a command missing something it needs shows the error, then
asks: `login` asks which provider (by number, e.g. `1,3`, or by name),
`start-task` which roles and what to do, `stop-task`, `resume-task`,
`watch-task`, `show-task`, `delete-task` and `open-pr` which task. Answer
with a number or a name; an empty answer gives up.

Tips appear as a `✦ tip` line after commands, once each. Turn them off with
`ackn4th disable-tips`, `ACKN4TH_NO_TIPS=1` or `tips: false` in the config;
`ackn4th enable-tips` replays them.

## Scripts and CI

With `--no-input`, `ACKN4TH_NO_INPUT=1`, a `CI` variable, or stdin that
isn't a terminal, ackn4th never prompts. Anything missing fails at once with
an error saying what to pass. Animations and tips only appear on a terminal.
So a script does each step explicitly:

```sh
export ACKN4TH_NO_INPUT=1
cd ~/code/my-project

# 1. Sign in to providers (once: they stay signed in until logout).
#    API keys come from stdin, never from a flag or an environment variable:
#    A piped key replaces the stored one; a key that fails the check is
#    never kept (the previous one stays).
op read op://dev/openai/key | ackn4th login --provider openai
#    An agent CLI already signed in (on this machine, or in the container)
#    is just checked and put to use:
ackn4th login --provider codex

# 2. Trust the workspace up front; nothing can ask later.
ackn4th trust .

# 3. Run in the foreground: exit code 1 if the task fails or can't start.
ackn4th start-task --provider codex --roles backend_dev,qa_engineer \
  --rounds 5 --prompt-file task.md

# …or in the background, named so the next commands can find it.
ackn4th start-task -d --name cli-docs --roles tech_writer "Document the CLI"
ackn4th watch-task cli-docs         # follow it; also: show-task, stop-task, resume-task
ackn4th list-tasks
```

`ackn4th login` with no `--provider`, non-interactively, signs in to every
provider it can without a key it doesn't already have. Browser sign-ins
(Claude, and a first CLI login) need a person once. After that, a CLI stays
signed in, and `ackn4th login` only renews ackn4th's 24-hour limit on using it. Each task's report
is written to `<data>/tasks/<id>/summary.md` (`show-task <id>` prints it).

## Code

Each command is a function returning a `*cobra.Command`, registered in
`root.go` in the order help lists them. `render.go` draws the live event
stream that `start-task`, `watch-task` and `show-task` print.
