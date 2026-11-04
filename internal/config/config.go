// Package config resolves ackn4th's on-disk locations and loads user settings.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dethmasque/ackn4th/internal/spend"
)

const appName = "ackn4th"

// Provider holds per-provider model settings.
type Provider struct {
	Model     string `yaml:"model"`
	MaxTokens int64  `yaml:"max_tokens"`
	// Effort trades depth of reasoning for speed and cost: low, medium,
	// high, xhigh or max. Claude uses it as output effort; Gemini maps it to
	// a thinking level (xhigh and max become high).
	Effort string `yaml:"effort,omitempty"`
	// CompactAt is the context size, in input tokens, at which an agent's
	// conversation is summarized so it can keep working. Claude compacts
	// server-side (minimum 50000); Gemini compacts between turns.
	CompactAt int64 `yaml:"compact_at,omitempty"`
	// Auth is how Gemini signs in: "key" (an AI Studio API key in the OS
	// keychain; no Google Cloud needed) or "vertex" (short-lived OAuth via
	// gcloud on Vertex AI). Empty means vertex when Project is set, else
	// key. Ignored by Claude.
	Auth string `yaml:"auth,omitempty"`
	// Project and Location select the GCP project and Vertex AI location
	// for Gemini with auth: vertex.
	Project  string `yaml:"project,omitempty"`
	Location string `yaml:"location,omitempty"`
	// BaseURL points openai at another API in OpenAI's Responses format
	// (DeepSeek, a local server, ...). It is read only from this file,
	// never the environment, and the API key is kept per host, so a
	// changed base_url needs its own login. Empty is api.openai.com.
	BaseURL string `yaml:"base_url,omitempty"`
}

// CheckBaseURL validates a base_url and returns it with a trailing slash.
// It must be https, or http to this machine for a local server.
func CheckBaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("base_url %q is not a plain http(s) URL", raw)
	}
	local := slices.Contains([]string{"localhost", "127.0.0.1", "::1"}, u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && local) {
		return "", fmt.Errorf("base_url %q must use https (http only to localhost)", raw)
	}
	return strings.TrimRight(u.String(), "/") + "/", nil
}

// ErrNoModel is returned when an agent's provider has no model configured
// (openai, until one is picked for the key).
var ErrNoModel = errors.New("no model set")

// Gemini auth methods.
const (
	GeminiKey    = "key"
	GeminiVertex = "vertex"
)

// GeminiAuth returns the provider's Gemini auth method.
func (p Provider) GeminiAuth() string {
	switch {
	case p.Auth != "":
		return p.Auth
	case p.Project != "":
		return GeminiVertex
	}
	return GeminiKey
}

// AgentProfile is a named agent configuration, selected with
// --agent role,profile=<name> or --profile. Empty fields fall back to the
// provider's settings.
type AgentProfile struct {
	Provider  string `yaml:"provider"`
	Model     string `yaml:"model,omitempty"`
	Effort    string `yaml:"effort,omitempty"`
	MaxTokens int64  `yaml:"max_tokens,omitempty"`
	CompactAt int64  `yaml:"compact_at,omitempty"`
}

// Limits bound how long agents may run.
type Limits struct {
	// MaxRounds is the default number of rounds per task. A round is one turn
	// by every agent. Zero means run until the agents agree they are done.
	MaxRounds int `yaml:"max_rounds"`
	// MaxStepsPerTurn caps tool-call iterations inside a single agent turn.
	MaxStepsPerTurn int `yaml:"max_steps_per_turn"`
	// MaxFileBytes caps the size of any file an agent reads or writes.
	MaxFileBytes int64 `yaml:"max_file_bytes"`
}

// Commands configures run_command: agents' shell commands, confined by
// the OS sandbox (Seatbelt on macOS, bubblewrap on Linux) to the
// workspace and a private scratch directory.
type Commands struct {
	Enabled bool `yaml:"enabled"`
	// Network lets commands reach the internet, e.g. for package
	// managers. Off by default; localhost always works.
	Network bool `yaml:"network"`
	// Timeout is the longest one command may run.
	Timeout time.Duration `yaml:"timeout"`
}

// Config is the user configuration, read from config.yaml.
type Config struct {
	// WorkspaceRoots lists the directories agents may work in. Every task
	// workspace must resolve to a path inside one of these.
	WorkspaceRoots []string `yaml:"workspace_roots"`
	// DefaultWorkspaceRoot is where new task workspaces are created when no
	// --workspace is given. It is always implicitly allowed.
	DefaultWorkspaceRoot string `yaml:"default_workspace_root"`
	// DefaultProvider powers every role not assigned one. A team on one
	// provider and model shares its cached context; see start-task --mix.
	DefaultProvider string              `yaml:"default_provider"`
	Providers       map[string]Provider `yaml:"providers"`
	// Agents are named agent profiles (provider, model, effort, ...).
	Agents map[string]AgentProfile `yaml:"agents,omitempty"`
	Limits Limits                  `yaml:"limits"`
	// Commands lets agents build, test and run code in a sandbox.
	Commands Commands `yaml:"commands"`
	// Effects toggles animated ASCII effects on interactive terminals.
	Effects bool `yaml:"effects"`
	// PreferCLI runs a provider through its signed-in local CLI, when it
	// has one, instead of its API: claude becomes claude-code when Claude
	// Code is installed and signed in. It applies only where no provider
	// was named (default_provider, --mix, the dashboard's default); an
	// explicit --provider claude still uses the API.
	PreferCLI bool `yaml:"prefer_cli"`
	// Tips shows first-run hints in command output and tooltips in the
	// dashboard. ACKN4TH_NO_TIPS=1 or `ackn4th disable-tips` turn them off.
	Tips bool `yaml:"tips"`
	// Voice reads the dashboard aloud: where you are, what is selected
	// and what changed. It runs on this computer (no account, no network
	// once installed); `ackn4th voice install` downloads it.
	Voice Voice `yaml:"voice"`
	// Spend caps what agents spend on billed providers (claude, gemini,
	// openai), estimated from their token counts: per task, per provider
	// and for all of ackn4th, a day and a month. Unset caps are no limit.
	Spend spend.Limits `yaml:"spend,omitempty"`
}

// Voice configures the dashboard's spoken feedback.
type Voice struct {
	Enabled bool `yaml:"enabled"`
	// Name is the speaker, e.g. af_heart (`ackn4th voice list`).
	Name string `yaml:"name,omitempty"`
	// Speed is the speaking rate: 1 is normal, 0.5 to 2.
	Speed float32 `yaml:"speed,omitempty"`
}

// Default returns the built-in configuration. In a sandbox, scratch
// workspaces are created inside it.
func Default() Config {
	root := "~/ackn4th-workspaces"
	if h, ok := Sandbox(); ok {
		root = filepath.Join(h, "workspaces")
	}
	providers := map[string]Provider{
		"claude": {Model: "claude-opus-5-5", MaxTokens: 64000, Effort: "high", CompactAt: 200000},
		"gemini": {Model: "gemini-3.1-pro-preview", MaxTokens: 65536, CompactAt: 200000, Location: "global"},
		// Claude Code manages output length and compaction itself. Sonnet at
		// medium effort stretches a subscription's usage limits, so agents
		// can collaborate for longer.
		"claude-code": {Model: "claude-sonnet-5-5", MaxTokens: 64000, Effort: "medium", CompactAt: 200000},
		// No OpenAI model until login picks one the key can use.
		"openai": {MaxTokens: 32000, Effort: "medium", CompactAt: 200000},
		// Agent CLIs on the user's own plans. "default" (Codex) and "auto"
		// let the CLI pick the model; they manage output and compaction.
		"codex":   {Model: "default", MaxTokens: 64000, CompactAt: 200000},
		"cursor":  {Model: "auto", MaxTokens: 64000, CompactAt: 200000},
		"copilot": {Model: "auto", MaxTokens: 64000, CompactAt: 200000},
	}
	if _, ok := Sandbox(); ok {
		// The offline mock provider exists only in a sandbox.
		providers["mock"] = Provider{Model: "mock-1", MaxTokens: 4096, CompactAt: MinCompactAt}
	}
	return Config{
		WorkspaceRoots:       []string{},
		DefaultWorkspaceRoot: root,
		Providers:            providers,
		Limits: Limits{
			MaxRounds:       10,
			MaxStepsPerTurn: 40,
			MaxFileBytes:    1 << 20,
		},
		Commands:        Commands{Enabled: true, Timeout: 10 * time.Minute},
		Effects:         true,
		Tips:            true,
		PreferCLI:       true,
		DefaultProvider: "claude",
	}
}

// HomeEnv names the variable that switches ackn4th into sandbox mode:
// config, OAuth grants, task data and default workspaces all live under
// that one directory, isolated from a normal install. The repo's justfile
// uses it so a clone can be built and tried out in place.
const HomeEnv = "ACKN4TH_HOME"

// Sandbox returns the sandbox directory when ACKN4TH_HOME is set.
func Sandbox() (string, bool) {
	h := os.Getenv(HomeEnv)
	if h == "" {
		return "", false
	}
	abs, err := filepath.Abs(h)
	if err != nil {
		return h, true
	}
	return abs, true
}

// Dir returns the configuration directory (config.yaml, roles, sign-ins):
// $ACKN4TH_HOME/config in a sandbox, else the OS's own place for it:
//
//	Linux    $XDG_CONFIG_HOME/ackn4th, or ~/.config/ackn4th
//	macOS    ~/Library/Application Support/ackn4th
//	Windows  %APPDATA%\ackn4th
func Dir() (string, error) {
	if h, ok := Sandbox(); ok {
		return filepath.Join(h, "config"), nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, appName), nil
}

// VoiceDir is where the voice's engine and model are downloaded: the OS's
// cache directory, even in a sandbox, since they are large, re-fetchable
// and hold nothing of the user's.
func VoiceDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, appName, "voice"), nil
}

// DataDir returns the state directory (tasks, project notes):
// $ACKN4TH_HOME/data in a sandbox, else the OS's own place for it:
//
//	Linux    $XDG_DATA_HOME/ackn4th, or ~/.local/share/ackn4th
//	macOS    ~/Library/Application Support/ackn4th, beside the config
//	Windows  %LOCALAPPDATA%\ackn4th
func DataDir() (string, error) {
	if h, ok := Sandbox(); ok {
		return filepath.Join(h, "data"), nil
	}
	switch runtime.GOOS {
	case "darwin":
		dir, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, appName), nil
	case "windows":
		dir := os.Getenv("LOCALAPPDATA")
		if dir == "" {
			return "", errors.New("%LOCALAPPDATA% is not defined")
		}
		return filepath.Join(dir, appName), nil
	}
	if x := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(x) {
		return filepath.Join(x, appName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", appName), nil
}

// Path returns the location of config.yaml.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// Load reads config.yaml, layering it over the defaults. A missing file is not an error.
func Load() (Config, error) {
	cfg := Default()
	path, err := Path()
	if err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	// Decoding onto the defaults keeps every key the file leaves out.
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Default(), fmt.Errorf("parse %s: %w", path, err)
	}
	// A provider entry in the file replaces the default entry wholesale, so
	// backfill the fields it omitted.
	for name, def := range Default().Providers {
		p := cfg.Providers[name]
		if p.Model == "" {
			p.Model = def.Model
		}
		if p.MaxTokens == 0 {
			p.MaxTokens = def.MaxTokens
		}
		if p.Effort == "" {
			p.Effort = def.Effort
		}
		if p.Location == "" {
			p.Location = def.Location
		}
		if p.CompactAt == 0 {
			p.CompactAt = def.CompactAt
		}
		cfg.Providers[name] = p
	}
	for name, p := range cfg.Providers {
		if p.BaseURL == "" {
			continue
		}
		if name != "openai" {
			return Default(), fmt.Errorf("%s: providers.%s.base_url: only openai takes a base_url", path, name)
		}
		if p.BaseURL, err = CheckBaseURL(p.BaseURL); err != nil {
			return Default(), fmt.Errorf("%s: providers.%s.%w", path, name, err)
		}
		cfg.Providers[name] = p
	}
	if err := cfg.Spend.Validate(); err != nil {
		return Default(), fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Save writes cfg to config.yaml with owner-only permissions.
func Save(cfg Config) (string, error) {
	path, err := Path()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return "", err
	}
	return path, writeFile(path, data)
}

// writeFile replaces path's contents all at once, so a crash mid-write
// never leaves a truncated config. A symlinked config.yaml (from a
// dotfiles repo, say) is written through, not replaced.
func writeFile(path string, data []byte) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// SetKey sets one key in config.yaml to value, creating the file if needed
// and leaving every other line, comment included, as it was. A dotted key
// (providers.openai.model) reaches into nested mappings, adding any that
// are missing.
func SetKey(key string, value any) (string, error) {
	path, err := Path()
	if err != nil {
		return "", err
	}
	var doc yaml.Node
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return "", err
	default:
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return "", fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	var val yaml.Node
	if err := val.Encode(value); err != nil {
		return "", err
	}
	node, parts := doc.Content[0], strings.Split(key, ".")
	for i, part := range parts {
		if node.Kind != yaml.MappingNode {
			where := "the top level"
			if i > 0 {
				where = strings.Join(parts[:i], ".")
			}
			return "", fmt.Errorf("%s: %s is not a mapping", path, where)
		}
		next := &val
		if i < len(parts)-1 {
			next = &yaml.Node{Kind: yaml.MappingNode}
		}
		found := false
		for j := 0; j+1 < len(node.Content); j += 2 {
			if node.Content[j].Value == part {
				if i == len(parts)-1 {
					node.Content[j+1] = next
				}
				next, found = node.Content[j+1], true
				break
			}
		}
		if !found {
			node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: part}, next)
		}
		node = next
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return path, writeFile(path, out)
}

// AllowedRoots returns the absolute workspace roots, default root included.
func (c Config) AllowedRoots() ([]string, error) {
	roots := append([]string{c.DefaultWorkspaceRoot}, c.WorkspaceRoots...)
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		abs, err := ExpandPath(r)
		if err != nil {
			return nil, err
		}
		out = append(out, abs)
	}
	return out, nil
}

// ExpandPath expands a leading ~ and makes the path absolute.
func ExpandPath(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return filepath.Abs(p)
}

// Efforts lists the accepted effort levels, lowest first.
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// MinCompactAt is the smallest compaction threshold the Claude API accepts;
// ackn4th applies it to every provider.
const MinCompactAt = 50000

// ResolveAgent builds a complete agent configuration: the named profile
// (if any) over its provider's settings, overridden by the
// non-empty fields of override. The result has every field set.
func (c Config) ResolveAgent(profile string, override AgentProfile) (AgentProfile, error) {
	var out AgentProfile
	if profile != "" {
		p, ok := c.Agents[profile]
		if !ok {
			return out, fmt.Errorf("unknown agent profile %q (define it under agents: in config.yaml)", profile)
		}
		out = p
	}
	if override.Provider != "" {
		if profile != "" && out.Provider != "" && out.Provider != override.Provider {
			return out, fmt.Errorf("profile %q is for %s, not %s", profile, out.Provider, override.Provider)
		}
		out.Provider = override.Provider
	}
	if out.Provider == "" {
		return out, errors.New("agent has no provider")
	}
	base, ok := c.Providers[out.Provider]
	if !ok {
		return out, fmt.Errorf("unknown provider %q", out.Provider)
	}
	pick := func(vals ...string) string {
		for _, v := range vals {
			if v != "" {
				return v
			}
		}
		return ""
	}
	pickN := func(vals ...int64) int64 {
		for _, v := range vals {
			if v != 0 {
				return v
			}
		}
		return 0
	}
	out.Model = pick(override.Model, out.Model, base.Model)
	out.Effort = strings.ToLower(pick(override.Effort, out.Effort, base.Effort))
	out.MaxTokens = pickN(override.MaxTokens, out.MaxTokens, base.MaxTokens)
	out.CompactAt = pickN(override.CompactAt, out.CompactAt, base.CompactAt)

	if out.Model == "" {
		return out, fmt.Errorf("%w for %s", ErrNoModel, out.Provider)
	}
	if out.Effort != "" && !slices.Contains(Efforts, out.Effort) {
		return out, fmt.Errorf("effort %q is not one of %s", out.Effort, strings.Join(Efforts, ", "))
	}
	if out.MaxTokens <= 0 {
		return out, fmt.Errorf("max_tokens must be positive, got %d", out.MaxTokens)
	}
	if out.CompactAt < MinCompactAt {
		return out, fmt.Errorf("compact_at must be at least %d tokens, got %d", MinCompactAt, out.CompactAt)
	}
	return out, nil
}
