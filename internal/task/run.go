package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dethmasque/ackn4th/internal/agent"
	"github.com/dethmasque/ackn4th/internal/event"
	"github.com/dethmasque/ackn4th/internal/notes"
	"github.com/dethmasque/ackn4th/internal/onboard"
	"github.com/dethmasque/ackn4th/internal/provider"
	"github.com/dethmasque/ackn4th/internal/role"
	"github.com/dethmasque/ackn4th/internal/sandbox"
	"github.com/dethmasque/ackn4th/internal/spend"
	"github.com/dethmasque/ackn4th/internal/tools"
	"github.com/dethmasque/ackn4th/internal/ui"
	"github.com/dethmasque/ackn4th/internal/workspace"
)

// digestBytes caps the changes digest an agent gets at turn start.
const digestBytes = 16 << 10

// operator attributes workspace changes made outside ackn4th.
const operator = "the human operator, outside ackn4th"

// shared is the task's view of the workspace between turns.
type shared struct {
	Snapshot *onboard.Snapshot `json:"snapshot"`
	// Authors maps a path to whoever changed it last.
	Authors map[string]string `json:"authors"`
}

// run is one execution of a task, from Run until it returns.
type run struct {
	r *Runner
	t *Task

	// mu guards t, sh and the inbox once agents may work concurrently.
	mu     sync.Mutex
	roles  []*role.Role
	agents []*agent.Agent
	labels []string // how each agent is credited for changes
	view   *workspace.Workspace
	book   *notes.Book
	sh     *shared
	closer []func() error

	systems []provider.System // each agent's system prompt
	tools   []provider.ToolSpec
	warmed  bool // shared prompts were prewarmed this run
	// changed counts the files each agent has changed this run.
	changed map[int]int
	// spendErr is a failure to record spending, which stops the task
	// when a cap depends on the record.
	spendErr error
}

// taskCap is the task's spending cap in dollars, 0 for none.
func (r *Runner) taskCap(t *Task) float64 {
	if t.MaxCost > 0 {
		return t.MaxCost
	}
	if r.Spend != nil {
		return r.Spend.Limits.PerTask
	}
	return 0
}

// priced refuses to run an agent under a spending cap when its model's
// price is unknown: the cap couldn't be kept.
func (r *Runner) priced(t *Task, spec AgentSpec) error {
	if r.Spend == nil || !r.Spend.Capped(spec.Provider, r.taskCap(t)) {
		return nil
	}
	if _, ok := r.Spend.Price(spec.Provider, spec.Model); !ok {
		return fmt.Errorf("%s (%s): a spending cap applies, but ackn4th doesn't know what %s costs. Add its price under spend.prices in the config (dollars per million tokens: input, cached, output)", spec.Role, spec.Provider, spec.Model)
	}
	return nil
}

// authorize is checked before every model request of an agent on prov:
// its sign-in, and the spending caps.
func (rn *run) authorize(prov string) error {
	if err := rn.r.authorize(prov); err != nil {
		return err
	}
	if rn.r.Spend == nil {
		return nil
	}
	rn.mu.Lock()
	cost, failed := rn.t.Cost(), rn.spendErr
	rn.mu.Unlock()
	if failed != nil && rn.r.Spend.Capped(prov, rn.r.taskCap(rn.t)) {
		return &spend.OverError{Msg: "spending can't be recorded, so the caps can't be kept: " + failed.Error()}
	}
	return rn.r.Spend.Check(prov, cost, rn.r.taskCap(rn.t), time.Now())
}

// spent records what agent n (0-based) just used.
func (rn *run) spent(n int, u provider.Usage) {
	if rn.r.Spend == nil {
		return
	}
	rn.mu.Lock()
	spec := rn.t.Agents[n]
	rn.mu.Unlock()
	usd, err := rn.r.Spend.Record(rn.t.ID, spec.Provider, spec.Model, spend.Tokens{
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CachedTokens: u.CachedTokens, CacheWriteTokens: u.CacheWriteTokens,
	}, time.Now())
	rn.mu.Lock()
	rn.t.Agents[n].Cost += usd
	if err != nil {
		rn.spendErr = err
	}
	rn.mu.Unlock()
}

// setup builds the agents, their tools and the shared state.
func (r *Runner) setup(t *Task) (*run, error) {
	rn := &run{r: r, t: t}
	team := make([]agent.Member, len(t.Agents))
	members := make([]string, len(t.Agents))
	for i, spec := range t.Agents {
		rl, err := r.Roles.Get(spec.Role)
		if err != nil {
			return nil, err
		}
		rn.roles = append(rn.roles, rl)
		team[i] = agent.Member{Role: rl, Provider: spec.Provider, Model: spec.Model}
		members[i] = rl.Name
	}
	for i, spec := range t.Agents {
		label := spec.Role
		if strings.Count(","+strings.Join(members, ",")+",", ","+spec.Role+",") > 1 {
			label = fmt.Sprintf("%s #%d", spec.Role, i+1)
		}
		rn.labels = append(rn.labels, label+", "+spec.Provider)
	}

	// Re-check the workspace every run: config may have changed since the
	// task was created, or the directory may have been swapped for a link.
	dir, err := workspace.Resolve(t.Workspace, r.AllowedRoots, false)
	if err != nil {
		return nil, err
	}
	// A read-only view for snapshots and the brief, and the project's notes.
	if rn.view, err = workspace.Open(dir, nil, r.MaxFileBytes); err != nil {
		return nil, err
	}
	rn.closer = append(rn.closer, rn.view.Close)
	rn.book = notes.Open(r.Store.DataDir(), dir)
	rn.sh = r.loadShared(t.ID)
	if rn.sh.Snapshot == nil {
		rn.sh.Snapshot = onboard.Take(rn.view, nil)
	}
	// Agents that have not joined yet catch up from this point: anything
	// teammates change before their first turn shows up as changes.
	for i := range t.Agents {
		if _, err := os.Stat(r.Store.SnapshotPath(t.ID, i)); err != nil && !t.Started[i] {
			if err := saveJSON(r.Store.SnapshotPath(t.ID, i), rn.sh.Snapshot); err != nil {
				rn.close()
				return nil, err
			}
		}
	}

	brief, err := rn.frozenBrief()
	if err != nil {
		rn.close()
		return nil, err
	}
	rn.tools = tools.Specs(tools.SpecOptions{Team: len(t.Agents) > 1, Commands: r.Commands})

	tm := &tools.Team{
		Members: members,
		Changed: func(n int, p string) {
			rn.mu.Lock()
			rn.sh.Authors[p] = rn.labels[n-1]
			if rn.changed == nil {
				rn.changed = map[int]int{}
			}
			rn.changed[n-1]++
			rn.mu.Unlock()
		},
		Send:    rn.send,
		Command: rn.command,
		Todo:    rn.addTodo,
		Resolve: rn.resolveTodo,
	}
	for i, spec := range t.Agents {
		if err := r.priced(t, spec); err != nil {
			rn.close()
			return nil, err
		}
		p, err := r.NewProvider(spec)
		if err != nil {
			rn.close()
			return nil, err
		}
		ws, err := workspace.Open(dir, rn.roles[i].Writable, r.MaxFileBytes)
		if err != nil {
			rn.close()
			return nil, err
		}
		rn.closer = append(rn.closer, ws.Close)
		system := agent.SystemPrompt(rn.roles[i], team, i+1, r.MaxSteps, t.Parallel(), brief, r.Commands)
		rn.systems = append(rn.systems, system)
		sess := p.NewSession(system, rn.tools)
		if raw, err := os.ReadFile(r.Store.AgentStatePath(t.ID, i)); err == nil {
			if sess, err = p.RestoreSession(system, rn.tools, json.RawMessage(raw)); err != nil {
				rn.close()
				return nil, err
			}
		}
		n := i + 1
		box := sandbox.Config{
			Workspace: dir, Scratch: r.Store.ScratchPath(t.ID, i),
			ReadOnly: rn.roles[i].ReadOnly(), Hide: r.Hide,
		}
		var cmds *tools.Commands
		if r.Commands != nil {
			box.Network = r.Commands.Network
			cmds = &tools.Commands{Policy: *r.Commands, Sandbox: box}
		}
		if c, ok := p.(provider.Confined); ok {
			// Agent CLIs run whole in the sandbox; what they change is
			// credited like a command's output.
			c.Confine(provider.Confinement{Sandbox: box, Credit: func(fn func()) []string { return rn.command(n, fn) }})
		}
		rn.agents = append(rn.agents, &agent.Agent{
			Index: n, Role: rn.roles[i], Provider: p, Session: sess,
			Exec: &tools.Executor{
				WS: ws, Notes: rn.book, Team: tm, Self: n, Commands: cmds,
				Author: fmt.Sprintf("%s (%s, task %s)", spec.Role, spec.Provider, t.ID),
			},
			MaxSteps: r.MaxSteps, Solo: len(t.Agents) == 1, Authorize: func() error { return rn.authorize(spec.Provider) },
			Spent: func(u provider.Usage) { rn.spent(n-1, u) },
			Inbox: func() (string, func()) { return rn.inbox(n) },
			Emit: func(e event.Event) {
				e.Round = t.Round
				r.emitRaw(t.ID, e)
			},
		})
	}
	return rn, nil
}

// command runs one of agent n's (1-based) commands and credits the files
// it changed to the agent.
func (rn *run) command(n int, run func()) []string {
	rn.mu.Lock()
	base := rn.sh.Snapshot
	rn.mu.Unlock()
	before := onboard.Take(rn.view, base)
	run()
	after := onboard.Take(rn.view, before)
	var paths []string
	rn.mu.Lock()
	defer rn.mu.Unlock()
	for _, c := range onboard.Diff(before, after) {
		rn.sh.Authors[c.Path] = rn.labels[n-1]
		paths = append(paths, c.Path)
	}
	return paths
}

// maxOpenTodos bounds the items agents can leave for the operator.
const maxOpenTodos = 40

// addTodo records an action item from agent n (1-based) for the operator.
func (rn *run) addTodo(n int, text string) (int, error) {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	t := rn.t
	if len(t.OpenTodos()) >= maxOpenTodos {
		return 0, fmt.Errorf("there are already %d open items; resolve or merge some first", maxOpenTodos)
	}
	id := 1
	for _, td := range t.Todos {
		id = max(id, td.ID+1)
	}
	t.Todos = append(t.Todos, Todo{ID: id, Text: text, By: t.Agents[n-1].Role, Round: t.Round, Time: time.Now()})
	return id, rn.r.comm().Save(t)
}

// resolveTodo closes item id on agent n's (1-based) word.
func (rn *run) resolveTodo(n, id int, note string) error {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	for i := range rn.t.Todos {
		td := &rn.t.Todos[i]
		if td.ID != id {
			continue
		}
		if td.Done {
			return fmt.Errorf("item #%d is already closed", id)
		}
		td.Done, td.Note = true, strings.TrimSpace(note+" ("+rn.t.Agents[n-1].Role+")")
		return rn.r.comm().Save(rn.t)
	}
	return fmt.Errorf("there is no item #%d", id)
}

// frozenBrief returns the task's project brief, written on first run.
func (rn *run) frozenBrief() (string, error) {
	p := rn.r.Store.BriefPath(rn.t.ID)
	if b, err := os.ReadFile(p); err == nil {
		return string(b), nil
	}
	brief := onboard.Brief(rn.view, onboard.BriefBytes)
	if gh, err := os.ReadFile(rn.r.Store.GitHubPath(rn.t.ID)); err == nil {
		brief = strings.TrimSpace(brief) + "\n\n" + string(gh)
	}
	return brief, writeAtomic(p, []byte(brief))
}

// prewarm writes each group's shared prompt to the prompt cache before
// agents on the same model start at once: a cache entry only becomes
// readable once its first request finishes, so without this every agent
// in the round would pay to write its own copy. Agents share an entry
// when provider, model and effort match (effort is part of the prompt).
func (rn *run) prewarm(ctx context.Context, due []int) {
	if rn.warmed {
		return
	}
	rn.warmed = true
	for _, all := range rn.t.CacheGroups() {
		var g []int
		for _, n := range all {
			if slices.Contains(due, n) {
				g = append(g, n)
			}
		}
		if len(g) < 2 {
			continue
		}
		w, ok := rn.agents[g[0]].Provider.(provider.Prewarmer)
		if !ok {
			continue
		}
		spec := rn.t.Agents[g[0]]
		if rn.authorize(spec.Provider) != nil {
			return // the turn itself reports it
		}
		u, err := w.Prewarm(ctx, provider.System{Shared: rn.systems[g[0]].Shared}, rn.tools)
		rn.spent(g[0], u)
		text := fmt.Sprintf("shared context cached for %d %s agents · %s tokens", len(g), spec.Model, ui.Tokens(u.InputTokens))
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			text = "could not prewarm the shared context (agents will cache it themselves): " + err.Error()
		}
		rn.r.emit(rn.t, event.Event{Kind: event.Notice, Provider: spec.Provider, Model: spec.Model, Text: text})
	}
}

func (rn *run) close() {
	for _, c := range rn.closer {
		c()
	}
	rn.closer = nil
}

// send queues a message for agent to; it is delivered with the
// recipient's next request.
func (rn *run) send(from, to int, text string) error {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	t := rn.t
	id := 1
	for _, m := range t.Inbox {
		id = max(id, m.ID+1)
	}
	t.Inbox = append(t.Inbox, Mail{ID: id, From: from, To: to, Round: t.Round, Text: text, Time: time.Now().UTC()})
	return rn.r.comm().Save(t)
}

// inbox formats agent n's undelivered messages. The returned ack removes
// them once they have been sent to the model.
func (rn *run) inbox(n int) (string, func()) {
	rn.mu.Lock()
	defer rn.mu.Unlock()
	var b strings.Builder
	var ids []int
	for _, m := range rn.t.Inbox {
		if m.To != n {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("# Messages from teammates\n")
		}
		fmt.Fprintf(&b, "\n**%s** (%s): %s\n", rn.t.Agents[m.From-1].Role, rn.t.Agents[m.From-1].Provider, m.Text)
		ids = append(ids, m.ID)
	}
	return strings.TrimSpace(b.String()), func() {
		if len(ids) == 0 {
			return
		}
		rn.mu.Lock()
		defer rn.mu.Unlock()
		rn.t.Inbox = slices.DeleteFunc(rn.t.Inbox, func(m Mail) bool { return slices.Contains(ids, m.ID) })
		rn.r.comm().Save(rn.t)
	}
}

// syncStart takes stock of the workspace before agents start work: changes
// since the last sync were not made by an agent, so they are the
// operator's. It returns the current snapshot.
func (rn *run) syncStart() *onboard.Snapshot {
	now := onboard.Take(rn.view, rn.sh.Snapshot)
	rn.mu.Lock()
	for _, c := range onboard.Diff(rn.sh.Snapshot, now) {
		rn.sh.Authors[c.Path] = operator
	}
	rn.sh.Snapshot = now
	rn.mu.Unlock()
	return now
}

// syncEnd records the workspace after agents finish. Their changes were
// already credited as they made them.
func (rn *run) syncEnd(prev *onboard.Snapshot) error {
	now := onboard.Take(rn.view, prev)
	rn.mu.Lock()
	defer rn.mu.Unlock()
	rn.sh.Snapshot = now
	return rn.r.saveShared(rn.t.ID, rn.sh)
}

// start announces agent n's turn.
func (rn *run) start(n int) {
	spec := rn.t.Agents[n]
	rn.r.emit(rn.t, event.Event{Kind: event.TurnStarted, Agent: n + 1, Role: spec.Role, Provider: spec.Provider, Model: spec.Model})
}

// briefing gathers the context agent n needs at the start of its turn:
// the project brief on its first turn, the project notes when it has not
// seen their current version, and what others changed in the workspace
// since it last worked. It emits a Context event describing what was
// included, and saves now as the agent's new baseline.
func (rn *run) briefing(n int, now *onboard.Snapshot) (string, error) {
	t := rn.t
	var b strings.Builder
	var parts []string
	if !t.Started[n] {
		// The brief itself is in the shared system prompt.
		parts = append(parts, "shared prompt "+ui.Bytes(len(rn.systems[n].Shared))+" incl. brief")
	}
	if v := rn.book.Version(); v != "" && v != t.Agents[n].NotesSeen {
		text, _ := rn.book.Read()
		label := "the team's shared knowledge about this project, kept with update_notes"
		if t.Started[n] {
			label = "updated since you last saw them"
		}
		fmt.Fprintf(&b, "\n# Project notes (%s)\n\n%s\n", label, strings.TrimPrefix(strings.TrimSpace(text), "# Project notes"))
		parts = append(parts, "notes "+ui.Bytes(len(text)))
	}
	rn.mu.Lock()
	open := t.OpenTodos()
	rn.mu.Unlock()
	if len(open) > 0 {
		b.WriteString("\n# Open items for the person who gave the task\n\nResolve any the team has handled with resolve_user_todo.\n\n")
		for _, td := range open {
			fmt.Fprintf(&b, "- #%d (%s): %s\n", td.ID, td.By, td.Text)
		}
		parts = append(parts, fmt.Sprintf("%d open item(s) for the user", len(open)))
	}
	var prev onboard.Snapshot
	if loadJSON(rn.r.Store.SnapshotPath(t.ID, n), &prev) == nil {
		changes := onboard.Diff(&prev, now)
		rn.mu.Lock()
		d, shown := onboard.Digest(changes, rn.sh.Authors, rn.labels[n], digestBytes)
		rn.mu.Unlock()
		if d != "" {
			b.WriteString("\n# Changes since your last turn\n\n" + d)
			parts = append(parts, fmt.Sprintf("%d changed file(s)", strings.Count(d, "\n  ")))
		}
		for _, c := range shown {
			rn.agents[n].Exec.Acknowledge(c.Path, c.Old.Text, c.New.Text)
		}
	}
	if err := saveJSON(rn.r.Store.SnapshotPath(t.ID, n), now); err != nil {
		return "", err
	}
	if len(parts) > 0 {
		spec := t.Agents[n]
		rn.r.emit(t, event.Event{Kind: event.Context, Agent: n + 1, Role: spec.Role, Provider: spec.Provider, Model: spec.Model, Text: strings.Join(parts, " · ")})
	}
	return b.String(), nil
}

// turn runs agent n's turn and records the result in the task. It does
// not touch other agents' state.
func (rn *run) turn(ctx context.Context, n int, input string) (agent.TurnResult, error) {
	t, a := rn.t, rn.agents[n]
	spec := t.Agents[n] // a copy, read before agents run concurrently
	persist := func() error {
		state, err := a.Session.State()
		if err != nil {
			return err
		}
		return writeAtomic(rn.r.Store.AgentStatePath(t.ID, n), state)
	}
	rn.mu.Lock()
	changedBefore := rn.changed[n]
	rn.mu.Unlock()
	res, err := a.Turn(ctx, input, persist)
	if err != nil {
		// What the turn used before it ended was spent all the same.
		rn.mu.Lock()
		t.Agents[n].Usage.Add(res.Usage)
		rn.mu.Unlock()
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if errors.As(err, new(unauthorized)) || errors.As(err, new(*spend.OverError)) {
			return res, err
		}
		return res, fmt.Errorf("%s (%s): %w", spec.Role, spec.Provider, err)
	}

	rn.mu.Lock()
	defer rn.mu.Unlock()
	s := &t.Agents[n]
	t.Started[n] = true
	s.Turns++
	s.Usage.Add(res.Usage)
	s.Context = res.ContextTokens
	s.NotesSeen = rn.book.Version()
	if res.Compacted {
		s.Compactions++
		// The notes were summarized away with everything else.
		s.NotesSeen = ""
	}
	s.Done, s.DoneSummary = res.Done, res.DoneSummary
	s.DoneStreak = 0
	if res.Done {
		s.DoneStreak = spec.DoneStreak + 1
	}
	s.SameHandoffs, s.Idle = 0, 0
	if s.Turns > 1 && !res.Done && res.Message == spec.Message {
		s.SameHandoffs = spec.SameHandoffs + 1
	}
	if res.Cut && !res.Done && rn.changed[n] == changedBefore {
		s.Idle = spec.Idle + 1
	}
	s.Message, s.MessageRound, s.RoundDone = res.Message, t.Round, t.Round
	// Follow-ups can't be added while the task runs, so the input had
	// them all.
	s.FollowUpsSeen = len(t.FollowUps)
	return res, rn.r.comm().Save(t)
}

// StuckError stops a task whose agent is going in circles, before it
// spends more: the same handoff three turns running, or two turns in a
// row out of tool calls (or repeating itself) without changing a file.
type StuckError struct{ msg string }

func (e *StuckError) Error() string { return e.msg }

// A team can split for good: some agents declare done every round while
// the others keep polishing (one copilot team went 25 rounds that way).
// After nudgeSplit such rounds the holdouts are told so; after maxSplit
// the task stops.
const (
	nudgeSplit = 2
	maxSplit   = 5
)

// splitVote counts a finished round in which some agents, but not all,
// declared done, and stops the task when that has gone on too long.
func splitVote(t *Task) error {
	var voted, working []string
	for _, a := range t.Agents {
		if a.DoneStreak > 0 {
			voted = append(voted, a.Role)
		} else {
			working = append(working, a.Role)
		}
	}
	if len(voted) == 0 || len(working) == 0 {
		t.SplitRounds = 0
		return nil
	}
	t.SplitRounds++
	if t.SplitRounds < maxSplit {
		return nil
	}
	return &StuckError{fmt.Sprintf("the team hasn't agreed for %d rounds: %s kept declaring the task done while %s kept working, so the task stopped to save cost. Read the latest handoffs, then give a follow-up with what's left, or resume it",
		t.SplitRounds, strings.Join(voted, ", "), strings.Join(working, ", "))}
}

// splitNudge is what a holdout is told once the team has been split for
// nudgeSplit rounds.
func splitNudge(t *Task, n int) string {
	if t.SplitRounds < nudgeSplit || t.Agents[n].DoneStreak > 0 {
		return ""
	}
	var voted []string
	for i, a := range t.Agents {
		if i != n && a.DoneStreak > 0 {
			voted = append(voted, fmt.Sprintf("%s (%s)", a.Role, plural(a.DoneStreak, "turn")))
		}
	}
	if len(voted) == 0 {
		return ""
	}
	return fmt.Sprintf("\n# Your teammates say the task is done\n\nFor %d rounds, some of the team has declared the task done while the rest kept working. Declared done in their latest turns, for that many turns running: %s. "+
		"If every requirement of the task is met, declare done this turn: polishing what already works keeps the task running and costs the person money every round. "+
		"What only the person can do (a browser or device check, a credential, a decision) belongs in add_user_todo, and open to-dos don't stop you declaring done. "+
		"If something the task requires is still missing, name it in your closing message and finish it now. After %d such rounds the task stops.\n",
		t.SplitRounds, strings.Join(voted, ", "), maxSplit)
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

func stuck(t *Task, n int) error {
	s := t.Agents[n]
	who := fmt.Sprintf("agent %d (%s, %s %s)", n+1, s.Role, s.Provider, s.Model)
	var what string
	switch {
	case s.SameHandoffs >= 2:
		what = fmt.Sprintf("gave the same handoff three turns running (“%s”)", oneLine(s.Message, 80))
	case s.Idle >= 2:
		what = "used its turn up two turns running without changing a file or handing off"
	default:
		return nil
	}
	return &StuckError{fmt.Sprintf("%s %s: it looks stuck, so the task stopped to save cost. Resume it on another model or provider, or give it a follow-up", who, what)}
}

// reminderMax is how much of the task a reminder quotes.
const reminderMax = 2000

// reminder restates the task at the start of every turn after an agent's
// first, so it stays in view however long the run, and survives the
// conversation being compacted into the model's own summary.
func reminder(t *Task, agent int) string {
	clip := func(s string) string {
		s = strings.TrimSpace(s)
		if r := []rune(s); len(r) > reminderMax {
			s = string(r[:reminderMax]) + "… (cut here; the full text was in your first turn)"
		}
		return s
	}
	s := "# The task (a reminder: keep your work on it)\n\n" + clip(t.Prompt) + "\n"
	// A follow-up the agent hasn't had yet is given in full below.
	if n := len(t.FollowUps); n > 0 && t.Agents[agent].FollowUpsSeen >= n {
		f := t.FollowUps[n-1]
		s += fmt.Sprintf("\nThe latest follow-up, from round %d:\n\n%s\n", f.Round, clip(f.Prompt))
	}
	return s
}

// oneLine is s on one line, at most n runes.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n-1]) + "…"
	}
	return s
}

// sequential runs agents one at a time, each handing off to the next.
func (rn *run) sequential(ctx context.Context) error {
	t := rn.t
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if t.MaxRounds > 0 && t.Round > t.MaxRounds {
			return ErrRoundLimit
		}
		n := t.Next
		if n == 0 {
			rn.r.emit(t, event.Event{Kind: event.RoundStarted})
		}
		rn.start(n)
		now := rn.syncStart()
		briefing, err := rn.briefing(n, now)
		if err != nil {
			return err
		}
		res, err := rn.turn(ctx, n, rn.input(n, briefing))
		if serr := rn.syncEnd(now); serr != nil && err == nil {
			err = serr
		}
		if err != nil {
			return err
		}
		if err := stuck(t, n); err != nil {
			return err
		}

		if !res.Done {
			// Consensus must be unanimous and current: one agent with
			// concerns resets everyone's vote.
			for i := range t.Agents {
				t.Agents[i].Done = false
			}
		}
		t.Handoff, t.HandoffFrom, t.Note = res.Message, n+1, ""
		t.Next = (n + 1) % len(t.Agents)
		if t.Next == 0 {
			if err := splitVote(t); err != nil {
				return err
			}
			t.Round++
		}
		if err := rn.r.comm().Save(t); err != nil {
			return err
		}
		if allDone(t) {
			t.Reason = summary(t)
			return nil
		}
	}
}

// parallel runs every agent's turn of a round at the same time. Agents
// coordinate through messages and the workspace; each round's closing
// messages go to everyone at the start of the next round.
func (rn *run) parallel(ctx context.Context) error {
	t := rn.t
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if t.MaxRounds > 0 && t.Round > t.MaxRounds {
			return ErrRoundLimit
		}
		// After a stop mid-round, only the agents still due run.
		var due []int
		for i, a := range t.Agents {
			if a.RoundDone < t.Round {
				due = append(due, i)
			}
		}
		if len(due) == len(t.Agents) {
			rn.r.emit(t, event.Event{Kind: event.RoundStarted})
		}
		now := rn.syncStart()
		inputs := map[int]string{}
		for _, n := range due {
			rn.start(n)
			briefing, err := rn.briefing(n, now)
			if err != nil {
				return err
			}
			inputs[n] = rn.input(n, briefing)
		}

		rn.prewarm(ctx, due)

		rctx, cancel := context.WithCancel(ctx)
		errs := make([]error, len(t.Agents))
		var wg sync.WaitGroup
		for _, n := range due {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := rn.turn(rctx, n, inputs[n]); err != nil {
					errs[n] = err
					cancel() // one failure stops the round; others resume later
				}
			}()
		}
		wg.Wait()
		cancel()
		if err := rn.syncEnd(now); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, err := range errs {
			if err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
		}
		// A stuck agent stops the task once the round is over, so its
		// teammates' turns aren't cut short.
		for _, n := range due {
			if err := stuck(t, n); err != nil {
				return err
			}
		}

		// The round is complete.
		if allDone(t) {
			t.Reason = summary(t)
			return nil
		}
		if err := splitVote(t); err != nil {
			return err
		}
		for i := range t.Agents {
			t.Agents[i].Done = false
		}
		t.Round++
		t.Note = ""
		if err := rn.r.comm().Save(t); err != nil {
			return err
		}
	}
}

// input composes what agent n receives at the start of its turn.
func (rn *run) input(n int, briefing string) string {
	t := rn.t
	var b strings.Builder
	if !t.Started[n] {
		fmt.Fprintf(&b, "# Task\n\n%s\n", t.Prompt)
	} else {
		b.WriteString(reminder(t, n))
	}
	b.WriteString(briefing)
	if t.Parallel() {
		var msgs strings.Builder
		for i, a := range t.Agents {
			if i != n && a.Message != "" && a.MessageRound >= t.Round-1 {
				fmt.Fprintf(&msgs, "\n## %s (%s), round %d\n\n%s\n", rn.roles[i].Title, a.Provider, a.MessageRound, a.Message)
			}
		}
		if msgs.Len() > 0 {
			b.WriteString("\n# Teammates' closing messages from their last turns\n" + msgs.String())
		}
	} else if t.HandoffFrom > 0 && t.HandoffFrom != n+1 {
		from := t.Agents[t.HandoffFrom-1]
		fmt.Fprintf(&b, "\n# Message from %s (%s)\n\n%s\n", rn.roles[t.HandoffFrom-1].Title, from.Provider, t.Handoff)
	}
	if t.Note != "" {
		fmt.Fprintf(&b, "\n# Note from the human operator\n\n%s\n", t.Note)
	}
	// Follow-ups go to every agent once, however its turns fall. An
	// agent's first turn has the task above, so it gets all of them.
	for _, f := range t.pendingFollowUps(n) {
		fmt.Fprintf(&b, "\n# Follow-up from the person who gave the task (round %d)\n\nThe task was finished; this is more work for the team, in the same workspace. Every agent has to declare done again.\n\n%s\n", f.Round, f.Prompt)
		if len(f.Changes) > 0 {
			fmt.Fprintf(&b, "\nThe team changed for this follow-up (the roles above are current):\n\n- %s\n", strings.Join(f.Changes, "\n- "))
		}
	}
	if t.Started[n] && t.Solo() {
		b.WriteString("Continue working on the task. Call declare_done once it is complete.\n")
	}
	var done []string
	for i, a := range t.Agents {
		if a.Done && i != n {
			done = append(done, a.Role)
		}
	}
	if len(done) > 0 {
		when := ""
		if t.Parallel() {
			when = " this round"
		}
		fmt.Fprintf(&b, "\nTeammates who have declared the task done%s: %s.\n", when, strings.Join(done, ", "))
	}
	if t.Parallel() && len(done) == 0 {
		// Votes are cleared between rounds; say who voted last time.
		var last []string
		for i, a := range t.Agents {
			if i != n && a.DoneStreak > 0 {
				last = append(last, a.Role)
			}
		}
		if len(last) > 0 {
			fmt.Fprintf(&b, "\nTeammates who declared the task done last round: %s. The task completes when every agent declares done in the same round.\n", strings.Join(last, ", "))
		}
	}
	b.WriteString(splitNudge(t, n))
	round := fmt.Sprintf("\n(Round %d", t.Round)
	if t.MaxRounds > 0 {
		round += fmt.Sprintf(" of %d", t.MaxRounds)
	}
	if t.Parallel() {
		round += "; all agents are working at the same time"
	}
	b.WriteString(round + ".)")
	return strings.TrimSpace(b.String())
}

func (r *Runner) loadShared(id string) *shared {
	sh := &shared{}
	loadJSON(r.Store.WorkspacePath(id), sh)
	if sh.Authors == nil {
		sh.Authors = map[string]string{}
	}
	return sh
}

func (r *Runner) saveShared(id string, sh *shared) error {
	return saveJSON(r.Store.WorkspacePath(id), sh)
}

func loadJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func saveJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeAtomic(path, data)
}
