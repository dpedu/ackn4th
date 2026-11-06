// Package task stores tasks on disk and runs them: a task is a prompt, a
// workspace, and a team of agents that take turns until they agree the
// work is done.
//
// Each task lives in its own directory under the data dir:
//
//	tasks/<id>/task.json      the Task record
//	tasks/<id>/name           the name people know it by
//	tasks/<id>/events.jsonl   append-only progress log
//	tasks/<id>/agents/N.json  agent N's provider conversation state
//	tasks/<id>/agents/N.snapshot.json  the workspace as agent N last left it
//	tasks/<id>/workspace.json latest workspace snapshot and who changed each file
//	tasks/<id>/runner.log     stdout/stderr of a background runner
//	tasks/<id>/run.lock       held (flock) while a runner is active
package task

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/dethmasque/ackn4th/internal/event"
	"github.com/dethmasque/ackn4th/internal/provider"
)

// Status is a task's lifecycle state.
type Status string

const (
	Pending   Status = "pending"   // created, never run
	Running   Status = "running"   // a runner holds the lock
	Stopped   Status = "stopped"   // stopped by the user or a round limit; resumable
	Failed    Status = "failed"    // stopped by an error; resumable
	Completed Status = "completed" // every agent declared done
)

// AgentSpec is one team member.
type AgentSpec struct {
	Role     string `json:"role"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Profile names the config agent profile the settings came from.
	Profile   string `json:"profile,omitempty"`
	Effort    string `json:"effort,omitempty"`
	MaxTokens int64  `json:"max_tokens,omitempty"`
	CompactAt int64  `json:"compact_at,omitempty"`

	Usage provider.Usage `json:"usage"`
	// Cost is the estimated dollars the agent has spent (billed
	// providers only).
	Cost  float64 `json:"cost,omitempty"`
	Turns int     `json:"turns"`
	// Context is how full the agent's context window was at its last request.
	Context     int64 `json:"context_tokens,omitempty"`
	Compactions int   `json:"compactions,omitempty"`
	// NotesSeen is the version of the project notes the agent last saw.
	NotesSeen string `json:"notes_seen,omitempty"`
	// RoundDone is the last round the agent finished (parallel tasks), so a
	// task stopped mid-round resumes only the agents still due.
	RoundDone int `json:"round_done,omitempty"`
	// Message is the agent's latest closing message, from MessageRound.
	Message      string `json:"message,omitempty"`
	MessageRound int    `json:"message_round,omitempty"`
	// Done records whether the agent declared done in its latest turn.
	Done        bool   `json:"done"`
	DoneSummary string `json:"done_summary,omitempty"`
	// FollowUpsSeen is how many of the task's follow-ups the agent has
	// been given.
	FollowUpsSeen int `json:"follow_ups_seen,omitempty"`
	// SameHandoffs counts consecutive turns whose handoff repeated the
	// one before, and Idle consecutive turns wound up for the agent (out
	// of tool calls, or repeating itself) without it changing a file: an
	// agent going in circles. See stuck.
	SameHandoffs int `json:"same_handoffs,omitempty"`
	Idle         int `json:"idle,omitempty"`
	// DoneStreak counts the agent's latest turns in a row that declared
	// done.
	DoneStreak int `json:"done_streak,omitempty"`
}

// Task is the persisted record of one task.
type Task struct {
	ID string `json:"id"`
	// Name is the person's name for the task, shown wherever it is. It is
	// stored apart from task.json (see Rename).
	Name      string      `json:"-"`
	Prompt    string      `json:"prompt"`
	Workspace string      `json:"workspace"`
	Agents    []AgentSpec `json:"agents"`
	Status    Status      `json:"status"`
	// Reason explains the current status (stop reason, error, summary).
	Reason string `json:"reason,omitempty"`
	// MaxCost caps the task's spending, in dollars over its whole life;
	// 0 means the config's spend.per_task.
	MaxCost float64 `json:"max_cost,omitempty"`
	// MaxRounds bounds the run; 0 means until the agents agree.
	MaxRounds int `json:"max_rounds"`
	// Round is the current round, starting at 1.
	Round int `json:"round"`
	// Next is the index of the agent whose turn is next.
	Next int `json:"next"`
	// Handoff is the last agent's message, delivered to the next agent.
	Handoff     string `json:"handoff,omitempty"`
	HandoffFrom int    `json:"handoff_from,omitempty"` // 1-based agent index
	Started     []bool `json:"started"`                // whether each agent has had its kickoff
	// Note is guidance from the human operator, delivered to the next agent
	// (to every agent in a parallel task).
	Note string `json:"note,omitempty"`
	// Mode is "" for agents taking turns, or ModeParallel.
	Mode string `json:"mode,omitempty"`
	// Inbox holds messages between agents not yet delivered.
	Inbox []Mail `json:"inbox,omitempty"`
	// SplitRounds counts the latest rounds in a row in which some agents
	// declared done and others didn't. See splitVote.
	SplitRounds int `json:"split_rounds,omitempty"`
	// FollowUps are more work asked of the team after a run, in order.
	FollowUps []FollowUp `json:"follow_ups,omitempty"`
	// Todos are what the agents need from the person who gave the task.
	Todos []Todo `json:"todos,omitempty"`
	// Repo is the GitHub repository the task works in (--repo, owner/name),
	// when one was named. The clone happens on the host that runs the task -
	// its disk, its gh login - so the record has to say which repository, not
	// just where the clone is meant to land.
	Repo string `json:"repo"`
	// Branch is the git branch the task works on (--branch), checked out
	// or created when it started.
	Branch string `json:"branch,omitempty"`
	// DraftPR opens a draft pull request with the agents' changes when the
	// task completes; PR is its URL once opened.
	DraftPR    bool      `json:"draft_pr,omitempty"`
	PR         string    `json:"pr,omitempty"`
	PID        int       `json:"pid,omitempty"`
	Background bool      `json:"background,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Todo is an action item for the human operator, recorded by an agent
// with add_user_todo: a decision, a credential, a manual step, something
// it couldn't verify.
type Todo struct {
	ID    int       `json:"id"`
	Text  string    `json:"text"`
	By    string    `json:"by"` // the agent's role
	Round int       `json:"round"`
	Time  time.Time `json:"time"`
	// Done is set when an agent resolved it itself, with Note saying how.
	Done bool   `json:"done,omitempty"`
	Note string `json:"note,omitempty"`
}

// OpenTodos lists the to-dos still waiting on the operator.
func (t *Task) OpenTodos() []Todo {
	var out []Todo
	for _, td := range t.Todos {
		if !td.Done {
			out = append(out, td)
		}
	}
	return out
}

// ModeParallel runs every agent's turn of a round at the same time.
const ModeParallel = "parallel"

// Mail is a message from one agent to another, sent with message_teammate.
type Mail struct {
	ID    int       `json:"id"`
	From  int       `json:"from"` // 1-based agent index
	To    int       `json:"to"`
	Round int       `json:"round"`
	Text  string    `json:"text"`
	Time  time.Time `json:"time"`
}

// Parallel reports whether agents take their turns at the same time.
func (t *Task) Parallel() bool { return t.Mode == ModeParallel && len(t.Agents) > 1 }

// Solo reports whether the task has a single agent.
func (t *Task) Solo() bool { return len(t.Agents) == 1 }

// CacheGroups splits the team (0-based agent indexes) into groups that
// can share one cached prompt prefix: same provider, model and effort.
// One group means every agent reads the shared context from one cache
// entry.
func (t *Task) CacheGroups() [][]int {
	var keys []string
	var groups [][]int
	for i, a := range t.Agents {
		k := a.Provider + "\x00" + a.Model + "\x00" + a.Effort
		j := slices.Index(keys, k)
		if j < 0 {
			keys, groups, j = append(keys, k), append(groups, nil), len(keys)
		}
		groups[j] = append(groups[j], i)
	}
	return groups
}

// Cost totals the agents' estimated spending, in dollars.
func (t *Task) Cost() float64 {
	var c float64
	for _, a := range t.Agents {
		c += a.Cost
	}
	return c
}

// Usage totals token usage across agents.
func (t *Task) Usage() provider.Usage {
	var u provider.Usage
	for _, a := range t.Agents {
		u.Add(a.Usage)
	}
	return u
}

// Resumable reports whether resume-task can continue the task.
func (t *Task) Resumable() bool {
	return t.Status == Stopped || t.Status == Failed || t.Status == Pending
}

// Store reads and writes tasks under a root directory.
type Store struct {
	Root string
	// data is the data dir this store was opened with. Root is where the
	// tasks live inside it; keeping both means a command can name the data
	// dir the person configured without re-deriving it from Root.
	data string
}

// NewStore returns a store rooted at dataDir/tasks.
func NewStore(dataDir string) *Store {
	return &Store{Root: filepath.Join(dataDir, "tasks"), data: dataDir}
}

// Dir is a task's directory. It joins, it does not check: every method that
// takes an ID from outside calls checkID first, so a path built here cannot
// leave Root.
func (s *Store) Dir(id string) string { return filepath.Join(s.Root, id) }

func (s *Store) taskPath(id string) string   { return filepath.Join(s.Dir(id), "task.json") }
func (s *Store) eventsPath(id string) string { return filepath.Join(s.Dir(id), "events.jsonl") }

// AgentStatePath is where agent n (0-based) keeps its conversation.
func (s *Store) AgentStatePath(id string, n int) string {
	return filepath.Join(s.Dir(id), "agents", fmt.Sprintf("%d.json", n+1))
}

// SnapshotPath is where agent n (0-based) keeps the workspace snapshot
// from the end of its last turn.
func (s *Store) SnapshotPath(id string, n int) string {
	return filepath.Join(s.Dir(id), "agents", fmt.Sprintf("%d.snapshot.json", n+1))
}

// WorkspacePath holds the latest workspace snapshot and who changed what.
func (s *Store) WorkspacePath(id string) string { return filepath.Join(s.Dir(id), "workspace.json") }

// BriefPath holds the project brief frozen when the task started. It is
// part of every agent's shared system prompt, which has to stay
// byte-identical to be read from the prompt cache.
func (s *Store) BriefPath(id string) string { return filepath.Join(s.Dir(id), "brief.md") }

// GitHubPath holds the issues and pull requests the prompt links to, as
// fetched when the task was created; the brief includes them.
func (s *Store) GitHubPath(id string) string { return filepath.Join(s.Dir(id), "github.md") }

// DataDir is the data directory the store lives in.
func (s *Store) DataDir() string { return filepath.Dir(s.Root) }

// SummaryPath is the task's end-of-run summary, rewritten whenever a run
// ends: the outcome, what each agent reported and the to-dos.
func (s *Store) SummaryPath(id string) string { return filepath.Join(s.Dir(id), "summary.md") }

// The run files a client can read back over the controller's interface. They
// are named here rather than derived from a request: a task's directory also
// holds its record, its event log, agent conversations and scratch dirs.
const (
	SummaryFile = "summary.md"
	GitHubFile  = "github.md"
	BriefFile   = "brief.md"
)

// ArtifactNames lists the files Artifact serves, by their own names.
var ArtifactNames = []string{SummaryFile, GitHubFile, BriefFile}

// artifactFiles maps every accepted spelling to the file it names. The short
// spelling is accepted because that is what the thing is called when a person
// asks for it - "the summary", not "summary.md" - and the extension is the
// store's business. Nothing here is a path: the value is chosen from this map,
// never built from the request.
var artifactFiles = map[string]string{
	"summary": SummaryFile, SummaryFile: SummaryFile,
	"github": GitHubFile, GitHubFile: GitHubFile,
	"brief": BriefFile, BriefFile: BriefFile,
}

// ErrNoArtifact means the task has no such run file: a name outside
// ArtifactNames, or one no run has written yet. A reader shows nothing for it.
var ErrNoArtifact = errors.New("no such task artifact")

// ArtifactPath names one of the allowlisted run files of task id, and refuses
// everything else. Both halves are client strings on the controller's port, so
// both are checked here: the name against the list, the id by checkID, and the
// answer against the task's own directory - one file inside it, never a path
// that climbs out of it.
func (s *Store) ArtifactPath(id, name string) (string, error) {
	if err := checkID(id); err != nil {
		return "", err
	}
	file, ok := artifactFiles[name]
	if !ok {
		return "", fmt.Errorf("%w: %q (a task has %s)", ErrNoArtifact, name, strings.Join(ArtifactNames, ", "))
	}
	p := filepath.Join(s.Dir(id), file)
	// Neither input can be a path any more, and this says so out loud rather
	// than relying on the two checks agreeing for ever.
	if filepath.Clean(filepath.Dir(p)) != filepath.Clean(s.Dir(id)) {
		return "", fmt.Errorf("%w: %q", ErrNoArtifact, name)
	}
	return p, nil
}

// Artifact reads one of the files ArtifactPath serves. The text comes back as
// it was written: these are Markdown documents, and a client on another machine
// shows them rather than opening a path it does not have.
//
// A refusal names the task and the artifact, never a directory: the path is the
// daemon's layout, and a client cannot open it anyway.
func (s *Store) Artifact(id, name string) (string, error) {
	p, err := s.ArtifactPath(id, name)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%w: %s has no %s yet", ErrNoArtifact, id, name)
	}
	if err != nil {
		// This error can reach a client on another machine, which cannot open
		// the file and has no business being told where the daemon keeps it, so
		// the path goes: the artifact's name and the cause stay.
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return "", fmt.Errorf("read the task's %s: %w", name, err)
	}
	return string(data), nil
}

// ScratchPath is agent n's (0-based) private directory for its commands:
// their HOME, TMPDIR and caches.
func (s *Store) ScratchPath(id string, n int) string {
	return filepath.Join(s.Dir(id), "scratch", fmt.Sprint(n+1))
}

// LogPath is the background runner's output file.
func (s *Store) LogPath(id string) string { return filepath.Join(s.Dir(id), "runner.log") }

// NewID returns a short random task ID like "k3f9qz".
func NewID() string {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	b := make([]byte, 6)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// checkID rejects an ID that cannot name a task. Both backends use it: the
// file store joins the ID onto its tasks root, so a separator would let a
// caller read, write or delete outside the data dir, so a separator would let
// them walk out of it.
//
// IDs come from NewID's safe alphabet. Everything else has to be refused here
// because this is a boundary of its own: a gRPC caller reaches these methods
// without passing through the CLI's checks, and the controller's port is not
// authenticated.
func checkID(id string) error {
	if id == "" {
		return errors.New("no task given")
	}
	if id == "." || id == ".." || strings.ContainsAny(id, ":\\/ \r\n\t\x00") {
		return fmt.Errorf("invalid task id %q", id)
	}
	return nil
}

// Create persists a new task.
func (s *Store) Create(t *Task) error { return s.CreateWithBrief(t, "") }

// CreateWithBrief is Create plus the GitHub context the task's brief starts
// from. The context is written first, so a task that exists always has the
// brief it was created with, whichever host fetched it.
func (s *Store) CreateWithBrief(t *Task, github string) error {
	if err := checkID(t.ID); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(s.Dir(t.ID), "agents"), 0o700); err != nil {
		return err
	}
	if github != "" {
		if err := writeAtomic(s.GitHubPath(t.ID), []byte(github)); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	t.CreatedAt, t.UpdatedAt = now, now
	t.Status = Pending
	t.Round = 1
	t.Started = make([]bool, len(t.Agents))
	if t.Name != "" {
		if err := s.Rename(t, t.Name); err != nil {
			return err
		}
	}
	return s.Save(t)
}

// Save writes the task atomically.
func (s *Store) Save(t *Task) error {
	if err := checkID(t.ID); err != nil {
		return err
	}
	t.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.taskPath(t.ID), data)
}

// Load reads a task by exact ID.
func (s *Store) Load(id string) (*Task, error) {
	if err := checkID(id); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.taskPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("no task %q (see `ackn4th list-tasks`)", id)
	}
	if err != nil {
		return nil, err
	}
	var t Task
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("task %s: %w", id, err)
	}
	t.Name = s.readName(id)
	return &t, nil
}

// Find resolves a task's ID, its name (ignoring case), or a unique ID
// prefix, in that order.
func (s *Store) Find(ref string) (*Task, error) {
	name := strings.ToLower(strings.Join(strings.Fields(ref), " "))
	idOrPrefix := strings.TrimSpace(strings.ToLower(ref))
	if idOrPrefix == "" {
		return nil, errors.New("no task given")
	}
	if !strings.ContainsAny(idOrPrefix, `/\.`) {
		if t, err := s.Load(idOrPrefix); err == nil {
			return t, nil
		}
	}
	ids, err := s.ids()
	if err != nil {
		return nil, err
	}
	var named []string
	for _, id := range ids {
		if strings.ToLower(s.readName(id)) == name {
			named = append(named, id)
		}
	}
	switch len(named) {
	case 1:
		return s.Load(named[0])
	case 0:
	default:
		return nil, fmt.Errorf("several tasks are named %q: use an ID instead (%s)", strings.TrimSpace(ref), strings.Join(named, ", "))
	}
	var match []string
	for _, id := range ids {
		if strings.HasPrefix(id, idOrPrefix) {
			match = append(match, id)
		}
	}
	switch len(match) {
	case 0:
		return nil, fmt.Errorf("no task %q (see `ackn4th list-tasks`)", strings.TrimSpace(ref))
	case 1:
		return s.Load(match[0])
	}
	return nil, fmt.Errorf("%q matches several tasks: %s", idOrPrefix, strings.Join(match, ", "))
}

func (s *Store) ids() ([]string, error) {
	entries, err := os.ReadDir(s.Root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}

// List returns every task, most recently updated first. A task marked
// running whose runner has died is reported as failed.
func (s *Store) List() ([]*Task, error) {
	ids, err := s.ids()
	if err != nil {
		return nil, err
	}
	var out []*Task
	for _, id := range ids {
		t, err := s.Load(id)
		if err != nil {
			continue
		}
		s.Reconcile(t)
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// Reconcile fixes up a task whose runner exited without recording why
// (crash, kill -9, reboot): if it claims to be running but nobody holds
// its lock, it is marked failed so it can be resumed.
func (s *Store) Reconcile(t *Task) {
	if locked, err := s.Locked(t.ID); t.Status == Running && err == nil && !locked {
		t.Status = Failed
		t.Reason = "runner exited unexpectedly"
		t.PID = 0
		s.Save(t)
	}
}

// AppendEvent adds an event to the task's log.
func (s *Store) AppendEvent(id string, e event.Event) error {
	if err := checkID(id); err != nil {
		return err
	}
	f, err := os.OpenFile(s.eventsPath(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	return err
}

// ReadEvents returns events starting at byte offset off, and the offset
// just past the last complete line read.
func (s *Store) ReadEvents(id string, off int64) ([]event.Event, int64, error) {
	if err := checkID(id); err != nil {
		return nil, off, err
	}
	f, err := os.Open(s.eventsPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, off, nil
	}
	if err != nil {
		return nil, off, err
	}
	defer f.Close()
	if _, err := f.Seek(off, 0); err != nil {
		return nil, off, err
	}
	var out []event.Event
	r := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break // EOF or a partial trailing line, re-read next time
		}
		off += int64(len(line))
		var e event.Event
		if json.Unmarshal(line, &e) == nil {
			out = append(out, e)
		}
	}
	return out, off, nil
}

// Switch moves agent n (0-based) to another provider, model and effort. A
// conversation can't move between providers, so the agent starts afresh:
// its saved conversation and workspace baseline are deleted, and its next
// turn is briefed like a first one, with the task and the notes. The
// workspace, the notes and the task's history stay. The caller saves t.
func (s *Store) Switch(t *Task, n int, provider, model, effort string) error {
	if err := checkID(t.ID); err != nil {
		return err
	}
	if locked, err := s.Locked(t.ID); err != nil {
		return err
	} else if locked {
		return fmt.Errorf("task %s is running: stop it first", t.ID)
	}
	for _, p := range []string{s.AgentStatePath(t.ID, n), s.SnapshotPath(t.ID, n)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	a := &t.Agents[n]
	a.Provider, a.Model, a.Effort, a.NotesSeen, a.Context = provider, model, effort, "", 0
	if n < len(t.Started) {
		t.Started[n] = false
	}
	return nil
}

// Delete removes a task's records (never its workspace).
func (s *Store) Delete(id string) error {
	if err := checkID(id); err != nil {
		return err
	}
	if locked, err := s.Locked(id); err != nil {
		return err
	} else if locked {
		return fmt.Errorf("task %s is running: stop it first", id)
	}
	return os.RemoveAll(s.Dir(id))
}

func writeAtomic(path string, data []byte) error {
	// The task's record may live in a shared backend, in which case nothing
	// has created this host's copy of its directory yet (agent state and
	// snapshots are written here the first time a remote runner works).
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
