package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dethmasque/ackn4th/internal/github"
	"github.com/dethmasque/ackn4th/internal/task"
	"github.com/dethmasque/ackn4th/internal/workspace"
)

// GitHub, through the GitHub CLI's own login: issues and pull requests the
// prompt links to go into the brief, --repo clones with gh's credentials,
// and --draft-pr (or open-pr) proposes the agents' changes.

// repoPath is where a clone of repo goes on this host: its name inside the
// first workspace root, the same place --scratch puts a task. Both ends agree
// because both call this - the client to say where the work will happen, the
// controller to know which directory to make.
func (a *app) repoPath(repo string, allowed []string) (string, error) {
	owner, name, err := github.ParseRepo(repo)
	if err != nil {
		return "", err
	}
	if owner == "" || name == "" {
		return "", fmt.Errorf("%q is not a repository", repo)
	}
	// The name becomes a directory inside a workspace root, so it has to be
	// one: "." is the root itself and ".." is its parent. ParseRepo's pattern
	// lets both through ("owner/.", "owner/..", and "owner/..git" whose name
	// is "."), and since the daemon computes this path and records it, that is
	// now a decision about which directory to work in, not a display detail.
	if strings.Trim(name, ".") == "" {
		return "", fmt.Errorf("github: %q is not a repository name", repo)
	}
	if len(allowed) == 0 {
		return "", fmt.Errorf("no workspace roots are configured for --repo: set workspace_roots on the host that runs the tasks")
	}
	dir := filepath.Join(allowed[0], name)
	if filepath.Dir(dir) != filepath.Clean(allowed[0]) {
		return "", fmt.Errorf("github: %q would not clone inside %s", repo, allowed[0])
	}
	return dir, nil
}

// repoDir is where --repo works on repo: its clone in the default
// workspace root, and whether one is there already. It changes nothing,
// so the new-task form can show it as you type.
func (a *app) repoDir(repo string) (dir string, exists bool, err error) {
	allowed, err := a.cfg.AllowedRoots()
	if err != nil {
		return "", false, err
	}
	if dir, err = a.repoPath(repo, allowed); err != nil {
		return "", false, err
	}
	if a.remote() {
		// Where the daemon will put the clone. Whether one is already there is
		// a question for that host, so the form shows "clones to" and not
		// "reuses the clone".
		return dir, false, nil
	}
	_, err = os.Stat(filepath.Join(dir, ".git"))
	return dir, err == nil, nil
}

// repoWorkspace clones owner/name into the default workspace root, or
// reuses an existing clone there, and returns it.
func (a *app) repoWorkspace(ctx context.Context, repo string, allowed []string, quiet bool) (string, error) {
	owner, name, err := github.ParseRepo(repo)
	if err != nil {
		return "", err
	}
	// One rule for where a clone of owner/name goes: repoPath's. That is what
	// the client records as the task's workspace and what prepareWorkspace
	// names before the clone; deriving it again here was a second answer, and
	// "owner/.." named the root's parent (R75).
	dir, err := a.repoPath(repo, allowed)
	if err != nil {
		return "", err
	}
	say := func(s string) {
		if !quiet {
			a.term.Println(a.term.Gray("  " + s))
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		if !github.IsClone(ctx, dir, owner, name) {
			return "", fmt.Errorf("%s is a clone of another repository, not %s/%s: move it, or pass --workspace", dir, owner, name)
		}
		say("using the clone of " + owner + "/" + name + " at " + tildeHome(dir))
	} else {
		if _, err := os.Stat(dir); err == nil {
			return "", fmt.Errorf("%s exists and isn't a git clone: move it, or pass --workspace", dir)
		}
		say("cloning " + owner + "/" + name + " with gh into " + tildeHome(dir) + " …")
		cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		if err := github.Clone(cctx, owner, name, dir); err != nil {
			return "", err
		}
	}
	return workspace.Resolve(dir, allowed, true)
}

// useBranch puts the workspace on branch, when one is given, and says
// what it did.
func (a *app) useBranch(ctx context.Context, ws, branch string, quiet bool) error {
	if branch == "" {
		return nil
	}
	// A client cannot see the daemon's checkout and must not stat or switch
	// it: `--repo owner/name --branch feature/x` failed here with
	// "--branch: /srv/… isn't a git repository", naming a path the person
	// never typed. The checkout happens on the machine with the clone, and the
	// task's record carries the branch.
	if a.remote() {
		return nil
	}
	if _, err := os.Stat(filepath.Join(ws, ".git")); err != nil {
		return fmt.Errorf("--branch: %s isn't a git repository", ws)
	}
	bctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	created, err := github.Checkout(bctx, ws, branch)
	if err != nil {
		return err
	}
	if !quiet {
		what := "on branch "
		if created {
			what = "created branch "
		}
		a.term.Println(a.term.Gray("  " + what + branch))
	}
	return nil
}

// prepareWorkspace makes the task's workspace ready on this host, where the
// work will happen: it clones the repository with this host's gh login, and
// records where the clone actually landed. The client cannot do any of it - it
// has no path here, and its github.md would go to a data dir no runner reads.
//
// It is safe on a task with no repository and no branch, and safe to call
// twice: a clone that already exists is reused.
func (a *app) prepareWorkspace(ctx context.Context, tk *task.Task) error {
	if tk.Repo == "" {
		return nil
	}
	allowed, err := a.cfg.AllowedRoots()
	if err != nil {
		return err
	}
	// Record where the clone is meant to land before making it, so a crash
	// mid-clone leaves a task that says where it was going rather than nowhere.
	want, err := a.repoPath(tk.Repo, allowed)
	if err != nil {
		return err
	}
	if err := a.recordWorkspace(tk.ID, want); err != nil {
		return err
	}
	tk.Workspace = want
	// A clone of a big repository takes minutes. Save only this field, and
	// only from a record read after the clone: rewriting the copy this call
	// started with would undo anything changed while it ran (R38).
	dir, err := a.repoWorkspace(ctx, tk.Repo, allowed, true)
	if err != nil {
		return err
	}
	if err := a.recordWorkspace(tk.ID, dir); err != nil {
		return err
	}
	tk.Workspace = dir
	return nil
}

// recordWorkspace points a task at dir, re-reading the record first so that a
// save made after a long clone touches one field and nothing else.
func (a *app) recordWorkspace(id, dir string) error {
	fresh, err := a.backend().Load(id)
	if err != nil {
		return err
	}
	if fresh.Workspace == dir {
		return nil
	}
	fresh.Workspace = dir
	if err := a.backend().Save(fresh); err != nil {
		return fmt.Errorf("record the workspace %s: %w", dir, err)
	}
	return nil
}

// remoteVCSErr names the two flags a client cannot honour. The branch is
// checked out, and the pull request opened, where the workspace is - on the
// machine running the controller - and this build has no request that asks it
// to. Accepting them quietly would be worse than refusing: the record carries
// the name, nothing on the host acts on it, and the agents' commits land on
// whichever branch the clone already had.
func remoteVCSErr(branch string, draftPR bool) error {
	switch {
	case branch != "":
		return errors.New("--branch: the branch is checked out where the workspace is, on the controller, and this build cannot ask it to switch yet: check it out on that host, or drop --branch")
	case draftPR:
		return errors.New("--draft-pr: the pull request is opened where the workspace is, on the controller, and this build cannot ask it to yet: drop --draft-pr, or run the task on that host")
	}
	return nil
}

// checkDraftPR is --draft-pr's preflight: gh must be signed in and the
// workspace must be a clone of a GitHub repository. Like useBranch it only
// means something where the workspace is, so a client skips it.
func (a *app) checkDraftPR(ctx context.Context, ws string) error {
	if a.remote() {
		return nil
	}
	return github.CheckPR(ctx, ws)
}

// githubBrief reads the issues and pull requests the prompt links to, for the
// brief. It runs where gh is signed in - usually the person's machine, which is
// the client - and returns the text; createTask then sends it to the host that
// runs the task, because a client's data dir is not where a runner looks (R62).
//
// Without gh it says so and goes on: the agents then see only the links. A link
// gh can't read stops the task from starting.
func (a *app) githubBrief(ctx context.Context, prompt string, quiet bool) (string, error) {
	refs := github.Refs(prompt)
	if len(refs) == 0 {
		return "", nil
	}
	t := a.term
	if _, err := github.Status(ctx); err != nil {
		if !quiet {
			t.Println(t.Amber("  ! "), inline(t, "the prompt links to GitHub, but "+strings.TrimPrefix(err.Error(), "github: ")+"; agents will see only the links, as they have no network"))
		}
		return "", nil
	}
	fctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	md, err := github.Context(fctx, refs)
	if err != nil {
		return "", err
	}
	if !quiet {
		var names []string
		for _, r := range refs {
			names = append(names, fmt.Sprintf("%s/%s#%d", r.Owner, r.Repo, r.Number))
		}
		t.Println(t.Green("  ✓ "), t.Gray("read from GitHub into the brief: "+strings.Join(names, ", ")))
	}
	return md, nil
}

// createTask creates tk and stores the GitHub context next to it, wherever the
// task will run: the controller carries the file over gRPC, so a client never
// writes its own copy (Briefed). A backend that cannot carry one is
// given a plain Create and the file goes in this host's data dir, which is where
// its runners read - they run here.
func (a *app) createTask(tk *task.Task, ghContext string) error {
	if ghContext != "" {
		if br, ok := a.backend().(task.Briefed); ok {
			return br.CreateWithBrief(tk, ghContext)
		}
	}
	if err := a.backend().Create(tk); err != nil {
		return err
	}
	if ghContext == "" {
		return nil
	}
	return os.WriteFile(a.store.GitHubPath(tk.ID), []byte(ghContext), 0o600)
}

// openPR proposes the agents' changes as a draft pull request and records
// its URL on the task.
func (a *app) openPR(ctx context.Context, tk *task.Task, files []string) error {
	// The task's name, when it has one, is the title; else the prompt's
	// first line.
	title := tk.Name
	if title == "" {
		title = strings.TrimSpace(strings.SplitN(tk.Prompt, "\n", 2)[0])
	}
	if r := []rune(title); len(r) > 72 {
		title = string(r[:71]) + "…"
	}
	var body strings.Builder
	fmt.Fprintf(&body, "Drafted by [ackn4th](https://github.com/dethmasque/ackn4th) task %s (`%s`): AI agents wrote these changes. Review them before merging.\n\n", tk.Title(), tk.ID)
	fmt.Fprintf(&body, "## Prompt\n\n> %s\n", strings.ReplaceAll(strings.TrimSpace(tk.Prompt), "\n", "\n> "))
	if tk.Reason != "" {
		fmt.Fprintf(&body, "\n## Outcome\n\n%s\n", tk.Reason)
	}
	if open := tk.OpenTodos(); len(open) > 0 {
		body.WriteString("\n## Left for a person\n\n")
		for _, td := range open {
			fmt.Fprintf(&body, "- [ ] %s\n", td.Text)
		}
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	pr := github.PR{Branch: "ackn4th/" + tk.ID, Title: title, Body: body.String(), Files: files}
	if tk.Branch != "" {
		// Commit on the task's own branch and propose it for the default
		// one, unless that is the default branch itself.
		def, err := github.DefaultBranch(pctx, tk.Workspace)
		if err != nil {
			return err
		}
		if tk.Branch != def {
			pr.Branch, pr.Existing, pr.Base = tk.Branch, true, def
		}
	}
	url, err := github.DraftPR(pctx, tk.Workspace, pr)
	if err != nil {
		return err
	}
	tk.PR = url
	return nil
}

// completed is the runner's hook: a task started with --draft-pr opens its
// pull request when every agent is done.
func (a *app) completed(r *task.Runner) func(*task.Task) {
	return func(tk *task.Task) {
		if !tk.DraftPR || tk.PR != "" {
			return
		}
		if err := a.openPR(context.Background(), tk, r.ChangedFiles(tk.ID)); err != nil {
			tk.Reason = strings.TrimSpace(tk.Reason + " · the draft pull request wasn't opened: " + err.Error() + " (fix that, then `ackn4th open-pr " + tk.ID + "`)")
		}
	}
}

func openPRCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "open-pr <task>",
		GroupID: "tasks",
		Short:   "Propose a task's changes as a draft GitHub pull request",
		Long: `Commit the files the task's agents changed (only those), push them to
origin and open a draft pull request, with the prompt and outcome as its
description:

  - a task started with --branch: the commit goes on that branch, proposed
    for the repository's default branch;
  - otherwise: on a new branch, ackn4th/<task-id>, proposed for the branch
    the workspace is on.

It uses the GitHub CLI's login (` + "`gh auth login`" + `), so it works even
where plain git isn't signed in to GitHub. start-task --draft-pr does this
when the task completes.`,
		Example: `  ackn4th open-pr k3f9qz`,
		Args:    upTo(1, "<task>"),
		RunE: func(cmd *cobra.Command, args []string) error {
			tk, err := a.taskArg(args, func(tk *task.Task) bool { return tk.PR == "" && tk.Status != task.Running })
			if err != nil {
				return err
			}
			a.backend().Reconcile(tk)
			if tk.PR != "" {
				return fmt.Errorf("task %s already has a draft pull request: %s", tk.Label(), tk.PR)
			}
			if tk.Status == task.Running {
				return errors.New("the task is still running: stop it, or let it finish, first")
			}
			r, err := a.runner()
			if err != nil {
				return err
			}
			if err := a.openPR(cmd.Context(), tk, r.ChangedFiles(tk.ID)); err != nil {
				return err
			}
			if err := a.backend().Save(tk); err != nil {
				return err
			}
			a.term.Println(a.term.Green("✓ "), "draft pull request: ", a.term.Lime(tk.PR))
			return nil
		},
	}
}
