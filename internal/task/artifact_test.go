package task

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// artifactTask is a task whose run has written nothing yet.
func artifactTask() *Task {
	return &Task{ID: NewID(), Prompt: "fix the thing", Agents: []AgentSpec{{Role: "backend_dev", Provider: "openai", Model: "m"}}}
}

// A task's directory holds more than the files a client may read: the record,
// the event log, agent conversations, the runner's log, scratch dirs. The name
// in an Artifact request arrives from a client over an unauthenticated port, so
// only the allowlist is served - whatever the request calls the file.
func TestStoreArtifactServesOnlyTheAllowlist(t *testing.T) {
	s := NewStore(t.TempDir())
	tk := artifactTask()
	if err := s.Create(tk); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.SummaryPath(tk.ID), []byte("# done\nall green\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Either the file's own name or the short one: the artifact is "the
	// summary", and the extension is the store's business.
	for _, name := range []string{"summary", SummaryFile} {
		got, err := s.Artifact(tk.ID, name)
		if err != nil {
			t.Fatalf("summary as %q: %v", name, err)
		}
		if got != "# done\nall green\n" {
			t.Errorf("summary as %q = %q", name, got)
		}
	}
	// Whatever the name, the answer is one file inside that task's directory -
	// the id is a client's string on the controller's port too.
	if p, err := s.ArtifactPath(tk.ID, "summary"); err != nil || filepath.Dir(p) != filepath.Clean(s.Dir(tk.ID)) {
		t.Errorf("ArtifactPath = %q, %v; want a file directly in %s", p, err, s.Dir(tk.ID))
	}

	for _, name := range []string{
		"task.json", "events.jsonl", "runner.log", "agents/1.json",
		"../" + tk.ID + "/task.json", "..", ".", "", "/etc/passwd",
		"summary.md/../task.json",
	} {
		data, err := s.Artifact(tk.ID, name)
		if !errors.Is(err, ErrNoArtifact) {
			t.Errorf("Artifact(%q) = %q, %v; want ErrNoArtifact", name, data, err)
		}
		if data != "" {
			t.Errorf("Artifact(%q) served %q", name, data)
		}
	}

	// A file the allowlist names, but no run has written yet, is absent too -
	// and the error does not carry this host's path.
	if _, err := s.Artifact(tk.ID, GitHubFile); !errors.Is(err, ErrNoArtifact) {
		t.Errorf("missing github.md: %v; want ErrNoArtifact", err)
	} else if strings.Contains(err.Error(), s.Dir(tk.ID)) {
		t.Errorf("a missing artifact's error names the path: %v", err)
	}

	// The task id is a path segment, so it is checked the same way every other
	// method checks it.
	if _, err := s.Artifact("../"+tk.ID, SummaryFile); err == nil {
		t.Error("an id outside the tasks root was served")
	}
}

// CreateWithBrief is how a client on another machine hands the daemon the
// GitHub context its brief starts from: it has to land next to the record, not
// in the client's data dir (R62).
func TestCreateWithBriefStoresTheGitHubContext(t *testing.T) {
	s := NewStore(t.TempDir())
	tk := artifactTask()
	if err := s.CreateWithBrief(tk, "### issue 3: broken\n"); err != nil {
		t.Fatalf("create with brief: %v", err)
	}
	got, err := os.ReadFile(s.GitHubPath(tk.ID))
	if err != nil {
		t.Fatalf("github.md was not written: %v", err)
	}
	if string(got) != "### issue 3: broken\n" {
		t.Errorf("github.md = %q", got)
	}
	if _, err := s.Load(tk.ID); err != nil {
		t.Errorf("the record was not written: %v", err)
	}

	// A plain Create writes no github.md: a task with no GitHub links in its
	// prompt must not get an empty brief file that reads as "gh said nothing".
	plain := artifactTask()
	if err := s.Create(plain); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Artifact(plain.ID, GitHubFile); !errors.Is(err, ErrNoArtifact) {
		t.Errorf("a plain Create wrote github.md: %v", err)
	}
}

// The whole point of the two calls above is that they cross the interface: a
// client's data dir is not where a remote runner looks, so the context travels
// with the record and the summary comes back from the host that wrote it.
func TestGRPCArtifactsComeFromTheDaemon(t *testing.T) {
	g, daemon := g27(t)
	tk := artifactTask()
	if err := g.CreateWithBrief(tk, "### issue 3: broken\n"); err != nil {
		t.Fatalf("create with brief: %v", err)
	}
	if _, err := os.Stat(daemon.GitHubPath(tk.ID)); err != nil {
		t.Errorf("the daemon has no github.md: %v", err)
	}
	if _, err := os.Stat(g.local.GitHubPath(tk.ID)); err == nil {
		t.Error("the client's data dir got a github.md: a remote runner never reads it")
	}
	if got, err := g.Artifact(tk.ID, GitHubFile); err != nil || got != "### issue 3: broken\n" {
		t.Errorf("github.md over the wire = %q, %v", got, err)
	}

	// The summary the daemon's run wrote is readable from here, and its text
	// arrives unchanged.
	if err := os.WriteFile(daemon.SummaryPath(tk.ID), []byte("done: all green\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := g.Artifact(tk.ID, SummaryFile); err != nil || got != "done: all green\n" {
		t.Errorf("summary over the wire = %q, %v", got, err)
	}

	// A file no run has written yet is "not there", not a failure: the CLI
	// shows nothing for it.
	if _, err := g.Artifact(tk.ID, BriefFile); !errors.Is(err, ErrNoArtifact) {
		t.Errorf("missing brief.md: %v; want ErrNoArtifact", err)
	}
	// And a name outside the allowlist never reaches the wire's payload - the
	// record is right next to the summary on the daemon's disk.
	if data, err := g.Artifact(tk.ID, "task.json"); err == nil || data != "" {
		t.Errorf("Artifact(task.json) = %q, %v; want a refusal", data, err)
	}
	// A task the daemon does not have says so, too.
	if _, err := g.Artifact(NewID(), SummaryFile); !errors.Is(err, ErrNoArtifact) {
		t.Errorf("a missing task: %v; want ErrNoArtifact", err)
	}
}

// A refusal that names itself twice is a message a person reads twice and
// believes once. The daemon wraps the sentinel into the text it sends, so the
// client must not wrap it again: one sentence, and errors.Is still true (R77).
func TestTheMissingArtifactTextIsSaidOnce(t *testing.T) {
	g, _ := g27(t)
	tk := artifactTask()
	if err := g.Create(tk); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err := g.Artifact(tk.ID, SummaryFile)
	if !errors.Is(err, ErrNoArtifact) {
		t.Fatalf("no summary yet: %v; want ErrNoArtifact", err)
	}
	const sentinel = "no such task artifact"
	if n := strings.Count(err.Error(), sentinel); n != 1 {
		t.Errorf("the sentinel is said %d times: %q", n, err)
	}
	if !strings.Contains(err.Error(), tk.ID) || !strings.Contains(err.Error(), SummaryFile) {
		t.Errorf("the refusal does not say which task and which file: %q", err)
	}

	// The same sentence, without a controller in between: the local caller
	// reads what the remote one does, because it is one rule and not two.
	local := NewStore(t.TempDir())
	lt := artifactTask()
	if err := local.Create(lt); err != nil {
		t.Fatal(err)
	}
	_, lerr := local.Artifact(lt.ID, SummaryFile)
	if !errors.Is(lerr, ErrNoArtifact) {
		t.Fatalf("the store's own refusal: %v; want ErrNoArtifact", lerr)
	}
	if n := strings.Count(lerr.Error(), sentinel); n != 1 {
		t.Errorf("the store says the sentinel %d times: %q", n, lerr)
	}
}
