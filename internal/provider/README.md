# Providers

[← README](../../README.md)

A provider runs an agent's model. You only need the ones you use. A
provider's CLI on your plan is usually the better choice: no API key to
manage and no API rate limits. **When you don't name a provider and its CLI
is signed in, ackn4th uses the CLI**: `claude` agents run through Claude
Code, `openai` agents through Codex (`prefer_cli: false` in the config turns
this off).

| Provider | Runs on | Sign in with | In the container |
| --- | --- | --- | --- |
| **Claude Code** (`claude-code`) | your Claude Pro/Max plan | `ackn4th login --provider claude-code` (runs `claude auth login`) | prints a URL; paste the code back |
| **Codex** (`codex`) | your ChatGPT plan | `ackn4th login --provider codex` (runs `codex login`) | uses a device code: open the URL, enter the code |
| **Copilot** (`copilot`) | your GitHub Copilot plan | `ackn4th login --provider copilot` (runs `copilot login`) | uses a device code |
| **Cursor** (`cursor`) | your Cursor plan | `ackn4th login --provider cursor` (runs `cursor-agent login`) | install the CLI first ([container](../../container/README.md)) |
| **Claude** (`claude`) | Anthropic API credits ([Console](https://console.anthropic.com) account) | `ackn4th login --provider claude`: browser sign-in through `ant` | open the printed URL in your browser |
| **Gemini** (`gemini`) | Google AI Studio (free tier) | paste a key from [AI Studio](https://aistudio.google.com/apikey) | same |
| **OpenAI** (`openai`) | OpenAI API credits | paste a key from [platform.openai.com](https://platform.openai.com/api-keys) | same |

A claude.ai, ChatGPT or Gemini app subscription doesn't cover API use: use
the CLI providers for those plans. The agent CLIs run inside ackn4th's
sandbox ([security model](../sandbox/README.md)) and are experimental: they
depend on each CLI's JSON output. Gemini on Vertex AI (`--gcp-project`, via
`gcloud`) is also supported, outside the container.

The dashboard's provider screen (`p`) shows the same: each provider as
ready, needing sign-in, or not installed, with the install command.

## Signing out

```sh
ackn4th logout                                      # every provider
ackn4th logout --provider openai --provider gemini  # just these; the rest stay signed in
```

Signing out of a provider removes what ackn4th holds for it: API keys are
deleted from the OS keychain, Gemini's Vertex AI refresh token is revoked,
Claude's tokens are deleted, and running tasks that use it stop at their
next request. Otherwise a provider stays signed in. ackn4th holds nothing
for the agent CLIs (Claude Code, Codex, Cursor, Copilot): they keep their
own logins, which you sign out of in the CLI itself. In the dashboard, `p`
then `x` (twice, to confirm) signs out of the providers you pick.

## Choosing models

Each agent has its own provider, model, effort, output limit and compaction
threshold. Anything you don't set comes from `providers:` in the
[config](../config/README.md).

```sh
# per agent, on the command line
ackn4th start-task \
  --agent backend_dev,provider=claude,model=claude-opus-5-5,effort=xhigh \
  --agent code_reviewer,provider=gemini,effort=low,compact_at=150k \
  "Add a /healthz endpoint and review it"

# with --roles, --agent configures just the roles it names
ackn4th start-task --roles backend_dev,qa_engineer --agent qa_engineer,model=claude-sonnet-5-5 "…"

# solo agents take flags
ackn4th run-agent --role tech_writer --model claude-sonnet-5-5 --effort medium "Document the CLI"
```

`--agent` settings are `provider`, `model`, `effort` (`low` … `max`; Gemini
maps it to a thinking level), `max_tokens`, `compact_at` (≥ 50k; `200k` and
`1m` work), and `profile`. Profiles are named presets in the config:

```yaml
agents:
  fast-writer: {provider: claude, model: claude-sonnet-5-5, effort: medium}
  deep-review: {provider: gemini, model: gemini-3.1-pro-preview, effort: high, compact_at: 400000}
```

```sh
ackn4th start-task --agent backend_dev --agent tech_writer,profile=fast-writer "…"
ackn4th run-agent --role code_reviewer --profile deep-review "Review internal/auth"
```

Models reason before they act, as much as `effort` allows: they decide how
much each step needs. Claude (API and Claude Code), Gemini, Codex, Cursor
and Copilot send a summary of that reasoning, which the dashboard shows
live and keeps in each task's log. The OpenAI provider doesn't show it.

OpenAI has no default model: `ackn4th login --provider openai` (or setup in
the dashboard) tries the configured one and, if your key can't use it, picks
the newest chat model that answers a test request and saves it to the
config. If none does, the error gives OpenAI's reason for each model tried.

The OpenAI provider uses the Responses API with storage off, so OpenAI
keeps no conversation. The model's reasoning comes back encrypted, is saved
with the task, and goes back with the next request, so it carries across
tool calls and resumes.

Before a task starts, ackn4th checks each distinct model with a real API
call, so a typo fails right away instead of halfway through. A task keeps
the settings it started with when resumed: models aren't switched
mid-conversation, because a conversation's thinking blocks belong to the
model that wrote them. `resume-task --provider claude-code` (or **Run on**
in the dashboard's resume dialog) moves a stopped task's agents to another
provider, e.g. when API credits run out.

### OpenAI-compatible APIs

`providers.openai.base_url` points the `openai` provider at another
OpenAI-compatible API, such as DeepSeek. The API must support OpenAI's
[Responses API](https://platform.openai.com/docs/api-reference/responses)
(`POST /responses`): one that only speaks Chat Completions
(`/chat/completions`) won't work. Set the model as well: login only picks
one on OpenAI itself.

```yaml
providers:
  openai: {base_url: https://api.deepseek.com, model: deepseek-v4-pro, effort: high}
spend:
  prices:
    deepseek-v4-pro: {input: 1.32, cached: 0.044, output: 3.96}
    deepseek-flash: {input: 0.30, cached: 0.006, output: 1.20}
```

```sh
printf '%s\n' "$DEEPSEEK_API_KEY" | ackn4th login --provider openai
```

The base URL is read only from `config.yaml`, never from the environment.
It must be https (http only to `localhost`). The key is stored for that
host alone: after a change to `base_url`, sign in again, and OpenAI's key
never goes anywhere else. APIs that return reasoning as text (DeepSeek)
show it live, and it goes back with each later request, as they require
once tools are in play.

## Code

```
provider.go    the provider-neutral interface every adapter implements
claude/        Anthropic API (Messages, prompt caching, server-side compaction)
gemini/        Gemini API (AI Studio key or Vertex AI)
openai/        OpenAI Responses API (stateless; model probing: PickModel)
claudecode/    local Claude Code as the model loop, with its own tools off
cliagent/      Codex, Cursor and Copilot, run inside the OS sandbox
clis/          the agent CLIs' install checks and sign-in commands
mcpbridge/     serves ackn4th's tools to the CLIs over localhost MCP
mock/          offline scripted agents (dev sandbox only)
```

How each provider is confined is in the
[security model](../sandbox/README.md#agent-clis).
