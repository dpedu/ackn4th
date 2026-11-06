// Package dash is ackn4th's interactive terminal dashboard: every task,
// where each of its agents is in its workflow, and what they have done,
// explorable with the keyboard. It reads the same task records and event
// logs the runners write, so it works for foreground and background tasks
// alike, and several dashboards can watch at once.
package dash

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dethmasque/ackn4th/internal/event"
	"github.com/dethmasque/ackn4th/internal/task"
	"github.com/dethmasque/ackn4th/internal/tools"
)

// Phase is where an agent is in its turn.
type Phase int

const (
	Idle     Phase = iota // not in a turn
	Starting              // turn started, catching up
	Thinking              // waiting on the model
	Tool                  // running a tool call
	Replying              // wrote its closing message
)

func (p Phase) String() string {
	return [...]string{"idle", "catching up", "thinking", "using tools", "handing off"}[p]
}

// Agent is one agent's live state, derived from events.
type Agent struct {
	Phase    Phase
	Since    time.Time // when Phase began
	Activity string    // what it is doing now, one line
	Tool     string    // current or last tool
	Steps    int       // tool calls this turn
	Turns    int       // turns started (this log)

	In, Out, Cached int64 // tokens, from turn ends
	Context         int64
	Compactions     int

	Message      string // latest closing message
	MessageRound int
	Voted        bool // declared done in its latest turn
	Errors       int  // failed tool calls
	Recent       []event.Event
	Files        map[string]int // paths it changed → times
	Calls        []time.Time    // tool calls, for the activity sparkline
	// Live is what its model is streaming right now, when the runner
	// shows it: reasoning, reply text, or a tool call being written.
	Live *task.Live
}

// liveFresh is how long a live view stays believable without an update.
const liveFresh = 90 * time.Second

// readLive refreshes each working agent's live view from the runner.
func (l *Live) readLive(s task.Backend) {
	for i, a := range l.Agents {
		a.Live = nil
		if a.Phase == Idle || l.Task.Status != task.Running {
			continue
		}
		if lv := s.ReadLive(l.Task.ID, i); lv != nil && time.Since(lv.Updated) < liveFresh {
			a.Live = lv
		}
	}
}

// LiveSummary is one line on what the agent's model is writing.
func (a *Agent) LiveSummary() string {
	lv := a.Live
	if lv == nil {
		return ""
	}
	switch lv.Phase {
	case "tool":
		s := "✎ " + lv.Tool
		if lv.Path != "" {
			s += " " + lv.Path
		}
		return s + " · " + bytesLabel(lv.Bytes) + " so far"
	case "replying":
		return withLast("replying", lv.Text)
	}
	return withLast("thinking", lv.Thinking)
}

// withLast is label, then the last line of s if it has one. Some
// providers stream no reasoning text, only that they are reasoning.
func withLast(label, s string) string {
	if l := lastLine(s); l != "" {
		return label + " · " + l
	}
	return label + "…"
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func bytesLabel(n int) string {
	if n < 1024 {
		return strconv.Itoa(n) + " B"
	}
	return strconv.FormatFloat(float64(n)/1024, 'f', 1, 64) + " KB"
}

// Rate buckets the agent's tool calls into n slots of d each, oldest
// first, ending now.
func (a *Agent) Rate(now time.Time, n int, d time.Duration) []int {
	out := make([]int, n)
	for _, t := range a.Calls {
		age := now.Sub(t)
		if age < 0 || age >= time.Duration(n)*d {
			continue
		}
		out[n-1-int(age/d)]++
	}
	return out
}

// File is one path agents changed.
type File struct {
	Path  string
	Ops   string // e.g. "W E E": write, edit, delete in order
	By    []string
	Last  time.Time
	Count int
}

// Live is one task's state as the dashboard knows it.
type Live struct {
	Task   *task.Task
	Events []event.Event
	Agents []*Agent
	Files  map[string]*File
	off    int64
}

// recentPerAgent bounds each agent's recent tool list.
const recentPerAgent = 12

func newLive(t *task.Task) *Live {
	l := &Live{Task: t, Files: map[string]*File{}}
	l.sync(t)
	return l
}

// sync adopts a freshly loaded task record, keeping derived state.
func (l *Live) sync(t *task.Task) {
	l.Task = t
	for len(l.Agents) < len(t.Agents) {
		l.Agents = append(l.Agents, &Agent{Files: map[string]int{}})
	}
	if t.Status != task.Running {
		for _, a := range l.Agents {
			if a.Phase != Idle {
				a.Phase, a.Activity = Idle, ""
			}
		}
	}
	for i, spec := range t.Agents {
		// The record is the authority on votes and usage totals.
		l.Agents[i].Voted = spec.Done
		if spec.Context > 0 {
			l.Agents[i].Context = spec.Context
		}
	}
}

// Apply folds one event into the state.
func (l *Live) Apply(e event.Event) {
	if e.Ephemeral() {
		return
	}
	l.Events = append(l.Events, e)
	if e.Kind == event.TaskStopped || e.Kind == event.TaskCompleted || e.Kind == event.TaskFailed {
		for _, a := range l.Agents {
			a.Phase, a.Activity, a.Since = Idle, "", e.Time
		}
		return
	}
	if e.Agent < 1 || e.Agent > len(l.Agents) {
		return
	}
	a := l.Agents[e.Agent-1]
	set := func(p Phase, activity string) {
		if a.Phase != p {
			a.Since = e.Time
		}
		a.Phase, a.Activity = p, activity
	}
	switch e.Kind {
	case event.TurnStarted:
		a.Turns++
		a.Steps = 0
		a.Voted = false
		a.Since = e.Time
		set(Starting, "starting turn")
	case event.Context:
		set(Starting, "catching up · "+e.Text)
	case event.ToolCall:
		a.Steps++
		a.Tool = e.Tool
		a.Calls = append(a.Calls, e.Time)
		if len(a.Calls) > 500 {
			a.Calls = a.Calls[len(a.Calls)-500:]
		}
		set(Tool, strings.TrimSpace(e.Tool+" "+e.Text))
		a.Recent = append(a.Recent, e)
		if len(a.Recent) > recentPerAgent {
			a.Recent = a.Recent[len(a.Recent)-recentPerAgent:]
		}
		l.touch(e, a)
	case event.ToolResult:
		if e.Error {
			a.Errors++
			a.Recent = append(a.Recent, e)
		}
		set(Thinking, "thinking")
	case event.Message:
		a.Message, a.MessageRound = e.Text, e.Round
		set(Replying, "handing off")
	case event.DoneVote:
		a.Voted = true
	case event.Compacted:
		a.Compactions++
	case event.TurnEnded:
		a.In += e.InTok
		a.Out += e.OutTok
		a.Cached += e.CacheTok
		if e.CtxTok > 0 {
			a.Context = e.CtxTok
		}
		set(Idle, "")
	}
}

// touch records a file change made by a tool call. The call's summary
// starts with the path, e.g. "api/rate.go (2.1 KB)".
func (l *Live) touch(e event.Event, a *Agent) {
	var op string
	switch e.Tool {
	case tools.WriteFile:
		op = "W"
	case tools.EditFile:
		op = "E"
	case tools.DeletePath:
		op = "D"
	default:
		return
	}
	fields := strings.Fields(e.Text)
	if len(fields) == 0 {
		return
	}
	p := fields[0]
	a.Files[p]++
	f := l.Files[p]
	if f == nil {
		f = &File{Path: p}
		l.Files[p] = f
	}
	f.Ops += op
	f.Count++
	f.Last = e.Time
	if !slices.Contains(f.By, e.Role) {
		f.By = append(f.By, e.Role)
	}
}

// Next is the agent (0-based) whose turn is next in a turn-taking task, or
// -1.
func (l *Live) Next() int {
	t := l.Task
	if t.Parallel() || t.Status != task.Running {
		return -1
	}
	for i, a := range l.Agents {
		if a.Phase != Idle && i < len(t.Agents) {
			return -1 // someone is mid-turn
		}
	}
	return t.Next
}

// Working counts agents in a turn.
func (l *Live) Working() int {
	n := 0
	for _, a := range l.Agents {
		if a.Phase != Idle {
			n++
		}
	}
	return n
}

// Votes counts agents whose latest turn declared done.
func (l *Live) Votes() int {
	n := 0
	for _, a := range l.Agents {
		if a.Voted {
			n++
		}
	}
	return n
}
