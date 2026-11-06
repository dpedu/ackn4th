package cli

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dethmasque/ackn4th/internal/event"
	"github.com/dethmasque/ackn4th/internal/spend"
	"github.com/dethmasque/ackn4th/internal/task"
	"github.com/dethmasque/ackn4th/internal/ui"
)

// renderer draws task events. It is used both live (with streamed text
// and the ping-pong animation) and to replay a task's event log.
type renderer struct {
	t    *ui.Term
	task *task.Task
	spin *ui.PingPong

	mu       sync.Mutex
	inTurn   bool
	streamed bool            // reply text for the current message was streamed
	partial  strings.Builder // streamed text not yet terminated by a newline
	thinking bool
	// toolName and toolBytes track a tool call while its input streams.
	toolName  string
	toolBytes int

	// parallel tasks interleave agents' events, so each line is tagged
	// with its agent and streamed text is not shown (messages are).
	parallel bool
	active   int // agents mid-turn
	tagWidth int
}

func newRenderer(t *ui.Term, tk *task.Task) *renderer {
	r := &renderer{t: t, task: tk, spin: t.NewPingPong(), parallel: tk.Parallel()}
	for _, a := range tk.Agents {
		r.tagWidth = max(r.tagWidth, len(a.Role))
	}
	return r
}

// pre is the left margin of an agent's lines: indentation, or in a
// parallel task the agent's name.
func (r *renderer) pre(e event.Event) string {
	if !r.parallel {
		return "   "
	}
	name := "ackn4th"
	if e.Agent >= 1 && e.Agent <= len(r.task.Agents) {
		name = r.task.Agents[e.Agent-1].Role
	}
	tag := fmt.Sprintf(" %-*s ", r.tagWidth, name)
	if e.Agent >= 1 && e.Agent <= len(r.task.Agents) {
		return r.t.ProviderColor(r.task.Agents[e.Agent-1].Provider, tag)
	}
	return r.t.Gray(tag)
}

func (r *renderer) working() string {
	if r.active == 1 {
		return "1 agent working"
	}
	return fmt.Sprintf("%d agents working", r.active)
}

const gutter = "   │ "

// line prints one line, pausing the animation around it.
func (r *renderer) line(parts ...string) {
	r.spin.Stop()
	r.t.Println(parts...)
	if r.inTurn {
		r.spin.Resume()
	}
}

func (r *renderer) agentLabel(i int) string {
	if i < 1 || i > len(r.task.Agents) {
		return "ackn4th"
	}
	a := r.task.Agents[i-1]
	return r.t.ProviderColor(a.Provider, a.Role)
}

// peer names the agent who receives the current agent's handoff.
func (r *renderer) peer(i int) string {
	if r.task.Solo() {
		return "workspace"
	}
	next := r.task.Agents[i%len(r.task.Agents)]
	return next.Role
}

// Handle renders one event. It is safe for concurrent use.
func (r *renderer) Handle(e event.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.t
	switch e.Kind {
	case event.TaskStarted:
		verb := "jacking in"
		if e.Text == "resume" {
			verb = "resuming"
		}
		r.line(t.Gray(fmt.Sprintf("» %s · %s", verb, e.Time.Local().Format("15:04:05"))))

	case event.FollowUp:
		r.line(t.Cyan("↪ follow-up ") + t.Gray(ui.Truncate(strings.Join(strings.Fields(e.Text), " "), 100)))

	case event.RoundStarted:
		label := fmt.Sprintf("ROUND %d", e.Round)
		if r.task.MaxRounds > 0 {
			label += fmt.Sprintf(" / %d", r.task.MaxRounds)
		}
		r.line()
		r.line(rule(t, t.Bold(t.Lime(label))))

	case event.TurnStarted:
		if r.parallel {
			r.active++
			r.line(r.pre(e), t.Bold(t.Green("▶ ")), t.Gray(e.Provider+" · "+e.Model))
			r.inTurn = true
			r.spin.Start("ackn4th", "workspace", r.working())
			break
		}
		r.line()
		r.line(" ", t.Bold(t.Green("▶")), " ", t.Bold(r.agentLabel(e.Agent)), t.Gray(" · "+e.Provider+" · "+e.Model))
		r.inTurn, r.streamed, r.thinking = true, false, false
		r.partial.Reset()
		r.spin.Start(e.Provider, r.peer(e.Agent), "thinking")

	case event.Thinking:
		if !r.thinking && !r.parallel {
			r.thinking = true
			r.spin.SetLabel("thinking")
		}

	case event.ToolInput:
		if r.parallel {
			break
		}
		if e.Tool != "" {
			r.toolName, r.toolBytes = e.Tool, 0
		}
		r.toolBytes += len(e.Text)
		r.thinking = false
		r.spin.SetLabel("writing " + r.toolName + " · " + ui.Bytes(r.toolBytes))

	case event.Delta:
		if r.parallel {
			break
		}
		r.thinking = false
		r.spin.SetLabel("writing")
		r.streamed = true
		r.partial.WriteString(e.Text)
		r.flushPartial(false)

	case event.Message:
		if r.streamed {
			r.flushPartial(true)
		} else {
			for _, l := range ui.Wrap(e.Text, t.Width-len(gutter)-2-r.tagWidth) {
				r.line(r.pre(e), t.Moss("│ "), l)
			}
		}
		r.streamed = false

	case event.ToolCall:
		r.flushPartial(true)
		if e.Tool == "declare_done" {
			break // shown as the done vote
		}
		if !r.parallel {
			r.spin.SetLabel(e.Tool)
		}
		r.line(r.pre(e), t.Moss("├─ "), t.Lime(fmt.Sprintf("%-16s", e.Tool)), " ", t.Gray(ui.Truncate(e.Text, t.Width-34-r.tagWidth)))

	case event.ToolResult:
		if e.Error {
			r.line(r.pre(e), t.Moss("│  "), t.Red("✗ "+ui.Truncate(e.Text, t.Width-12-r.tagWidth)))
		}

	case event.DoneVote:
		r.line(r.pre(e), t.Moss("╞═ "), t.Bold(t.Green("◆ declared done:")), " ", t.Gray(ui.Truncate(e.Text, t.Width-24-r.tagWidth)))

	case event.TurnEnded:
		r.flushPartial(true)
		stats := fmt.Sprintf("turn ended · %s in / %s out", ui.Tokens(e.InTok), ui.Tokens(e.OutTok)) + cached(e.CacheTok, e.InTok)
		if r.parallel {
			r.active = max(0, r.active-1)
			if r.active == 0 {
				r.inTurn = false
				r.spin.Stop()
			} else {
				r.spin.SetLabel(r.working())
			}
		} else {
			r.inTurn = false
			r.spin.Stop()
		}
		r.line(r.pre(e), t.Moss("╰─ "), t.Gray(stats), r.contextGauge(e))

	case event.Context:
		r.line(r.pre(e), t.Moss("├─ "), t.Cyan(fmt.Sprintf("%-16s", "catch-up")), " ", t.Gray(e.Text))

	case event.Compacted:
		r.line(r.pre(e), t.Moss("╞═ "), t.Bold(t.Magenta("⧉ context compacted")), t.Gray(" · earlier conversation replaced by a summary"))

	case event.Notice:
		msg := t.Amber("! " + e.Text)
		if e.Error {
			msg = t.Red("! " + e.Text)
		}
		r.line(r.pre(e), t.Moss("│  "), msg)

	case event.TaskStopped, event.TaskCompleted, event.TaskFailed:
		r.flushPartial(true)
		r.inTurn = false
		r.spin.Stop()
	}
}

// contextGauge shows how full the agent's context is relative to its
// compaction threshold.
func (r *renderer) contextGauge(e event.Event) string {
	if e.CtxTok <= 0 || e.Agent < 1 || e.Agent > len(r.task.Agents) {
		return ""
	}
	limit := r.task.Agents[e.Agent-1].CompactAt
	if limit <= 0 {
		return r.t.Gray(" · context " + ui.Tokens(e.CtxTok))
	}
	frac := min(1, float64(e.CtxTok)/float64(limit))
	return r.t.Gray(" · context ") + r.t.Gauge(frac, 10) + r.t.Gray(" "+ui.Tokens(e.CtxTok)+"/"+ui.Tokens(limit))
}

// flushPartial prints complete streamed lines; with all set, it also
// prints any trailing partial line.
func (r *renderer) flushPartial(all bool) {
	text := r.partial.String()
	cut := strings.LastIndexByte(text, '\n')
	if all {
		cut = len(text)
	}
	if cut < 0 {
		return
	}
	ready := strings.TrimRight(text[:cut], "\n")
	rest := strings.TrimPrefix(text[cut:], "\n")
	r.partial.Reset()
	r.partial.WriteString(rest)
	if strings.TrimSpace(ready) == "" {
		return
	}
	for _, l := range ui.Wrap(ready, r.t.Width-len(gutter)-2) {
		r.line(r.t.Moss(gutter), l)
	}
}

// Close stops any animation.
func (r *renderer) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inTurn = false
	r.spin.Stop()
}

func rule(t *ui.Term, label string) string {
	w := t.Width - 2
	rest := w - len(stripANSI(label)) - 6
	if rest < 3 {
		rest = 3
	}
	return t.Green("━━━━ ") + label + " " + t.Green(strings.Repeat("━", rest))
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\x1b' {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// teamLine renders the team as "a·claude )) <==> (( b·gemini".
func teamLine(t *ui.Term, tk *task.Task) string {
	parts := make([]string, len(tk.Agents))
	for i, a := range tk.Agents {
		parts[i] = t.ProviderColor(a.Provider, a.Role) + t.Gray("·"+a.Provider)
	}
	return strings.Join(parts, t.Moss(" )) <==> (( "))
}

// agentSettings describes an agent's model configuration in one line.
func agentSettings(t *ui.Term, a task.AgentSpec) string {
	parts := []string{t.ProviderColor(a.Provider, a.Model)}
	if a.Effort != "" {
		parts = append(parts, "effort "+a.Effort)
	}
	if a.CompactAt > 0 {
		parts = append(parts, "compact @"+ui.Tokens(a.CompactAt))
	}
	if a.Profile != "" {
		parts = append(parts, "profile "+a.Profile)
	}
	return parts[0] + t.Gray(" · "+strings.Join(parts[1:], " · "))
}

// contextLine says which agents share a cached copy of the team context.
func contextLine(t *ui.Term, tk *task.Task) string {
	groups := tk.CacheGroups()
	if len(groups) == 1 {
		return t.Lime("shared") + t.Gray(fmt.Sprintf(" · all %d agents read one cached copy", len(tk.Agents)))
	}
	var parts []string
	for _, g := range groups {
		ns := make([]string, len(g))
		for i, n := range g {
			ns[i] = strconv.Itoa(n + 1)
		}
		parts = append(parts, "agents "+strings.Join(ns, ","))
	}
	return t.Amber(fmt.Sprintf("%d groups", len(groups))) + t.Gray(" · "+strings.Join(parts, " | ")+" cache separately")
}

// header prints the task's identity card.
func header(t *ui.Term, tk *task.Task) {
	rounds := "until consensus"
	if tk.MaxRounds > 0 {
		rounds = fmt.Sprintf("%d max", tk.MaxRounds)
	}
	kv := [][2]string{
		{"status", statusLabel(t, tk.Status)},
		{"team", teamLine(t, tk)},
	}
	for i, a := range tk.Agents {
		key := fmt.Sprintf("agent %d", i+1)
		if tk.Solo() {
			key = "agent"
		}
		kv = append(kv, [2]string{key, agentSettings(t, a)})
	}
	if !tk.Solo() {
		kv = append(kv, [2]string{"context", contextLine(t, tk)})
	}
	if tk.Parallel() {
		kv = append(kv, [2]string{"mode", t.Cyan("parallel") + t.Gray(" · every agent works each round at once")})
	}
	kv = append(kv, [2]string{"workspace", t.Lime(tk.Workspace)}, [2]string{"rounds", rounds})
	lines := t.KV(kv...)
	lines = append(lines, "")
	for _, l := range ui.Wrap(tk.Prompt, t.Width-8) {
		lines = append(lines, t.Bold(l))
	}
	t.Box(strings.ToUpper(tk.Title())+" · "+tk.ID, lines)
}

// cached describes how much input came from the prompt cache.
func cached(hit, in int64) string {
	if hit <= 0 || in <= 0 {
		return ""
	}
	return fmt.Sprintf(" · %d%% cached", hit*100/in)
}

// outcome prints the closing card for a finished run.
// summaryView is how a task's summary.md can be shown here: the path when this
// host has the file, the text when it came from the controller. A client has no
// path to print - the file is on the other machine - so it prints the text.
type summaryView struct {
	path string
	text string
}

func outcome(t *ui.Term, tk *task.Task, started time.Time, summary summaryView) {
	u := tk.Usage()
	stats := t.Gray(fmt.Sprintf("round %d · %s in / %s out", tk.Round, ui.Tokens(u.InputTokens), ui.Tokens(u.OutputTokens)) + cached(u.CachedTokens, u.InputTokens))
	if c := tk.Cost(); c > 0 {
		stats += t.Gray(" · ≈" + spend.USD(c))
	}
	if !started.IsZero() {
		stats += t.Gray(" · " + ui.Duration(time.Since(started).Round(time.Second)))
	}
	t.Println()
	switch tk.Status {
	case task.Completed:
		lines := []string{t.Bold(t.Lime("✓ every agent declared the task done")), stats, ""}
		lines = append(lines, ui.Wrap(tk.Reason, t.Width-8)...)
		if tk.PR != "" {
			lines = append(lines, "", t.Gray("draft PR: ")+t.Lime(tk.PR))
		}
		t.Box("COMPLETE", lines)
	case task.Stopped:
		t.Box("STOPPED", []string{t.Amber("■ " + tk.Reason), stats, "",
			t.Gray("resume:  ") + t.Lime("ackn4th resume-task "+tk.ID)})
	case task.Failed:
		lines := []string{}
		for _, l := range ui.Wrap(tk.Reason, t.Width-10) {
			lines = append(lines, t.Red(l))
		}
		lines = append(lines, stats, "", t.Gray("resume:  ")+t.Lime("ackn4th resume-task "+tk.ID))
		t.Box("FAILED", lines)
	}
	forYou(t, tk, summary)
}

// forYou lists the action items the agents left for the operator.
func forYou(t *ui.Term, tk *task.Task, summary summaryView) {
	open := tk.OpenTodos()
	var lines []string
	if len(open) == 0 {
		lines = append(lines, t.Gray("no action items: the agents didn't leave you anything to do"))
	}
	for _, td := range open {
		for i, l := range ui.Wrap(td.Text, t.Width-14) {
			lead := "      "
			if i == 0 {
				lead = t.Amber(fmt.Sprintf("☐ #%-3d", td.ID))
			}
			lines = append(lines, lead+l)
		}
		lines = append(lines, "      "+t.Gray(fmt.Sprintf("%s · round %d", td.By, td.Round)))
	}
	title := "FOR YOU"
	if len(open) > 0 {
		title = fmt.Sprintf("FOR YOU · %d OPEN", len(open))
	}
	t.Box(title, lines)
	switch {
	case summary.path != "":
		t.Println(t.Gray("  full summary: "), t.Lime(summary.path))
	case summary.text != "":
		t.Println()
		t.Println(t.Gray("  full summary, from the controller:"))
		for _, l := range strings.Split(summary.text, "\n") {
			t.Println("  " + l)
		}
	}
}

func statusLabel(t *ui.Term, s task.Status) string {
	switch s {
	case task.Running:
		return t.Green("● running")
	case task.Stopped:
		return t.Amber("■ stopped")
	case task.Failed:
		return t.Red("✗ failed")
	case task.Completed:
		return t.Lime("✓ completed")
	}
	return t.Gray("○ pending")
}
