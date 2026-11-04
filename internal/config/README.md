# Configuration

[← README](../../README.md)

`<config>/config.yaml` (run `ackn4th config init` to create it, `ackn4th
config` to see the effective settings). The everyday ones can also be
changed in the dashboard: **`C`** opens its
[configuration](../dash/README.md#configuration), which saves each change
to this file.

```yaml
workspace_roots: [~/Git/ackn4th]          # trusted directories (ackn4th trust adds here)
default_workspace_root: ~/ackn4th-workspaces   # --scratch and --repo work here
default_provider: claude                  # one provider per team shares cached context
providers:
  claude: {model: claude-opus-5-5, max_tokens: 64000, effort: high, compact_at: 200000}
  gemini: {model: gemini-3.1-pro-preview, max_tokens: 65536, compact_at: 200000}   # auth: key (AI Studio)
  claude-code: {model: claude-sonnet-5-5, effort: medium}   # your Claude plan via local Claude Code
  openai: {max_tokens: 32000, effort: medium}  # model: login picks the newest your key can use
  # another API in OpenAI's format:  openai: {base_url: https://api.deepseek.com, model: deepseek-v4-pro}
  codex: {model: default}                 # "default" lets the CLI choose; also cursor, copilot
  # Vertex AI instead:  gemini: {auth: vertex, project: my-gcp-project, location: global}
agents:                                   # named per-agent presets (see internal/provider/README.md)
  fast-writer: {provider: claude, model: claude-sonnet-5-5, effort: medium}
commands:                                 # run_command: sandboxed builds and tests
  enabled: true
  network: false                          # true lets package managers fetch dependencies
  timeout: 10m
limits:
  max_rounds: 10
  max_steps_per_turn: 40
  max_file_bytes: 1048576
effects: true
tips: true                                # first-run hints; ackn4th disable-tips sets false
prefer_cli: true                          # default claude → claude-code, openai → codex when signed in
voice:                                    # the dashboard reads itself aloud (ackn4th voice on)
  enabled: false
  name: af_heart                          # ackn4th voice list
  speed: 1                                # 0.5 to 2
spend:                                    # spending caps in dollars, unset by default (no limit)
  per_task: 5                             # each task, over its whole life (start-task --max-cost overrides)
  daily: 20                               # everything ackn4th spends, on every provider
  monthly: 200
  providers:
    gemini: {daily: 5, monthly: 50}
  prices:                                 # dollars per million tokens, for models ackn4th doesn't know
    gpt-6-astra: {input: 2, cached: 0.2, output: 12}
```

See [spending caps](../task/README.md#spending-caps).

`disable-tips`, `voice on`, the OpenAI model choice at login and the
dashboard's configuration change just their key, keeping the rest of the
file and its comments.

## Where files live

ackn4th keeps its files where each OS expects an app's files:

| | `<config>`: settings, roles, sign-ins | `<data>`: tasks, notes |
|---|---|---|
| Linux | `$XDG_CONFIG_HOME/ackn4th` (`~/.config/ackn4th`) | `$XDG_DATA_HOME/ackn4th` (`~/.local/share/ackn4th`) |
| macOS | `~/Library/Application Support/ackn4th` | the same folder |
| Windows | `%APPDATA%\ackn4th` | `%LOCALAPPDATA%\ackn4th` |

- **API keys** go in the OS keychain, not in these folders.
- **The voice**, a large download that holds nothing of yours, goes in the
  OS's cache folder ([voice](../voice/README.md#install)).
- **Temporary files** go in the OS's temp directory (`$TMPDIR`, else `/tmp`;
  `%TEMP%` on Windows), except the agents' own scratch space, which stays
  with its task.
- **Claude Code** files each agent's session with its workspace's project,
  as it does for your own sessions, so `claude --resume` in the workspace
  lists them.
- **`ACKN4TH_HOME`** moves everything but the voice into one folder
  (`config/` and `data/`), as the [dev sandbox](../../sandbox/README.md)
  does.

Task state lives in `<data>/tasks/<id>/` ([what's in it](../task/README.md#code)).
Project notes are in `<data>/projects/<hash>/notes.md`.

## TODO

- **Paths relative to `config.yaml`.** `workspace_roots` (and
  `default_workspace_root`) take absolute paths or `~/…`; a relative path is
  resolved against the directory ackn4th runs in (`ExpandPath`), and `trust`
  always writes absolute paths. Resolve relative paths against the folder
  holding `config.yaml` instead, and have `trust` write them relative when
  the directory sits under that folder, so a config can be copied along
  with its projects (e.g. a run template under `data/`) and still point at
  its own copies.
