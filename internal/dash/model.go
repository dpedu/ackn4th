package dash

import (
	"errors"
	"fmt"
	"image/color"
	"os/exec"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/dethmasque/ackn4th/internal/task"
	"github.com/dethmasque/ackn4th/internal/tips"
)

// Options configures a dashboard.
type Options struct {
	// State is where task records, events and live previews are read from:
	// the backend the CLI already chose (the file store or a controller), not
	// a second decision taken here. When a caller leaves it out, newModel
	// fills it from Store once, at construction - the same rule as the CLI's
	// single task.OpenBackend call.
	State task.Backend
	// Store holds the artifacts that live on the machine running the
	// runner: log paths and scratch dirs. State reads task state.
	Store *task.Store
	// Focus preselects a task by ID or unique prefix.
	Focus string
	// Tour starts the walkthrough on open, as the t key does.
	Tour bool
	// Effects enables animation (spinners, the header's glyph rain).
	Effects bool
	// Stop asks a task's runner to stop; Resume starts a stopped task in
	// the background, first moving its agents to provider unless that is
	// empty. Either may be nil to disable the action. Resume may return
	// *LoginRequired.
	Stop   func(*task.Task) error
	Resume func(t *task.Task, provider string) error
	// Delete removes a task's records; nil disables it.
	Delete func(*task.Task) error
	// FollowUp gives a task that isn't running a new prompt and starts it
	// again in the background; nil disables it. team, unless nil, is each
	// agent's role, model and effort from now on (providers unchanged). It
	// may return *LoginRequired.
	FollowUp func(t *task.Task, prompt string, team []NewAgent) error

	// Launch starts a task from the new-task form in the background and
	// returns its ID; nil hides the form. It may return *LoginRequired.
	Launch func(NewTask) (string, error)
	// Login is the sign-in command the form hands the terminal to.
	Login func(providers []string) *exec.Cmd
	// Trusted reports whether agents may work in a directory.
	Trusted func(dir string) bool
	// RepoDir is where a task for a GitHub repo works: the clone it makes,
	// or reuses (exists), in the default workspace root. It changes
	// nothing. nil hides it.
	RepoDir func(repo string) (dir string, exists bool, err error)
	// Uncommitted counts the files in a workspace with changes git hasn't
	// committed; an error means it can't tell (not a repository). nil
	// hides it.
	Uncommitted func(dir string) (int, error)
	// Roles and Providers are what the form offers; Defaults seed it.
	Roles     []Choice
	Providers []ProviderChoice
	Defaults  FormDefaults

	// Tips drives the first-run walkthrough; nil shows none. DisableTips
	// saves "no more tips" when the user presses ctrl+t on one.
	Tips        *tips.Book
	DisableTips func() error
	// Setup is the provider-setup screen; nil hides it.
	Setup *Setup
	// Voice reads the dashboard aloud (v); nil hides it.
	Voice *Voice
	// Copy puts text on the system clipboard; nil uses the platform's
	// tool (pbcopy, wl-copy, xclip, xsel, clip.exe).
	Copy func(string) error
	// RoleAdmin is the roles screen (R): add, edit, import and remove
	// roles; nil hides it.
	RoleAdmin *Roles
	// Settings is the configuration screen (C); nil hides it.
	Settings *Settings
}

// Run shows the dashboard until the user quits.
func Run(opt Options) error {
	m := newModel(opt)
	_, err := tea.NewProgram(m).Run()
	m.closed = true
	if m.speaker != nil {
		m.speaker.Close()
	}
	return err
}

// view is a tab of the task panel.
type view int

const (
	vOverview view = iota
	vActivity
	vFiles
	vMessages
	vAgent // one agent in detail (from the overview)
)

var tabNames = []string{"OVERVIEW", "ACTIVITY", "FILES", "MESSAGES"}

const (
	tickEvery = 120 * time.Millisecond
	pollTicks = 4 // re-read tasks every pollTicks ticks (~0.5s)
	// maxLoaded bounds how many non-running tasks get their logs read
	// before they are selected.
	maxLoaded = 30
)

type tickMsg time.Time

func tick() tea.Cmd {
	return tea.Tick(tickEvery, func(t time.Time) tea.Msg { return tickMsg(t) })
}

type model struct {
	replay bool // the walkthrough was asked for (t), so it starts at welcome
	opt    Options
	w, h   int

	tasks []*task.Task
	live  map[string]*Live
	sel   int // selected task
	agent int // selected agent; -1 = all (activity, messages)

	view      view
	focusMain bool
	scroll    int
	follow    bool
	evSel     int     // the activity tab's cursor, when not following
	open      *detail // the activity event opened with enter
	drag      *dragSel
	screen    string // the screen as last drawn, for copying from
	// copyButton is where the open panel's ⧉ copy button is.
	copyButton rect
	help       bool
	frame      int

	form     *form         // the new-task form, while open
	setup    *setupView    // provider setup, while open
	roles    *rolesView    // the roles screen, while open
	settings *settingsView // the configuration screen, while open
	confirm  string        // pending action: "stop" or "resume", asked in a dialog
	no       bool          // the dialog's NO button is selected
	runOn    string        // the resume dialog's provider; empty keeps each agent's
	// resuming is a resume waiting on a sign-in, retried when it ends.
	resuming *resumeReq
	flash    string
	flashAt  time.Time
	failed   string // the last action's error; shown until the next key
	err      error

	speaker      Speaker                 // the voice, while on
	entered      bool                    // while speaking: a task or tab was just reached
	answered     bool                    // a voice key was answered: speak says nothing more
	changes      map[string]*changeState // what git says of stopped tasks' workspaces
	voiceBusy    bool                    // the voice is starting
	voiceRestart bool                    // it is starting again with a new voice or rate
	closed       bool                    // the dashboard has quit
	news         []string                // changes to say at the next tick
}

func newModel(opt Options) *model {
	// The one place a missing State is filled: every dashboard is built here,
	// so no reader below has to fall back to the store on its own.
	opt.defaultState()
	m := &model{opt: opt, live: map[string]*Live{}, follow: true}
	m.poll()
	if opt.Tour {
		m.replayTour()
	}
	if opt.Focus != "" {
		for i, t := range m.tasks {
			if strings.HasPrefix(t.ID, opt.Focus) {
				m.sel = i
				m.focusMain = true
				break
			}
		}
	}
	return m
}

func (m *model) Init() tea.Cmd {
	if v := m.opt.Voice; v != nil && v.On {
		return tea.Batch(tick(), m.startVoice())
	}
	return tick()
}

// state is the backend task state is read from. An accessor, not a choice:
// newModel resolved it once (see defaultState), and no reader here re-decides
// between implementations.
func (o *Options) state() task.Backend { return o.State }

// defaultState fills State when a caller built Options without one - a test, or
// an embedder that has only a store. It is called once, from newModel, before
// anything reads - not from Run, because a test may build a dashboard through
// newModel alone - so the fill exists in one visible place instead of behind
// every read.
func (o *Options) defaultState() {
	if o.State == nil {
		o.State = o.Store
	}
}

// poll reloads task records and reads new events.
func (m *model) poll() {
	tasks, err := m.opt.state().List()
	if err != nil {
		m.err = err
		return
	}
	var before map[string]task.Status
	if m.speaker != nil {
		before = map[string]task.Status{}
		for _, t := range m.tasks {
			before[t.ID] = t.Status
		}
		defer func() { m.noteChanges(before) }()
	}
	selID := ""
	if cur := m.current(); cur != nil {
		selID = cur.Task.ID
	}
	m.tasks = tasks
	// The backend answered: a poll error from an earlier tick is stale, and a
	// banner that outlives its cause is worse than none (R63).
	m.err = nil
	m.sel = 0
	for i, t := range tasks {
		m.opt.state().Reconcile(t)
		if t.ID == selID {
			m.sel = i
		}
		l := m.live[t.ID]
		if l == nil {
			l = newLive(t)
			m.live[t.ID] = l
		} else {
			l.sync(t)
		}
		if t.Status == task.Running || i == m.sel || (l.off == 0 && i < maxLoaded) {
			m.read(l)
		}
	}
}

func (m *model) read(l *Live) {
	evs, off, err := m.opt.state().ReadEvents(l.Task.ID, l.off)
	if err != nil {
		return
	}
	l.off = off
	for _, e := range evs {
		l.Apply(e)
	}
	l.sync(l.Task) // the record wins over replayed history
	l.readLive(m.opt.state())
}

func (m *model) current() *Live {
	if m.sel < 0 || m.sel >= len(m.tasks) {
		return nil
	}
	return m.live[m.tasks[m.sel].ID]
}

func (m *model) say(s string) { m.flash, m.flashAt = s, time.Now() }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	before := spoken{flashAt: m.flashAt.UnixNano(), failed: m.failed, field: m.focusedField(), place: m.place()}
	res, cmd := m.update(msg)
	m.speak(msg, before)
	return res, cmd
}

func (m *model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case voiceMsg:
		m.voiceStarted(msg)
	case roleImportedMsg:
		m.roleImported(msg)
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.drag = nil
	case copiedMsg:
		m.copied(msg)
	case tea.MouseMotionMsg, tea.MouseReleaseMsg:
		cmd, _ := m.mouse(msg)
		return m, cmd
	case tickMsg:
		m.frame++
		if m.frame%pollTicks == 0 {
			m.poll()
		}
		m.maybeOfferSettings()
		return m, tea.Batch(tick(), m.maybeOfferSetup(), m.checkChanges())
	case changesMsg:
		m.changesChecked(msg)
	case setupStatusMsg:
		if m.setup != nil {
			m.setup.states, m.setup.loading = msg, false
			m.setup.idx = min(m.setup.idx, max(0, len(msg)-1))
		}
	case tea.KeyPressMsg:
		// A selection on screen: ctrl+c copies it (and doesn't quit);
		// any other key clears it.
		if d := m.drag; d != nil {
			m.drag = nil
			if d.done && msg.String() == "ctrl+c" {
				m.drag = d
				return m, m.copyDrag()
			}
		}
		// The voice's keys answer anywhere and change nothing, not even
		// a tip or the help panel.
		if m.voiceKey(msg.String()) {
			return m, nil
		}
		if _, typing := m.roles.typingRole(); typing {
			return m, m.rolesKey(msg.String())
		}
		if m.settingsTyping() {
			return m, m.settingsKey(msg.String())
		}
		if k := msg.String(); m.form == nil && m.confirm == "" && (k == "v" || k == "V") {
			if k == "V" {
				if m.speaker != nil {
					m.speaker.Repeat()
				}
				return m, nil
			}
			m.help = false
			return m, m.toggleVoice()
		}
		if m.roles != nil {
			return m, m.rolesKey(msg.String())
		}
		if m.settings != nil {
			return m, m.settingsKey(msg.String())
		}
		if m.setup != nil {
			return m, m.setupKey(msg.String())
		}
		if m.tipKey(msg.String()) {
			return m, nil
		}
		if m.form != nil && msg.String() != "ctrl+c" {
			cmd, done := m.form.key(msg.String())
			if done {
				m.form = nil
			}
			return m, cmd
		}
		return m.key(msg.String())
	case tea.PasteMsg:
		if m.settingsTyping() {
			m.settings.edit.insert(strings.TrimSpace(msg.Content))
		} else if m.roles != nil {
			m.rolePaste(msg.Content)
		} else if m.form != nil {
			m.form.paste(msg.Content)
		}
	case followUpMsg:
		m.form = nil
		return m, m.resume(msg.r)
	case launchedMsg:
		if m.form != nil && m.form.launched(msg) {
			m.form = nil
			m.poll()
			for i, t := range m.tasks {
				if t.ID == msg.id {
					m.sel, m.view, m.focusMain, m.scroll, m.follow = i, vOverview, true, 0, true
				}
			}
			m.say("launched task " + msg.id + " in the background · closing the dashboard never stops it")
		}
	case loginDoneMsg:
		if m.setup != nil {
			return m, m.setupLoggedIn(msg.err)
		}
		if m.form != nil {
			if msg.err != nil {
				m.form.err = "sign-in didn't finish (the reason was on the sign-in screen)"
			} else {
				m.form.err, m.form.login = "", nil
				return m, m.form.launch()
			}
		} else if r := m.resuming; r != nil {
			m.resuming = nil
			if msg.err != nil {
				m.failed = "couldn't start it: sign-in didn't finish (the reason was on the sign-in screen)"
			} else {
				return m, m.resume(r)
			}
		}
	case tea.MouseClickMsg:
		if cmd, used := m.mouse(msg); used {
			return m, cmd
		}
		// The NEW TASK buttons: atop the task list, or anywhere on the
		// empty welcome panel.
		if m.form == nil && m.confirm == "" && m.settings == nil && m.setup == nil && m.roles == nil && msg.Button == tea.MouseLeft {
			top := len(m.hero()) + 1
			onList := msg.X < min(36, max(28, m.w/4)) && msg.Y >= top && msg.Y <= top+1
			if onList || len(m.tasks) == 0 {
				m.openForm()
			}
		}
	case tea.MouseWheelMsg:
		m.drag = nil
		if msg.Button == tea.MouseWheelUp {
			m.scrollBy(-3)
		} else {
			m.scrollBy(3)
		}
	}
	return m, nil
}

func (m *model) key(k string) (tea.Model, tea.Cmd) {
	m.failed = ""
	if m.confirm != "" {
		act := m.confirm
		switch k {
		case "y", "Y":
			m.confirm = ""
			return m, m.act(act)
		case "enter", "space":
			m.confirm = ""
			if m.no {
				m.say("cancelled")
			} else {
				return m, m.act(act)
			}
		case "n", "N", "esc", "q":
			m.confirm = ""
			m.say("cancelled")
		case "left", "right", "tab", "shift+tab", "h", "l":
			m.no = !m.no
		case "up", "down", "k", "j", "p":
			if act == "resume" {
				m.cycleRunOn(k == "up" || k == "k")
			}
		case "ctrl+c":
			return m, tea.Quit
		}
		return m, nil
	}
	if m.help {
		m.help = false
		if k != "q" && k != "ctrl+c" {
			return m, nil
		}
	}
	if m.open != nil && k != "ctrl+c" {
		if k == "c" {
			return m, m.copySelected()
		}
		m.detailKey(k)
		return m, nil
	}
	l := m.current()
	switch k {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "n":
		m.openForm()
	case "p":
		return m, m.openSetup()
	case "R":
		m.openRoles()
	case "C":
		m.openSettings(false)
	case "t":
		m.replayTour()
	case "e":
		if l != nil {
			m.open = &detail{task: true}
		}
	case "a":
		m.openFollowUp()
	case "c", "y":
		return m, m.copySelected()
	case "?":
		m.help = true
	case "tab", "shift+tab":
		m.focusMain = !m.focusMain
	case "esc":
		switch {
		case m.view == vAgent:
			m.view = vOverview
		case m.focusMain:
			m.focusMain = false
		}
	case "1", "2", "3", "4":
		m.setView(view(k[0] - '1'))
	case "enter":
		if l == nil {
			m.openForm()
		} else if !m.focusMain {
			m.focusMain = true
		} else if m.view == vActivity {
			m.openDetail()
		} else if m.view == vOverview && m.agent < 0 {
			m.open = &detail{task: true} // the summary: the whole task
		} else if m.view == vOverview && l != nil && len(l.Agents) > 0 {
			m.view, m.scroll = vAgent, 0
		}
	case "up", "k":
		m.vertical(-1)
	case "down", "j":
		m.vertical(1)
	case "left", "h", "right", "l":
		// Keys move only within the focused pane: from the task list, the
		// agents are a tab (or enter) away.
		if m.focusMain {
			m.pickAgent(map[bool]int{true: -1, false: 1}[k == "left" || k == "h"])
		}
	case "[":
		m.pickTask(-1)
	case "]":
		m.pickTask(1)
	case "pgup", "ctrl+u":
		m.vertical(-m.pageSize())
	case "pgdown", "ctrl+d", "space":
		m.vertical(m.pageSize())
	case "home", "g":
		m.scroll, m.evSel, m.follow = 0, 0, false
	case "end", "G":
		m.follow = true
	case "f":
		m.follow = !m.follow
		m.say(map[bool]string{true: "following new activity", false: "follow paused"}[m.follow])
	case "s":
		switch {
		case l == nil || m.opt.Stop == nil:
		case l.Task.Status != task.Running:
			m.say("task " + l.Task.Title() + " is not running")
		default:
			m.confirm, m.no = "stop", false
		}
	case "x", "delete":
		switch {
		case l == nil || m.opt.Delete == nil:
		case l.Task.Status == task.Running:
			m.failed = "task " + l.Task.Title() + " is running: stop it first (s), then delete it"
		default:
			m.confirm, m.no = "delete", true // deleting can't be undone: default to cancel
		}
	case "r":
		switch {
		case l == nil || m.opt.Resume == nil:
		case !l.Task.Resumable():
			m.say("task " + l.Task.Title() + " is " + string(l.Task.Status) + "; nothing to resume")
		default:
			m.confirm, m.no, m.runOn = "resume", false, m.suggestRunOn(l.Task)
		}
	}
	return m, nil
}

func (m *model) openForm() {
	if m.opt.Launch != nil {
		m.form = newForm(&m.opt)
	}
}

func (m *model) setView(v view) {
	if v == m.view {
		return
	}
	m.view, m.scroll, m.follow = v, 0, true
	m.focusMain = true
	if v == vActivity || v == vMessages {
		m.agent = -1
	} else if m.agent < 0 {
		m.agent = 0
	}
}

// vertical moves the task selection, or within the main panel.
func (m *model) vertical(d int) {
	switch {
	case !m.focusMain:
		m.pickTask(d)
	case m.view == vOverview:
		m.pickAgent(max(-1, min(d, 1)))
	case m.view == vActivity:
		m.moveEvent(d)
	default:
		m.scrollBy(d)
	}
}

func (m *model) pickTask(d int) {
	if len(m.tasks) == 0 {
		return
	}
	m.sel = (m.sel + d + len(m.tasks)) % len(m.tasks)
	m.scroll, m.follow = 0, true
	if m.view == vAgent {
		m.view = vOverview
	}
	if m.agent >= 0 {
		m.agent = 0
	}
	if l := m.current(); l != nil {
		m.read(l)
	}
}

// pickAgent cycles the selected agent; activity and messages include
// "all" (-1).
func (m *model) pickAgent(d int) {
	l := m.current()
	if l == nil || len(l.Agents) == 0 {
		return
	}
	// -1 is "all" in activity and messages, and the task's summary (its
	// prompt and how it ended) on the overview.
	lo, n := 0, len(l.Agents)
	if m.view == vActivity || m.view == vMessages || m.view == vOverview {
		lo, n = -1, n+1
	}
	m.agent = (m.agent-lo+d+n)%n + lo
	m.scroll, m.follow = 0, true
}

func (m *model) scrollBy(d int) {
	m.scroll = max(0, m.scroll+d)
	if d < 0 {
		m.follow = false
	}
}

func (m *model) pageSize() int { return max(1, m.h-14) }

func (m *model) act(what string) tea.Cmd {
	l := m.current()
	if l == nil {
		return nil
	}
	var err error
	switch what {
	case "stop":
		err = m.opt.Stop(l.Task)
		if err == nil {
			m.say("stopping " + l.Task.Title() + "… it saves its state and can be resumed")
		}
	case "resume":
		return m.resume(&resumeReq{task: l.Task, provider: m.runOn})
	case "delete":
		err = m.opt.Delete(l.Task)
		if err == nil {
			delete(m.live, l.Task.ID)
			m.poll()
			m.sel = min(m.sel, max(0, len(m.tasks)-1))
			m.view, m.scroll, m.follow = vOverview, 0, true
			if len(m.tasks) == 0 {
				m.focusMain = false
			}
			m.say("deleted task " + l.Task.Title() + " · its workspace is untouched")
		}
	}
	if err != nil {
		m.flash, m.failed = "", what+" failed: "+err.Error()
	}
	return nil
}

// resumeReq is a resume the user confirmed.
type resumeReq struct {
	task     *task.Task
	provider string
	followUp string     // a follow-up's prompt, to start with that instead
	team     []NewAgent // with a follow-up: every agent's new settings; nil keeps them
}

// resume starts r's task again. When a provider needs a sign-in first,
// it hands the terminal to the sign-in and retries afterwards.
func (m *model) resume(r *resumeReq) tea.Cmd {
	var err error
	if r.followUp != "" {
		err = m.opt.FollowUp(r.task, r.followUp, r.team)
	} else {
		err = m.opt.Resume(r.task, r.provider)
	}
	var lr *LoginRequired
	switch {
	case err == nil && r.followUp != "":
		m.say("follow-up started: " + r.task.Title() + " runs again in the background")
		m.poll()
	case err == nil:
		on := ""
		if r.provider != "" {
			on = " on " + r.provider
		}
		m.say("resumed " + r.task.Title() + on + " in the background")
		m.poll()
	case errors.As(err, &lr) && m.opt.Login != nil:
		m.resuming = r
		return tea.ExecProcess(m.opt.Login(lr.Providers), func(err error) tea.Msg { return loginDoneMsg{err} })
	default:
		m.flash, m.failed = "", "resume failed: "+err.Error()
	}
	return nil
}

// runOnChoices are the resume dialog's providers: as before, then each
// provider on offer.
func (m *model) runOnChoices() []string {
	out := []string{""}
	for _, p := range m.opt.Providers {
		out = append(out, p.Name)
	}
	return out
}

func (m *model) cycleRunOn(back bool) {
	cs := m.runOnChoices()
	i := max(0, slices.Index(cs, m.runOn))
	if back {
		i += len(cs) - 1
	} else {
		i++
	}
	m.runOn = cs[i%len(cs)]
}

// suggestRunOn picks the resume dialog's provider: Claude Code when the
// task failed for want of API credits and Claude Code is on offer, else
// as before.
func (m *model) suggestRunOn(t *task.Task) string {
	if t.Status == task.Failed && strings.Contains(t.Reason, "out of API credits") && slices.Contains(m.runOnChoices(), "claude-code") {
		return "claude-code"
	}
	return ""
}

func (m *model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.BackgroundColor = color.RGBA{bg0.r, bg0.g, bg0.b, 255}
	v.WindowTitle = "ackn4th dashboard"
	return v
}

// render draws the screen, with any mouse selection over it.
func (m *model) render() string {
	m.copyButton = rect{}
	m.screen = m.draw()
	return m.highlight(m.screen)
}

func (m *model) draw() string {
	if m.w == 0 {
		return "loading…"
	}
	if m.w < 80 || m.h < 22 {
		return fmt.Sprintf("ackn4th dash needs at least 80×22 cells (this is %d×%d).\nEnlarge the window, or press q to quit.", m.w, m.h)
	}
	hero := m.hero()
	bodyH := m.h - len(hero) - 1
	leftW := min(36, max(28, m.w/4))
	lines := append(hero, strings.Split(joinColumns(m.taskList(leftW, bodyH), m.mainPanel(m.w-leftW, bodyH)), "\n")...)
	lines = append(lines, m.footer())
	screen := strings.Join(lines, "\n")
	switch {
	case m.settings != nil:
		screen = m.drawSettings(lines)
	case m.roles != nil:
		screen = m.drawRoles(lines)
	case m.setup != nil:
		screen = m.drawSetup(lines)
	case m.form != nil:
		screen = m.drawForm(lines)
	case m.confirm != "" && m.current() != nil:
		screen = m.drawConfirm(lines)
	case m.open != nil:
		block := m.detailBox(m.w, m.h-2)
		screen = overlay(screen, block, m.w)
		bh := strings.Count(block, "\n") + 1
		m.noteCopyButton(block, max(0, (m.w-width(strings.SplitN(block, "\n", 2)[0]))/2), max(0, (m.h-bh)/2))
	case m.help:
		screen = overlay(screen, helpBox(m.w, m.h, m.opt.Voice != nil), m.w)
	}
	return m.drawTip(screen)
}
