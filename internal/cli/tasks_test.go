package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dethmasque/ackn4th/internal/config"
	"github.com/dethmasque/ackn4th/internal/dash"
	"github.com/dethmasque/ackn4th/internal/task"
	"github.com/dethmasque/ackn4th/internal/tips"
	"github.com/dethmasque/ackn4th/internal/ui"
)

func TestParseAgent(t *testing.T) {
	r, err := parseAgent("backend-dev, provider=Claude, model=claude-sonnet-5-5, effort=MEDIUM, compact_at=150k, max_tokens=32000")
	if err != nil {
		t.Fatal(err)
	}
	if r.role != "backend_dev" || r.set.Provider != "claude" || r.set.Model != "claude-sonnet-5-5" ||
		r.set.Effort != "medium" || r.set.CompactAt != 150000 || r.set.MaxTokens != 32000 {
		t.Fatalf("parsed %+v", r)
	}
	if r, _ := parseAgent("role=qa_engineer,profile=fast"); r.role != "qa_engineer" || r.profile != "fast" {
		t.Fatalf("parsed %+v", r)
	}
	for _, bad := range []string{"", "model=x", "qa_engineer,temperature=1", "qa_engineer,oops", "qa_engineer,compact_at=lots"} {
		if _, err := parseAgent(bad); err == nil {
			t.Errorf("parseAgent(%q) should fail", bad)
		}
	}
	if n, _ := parseTokens("1.5m"); n != 1500000 {
		t.Errorf("1.5m = %d", n)
	}
}

func TestPick(t *testing.T) {
	opts := []option{{"claude", ""}, {"gemini", ""}, {"backend_dev", ""}}
	for in, want := range map[string]string{"1": "claude", "2": "gemini", "Gemini": "gemini", "backend-dev": "backend_dev"} {
		if got, ok := pick(in, opts); !ok || got != want {
			t.Errorf("pick(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"0", "4", "openai"} {
		if _, ok := pick(in, opts); ok {
			t.Errorf("pick(%q) accepted", in)
		}
	}
}

func TestNoInputFailsFast(t *testing.T) {
	missing := errors.New("missing thing")
	for _, a := range []*app{{noInput: true}, {}} { // the flag; and tests have no terminal
		if _, err := a.ask(missing, "thing", nil); err != missing {
			t.Errorf("ask = %v, want the original error", err)
		}
		if _, err := a.choose(missing, "X", []option{{"a", ""}}, false); err != missing {
			t.Errorf("choose = %v, want the original error", err)
		}
		if err := a.confirm(missing, "ok?"); err != missing {
			t.Errorf("confirm = %v, want the original error", err)
		}
	}
	t.Setenv(NoInputEnv, "1")
	if (&app{}).interactive() {
		t.Error(NoInputEnv + "=1 ignored")
	}
}

func TestPlanSwitchToClaudeCode(t *testing.T) {
	a := &app{cfg: config.Default()}
	tk := &task.Task{Agents: []task.AgentSpec{
		{Role: "backend_dev", Provider: "claude", Model: "claude-opus-5-5", Effort: "high"},
		{Role: "qa_engineer", Provider: "gemini", Model: "gemini-3.1-pro-preview"},
		{Role: "ux_designer", Provider: "claude-code", Model: "claude-haiku-5-5", Effort: "low"},
	}}
	moves, err := a.planSwitch(tk, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	want := []move{{0, "claude-code", "claude-opus-5-5", "medium"}, {1, "claude-code", "claude-sonnet-5-5", "medium"}}
	if !slices.Equal(moves, want) {
		t.Errorf("moves = %v, want %v", moves, want)
	}
	if got := taskProviders(tk, moves); !slices.Equal(got, []string{"claude-code", "claude-code", "claude-code"}) {
		t.Errorf("providers = %v", got)
	}
}

// The home directory can't be trusted under any spelling of its path.
func TestTrustableRefusesHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	for _, d := range []string{home, home + string(filepath.Separator), filepath.Dir(home), strings.ToUpper(home), strings.ToLower(home)} {
		if _, err := os.Stat(d); err != nil {
			continue // another case doesn't exist on a case-sensitive filesystem
		}
		if _, err := trustable(d); err == nil {
			t.Errorf("trusted %s", d)
		}
	}
}

// The dashboard gets the provider-setup screen (its `p` key and first-run
// offer).
func TestDashOffersSetup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := &app{cfg: config.Default(), term: ui.NewTerm(io.Discard, false), tips: tips.Load(t.TempDir(), true)}
	opt, err := a.dashOptions(t.Context(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if opt.Setup == nil || opt.Setup.Status == nil || opt.Setup.Done == nil {
		t.Errorf("setup = %+v", opt.Setup)
	}
}

// A follow-up can change agents' roles, models and efforts: a new model
// starts that agent's conversation fresh, and every agent is told.
func TestChangeTeam(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	st := task.NewStore(t.TempDir())
	a := &app{cfg: config.Default(), be: st, store: st}
	tk := &task.Task{ID: task.NewID(), Prompt: "p", Workspace: t.TempDir(), Status: task.Completed, Agents: []task.AgentSpec{
		{Role: "backend_dev", Provider: "claude", Model: "claude-opus-5-5", Effort: "high"},
		{Role: "qa_engineer", Provider: "claude", Model: "claude-opus-5-5", Effort: "high"},
	}}
	if err := a.store.Create(tk); err != nil {
		t.Fatal(err)
	}
	tk.Started = []bool{true, true}
	if err := tk.AddFollowUp("more", -1, time.Now()); err != nil {
		t.Fatal(err)
	}
	same := []dash.NewAgent{
		{Role: "backend_dev", Provider: "claude", Model: "claude-opus-5-5", Effort: "high"},
		{Role: "qa_engineer", Provider: "claude", Model: "claude-opus-5-5", Effort: "high"},
	}
	if err := a.changeTeam(tk, same); err != nil || tk.FollowUps[0].Changes != nil {
		t.Fatalf("no change: %v %v", err, tk.FollowUps[0].Changes)
	}
	team := []dash.NewAgent{
		{Role: "backend_dev+tech_lead", Provider: "claude", Model: "claude-opus-5-5", Effort: "max"},
		{Role: "qa_engineer", Provider: "claude", Model: "claude-sonnet-5-5", Effort: "high"},
	}
	if err := a.changeTeam(tk, team); err != nil {
		t.Fatal(err)
	}
	b, q := tk.Agents[0], tk.Agents[1]
	if b.Role != "backend_dev+tech_lead" || b.Effort != "max" || !tk.Started[0] {
		t.Errorf("a new role and effort keep the conversation: %+v started %v", b, tk.Started[0])
	}
	if q.Model != "claude-sonnet-5-5" || tk.Started[1] {
		t.Errorf("a new model starts fresh: %+v started %v", q, tk.Started[1])
	}
	ch := strings.Join(tk.FollowUps[0].Changes, "\n")
	for _, w := range []string{"agent 1 is now backend_dev+tech_lead (was backend_dev), uses max effort (was high)", "agent 2 runs on model claude-sonnet-5-5"} {
		if !strings.Contains(ch, w) {
			t.Errorf("want %q in %q", w, ch)
		}
	}
	bad := [][]dash.NewAgent{
		team[:1],
		{team[0], {Role: "qa_engineer", Provider: "gemini", Model: "x"}},
		{team[0], {Role: "no_such_role", Provider: "claude", Model: "claude-opus-5-5"}},
	}
	for i, b := range bad {
		if err := a.changeTeam(tk, b); err == nil {
			t.Errorf("%d: should fail", i)
		}
	}
}
