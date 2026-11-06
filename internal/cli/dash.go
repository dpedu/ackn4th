package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/dethmasque/ackn4th/internal/auth"
	"github.com/dethmasque/ackn4th/internal/config"
	"github.com/dethmasque/ackn4th/internal/dash"
	"github.com/dethmasque/ackn4th/internal/event"
	"github.com/dethmasque/ackn4th/internal/github"
	"github.com/dethmasque/ackn4th/internal/provider/claudecode"
	"github.com/dethmasque/ackn4th/internal/provider/openai"
	"github.com/dethmasque/ackn4th/internal/role"
	"github.com/dethmasque/ackn4th/internal/task"
	"github.com/dethmasque/ackn4th/internal/voice"
	"github.com/dethmasque/ackn4th/internal/workspace"
)

func dashCmd(a *app) *cobra.Command {
	var speak bool
	cmd := &cobra.Command{
		Use:     "dash [task]",
		GroupID: "tasks",
		Aliases: []string{"dashboard", "top", "ui"},
		Short:   "Explore every task and where each agent is, in a keyboard-driven dashboard",
		Long: `Open the dashboard: every task on the left; on the right, the selected
task's workflow, a live card per agent (what it's doing right now, for how
long, its context and cache use), the full activity log, the files agents
changed, and every handoff message.

From it you can start a task with a form (n), stop, resume or delete tasks
(s, r, x), sign in to providers (p), manage roles (R), replay the
walkthrough (t) and have it read aloud (v). Press ? inside for every key.

It reads the same records the runners write, so it shows foreground and
background tasks alike, and quitting it never stops a task. Bare ` + "`ackn4th`" + `
opens it too.

--voice reads it aloud from the start, without changing your settings;
` + "`ackn4th voice on`" + ` makes that the default.`,
		Example: `  ackn4th dash
  ackn4th start-task -d --parallel --roles backend_dev,qa_engineer "Add rate limiting" && ackn4th dash
  ackn4th dash k3f9qz            # open with a task selected
  ackn4th dash --voice           # read aloud (installs the voice the first time)`,
		Args: upTo(1, "[task]"),
		RunE: func(cmd *cobra.Command, args []string) error {
			focus := ""
			if len(args) == 1 {
				tk, err := a.backend().Find(args[0])
				if err != nil {
					return err
				}
				focus = tk.ID
			}
			return a.dash(cmd.Context(), focus, voiceFlag(cmd, speak))
		},
	}
	cmd.Flags().BoolVar(&speak, "voice", false, "read the dashboard aloud this time (--voice=false keeps it quiet)")
	return cmd
}

// voiceFlag is the --voice flag: nil when it wasn't given.
func voiceFlag(cmd *cobra.Command, v bool) *bool {
	if !cmd.Flags().Changed("voice") {
		return nil
	}
	return &v
}

func walkthroughCmd(a *app) *cobra.Command {
	var speak bool
	cmd := &cobra.Command{
		Use:     "walkthrough",
		GroupID: "info",
		Aliases: []string{"tour"},
		Short:   "Open the dashboard with its walkthrough tooltips, from the start",
		Long: `Open the dashboard and show the walkthrough again, a tooltip at a time,
even if tips are off. In the dashboard, t does the same. To see the tips in
command output again too, run ` + "`ackn4th enable-tips`" + `.`,
		Example: `  ackn4th walkthrough
  ackn4th tour --voice           # with every tooltip read aloud`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !a.interactive() {
				return errors.New("the walkthrough is in the dashboard, which needs an interactive terminal")
			}
			opt, err := a.dashOptions(cmd.Context(), "", voiceFlag(cmd, speak))
			if err != nil {
				return err
			}
			opt.Tour = true
			return dash.Run(opt)
		},
	}
	cmd.Flags().BoolVar(&speak, "voice", false, "read the dashboard aloud this time")
	return cmd
}

// dash opens the dashboard, focused on a task when focus is set.
// speak, when set, overrides the configured voice for this run.
func (a *app) dash(ctx context.Context, focus string, speak *bool) error {
	if !a.interactive() {
		return errors.New("the dashboard needs an interactive terminal; use `ackn4th list-tasks` or `ackn4th watch-task <id>` instead")
	}
	opt, err := a.dashOptions(ctx, focus, speak)
	if err != nil {
		return err
	}
	return dash.Run(opt)
}

// errRemoteChanges says a task's workspace is not on this machine, so git here
// cannot count its uncommitted files. The dashboard shows nothing for it, which
// is what "unknown" looks like there.
var errRemoteChanges = errors.New("the workspace is on the controller")

// dashOptions wires the dashboard to the store, the runner and the login
// and provider-setup flows.
func (a *app) dashOptions(ctx context.Context, focus string, speak *bool) (dash.Options, error) {
	v := a.dashVoice(ctx)
	if speak != nil {
		if v == nil && *speak {
			if err := voice.Supported(); err != nil {
				return dash.Options{}, err
			}
			return dash.Options{}, errors.New("voice: no cache directory to install it in")
		}
		if v != nil {
			v.On = *speak
		}
	}
	// A first run, with no tasks yet, opens on the configuration. A controller
	// that cannot be asked is not a first run: showing somebody with ten tasks
	// the setup screen because their daemon is restarting reads as "all my work
	// is gone", and the next thing they do is reconfigure.
	firstRun := false
	if a.store != nil {
		if tasks, err := a.backend().List(); err == nil {
			firstRun = len(tasks) == 0
		}
	}
	opt := dash.Options{
		State:   a.backend(),
		Store:   a.store,
		Focus:   focus,
		Effects: a.term.Effects,
		Stop: func(tk *task.Task) error {
			return a.stopRunner(context.Background(), tk)
		},
		Resume:   a.resumeBackground,
		Delete:   func(tk *task.Task) error { return a.backend().Delete(tk.ID) },
		FollowUp: a.followUpBackground,
		Launch:   func(req dash.NewTask) (string, error) { return a.dashLaunch(ctx, req) },
		Login:    loginCommand,
		Setup:    a.dashSetup(),
		Voice:    v,

		RoleAdmin: a.dashRoles(ctx),
		Settings:  a.dashSettings(firstRun),
		Tips:      a.tips,
		DisableTips: func() error {
			_, err := config.SetKey("tips", false)
			return err
		},
		RepoDir: a.repoDir,
		Uncommitted: func(dir string) (int, error) {
			if a.remote() {
				// The workspace is on the controller: git here cannot see it, so
				// the count is unknown (an error, which the dashboard shows as
				// nothing) rather than a git process per poll for a path that
				// does not exist on this machine (R68).
				return 0, errRemoteChanges
			}
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			return github.Uncommitted(ctx, dir)
		},
		Trusted: func(dir string) bool {
			if a.remote() {
				return true // the roots the daemon checks are the daemon's
			}
			allowed, err := a.cfg.AllowedRoots()
			if err != nil {
				return false
			}
			_, err = workspace.Resolve(expandHome(dir), allowed, false)
			return err == nil
		},
	}
	err := a.formChoices(&opt)
	return opt, err
}

// setupOffered records that the dashboard offered provider setup.
const setupOffered = "setup.offered"

// dashRoles is the dashboard's roles screen, on the same store as
// `ackn4th roles`.
func (a *app) dashRoles(ctx context.Context) *dash.Roles {
	store, err := roleStore()
	if err != nil {
		return nil
	}
	return &dash.Roles{
		List: func() ([]dash.RoleEntry, []string, error) {
			reg, err := a.roles()
			if err != nil {
				return nil, nil, err
			}
			var out []dash.RoleEntry
			for _, r := range reg.All() {
				out = append(out, dash.RoleEntry{Role: r, Kind: roleKind(r, store)})
			}
			var invalid []string
			for _, e := range reg.Invalid {
				invalid = append(invalid, e.Error())
			}
			return out, invalid, nil
		},
		Save: func(r *role.Role, overwrite bool) error {
			_, err := store.Save(r, overwrite)
			return err
		},
		Import: func(src string, overwrite bool) (*role.Role, error) {
			data, err := role.Fetch(ctx, src)
			if err != nil {
				return nil, err
			}
			r, _, err := store.SaveFile(data, src, overwrite)
			return r, err
		},
		Remove: store.Remove,
	}
}

// dashVoice reads the dashboard aloud; nil where no engine is published.
func (a *app) dashVoice(ctx context.Context) *dash.Voice {
	dir, err := config.VoiceDir()
	if err != nil || voice.Supported() != nil {
		return nil
	}
	return &dash.Voice{
		On:        a.cfg.Voice.Enabled,
		Installed: func() bool { return voice.Installed(dir) },
		Start: func() (dash.Speaker, error) {
			if !voice.Installed(dir) {
				if err := voice.Install(ctx, dir, nil); err != nil {
					return nil, err
				}
			}
			exe, err := os.Executable()
			if err != nil {
				return nil, err
			}
			sp, err := voice.Start(exe, dir, a.voiceSettings())
			if err != nil {
				return nil, err
			}
			return sp, nil
		},
		Save: func(on bool) error {
			_, err := config.SetKey("voice.enabled", on)
			return err
		},
	}
}

// dashSetup is the dashboard's provider-setup screen. It is offered on
// open while no provider is signed in, until the user answers it once.
func (a *app) dashSetup() *dash.Setup {
	return &dash.Setup{
		Offer:  !a.tips.Seen(setupOffered) && !anySignedIn(),
		Status: a.providerStates,
		Logout: signOut,
		Done:   func() { a.tips.Mark(setupOffered) },
	}
}

// providerStates checks every provider at once; the CLIs each take a
// moment to answer.
func (a *app) providerStates() []dash.ProviderState {
	out := make([]dash.ProviderState, len(auth.Providers))
	var wg sync.WaitGroup
	for i, p := range auth.Providers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = a.providerState(p)
		}()
	}
	wg.Wait()
	return out
}

func (a *app) providerState(p string) dash.ProviderState {
	st := dash.ProviderState{Name: p, Note: providerNotes[p]}
	if err := a.missingTool(p); err != nil {
		st.Missing, st.Hint = true, auth.Tools[p].Install
		return st
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	signedIn := false
	switch p {
	case "gemini":
		signedIn = auth.HasGeminiKey() || auth.HasGrant(p)
	case "openai":
		signedIn = auth.HasOpenAIKey()
	case "claude":
		signedIn = auth.HasGrant(p)
	default:
		if g, err := auth.CurrentGrant(ctx, p); err == nil {
			signedIn = true
			st.Detail = g.Account
		}
	}
	switch {
	case signedIn:
		st.Ready = true
		if st.Detail == "" {
			st.Detail = "ready"
		}
	case p == "openai":
		st.Detail = "needs an API key"
	case p == "gemini" && a.cfg.Providers["gemini"].GeminiAuth() == config.GeminiKey:
		st.Detail = "needs an API key"
	default:
		st.Detail = "needs sign-in"
	}
	return st
}

// anySignedIn reports whether some provider is signed in, asking them all
// at once.
func anySignedIn() bool {
	ok := make(chan bool, len(auth.Providers))
	for _, p := range auth.Providers {
		go func() { ok <- auth.SignedIn(p) }()
	}
	for range auth.Providers {
		if <-ok {
			return true
		}
	}
	return false
}

// formChoices fills in what the dashboard's new-task form offers.
func (a *app) formChoices(opt *dash.Options) error {
	roles, err := a.roles()
	if err != nil {
		return err
	}
	for _, r := range roles.All() {
		opt.Roles = append(opt.Roles, dash.Choice{Value: r.Name, Label: r.Name, Note: r.Title})
	}
	provs := slices.Clone(auth.Providers)
	if validProvider("mock") {
		provs = append(provs, "mock")
	}
	for _, name := range provs {
		p := a.cfg.Providers[name]
		models := []string{p.Model}
		if name == "claude" || name == claudecode.Name {
			models = append(models, claudeModels...)
		}
		models = append(models, cliModels[name]...)
		for _, prof := range a.cfg.Agents {
			if prof.Provider == name && prof.Model != "" {
				models = append(models, prof.Model)
			}
		}
		choice := dash.ProviderChoice{
			Name: name, Note: providerNotes[name],
			Models: uniq(models), Efforts: config.Efforts, DefaultEffort: "",
		}
		if name == openai.Name && p.Model == "" {
			choice.AutoModel = "newest your key can use"
		}
		opt.Providers = append(opt.Providers, choice)
	}
	cwd, _ := os.Getwd()
	def := a.preferCLI(a.cfg.DefaultProvider)
	if !slices.Contains(provs, def) {
		def = provs[0]
	}
	opt.Defaults = dash.FormDefaults{
		Workspace: tildeHome(cwd),
		Rounds:    a.cfg.Limits.MaxRounds,
		MaxCost:   a.cfg.Spend.PerTask,
		Provider:  def,
		Roles:     []string{"backend_dev", "code_reviewer"},
	}
	return nil
}

// claudeModels are suggested in the form after the configured model.
// cliModels are suggested for the agent CLIs; "type a model id…" takes
// any other.
var cliModels = map[string][]string{
	"codex":   {"gpt-5.3-codex", "gpt-5.2"},
	"cursor":  {"composer-2.5", "gpt-5.3-codex", "claude-opus-5-thinking-high", "gpt-5.2"},
	"copilot": {"claude-sonnet-5-5", "gpt-5.2"},
	"openai":  {"gpt-5.2", "gpt-5-mini"},
}

var claudeModels = []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5", "claude-fable-5-1"}

var providerNotes = map[string]string{
	"claude":      "Anthropic",
	"gemini":      "Google",
	"claude-code": "your Claude plan, via local Claude Code",
	"openai":      "OpenAI · API key",
	"codex":       "your ChatGPT plan, via the Codex CLI (sandboxed)",
	"cursor":      "your Cursor plan, via the Cursor CLI (sandboxed)",
	"copilot":     "your Copilot plan, via the Copilot CLI (sandboxed)",
	"mock":        "offline, sandbox only",
}

func uniq(ss []string) []string {
	var out []string
	for _, s := range ss {
		if s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// dashLaunch starts a task from the dashboard's form in a background
// runner. It checks everything start-task would, but asks nothing: the
// form already collected the answers.
func (a *app) dashLaunch(ctx context.Context, req dash.NewTask) (string, error) {
	reqs := make([]agentReq, len(req.Agents))
	for i, ag := range req.Agents {
		reqs[i] = agentReq{role: ag.Role, set: config.AgentProfile{Provider: ag.Provider, Model: ag.Model, Effort: ag.Effort}}
	}
	agents, provs, err := a.resolveAgents(reqs)
	if err != nil {
		return "", err
	}
	if req.Parallel && len(agents) < 2 {
		return "", errors.New("parallel needs at least two agents")
	}
	id := task.NewID()
	dir := expandHome(req.Workspace)
	var resolved string
	if a.remote() {
		// The dashboard is the same entry point as start-task, so it resolves
		// the workspace the same way. The daemon owns the directory, its roots
		// and its clone: a tick box here cannot trust a path on that host (it
		// would write *this* machine's config.yaml, which the daemon never
		// reads), and --repo must not clone onto a disk no runner can see.
		if err := remoteVCSErr(req.Branch, req.DraftPR); err != nil {
			return "", err
		}
		f := &runFlags{workspace: dir, repo: req.Repo, branch: req.Branch, draftPR: req.DraftPR}
		if resolved, err = a.taskWorkspace(ctx, f, id); err != nil {
			return "", err
		}
	} else {
		allowed, err := a.cfg.AllowedRoots()
		if err != nil {
			return "", err
		}
		if req.Repo != "" {
			resolved, err = a.repoWorkspace(ctx, req.Repo, allowed, true)
		} else {
			resolved, err = workspace.Resolve(dir, allowed, true)
		}
		if errors.Is(err, workspace.ErrUntrusted) && req.Trust && req.Repo == "" {
			if _, err := a.trust(dir); err != nil {
				return "", err
			}
			allowed, _ = a.cfg.AllowedRoots()
			resolved, err = workspace.Resolve(dir, allowed, true)
		}
		if err != nil {
			return "", err
		}
	}
	if err := a.useBranch(ctx, resolved, req.Branch, true); err != nil {
		return "", err
	}
	if req.DraftPR {
		if err := a.checkDraftPR(ctx, resolved); err != nil {
			return "", fmt.Errorf("draft PR: %w", err)
		}
	}
	if err := a.dashSignedIn(provs); err != nil {
		return "", err
	}
	if err := a.preflight(ctx, agents, false); err != nil {
		return "", err
	}
	tk := &task.Task{ID: id, Name: cmp.Or(req.Name, task.DefaultName(req.Prompt)), Prompt: req.Prompt, Workspace: resolved, Repo: req.Repo, Agents: agents, MaxRounds: req.Rounds, MaxCost: req.MaxCost, DraftPR: req.DraftPR, Branch: req.Branch}
	if req.Parallel {
		tk.Mode = task.ModeParallel
	}
	ghContext, err := a.githubBrief(ctx, req.Prompt, true)
	if err != nil {
		return "", err
	}
	if err := a.createTask(tk, ghContext); err != nil {
		return "", err
	}
	if err := a.spawnRunner(ctx, tk.ID); err != nil {
		return tk.ID, err
	}
	return tk.ID, nil
}

// dashSignedIn is requireSignedIn for the dashboard: when signing in would
// fix it, it returns *dash.LoginRequired so the dashboard can offer that.
func (a *app) dashSignedIn(provs []string) error {
	err := a.requireSignedIn(provs...)
	if err == nil {
		return nil
	}
	need := slices.DeleteFunc(slices.Clone(provs), func(p string) bool { return p == "mock" })
	// A sign-in can't succeed without its CLI: say so instead of offering
	// one.
	for _, p := range need {
		if err := a.missingTool(p); err != nil {
			return fmt.Errorf("not signed in, and %w", err)
		}
	}
	return &dash.LoginRequired{Providers: need, Err: err}
}

// loginCommand runs `ackn4th login` for providers, for the dashboard to
// hand the terminal to.
func loginCommand(providers []string) *exec.Cmd {
	exe, err := os.Executable()
	if err != nil {
		exe = "ackn4th"
	}
	args := []string{"login", "--pause"}
	for _, p := range providers {
		args = append(args, "--provider", p)
	}
	return exec.Command(exe, args...)
}

// expandHome expands a leading ~ to the home directory.
func expandHome(dir string) string {
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(dir, "~"))
		}
	}
	return dir
}

// tildeHome shortens a path under the home directory to ~/….
func tildeHome(dir string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return dir
	}
	if dir == home {
		return "~"
	}
	if rel, ok := strings.CutPrefix(dir, home+string(filepath.Separator)); ok {
		return "~/" + rel
	}
	return dir
}

// resumeBackground resumes a stopped task in a background runner, for the
// dashboard, first moving its agents to prov unless that is empty. It
// checks what resume-task would, without asking anything.
// followUpBackground gives tk a follow-up and starts it in the
// background, for the dashboard.
func (a *app) followUpBackground(tk *task.Task, prompt string, team []dash.NewAgent) error {
	tk, err := a.backend().Load(tk.ID) // the dashboard's copy is for showing
	if err != nil {
		return err
	}
	a.backend().Reconcile(tk)
	// Sign-ins first: if one is needed, the dashboard retries afterwards,
	// and the follow-up must not have been added twice.
	if err := a.dashSignedIn(taskProviders(tk, nil)); err != nil {
		return err
	}
	if err := tk.AddFollowUp(prompt, -1, time.Now()); err != nil {
		return err
	}
	if err := a.changeTeam(tk, team); err != nil {
		return err
	}
	if err := a.backend().Save(tk); err != nil {
		return err
	}
	return a.spawnRunner(context.Background(), tk.ID)
}

// changeTeam gives tk's agents the roles, models and efforts in team (one
// entry per agent, providers unchanged), and records what changed on the
// latest follow-up so every agent is told. A new role or effort keeps the
// agent's conversation; a new model starts it fresh (a conversation can't
// always move between models), with the workspace, notes and the task's
// history carried over.
func (a *app) changeTeam(tk *task.Task, team []dash.NewAgent) error {
	if team == nil {
		return nil
	}
	if len(team) != len(tk.Agents) {
		return fmt.Errorf("task %s has %d agents, not %d", tk.Label(), len(tk.Agents), len(team))
	}
	reg, err := a.roles()
	if err != nil {
		return err
	}
	effort := func(e string) string {
		if e == "" {
			return "default"
		}
		return e
	}
	var changes []string
	for i, n := range team {
		old := tk.Agents[i]
		if n.Provider != "" && n.Provider != old.Provider {
			return fmt.Errorf("agent %d: a follow-up keeps each agent's provider (%s); resume-task --provider moves agents", i+1, old.Provider)
		}
		for _, r := range strings.Split(n.Role, "+") {
			if _, err := reg.Get(r); err != nil {
				return fmt.Errorf("agent %d: %w", i+1, err)
			}
		}
		var what []string
		if n.Role != old.Role {
			what = append(what, fmt.Sprintf("is now %s (was %s)", n.Role, old.Role))
		}
		if n.Model != old.Model {
			what = append(what, fmt.Sprintf("runs on model %s (was %s), starting a fresh conversation", n.Model, old.Model))
			if err := a.backend().Switch(tk, i, old.Provider, n.Model, n.Effort); err != nil {
				return err
			}
		}
		if n.Effort != old.Effort {
			what = append(what, fmt.Sprintf("uses %s effort (was %s)", effort(n.Effort), effort(old.Effort)))
		}
		if len(what) == 0 {
			continue
		}
		tk.Agents[i].Role, tk.Agents[i].Effort = n.Role, n.Effort
		changes = append(changes, fmt.Sprintf("agent %d %s", i+1, strings.Join(what, ", ")))
	}
	if len(changes) == 0 {
		return nil
	}
	f := &tk.FollowUps[len(tk.FollowUps)-1]
	f.Changes = changes
	a.backend().AppendEvent(tk.ID, event.Event{Kind: event.Notice, Time: time.Now(), Round: tk.Round, Text: "team changed: " + strings.Join(changes, "; ")})
	return nil
}

func (a *app) resumeBackground(tk *task.Task, prov string) error {
	tk, err := a.backend().Load(tk.ID) // the dashboard's copy is for showing
	if err != nil {
		return err
	}
	a.backend().Reconcile(tk)
	if !tk.Resumable() {
		return fmt.Errorf("task %s is %s", tk.Label(), tk.Status)
	}
	if tk.MaxRounds > 0 && tk.Round > tk.MaxRounds {
		return fmt.Errorf("task %s used all %d rounds: ackn4th resume-task %s --rounds N", tk.Label(), tk.MaxRounds, tk.ID)
	}
	moves, err := a.planSwitch(tk, prov)
	if err != nil {
		return err
	}
	if err := a.dashSignedIn(taskProviders(tk, moves)); err != nil {
		return err
	}
	if err := a.applySwitch(tk, moves); err != nil {
		return err
	}
	return a.spawnRunner(context.Background(), tk.ID)
}
