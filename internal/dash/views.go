package dash

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/dethmasque/ackn4th/internal/event"
	"github.com/dethmasque/ackn4th/internal/spend"
	"github.com/dethmasque/ackn4th/internal/task"
)

var (
	spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	pulse   = []string{"◐", "◓", "◑", "◒"}
)

const tagline = "back and forth, forever"

// f is the animation frame, frozen when effects are off.
func (m *model) f() int {
	if !m.opt.Effects {
		return 0
	}
	return m.frame
}

func (m *model) spin(set []string) string { return set[m.f()%len(set)] }

// heroRows is the header's height: a taller pixel scene when there is room.
func (m *model) heroRows() int {
	if m.h >= 34 {
		return 4
	}
	return 3
}

// hero is the header: the pixel wordmark over digital rain, two agents
// volleying, and the task counts.
func (m *model) hero() []string {
	rows := m.heroRows()
	info := m.heroInfo(rows)
	iw := 0
	for _, l := range info {
		iw = max(iw, width(l))
	}
	iw += 2
	pw := m.w - iw
	c := newCanvas(pw, rows)
	mx, my := 2, (c.h-5)/2
	ww := wordmarkWidth()
	pongW := min(40, pw-ww-8)
	px0 := pw - pongW - 1
	c.rain(m.f(), bg0, func(x, y int) float64 {
		if x >= mx-2 && x <= mx+ww+2 || pongW >= 14 && x >= px0-2 {
			return 0 // clear plates behind the wordmark and the court
		}
		return 0.8
	})
	c.text(wordmark, mx, my, white, green, hex("#0c3a1a"))
	if pongW >= 14 {
		left, right := amber, cyan
		if l := m.current(); l != nil && len(l.Task.Agents) > 1 {
			left = providerColor(l.Task.Agents[0].Provider)
			right = providerColor(l.Task.Agents[len(l.Task.Agents)-1].Provider)
		}
		c.pong(px0, pongW, m.f(), left, right)
	}
	px := c.rows(bg0)
	out := make([]string, rows)
	for i := range out {
		l := ""
		if i < len(info) {
			l = info[i]
		}
		out[i] = px[i] + fill(" "+l, iw, bg0)
	}
	return out
}

func (m *model) heroInfo(rows int) []string {
	counts := map[task.Status]int{}
	for _, t := range m.tasks {
		counts[t.Status]++
	}
	var parts []string
	for _, st := range []task.Status{task.Running, task.Stopped, task.Failed, task.Completed} {
		if counts[st] > 0 {
			parts = append(parts, statusGlyph(st, m)+" "+pText(fmt.Sprint(counts[st]))+" "+pDim(string(st)))
		}
	}
	if len(parts) == 0 {
		parts = []string{pDim("no tasks yet")}
	}
	dot := pGreen("●")
	if m.f()%8 >= 4 {
		dot = pMoss("●")
	}
	lines := []string{
		strings.Join(parts, "  "),
		pFaint(")) <==> ((  ") + pDim(tagline),
		dot + pDim(" live  "+time.Now().Format("15:04:05")),
	}
	if rows >= 4 {
		lines = append([]string{bold(pLime("DASHBOARD")) + pDim(" · ackn4th")}, lines...)
	}
	return lines
}

func statusGlyph(st task.Status, m *model) string {
	switch st {
	case task.Running:
		return pGreen(m.spin(pulse))
	case task.Stopped:
		return pAmber("■")
	case task.Failed:
		return pRed("✗")
	case task.Completed:
		return pLime("✓")
	}
	return pDim("○")
}

func statusPill(st task.Status) string {
	switch st {
	case task.Running:
		return pill("RUNNING", green, bg0)
	case task.Stopped:
		return pill("STOPPED", amber, bg0)
	case task.Failed:
		return pill("FAILED", red, bg0)
	case task.Completed:
		return pill("DONE", lime, bg0)
	}
	return pill("PENDING", faint, text)
}

// footer: key chips, a confirmation, or a recent message.
func (m *model) footer() string {
	chip := func(k, label string) string { return pill(k, bg3, lime) + pDim(" "+label+"  ") }
	switch {
	case m.confirm != "":
		return fill(" "+chip("y ⏎", "yes")+chip("n esc", "no")+chip("←→", "choose"), m.w, bg0)
	case m.failed != "":
		return fill(" "+pill("✗", red, bg0)+" "+pRed(oneLine(m.failed))+pDim("  · any key dismisses"), m.w, bg0)
	case m.err != nil:
		// The backend could not be read - usually a controller that is not
		// answering. Say it on the first frame instead of showing an empty
		// dashboard as if this host had no tasks (R67); it clears on the first
		// poll that answers.
		return fill(" "+pill("!", amber, bg0)+" "+pAmber(oneLine(m.err.Error()))+pDim("  · retrying"), m.w, bg0)
	case m.flash != "" && time.Since(m.flashAt) < 4*time.Second:
		return fill(" "+pAmber("▸ ")+pText(m.flash), m.w, bg0)
	}
	keys := [][2]string{{"↑↓", "move"}, {"tab", "panel"}, {"←→", "agent"}, {"⏎", "zoom"}, {"e", "expand"}, {"c", "copy"}, {"1-4", "tabs"}, {"[ ]", "task"}, {"f", "follow"}, {"s", "stop"}, {"r", "resume"}, {"a", "follow-up"}, {"x", "delete"}, {"n", "new"}, {"p", "providers"}, {"R", "roles"}, {"C", "config"}, {"t", "tour"}, {"?", "help"}, {"q", "quit"}}
	switch {
	case m.open != nil && m.open.task:
		keys = [][2]string{{"↑↓", "scroll"}, {"c", "copy it all"}, {"esc", "close"}}
	case m.open != nil:
		keys = [][2]string{{"↑↓", "scroll"}, {"←→", "previous / next event"}, {"c", "copy it all"}, {"esc", "close"}}
	case m.focusMain && m.view == vActivity:
		keys[0], keys[3] = [2]string{"↑↓", "select"}, [2]string{"⏎", "details"}
	case !m.focusMain:
		// The task list: ←→ and zooming belong to the task's pane.
		keys = append([][2]string{{"↑↓", "task"}, {"⏎ tab", "open it"}}, keys[4:]...)
	}
	if m.opt.Voice != nil && m.open == nil {
		keys = append(keys[:len(keys)-2:len(keys)-2], [2]string{"v", "voice"}, keys[len(keys)-2], keys[len(keys)-1])
	}
	var b strings.Builder
	b.WriteString(" ")
	for _, k := range keys {
		b.WriteString(chip(k[0], k[1]))
	}
	return fill(b.String(), m.w, bg0)
}

// taskList is the left column: one card per task, the selected one lit.
func (m *model) taskList(w, h int) string {
	var lines []string
	top := 0 // rows above the task cards
	if m.opt.Launch != nil {
		// Always one key or click away: a new task, whatever is selected.
		lines = append(lines, " "+button("+ NEW TASK", false, bg1, true)+pDim("  n"), " "+buttonShadow("+ NEW TASK", bg1), "")
		top = len(lines)
	}
	if len(m.tasks) == 0 {
		lines = append(lines, pDim(" no tasks yet"))
	}
	const per = 4
	visible := max(1, (h-2-top)/per)
	first := 0
	if m.sel >= visible {
		first = m.sel - visible + 1
	}
	inner := w - 3
	for i := first; i < len(m.tasks) && i < first+visible; i++ {
		t := m.tasks[i]
		l := m.live[t.ID]
		surface, accent := bg1, bg1
		id := bold(pText(t.Title()))
		if i == m.sel {
			surface, accent, id = bg3, lime, bold(pWhite(t.Title()))
			if m.focusMain {
				accent = moss
			}
		}
		round := fmt.Sprintf("r%d", t.Round)
		if t.MaxRounds > 0 {
			round = fmt.Sprintf("r%d/%d", min(t.Round, t.MaxRounds), t.MaxRounds)
		}
		head := fit(statusGlyph(t.Status, m)+" "+id, inner-width(round)-2) + pDim(round)
		var team string
		for _, ag := range t.Agents {
			team += fgp(providerColor(ag.Provider))("●")
		}
		meta := team
		if l != nil && t.Status == task.Running && l.Working() > 0 {
			meta += pGreen(fmt.Sprintf("  %s %d working", m.spin(spinner), l.Working()))
		} else {
			meta += pDim("  " + ago(t.UpdatedAt))
		}
		if l != nil && l.Votes() > 0 {
			meta += pLime(fmt.Sprintf("  ◆%d/%d", l.Votes(), len(t.Agents)))
		}
		for _, row := range []string{head, pFaint(t.ID+" ") + pDim(oneLine(t.Prompt)), meta} {
			lines = append(lines, fill(fgp(accent)("▌"), 1, surface)+fill(" "+row, inner, surface))
		}
		lines = append(lines, "")
	}
	right := ""
	if len(m.tasks) > visible {
		right = pDim(fmt.Sprintf("%d/%d", m.sel+1, len(m.tasks)))
	}
	// A legend and totals at the foot, when there's room.
	var agents int
	var in, cached int64
	for _, t := range m.tasks {
		u := t.Usage()
		agents += len(t.Agents)
		in += u.InputTokens
		cached += u.CachedTokens
	}
	foot := []string{pFaint(" " + strings.Repeat("─", inner))}
	foot = append(foot, m.legend(inner)...)
	foot = append(foot,
		" "+pDim(fmt.Sprintf("%s · %s", plural(len(m.tasks), "task"), plural(agents, "agent"))),
		" "+pDim(tokens(in)+" tokens in"))
	if in > 0 && cached > 0 {
		foot[len(foot)-1] += pLime(fmt.Sprintf(" · ↯%d%% cached", cached*100/in))
	}
	if room := h - 2 - len(lines); room > len(foot) {
		for i := 0; i < room-len(foot); i++ {
			lines = append(lines, "")
		}
		lines = append(lines, foot...)
	}
	return panel("TASKS", right, lines, w, h, !m.focusMain, bg1)
}

// legend names each provider's color, in lines at most w cells wide: every provider
// ackn4th supports, then any other a task's agents use.
func (m *model) legend(w int) []string {
	var names []string
	for _, p := range m.opt.Providers {
		names = append(names, p.Name)
	}
	for _, t := range m.tasks {
		for _, ag := range t.Agents {
			if !slices.Contains(names, ag.Provider) {
				names = append(names, ag.Provider)
			}
		}
	}
	var lines []string
	line, n := " ", 1
	for _, name := range names {
		item := fgp(providerColor(name))("●") + pDim(" "+name)
		iw := 2 + len([]rune(name)) // "● name"
		if n > 1 && n+2+iw > w {
			lines = append(lines, line)
			line, n = " ", 1
		}
		if n > 1 {
			line, n = line+"  ", n+2
		}
		line, n = line+item, n+iw
	}
	if n > 1 {
		lines = append(lines, line)
	}
	return lines
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// mainPanel is the selected task: tabs, then the current view.
func (m *model) mainPanel(w, h int) string {
	l := m.current()
	if l == nil {
		return m.emptyPanel(w, h)
	}
	inner, rows := w-3, h-2
	var tabs strings.Builder
	for i, name := range tabNames {
		label := fmt.Sprintf("%d %s", i+1, name)
		if view(i) == m.view || (m.view == vAgent && i == 0) {
			tabs.WriteString(pill(label, green, bg0))
		} else {
			tabs.WriteString(pill(label, bg2, dim))
		}
		tabs.WriteString(" ")
	}
	body := []string{tabs.String(), pFaint(strings.Repeat("─", inner))}
	switch m.view {
	case vOverview:
		body = append(body, m.overview(l, inner, rows-2)...)
	case vAgent:
		body = append(body, m.scrolled(m.agentDetail(l, inner), rows-2)...)
	case vActivity:
		body = append(body, m.activity(l, inner, rows-2)...)
	case vFiles:
		body = append(body, m.scrolled(m.files(l, inner), rows-2)...)
	case vMessages:
		body = append(body, m.messages(l, inner, rows-2)...)
	}
	for i := range body {
		body[i] = " " + body[i]
	}
	return panel(strings.ToUpper(l.Task.Title())+" · "+l.Task.ID, statusPill(l.Task.Status), body, w, h, m.focusMain, bg1)
}

// emptyPanel greets a fresh install with the volley scene.
func (m *model) emptyPanel(w, h int) string {
	c := newCanvas(w-4, 5)
	c.rain(m.f(), bg1, func(int, int) float64 { return 0.3 })
	c.pong(2, w-8, m.f(), amber, cyan)
	lines := []string{""}
	lines = append(lines, c.rows(bg1)...)
	lines = append(lines, "", bold(pText("Nothing here yet. Put a team of agents to work:")), "")
	if m.opt.Launch != nil {
		btn := button("+ NEW TASK", true, bg1, true)
		lines = append(lines, btn, buttonShadow("+ NEW TASK", bg1), "",
			pDim("press ")+pill("n", bg3, lime)+pDim(" or ")+pill("⏎", bg3, lime)+pDim(", or click: pick agents, models and efforts, write a prompt"), "")
	}
	lines = append(lines, pFaint("or from the command line:"),
		pMoss(`ackn4th start-task -d --parallel --roles backend_dev,qa_engineer "…"`),
		pMoss(`ackn4th run-agent -d --role tech_writer "…"`))
	for i := range lines {
		lines[i] = " " + lines[i]
	}
	return panel("WELCOME", "", lines, w, h, true, bg1)
}

// scrolled shows a window of lines, honoring scroll and follow.
func (m *model) scrolled(lines []string, h int) []string {
	maxOff := max(0, len(lines)-h)
	if m.follow && (m.view == vActivity || m.view == vMessages) {
		m.scroll = maxOff
	}
	m.scroll = min(m.scroll, maxOff)
	return lines[m.scroll:min(len(lines), m.scroll+h)]
}

// overview: the summary, the workflow, agent cards and recent activity.
func (m *model) overview(l *Live, w, h int) []string {
	t := l.Task
	// The summary (prompt, and how the task ended) is cut to fit. Like an
	// agent card it can be selected (↑ from the first agent), and enter
	// then shows all of it.
	picked := m.focusMain && m.agent < 0
	mark := func(lines []string) []string {
		if !picked {
			return lines
		}
		for i := range lines {
			lines[i] = fill(lines[i], w-1, bg3)
		}
		return lines
	}
	var out []string
	lines := wrap(t.Prompt, w-1)
	cut := len(lines) > 2
	if cut {
		lines = lines[:2]
	}
	var prompt []string
	for i, l := range lines {
		if i < len(lines)-1 {
			prompt = append(prompt, bold(pWhite(l)))
			continue
		}
		more, hint := "", ""
		if cut {
			more = pDim(" …")
		}
		if picked {
			hint = "  " + pLime("⏎ shows it all")
		}
		l = ansi.Truncate(l, w-1-width(more)-width(hint), "")
		prompt = append(prompt, bold(pWhite(l))+more+hint)
	}
	out = append(out, mark(prompt)...)
	if n := len(t.FollowUps); n > 0 {
		f := t.FollowUps[n-1]
		head := fmt.Sprintf("↪ follow-up %d, round %d: ", n, f.Round)
		out = append(out, mark([]string{fgp(cyan)(head) + pText(ansi.Truncate(oneLine(f.Prompt), w-1-width(head), "…"))})...)
	}
	u := t.Usage()
	stats := pill("TURNS", bg2, lime)
	if t.Parallel() {
		stats = pill("PARALLEL", bg2, cyan)
	}
	if t.MaxRounds > 0 {
		r := min(t.Round, t.MaxRounds)
		stats += " " + pText(fmt.Sprintf("round %d/%d ", r, t.MaxRounds)) + bar(float64(r)/float64(t.MaxRounds), 12, bg2)
	} else {
		stats += " " + pText(fmt.Sprintf("round %d", t.Round)) + pDim(" · until consensus")
	}
	stats += pDim("   " + tokens(u.InputTokens) + " in · " + tokens(u.OutputTokens) + " out")
	if u.CachedTokens > 0 && u.InputTokens > 0 {
		stats += pLime(fmt.Sprintf(" · ↯%d%% cached", u.CachedTokens*100/u.InputTokens))
	}
	stats += pDim(costLabel(t.Cost(), t.MaxCost))
	out = append(out, "", stats)
	// Where the work is: with --repo it's a clone, not the directory the
	// task was started from.
	where := pDim("in ") + pLime(tildePath(t.Workspace))
	if t.Branch != "" {
		where += pDim(" on branch ") + pText(t.Branch)
	}
	out = append(out, where+pDim("  · updated "+ago(t.UpdatedAt)))
	if t.Reason != "" && t.Status != task.Running {
		// Up to three lines: provider errors carry the fix at the end.
		col := pAmber
		if t.Status == task.Failed {
			col = pRed
		}
		reason := wrap(oneLine(t.Reason), w-2)
		if len(reason) > 3 {
			reason = append(reason[:2], ansi.Truncate(reason[2], w-6, "")+" …")
		}
		for i, r := range markdown(reason, col) {
			lead := "  "
			if i == 0 {
				lead = "▸ "
			}
			reason[i] = col(lead) + r
		}
		out = append(out, mark(reason)...)
	}
	out = append(out, m.changesNote(l, w-1)...)
	if (t.Status == task.Failed || t.Status == task.Stopped) && m.opt.Resume != nil {
		out = append(out, pDim("  press ")+pill("r", bg3, lime)+pDim(" to resume where it left off"))
	}
	out = append(out, "")
	out = append(out, m.workflow(l, w)...)
	out = append(out, "")

	// Agent cards in a grid.
	cols := max(1, min(len(l.Agents), w/34))
	cw := w / cols
	for i := 0; i < len(l.Agents); i += cols {
		var row []string
		for j := i; j < i+cols; j++ {
			if j < len(l.Agents) {
				row = append(row, m.card(l, j, cw))
			} else {
				row = append(row, strings.TrimSuffix(strings.Repeat(fill("", cw, bg1)+"\n", cardRows), "\n"))
			}
		}
		out = append(out, strings.Split(joinColumns(row...), "\n")...)
	}

	// What the agents need from the operator.
	if open := t.OpenTodos(); len(open) > 0 {
		out = append(out, "", bold(pAmber(fmt.Sprintf("FOR YOU · %d open", len(open))))+pDim("  ·  what the agents need from you"))
		for j, td := range open {
			if j == 6 {
				out = append(out, pDim(fmt.Sprintf("  … %d more", len(open)-j)))
				break
			}
			by := "  " + td.By
			out = append(out, pAmber(fmt.Sprintf("  ☐ #%-3d", td.ID))+pText(fit(oneLine(td.Text), max(10, w-10-width(by))))+pDim(by))
		}
	}

	// Whatever height is left shows the latest activity.
	if left := h - len(out) - 2; left > 2 {
		out = append(out, "", bold(pLime("LATEST"))+pDim("  ·  2 for the full log, where ⏎ opens any line"))
		evs := filterEvents(l, -1)
		for _, e := range evs[max(0, len(evs)-left):] {
			out = append(out, m.eventLine(l, e))
		}
	}
	return out
}

// workflow draws the team as a pipeline: the baton passing back and forth,
// or parallel lanes.
func (m *model) workflow(l *Live, w int) []string {
	t := l.Task
	next := l.Next()
	var chips []string
	for i, spec := range t.Agents {
		a := l.Agents[i]
		pc := providerColor(spec.Provider)
		icon := "·"
		switch {
		case a.Phase != Idle:
			icon = m.spin(spinner)
		case a.Voted:
			icon = "◆"
		case i == next:
			icon = "▸"
		}
		chipBg, chipFg := mix(bg1, pc, 0.18), pc
		switch {
		case i == m.agent && (m.view == vOverview || m.view == vAgent):
			chipBg, chipFg = pc, bg0
		case a.Phase != Idle && m.f()%6 < 3:
			chipBg = mix(bg1, pc, 0.34) // working agents breathe
		}
		chips = append(chips, pill(fmt.Sprintf("%s %d %s", icon, i+1, spec.Role), chipBg, chipFg))
	}
	sep, lead := pMoss(" ━━▶ "), pDim("FLOW  ")
	if t.Parallel() {
		sep, lead = pMoss("  ┃  "), pDim("LANES ")
	}
	var lines []string
	line := lead
	for i, c := range chips {
		piece := c
		if i > 0 {
			piece = sep + c
		}
		if width(line)+width(piece) > w-3 && line != lead {
			lines = append(lines, line)
			line = "      " + c
			continue
		}
		line += piece
	}
	loop := !t.Parallel() && len(chips) > 1
	if loop {
		line += pMoss(" ━╮")
	}
	lines = append(lines, line)
	if loop {
		lines = append(lines, "      "+pMoss("╰"+strings.Repeat("━", max(0, min(w-10, width(line)-8)))+"╯")+pDim(" back and forth"))
	}
	votes := ""
	for i := range t.Agents {
		if l.Agents[i].Voted {
			votes += pLime("◆")
		} else {
			votes += pFaint("◇")
		}
	}
	n := len(t.Agents)
	rule := fmt.Sprintf("  %d/%d declared done this round · the task completes when all %d do in one round", l.Votes(), n, n)
	if !t.Parallel() {
		rule = fmt.Sprintf("  %d/%d declared done · the task completes when all %d do, one after another", l.Votes(), n, n)
	}
	if n == 1 {
		rule = "  the task completes when the agent declares done"
	}
	tally := votes + pDim(rule)
	if t.SplitRounds >= 2 {
		tally += pAmber(fmt.Sprintf(" · split for %d rounds", t.SplitRounds))
	}
	return append(lines, tally)
}

const cardRows = 8

// card is one agent's live status, cw cells wide and cardRows tall.
func (m *model) card(l *Live, i, cw int) string {
	spec, a := l.Task.Agents[i], l.Agents[i]
	pc := providerColor(spec.Provider)
	selected := i == m.agent && m.focusMain
	surface := bg2
	if selected {
		surface = mix(bg2, pc, 0.10)
	}
	inner := cw - 3
	head := fit(pill(fmt.Sprintf("%d %s", i+1, strings.ToUpper(spec.Role)), mix(bg2, pc, 0.35), pc), inner-width(spec.Provider)-1) + fgp(pc)(spec.Provider)
	model := pDim(spec.Model)
	if spec.Effort != "" {
		model += pFaint(" · ") + pDim(spec.Effort)
	}
	var phase, activity string
	switch {
	case a.Phase != Idle:
		phase = pill(m.spin(spinner)+" "+strings.ToUpper(a.Phase.String()), green, bg0) + pDim(fmt.Sprintf("  %s · step %d", short(time.Since(a.Since)), a.Steps))
		activity = pText(a.Activity)
		if s := a.LiveSummary(); s != "" {
			activity = fgp(pc)(s)
		}
	case l.Task.Parallel() && l.Task.Status == task.Running && spec.RoundDone >= l.Task.Round:
		// In parallel, everyone finishes the round, then waits for the
		// rest: that is what this agent is doing, whether or not it also
		// voted the whole task done.
		phase, activity = pill("✓ ROUND DONE", bg3, lime), pDim("waiting for teammates")
		if a.Voted {
			activity = pLime("◆ voted the task done") + pDim(" · waiting for teammates")
		}
	case a.Voted:
		phase, activity = pill("◆ VOTED DONE", lime, bg0), pDim("says the whole task is done")
	case i == l.Next():
		phase, activity = pill("▸ UP NEXT", amber, bg0), pDim("waiting for the handoff")
	default:
		phase, activity = pill("○ IDLE", bg3, dim), pDim(fmt.Sprintf("%d turns so far", spec.Turns))
	}
	ctx := pDim("ctx ")
	if spec.CompactAt > 0 {
		ctx += bar(float64(a.Context)/float64(spec.CompactAt), max(6, inner-18), bg1) + pDim(" "+tokens(a.Context)+"/"+tokens(spec.CompactAt))
	} else {
		ctx += pDim(tokens(a.Context))
	}
	cache := pFaint("↯ —")
	if u := spec.Usage; u.CachedTokens > 0 && u.InputTokens > 0 {
		cache = pLime(fmt.Sprintf("↯%d%%", u.CachedTokens*100/u.InputTokens))
	}
	tail := fit(cache, 7) + spark(a.Rate(time.Now(), max(4, inner-14), 30*time.Second), pc) + pDim(" tools")

	border := fgp(mix(edge, pc, 0.35))
	if selected {
		border = fgp(pc)
	}
	var b strings.Builder
	b.WriteString(fill(border("╭"+strings.Repeat("─", cw-2)+"╮"), cw, bg1) + "\n")
	for _, line := range []string{head, model, phase, activity, ctx, tail} {
		b.WriteString(fill(border("│"), 1, bg1) + fill(" "+line, cw-2, surface) + fill(border("│"), 1, bg1) + "\n")
	}
	b.WriteString(fill(border("╰"+strings.Repeat("─", cw-2)+"╯"), cw, bg1))
	return b.String()
}

func short(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return short(time.Since(t)) + " ago"
}

// filterEvents lists the events worth showing, for one agent (0-based) or
// all (-1).
func filterEvents(l *Live, agent int) []event.Event {
	var out []event.Event
	for _, i := range eventIdx(l, agent) {
		out = append(out, l.Events[i])
	}
	return out
}

// eventLine renders one event on a line, like the CLI's live output.
func (m *model) eventLine(l *Live, e event.Event) string {
	lead, what := m.eventParts(l, e)
	return lead + what
}

// eventLines renders one event in at most n lines of w cells: what
// happened wraps under itself, after the time and who.
func (m *model) eventLines(l *Live, e event.Event, w, n int) []string {
	lead, what := m.eventParts(l, e)
	lw := width(lead)
	if w-lw < 16 {
		return []string{lead + what}
	}
	lines := strings.Split(ansi.Wrap(what, w-lw, ""), "\n")
	if len(lines) > n {
		lines = append(lines[:n-1:n-1], ansi.Truncate(lines[n-1], w-lw-1, "")+pDim("…"))
	}
	pad := strings.Repeat(" ", lw)
	for i := range lines {
		if i == 0 {
			lines[i] = lead + lines[i]
		} else {
			lines[i] = pad + lines[i]
		}
	}
	return lines
}

// eventParts is an event's time and who, and what happened.
func (m *model) eventParts(l *Live, e event.Event) (lead, what string) {
	who := pDim(fit("ackn4th", 14))
	if e.Agent > 0 && e.Agent <= len(l.Task.Agents) {
		spec := l.Task.Agents[e.Agent-1]
		who = fgp(providerColor(spec.Provider))(fit(spec.Role, 14))
	}
	switch e.Kind {
	case event.TaskStarted:
		what = pGreen("■ task " + e.Text)
	case event.FollowUp:
		what = bold(fgp(cyan)("↪ follow-up ")) + pText(oneLine(e.Text))
	case event.RoundStarted:
		what = bold(pGreen(fmt.Sprintf("━━━ ROUND %d ━━━", e.Round)))
	case event.TurnStarted:
		what = pGreen("▶ turn started")
	case event.Context:
		what = pDim("⟲ catch-up · " + e.Text)
	case event.ToolCall:
		what = pMoss("├─ ") + pText(fmt.Sprintf("%-15s ", e.Tool)) + pDim(e.Text)
	case event.ToolResult:
		what = pRed("│  ✗ " + e.Text)
	case event.Message:
		what = pLime("✉ ") + mdLine(oneLine(e.Text), pText)
	case event.Reasoning:
		what = fgp(violet)("∴ ") + mdLine(oneLine(e.Text), pDim)
	case event.DoneVote:
		what = bold(pLime("◆ declared done ")) + mdLine(oneLine(e.Text), pDim)
	case event.TurnEnded:
		what = pMoss("╰─ ") + pDim(fmt.Sprintf("turn ended · %s in / %s out", tokens(e.InTok), tokens(e.OutTok)))
		if e.CacheTok > 0 && e.InTok > 0 {
			what += pLime(fmt.Sprintf(" · ↯%d%%", e.CacheTok*100/e.InTok))
		}
	case event.Compacted:
		what = pAmber("⧉ context compacted")
	case event.Notice:
		what = pAmber("! " + e.Text)
	case event.TaskStopped:
		what = pAmber("■ stopped · " + e.Text)
	case event.TaskCompleted:
		what = bold(pLime("✓ completed"))
	case event.TaskFailed:
		what = pRed("✗ failed · " + e.Text)
	default:
		what = pDim(string(e.Kind) + " " + e.Text)
	}
	return pFaint(e.Time.Local().Format("15:04:05")) + " " + pDim(fmt.Sprintf("r%-2d", e.Round)) + " " + who + " ", what
}

// filterBar shows which agent the log is filtered to.
func (m *model) filterBar(l *Live) string {
	var b strings.Builder
	b.WriteString(pDim("←→ "))
	for i := -1; i < len(l.Task.Agents); i++ {
		name, c := "all", lime
		if i >= 0 {
			name, c = l.Task.Agents[i].Role, providerColor(l.Task.Agents[i].Provider)
		}
		if i == m.agent {
			b.WriteString(pill(name, c, bg0) + " ")
		} else {
			b.WriteString(pill(name, bg2, dim) + " ")
		}
	}
	if m.follow {
		b.WriteString("  " + pGreen("● following"))
	} else {
		b.WriteString("  " + pDim("○ paused · f"))
	}
	if m.focusMain && m.view == vActivity {
		b.WriteString(pDim("  ⏎ details · c copies"))
	}
	return b.String()
}

func (m *model) activity(l *Live, w, h int) []string {
	evs := filterEvents(l, m.agent)
	if len(evs) == 0 {
		return []string{m.filterBar(l), "", pDim("no activity yet")}
	}
	// Each event wraps onto up to three lines; enter shows the rest.
	rows, sel := max(1, h-2), m.selected(len(evs))
	memo := map[int][]string{}
	lines := func(i int) []string {
		if ls, ok := memo[i]; ok {
			return ls
		}
		ls := m.eventLines(l, evs[i], w-1, 3)
		memo[i] = ls
		return ls
	}
	// m.scroll is the first event shown. Keep the cursor's event whole on
	// screen, and the screen full when the log's end is in view.
	sum := func(from, to int) int { // lines of events from..to
		n := 0
		for i := from; i <= to && n <= rows; i++ {
			n += len(lines(i))
		}
		return n
	}
	// Events more than a screen above the cursor can't show: start there.
	m.scroll = min(max(m.scroll, sel-rows), sel)
	for m.scroll < sel && sum(m.scroll, sel) > rows {
		m.scroll++
	}
	for m.scroll > 0 && sum(m.scroll-1, len(evs)-1) <= rows {
		m.scroll--
	}
	var out []string
	for i := m.scroll; i < len(evs) && len(out) < rows; i++ {
		for _, line := range lines(i) {
			if i == sel && m.focusMain {
				line = fill(line, w-1, bg3)
			}
			out = append(out, line)
		}
	}
	if len(out) > rows {
		out = out[:rows]
	}
	return append([]string{m.filterBar(l), ""}, out...)
}

func (m *model) messages(l *Live, w, h int) []string {
	var lines []string
	for _, e := range l.Events {
		if e.Kind != event.Message || (m.agent >= 0 && e.Agent != m.agent+1) || e.Agent < 1 || e.Agent > len(l.Task.Agents) {
			continue
		}
		spec := l.Task.Agents[e.Agent-1]
		pc := providerColor(spec.Provider)
		lines = append(lines, pill(fmt.Sprintf("ROUND %d", e.Round), bg2, dim)+" "+pill(spec.Role, mix(bg1, pc, 0.25), pc)+pFaint("  "+e.Time.Local().Format("Jan 2 15:04")))
		for _, line := range markdown(wrap(clean(e.Text), w-4), pText) {
			lines = append(lines, fgp(mix(bg1, pc, 0.6))("  ┃ ")+line)
		}
		lines = append(lines, "")
	}
	if len(lines) == 0 {
		lines = []string{pDim("no handoff messages yet")}
	}
	return append([]string{m.filterBar(l), ""}, m.scrolled(lines, h-2)...)
}

func (m *model) files(l *Live, w int) []string {
	list := make([]*File, 0, len(l.Files))
	for _, f := range l.Files {
		list = append(list, f)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Last.After(list[j].Last) })
	if len(list) == 0 {
		return []string{pDim("no files changed by agents yet")}
	}
	pw := max(20, w-46)
	head := func(s string, n int) string { return pill(fit(s, n-2), bg2, dim) }
	out := []string{head("PATH", pw) + " " + head("CHANGES", 10) + head("BY", 24) + head("WHEN", 10), ""}
	for _, f := range list {
		var by []string
		for _, r := range f.By {
			by = append(by, fgp(roleColor(l, r))(r))
		}
		dir, base := filepath.Split(f.Path)
		ops := ""
		for _, op := range lastN(f.Ops, 8) {
			switch op {
			case 'W':
				ops += pGreen("█")
			case 'E':
				ops += pLime("▆")
			case 'D':
				ops += pRed("▁")
			default:
				ops += pDim(string(op))
			}
		}
		out = append(out, fit(" "+pDim(dir)+bold(pWhite(base)), pw)+" "+fit(" "+ops, 10)+fit(" "+strings.Join(by, " "), 24)+pDim(" "+ago(f.Last)))
	}
	return append(out, "", pGreen("█")+pDim(" write  ")+pLime("▆")+pDim(" edit  ")+pRed("▁")+pDim(" delete  · oldest first"))
}

func lastN(s string, n int) string {
	if len(s) > n {
		return "…" + s[len(s)-n+1:]
	}
	return s
}

func roleColor(l *Live, role string) rgb {
	for _, spec := range l.Task.Agents {
		if spec.Role == role {
			return providerColor(spec.Provider)
		}
	}
	return dim
}

// agentDetail is one agent zoomed in.
func (m *model) agentDetail(l *Live, w int) []string {
	i := max(0, min(m.agent, len(l.Agents)-1))
	spec, a := l.Task.Agents[i], l.Agents[i]
	pc := providerColor(spec.Provider)
	kv := func(k, v string) string { return pDim(fit(k, 11)) + v }
	out := []string{
		pill(fmt.Sprintf("AGENT %d", i+1), pc, bg0) + " " + bold(fgp(pc)(strings.ToUpper(spec.Role))) + pDim("    ←→ other agents · esc back"),
		"",
		kv("model", fgp(pc)(spec.Provider)+" "+pText(spec.Model)),
	}
	var settings []string
	if spec.Effort != "" {
		settings = append(settings, "effort "+spec.Effort)
	}
	if spec.MaxTokens > 0 {
		settings = append(settings, "max_tokens "+tokens(spec.MaxTokens))
	}
	if spec.CompactAt > 0 {
		settings = append(settings, "compact @"+tokens(spec.CompactAt))
	}
	if spec.Profile != "" {
		settings = append(settings, "profile "+spec.Profile)
	}
	if len(settings) > 0 {
		out = append(out, kv("settings", pDim(strings.Join(settings, " · "))))
	}
	now := pill("○ IDLE", bg3, dim)
	if a.Phase != Idle {
		now = pill(m.spin(spinner)+" "+strings.ToUpper(a.Phase.String()), green, bg0) + pDim(" for "+short(time.Since(a.Since))+" · ") + pText(a.Activity)
	} else if a.Voted {
		now = pill("◆ VOTED DONE", lime, bg0) + pDim(" says the whole task is done")
	}
	out = append(out, kv("now", now))
	u := spec.Usage
	usage := pText(fmt.Sprintf("%d turns", spec.Turns)) + pDim(fmt.Sprintf(" · %s in / %s out", tokens(u.InputTokens), tokens(u.OutputTokens)))
	if u.CachedTokens > 0 && u.InputTokens > 0 {
		usage += pLime(fmt.Sprintf(" · ↯%d%% cached", u.CachedTokens*100/u.InputTokens))
	}
	usage += pDim(costLabel(spec.Cost, 0))
	out = append(out, kv("usage", usage))
	ctx := pText(tokens(a.Context))
	if spec.CompactAt > 0 {
		ctx = bar(float64(a.Context)/float64(spec.CompactAt), 24, bg2) + pDim(" "+tokens(a.Context)+"/"+tokens(spec.CompactAt))
	}
	if a.Compactions > 0 {
		ctx += pAmber(fmt.Sprintf(" · compacted %d×", a.Compactions))
	}
	out = append(out, kv("context", ctx))
	out = append(out, kv("activity", spark(a.Rate(time.Now(), 30, 30*time.Second), pc)+pDim("  tool calls, last 15 min")))

	section := func(s string) { out = append(out, "", bold(pLime(s))) }
	if a.Phase != Idle {
		section("LIVE · what the model is writing right now")
		out = append(out, m.liveLines(spec, a, w-6)...)
	}
	section(fmt.Sprintf("LATEST HANDOFF · round %d", a.MessageRound))
	if a.Message == "" {
		out = append(out, pDim("  none yet"))
	}
	for _, line := range markdown(wrap(clean(a.Message), w-6), pText) {
		out = append(out, fgp(mix(bg1, pc, 0.6))("  ┃ ")+line)
	}
	section("RECENT TOOLS")
	if len(a.Recent) == 0 {
		out = append(out, pDim("  none yet"))
	}
	for j := len(a.Recent) - 1; j >= 0; j-- {
		out = append(out, "  "+m.eventLine(l, a.Recent[j]))
	}
	section("FILES CHANGED")
	if len(a.Files) == 0 {
		out = append(out, pDim("  none"))
	}
	paths := make([]string, 0, len(a.Files))
	for p := range a.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		out = append(out, "  "+pText(p)+pDim(fmt.Sprintf(" ×%d", a.Files[p])))
	}
	return out
}

// liveLines shows the end of what an agent's model is streaming, at most
// w cells wide.
func (m *model) liveLines(spec task.AgentSpec, a *Agent, w int) []string {
	lv := a.Live
	if lv == nil {
		why := "waiting on the model"
		if spec.Provider == "gemini" {
			why += " · Gemini replies all at once, so there's nothing to show until it does"
		}
		return []string{pDim("  " + m.spin(spinner) + " " + why)}
	}
	pc := providerColor(spec.Provider)
	gutter := fgp(mix(bg1, pc, 0.6))("  ┃ ")
	var out []string
	add := func(text string, n int, paint func(string) string) {
		var lines []string
		for _, raw := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
			raw = strings.ReplaceAll(ansi.Strip(raw), "\t", "    ")
			lines = append(lines, raw)
		}
		for _, l := range lines[max(0, len(lines)-n):] {
			out = append(out, gutter+paint(ansi.Truncate(l, w-4, "…")))
		}
	}
	switch lv.Phase {
	case "tool":
		out = append(out, "  "+fgp(pc)(bold(a.LiveSummary())))
		if lv.Preview != "" {
			add(lv.Preview, 14, pText)
		}
	case "replying":
		add(strings.Join(wrap(lv.Text, w-4), "\n"), 10, pText)
	default:
		if strings.TrimSpace(lv.Thinking) == "" {
			out = append(out, pDim("  "+m.spin(spinner)+" reasoning (this model doesn't show its reasoning)"))
		} else {
			add(strings.Join(wrap(lv.Thinking, w-4), "\n"), 8, pDim)
		}
	}
	out = append(out, pFaint("  updated "+ago(lv.Updated)))
	return out
}

// drawConfirm floats the stop/resume dialog over the dashboard.
func (m *model) drawConfirm(lines []string) string {
	l := m.current()
	t := l.Task
	act := m.confirm
	w := min(64, m.w-6)
	var body []string
	switch act {
	case "resume":
		body = append(body, pText("Start it again in the background, from round "+fmt.Sprint(t.Round)+"."))
		if m.runOn == "" {
			body = append(body, pDim("Agents keep their conversations, notes and changes."))
		} else {
			body = append(body, pDim("Agents moving to "+m.runOn+" start fresh conversations;"), pDim("the workspace and the notes carry over."))
		}
		if t.Status == task.Failed && t.Reason != "" {
			body = append(body, "", pRed("It failed with:"))
			for i, r := range wrap(oneLine(t.Reason), w-6) {
				if i == 3 {
					body[len(body)-1] += pDim(" …")
					break
				}
				body = append(body, pRed(r))
			}
			if m.runOn == "" {
				body = append(body, pDim("If that isn't fixed yet, it will fail the same way."))
			}
		}
		if len(m.opt.Providers) > 0 {
			name, col := "as before", text
			if m.runOn != "" {
				name, col = m.runOn, providerColor(m.runOn)
			}
			row := pDim("Run on  ") + onp(bg3)(fgp(col)(bold(" ◂ "+name+" ▸ "))) + pFaint("  ↑↓ change")
			body = append(body, "", row)
			for _, p := range m.opt.Providers {
				if p.Name == m.runOn && p.Note != "" {
					body = append(body, pDim("        "+p.Note))
				}
			}
		}
	case "delete":
		body = append(body, pText("Removes its transcript and agent state. This can't be undone."),
			pDim("The workspace, its files and its notes stay:"), pLime(t.Workspace))
	case "stop":
		body = append(body, pText("Each agent finishes its current step, then the task"),
			pText("saves its state and stops."), pDim("Resume it any time with r."))
	}
	yes, no := strings.ToUpper(act), "CANCEL"
	yb, nb := button(yes, !m.no, bg2, true), button(no, m.no, bg2, false)
	if act == "delete" {
		face, fgc := mix(bg3, red, 0.35), white
		if !m.no {
			face, fgc = red, bg0
		}
		yb = onp(face)(fgp(fgc)(bold("  "+yes+"  "))) + onp(bg2)(fgp(shadowColor)("▄"))
	}
	pad := strings.Repeat(" ", max(0, w-4-width(yb)-width(nb)-2))
	out := []string{""}
	for _, b := range body {
		out = append(out, " "+b)
	}
	out = append(out, "", " "+pad+nb+"  "+yb, " "+pad+buttonShadow(no, bg2)+"  "+buttonShadow(yes, bg2))
	box := panel(strings.ToUpper(act)+" “"+t.Title()+"”?", "", out, w, len(out)+2, true, bg2)
	for i := range lines[:len(lines)-1] {
		lines[i] = onp(bg0)(fgp(backdropFg)(ansi.Strip(lines[i])))
	}
	h := len(out) + 2
	x, y := (m.w-w-2)/2, max(0, (m.h-1-h-1)/2)
	shadow(lines, x, y, w, h, m.w)
	overlayAt(lines, box, x, y, m.w)
	lines[len(lines)-1] = m.footer()
	return strings.Join(lines, "\n")
}

// helpRows are the dashboard's keys, as the help panel lists them.
var helpRows = [][2]string{
	{"↑ ↓  j k", "select a task, an agent or the summary · scroll"},
	{"tab", "switch between the task list and the task panel"},
	{"← →  h l", "select an agent; in activity and messages, filter"},
	{"⏎ enter", "open · zoom into an agent · an activity line's details"},
	{"esc", "back"},
	{"1 2 3 4", "overview · activity log · files · handoffs"},
	{"[  ]", "previous / next task from anywhere"},
	{"pgup pgdn g G", "page · top · bottom (and follow)"},
	{"f", "follow new activity on / off"},
	{"s  r  x", "stop · resume · delete a task (each asks first)"},
	{"a", "follow-up: more work for a finished task's team"},
	{"n", "new task: agents, models, efforts and a prompt"},
	{"p  R", "providers (sign in or out) · roles (add, edit, import)"},
	{"C", "configuration: spending caps, limits, models, voice"},
	{"e", "the task in full: prompt, why it stopped, to-dos"},
	{"c", "copy what's selected, in full"},
	{"drag", "select text to copy it · right-click copies a line"},
	{"t", "the walkthrough's tips again"},
	{"q", "quit · tasks keep running"},
}

// helpBox lists every key, at most w cells wide and, short of spacing, h tall.
func helpBox(w, h int, voice bool) string {
	rows := helpRows
	if voice {
		n := len(rows) - 1 // before q
		rows = append(append(rows[:n:n],
			[2]string{"v  V", "voice: read the dashboard aloud on / off · repeat"},
			[2]string{"w  i  o", "voice: where you are · the selection · what to do · or F2 F3 F4"}), rows[n])
	}
	lines := []string{""}
	for _, r := range rows {
		lines = append(lines, " "+pill(fit(r[0], 14), bg3, lime)+"  "+pText(r[1]))
	}
	lines = append(lines, "", pDim(" Every command: ")+pLime("ackn4th --help")+pFaint("   ·   press any key"))
	if len(lines)+2 > h {
		lines = append(lines[1:len(lines)-2], lines[len(lines)-1]) // no spacing
	}
	return panel("KEYS", "", lines, min(80, w-4), len(lines)+2, true, bg2)
}

// overlay centers a block over the screen.
func overlay(screen, block string, w int) string {
	bg := strings.Split(screen, "\n")
	top := strings.Split(block, "\n")
	y0 := max(0, (len(bg)-len(top))/2)
	bw := width(top[0])
	x0 := max(0, (w-bw)/2)
	for i, line := range top {
		if y0+i >= len(bg) {
			break
		}
		row := bg[y0+i]
		bg[y0+i] = ansi.Cut(row, 0, x0) + "\x1b[0m" + line + "\x1b[0m" + ansi.Cut(row, x0+bw, w)
	}
	return strings.Join(bg, "\n")
}

// costLabel is " · ≈$1.23" (of a cap, if any): estimated spending on
// billed providers. Nothing when nothing was spent.
func costLabel(usd, capUSD float64) string {
	if usd <= 0 && capUSD <= 0 {
		return ""
	}
	s := " · ≈" + spend.USD(usd)
	if capUSD > 0 {
		s += " of " + spend.USD(capUSD)
	}
	return s
}
