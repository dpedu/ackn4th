// Package cli implements the ackn4th command-line interface.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dethmasque/ackn4th/internal/auth"
	"github.com/dethmasque/ackn4th/internal/config"
	"github.com/dethmasque/ackn4th/internal/provider"
	"github.com/dethmasque/ackn4th/internal/provider/claude"
	"github.com/dethmasque/ackn4th/internal/provider/claudecode"
	"github.com/dethmasque/ackn4th/internal/provider/cliagent"
	"github.com/dethmasque/ackn4th/internal/provider/clis"
	"github.com/dethmasque/ackn4th/internal/provider/gemini"
	"github.com/dethmasque/ackn4th/internal/provider/mock"
	"github.com/dethmasque/ackn4th/internal/provider/openai"
	"github.com/dethmasque/ackn4th/internal/role"
	"github.com/dethmasque/ackn4th/internal/sandbox"
	"github.com/dethmasque/ackn4th/internal/spend"
	"github.com/dethmasque/ackn4th/internal/task"
	"github.com/dethmasque/ackn4th/internal/tips"
	"github.com/dethmasque/ackn4th/internal/tools"
	"github.com/dethmasque/ackn4th/internal/ui"
)

// Version is set at build time with -ldflags "-X github.com/dethmasque/ackn4th/internal/cli.Version=v1.2.3".
// A `go install …@v1.2.3` build has no ldflags; it reports the module version.
var Version = "dev"

func init() {
	if bi, ok := debug.ReadBuildInfo(); ok && Version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		Version = bi.Main.Version
	}
}

// app carries what every command needs.
type app struct {
	cfg   config.Config
	term  *ui.Term
	store *task.Store
	noFX  bool
	speak bool // --voice on bare ackn4th
	// noInput disables prompts for missing input (--no-input).
	noInput bool
	in      *bufio.Reader
	// bannerShown is set once the animated banner has been printed, so help
	// does not print a second one.
	bannerShown bool
	// tips records which first-run hints the user has seen.
	tips *tips.Book
	// ledger records spending for every runner, in the data dir.
	ledger *spend.Ledger
}

func (a *app) load() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	a.cfg = cfg
	a.term = ui.NewTerm(os.Stdout, cfg.Effects && !a.noFX)
	data, err := config.DataDir()
	if err != nil {
		return err
	}
	a.store = task.NewStore(data)
	a.ledger = &spend.Ledger{Dir: filepath.Join(data, "spend")}
	a.tips = tips.Load(data, cfg.Tips)
	return nil
}

func (a *app) roles() (*role.Registry, error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	return role.Load(filepath.Join(dir, "roles"))
}

// newProvider builds a provider with its configured defaults.
func (a *app) newProvider(name string) (provider.Provider, error) {
	if !validProvider(name) {
		return nil, fmt.Errorf("unknown provider %q (supported: %s)", name, providerNames())
	}
	p, err := a.cfg.ResolveAgent("", config.AgentProfile{Provider: name})
	if errors.Is(err, config.ErrNoModel) && name == openai.Name {
		// Signing in picks the model (chooseOpenAIModel), so it can't
		// need one first.
		err = nil
	}
	if err != nil {
		return nil, err
	}
	return a.providerFor(task.AgentSpec{Provider: name, Model: p.Model, Effort: p.Effort, MaxTokens: p.MaxTokens, CompactAt: p.CompactAt})
}

// providerFor builds one agent's provider from its OAuth grant and the
// agent's own model settings.
func (a *app) providerFor(spec task.AgentSpec) (provider.Provider, error) {
	pc := a.cfg.Providers[spec.Provider]
	// Tasks created before per-agent settings existed fall back to config.
	if spec.MaxTokens == 0 {
		spec.MaxTokens = pc.MaxTokens
	}
	if spec.CompactAt == 0 {
		spec.CompactAt = pc.CompactAt
	}
	switch spec.Provider {
	case "mock":
		if !validProvider("mock") {
			return nil, fmt.Errorf("the mock provider only runs in a sandbox (%s set)", config.HomeEnv)
		}
		delay := 600 * time.Millisecond
		if d, err := time.ParseDuration(os.Getenv("ACKN4TH_MOCK_DELAY")); err == nil {
			delay = d
		}
		return mock.New(spec.Model, delay), nil
	case "claude":
		profile, err := auth.LoadClaudeProfile()
		if err != nil {
			return nil, err
		}
		return claude.New(profile, claude.Config{Model: spec.Model, MaxTokens: spec.MaxTokens, Effort: spec.Effort, CompactAt: spec.CompactAt}), nil
	case openai.Name:
		key, err := auth.OpenAIKey()
		if err != nil {
			return nil, err
		}
		return openai.New(openai.Config{APIKey: key, Model: spec.Model, MaxTokens: spec.MaxTokens, Effort: spec.Effort, CompactAt: spec.CompactAt, BaseURL: pc.BaseURL}), nil
	case clis.CodexName, clis.CursorName, clis.CopilotName:
		return cliagent.New(auth.CLIDriver(spec.Provider), spec.Model, spec.Effort), nil
	case claudecode.Name:
		dir, err := config.DataDir()
		if err != nil {
			return nil, err
		}
		return claudecode.New(claudecode.Config{Model: spec.Model, Effort: spec.Effort, Dir: filepath.Join(dir, "claude-code")}), nil
	case "gemini":
		gc := gemini.Config{
			Model: spec.Model, MaxTokens: spec.MaxTokens, Effort: spec.Effort, CompactAt: spec.CompactAt,
			Project: pc.Project, Location: pc.Location,
		}
		switch pc.GeminiAuth() {
		case config.GeminiKey:
			key, err := auth.GeminiKey()
			if err != nil {
				return nil, err
			}
			gc.APIKey = key
			return gemini.New(nil, gc), nil
		case config.GeminiVertex:
			creds, err := auth.LoadGoogleCredentials()
			if err != nil {
				return nil, err
			}
			return gemini.New(creds, gc), nil
		}
		return nil, fmt.Errorf("providers.gemini.auth: %q is not key or vertex", pc.Auth)
	}
	return nil, fmt.Errorf("unknown provider %q (supported: %s)", spec.Provider, providerNames())
}

// runner builds a task runner from the configuration.
func (a *app) runner() (*task.Runner, error) {
	roles, err := a.roles()
	if err != nil {
		return nil, err
	}
	allowed, err := a.cfg.AllowedRoots()
	if err != nil {
		return nil, err
	}
	cmds, hide, err := a.commandPolicy()
	if err != nil {
		return nil, err
	}
	r := &task.Runner{
		Store: a.store, Roles: roles, NewProvider: a.providerFor, AllowedRoots: allowed,
		Commands: cmds, Hide: hide,
		MaxSteps: a.cfg.Limits.MaxStepsPerTurn, MaxFileBytes: a.cfg.Limits.MaxFileBytes,
		// Checked before every model request, so signing out of a
		// provider stops the tasks using it.
		Authorize: func(p string) error {
			if p == "mock" {
				return nil
			}
			return auth.StillHeld(p)
		},
		Spend: &spend.Policy{Limits: a.cfg.Spend, Ledger: a.ledger},
	}
	r.Completed = a.completed(r)
	return r, nil
}

// commandPolicy is how agents may run commands: nil when the config
// turns them off or this system can't sandbox them. hide lists ackn4th's
// own directories, which commands may never read.
func (a *app) commandPolicy() (*tools.CommandPolicy, []string, error) {
	c := a.cfg.Commands
	if !c.Enabled || sandbox.Available() != nil {
		return nil, nil, nil
	}
	if c.Timeout <= 0 {
		c.Timeout = 10 * time.Minute
	}
	cfgDir, err := config.Dir()
	if err != nil {
		return nil, nil, err
	}
	dataDir, err := config.DataDir()
	if err != nil {
		return nil, nil, err
	}
	return &tools.CommandPolicy{Network: c.Network, Timeout: c.Timeout}, []string{cfgDir, dataDir}, nil
}

// requireSignedIn fails unless every provider is signed in, naming each
// one that isn't. The sandbox's mock provider needs no sign-in.
func (a *app) requireSignedIn(providers ...string) error {
	real := slices.DeleteFunc(slices.Clone(providers), func(p string) bool { return p == "mock" })
	return auth.Require(real...)
}

// validProvider reports whether name can power an agent here: any real
// provider, and mock only in a sandbox.
func validProvider(name string) bool {
	if name == "mock" {
		_, ok := config.Sandbox()
		return ok
	}
	return auth.ValidProvider(name)
}

// providerNames lists the providers usable here, for error messages.
func providerNames() string {
	names := slices.Clone(auth.Providers)
	if _, ok := config.Sandbox(); ok {
		names = append(names, "mock")
	}
	return strings.Join(names, ", ")
}

// errReported fails a command whose error was already shown.
var errReported = errors.New("error already reported")

// Execute runs the CLI.
func Execute(ctx context.Context) int {
	a := &app{}
	root := &cobra.Command{
		Use:   "ackn4th",
		Short: "Back and forth, forever: orchestrate teams of AI agents on your code",
		Long: `ackn4th runs teams of AI agents, each playing a software role, that work
in a sandboxed workspace, taking turns or all at once, until they agree the
task is done.

A hobby tool: it iterates on a prompt until the result is ready to demo, on
your own accounts and under each provider's terms. MIT licensed, no warranty.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return a.load()
		},
		// Alone on a terminal, ackn4th opens the dashboard: a friendlier
		// first step than a wall of help. Elsewhere it prints the help.
		RunE: func(cmd *cobra.Command, args []string) error {
			if a.interactive() {
				return a.dash(cmd.Context(), "", voiceFlag(cmd, a.speak))
			}
			a.term.Banner()
			a.bannerShown = true
			return cmd.Help()
		},
	}
	installHelp(root, a)
	root.Flags().BoolVar(&a.speak, "voice", false, "read the dashboard aloud this time (see ackn4th voice)")
	root.PersistentFlags().BoolVar(&a.noFX, "no-fx", false, "disable animations (also: ACKN4TH_NO_FX=1)")
	root.PersistentFlags().BoolVar(&a.noInput, "no-input", false, "never prompt; fail on missing input (also: "+NoInputEnv+"=1, CI, or stdin not a terminal)")
	// Help lists commands in this order: the order you'd use them in.
	cobra.EnableCommandSorting = false
	root.AddCommand(
		startTaskCmd(a), runAgentCmd(a), dashCmd(a), watchTaskCmd(a), stopTaskCmd(a), resumeTaskCmd(a), followUpCmd(a),
		listTasksCmd(a), showTaskCmd(a), renameTaskCmd(a), deleteTaskCmd(a),
		notesCmd(a), briefCmd(a), trustCmd(a), untrustCmd(a),
		loginCmd(a), statusCmd(a), logoutCmd(a),
		rolesCmd(a), configCmd(a), spendCmd(a), walkthroughCmd(a), openPRCmd(a), voiceCmd(a), disableTipsCmd(a), enableTipsCmd(a), versionCmd(), internalRunCmd(a),
	)
	if cmd, err := root.ExecuteContextC(ctx); err != nil {
		if !errors.Is(err, errReported) {
			printError(ui.NewTerm(os.Stderr, false), cmd, err)
		}
		return 1
	}
	return 0
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "version",
		GroupID: "info",
		Short:   "Print the version",
		Run:     func(cmd *cobra.Command, args []string) { fmt.Println("ackn4th", Version) },
	}
}

// pickOpenAIModel picks the newest model the OpenAI key can use, when
// none is configured, and saves it, as signing in does. Tasks started
// from the dashboard or a script reach it when sign-in didn't pick one.
func (a *app) pickOpenAIModel(ctx context.Context) error {
	key, err := auth.OpenAIKey()
	if err != nil {
		return fmt.Errorf("%w for openai, and it isn't signed in: run `ackn4th login --provider openai` (p in the dashboard), which picks the newest model your key can use", config.ErrNoModel)
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	m, err := openai.New(openai.Config{APIKey: key, BaseURL: a.cfg.Providers[openai.Name].BaseURL}).PickModel(ctx)
	if err != nil {
		return fmt.Errorf("%w for openai, and none could be picked (%v): set one with C in the dashboard (Models) or providers.openai.model", config.ErrNoModel, err)
	}
	if _, err := config.SetKey("providers.openai.model", m); err != nil {
		return err
	}
	pc := a.cfg.Providers[openai.Name]
	pc.Model = m
	a.cfg.Providers[openai.Name] = pc
	return nil
}
