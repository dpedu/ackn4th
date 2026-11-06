package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dethmasque/ackn4th/internal/config"
	"github.com/dethmasque/ackn4th/internal/notes"
	"github.com/dethmasque/ackn4th/internal/onboard"
	"github.com/dethmasque/ackn4th/internal/ui"
	"github.com/dethmasque/ackn4th/internal/workspace"
)

// projectDir resolves a workspace argument (default: the current
// directory, or a task's workspace by ID) to the real path tasks use, so
// notes match. Only the operator reads the result, so it need not be
// trusted.
func (a *app) projectDir(args []string) (string, error) {
	dir := "."
	if len(args) > 0 {
		dir = args[0]
		// A directory that exists wins over a task of the same name.
		if abs, err := config.ExpandPath(dir); err == nil {
			if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
				return filepath.EvalSymlinks(abs)
			}
		}
		if tk, err := a.backend().Find(dir); err == nil {
			return tk.Workspace, nil
		}
	}
	abs, err := config.ExpandPath(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

func notesCmd(a *app) *cobra.Command {
	var clear, edit, showPath bool
	cmd := &cobra.Command{
		Use:     "notes [dir|task]",
		GroupID: "project",
		Short:   "Show or edit a project's shared notes",
		Long: `Show a project's shared notes: what agents have learned about the project
(architecture, conventions, how to build and test, decisions, gotchas),
written with their update_notes tool. Every agent on every task in the
workspace reads them when it joins, so they carry knowledge from task to
task.

Seed or correct them yourself with --edit (opens $VISUAL or $EDITOR): a few lines on
how the project is built and tested save every agent the discovery. Notes
live in ackn4th's data dir, not in the project.`,
		Example: `  ackn4th notes                  # the current directory's project
  ackn4th notes ~/Git/ackn4th --edit
  ackn4th notes k3f9qz           # the workspace of task k3f9qz
  ackn4th notes --clear`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t := a.term
			dir, err := a.projectDir(args)
			if err != nil {
				return err
			}
			book := notes.Open(a.store.DataDir(), dir)
			switch {
			case showPath:
				fmt.Println(book.Path())
				return nil
			case clear:
				if err := book.Clear(); err != nil {
					return err
				}
				t.Println(t.Green("✓ "), "cleared notes for ", t.Lime(dir))
				return nil
			case edit:
				return editNotes(book)
			}
			text, err := book.Read()
			if err != nil {
				return err
			}
			if text == "" {
				t.Box("NOTES", []string{
					t.Gray("no notes yet for ") + t.Lime(dir),
					t.Gray("agents write them as they learn; seed them with ") + t.Lime("ackn4th notes --edit"),
				})
				return nil
			}
			var lines []string
			for _, l := range strings.Split(strings.TrimSpace(text), "\n") {
				switch {
				case strings.HasPrefix(l, "# "):
					continue
				case strings.HasPrefix(l, "## "):
					lines = append(lines, t.Bold(t.Lime(strings.TrimPrefix(l, "## "))))
				case strings.HasPrefix(l, "_— "):
					lines = append(lines, t.Gray(strings.Trim(l, "_")))
				default:
					for _, w := range ui.Wrap(l, t.Width-8) {
						lines = append(lines, inline(t, w))
					}
				}
			}
			t.Box("NOTES · "+dir, lines)
			t.Println(t.Gray(fmt.Sprintf("  %s of %s · %s", ui.Bytes(len(text)), ui.Bytes(notes.MaxBytes), book.Path())))
			return nil
		},
	}
	cmd.Flags().BoolVar(&edit, "edit", false, "edit the notes in your editor ($VISUAL or $EDITOR)")
	cmd.Flags().BoolVar(&clear, "clear", false, "delete the notes")
	cmd.Flags().BoolVar(&showPath, "path", false, "print the notes file's path")
	return cmd
}

func editNotes(book *notes.Book) error {
	text, err := book.Read()
	if err != nil {
		return err
	}
	if text == "" {
		text = "# Project notes\n\n## Build and test\n\n\n## Conventions\n\n"
	}
	if err := os.MkdirAll(filepath.Dir(book.Path()), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(book.Path(), []byte(text), 0o600); err != nil {
		return err
	}
	c, err := editorCmd(book.Path())
	if err != nil {
		return err
	}
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("%s: %w", c.Path, err)
	}
	if after, _ := book.Read(); len(after) > notes.MaxBytes {
		return errors.New("notes are over the 24 KB limit; agents will be unable to update them until they are shortened")
	}
	return nil
}

func briefCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "brief [dir|task]",
		GroupID: "project",
		Short:   "Preview the onboarding brief agents get for a project",
		Long: `Print the project brief an agent receives on its first turn: the
workspace layout, languages, agent guidance files (AGENTS.md, CLAUDE.md, ...),
the README, build manifests and CI. It is generated locally, without any
model calls. Add an AGENTS.md to steer every agent that joins.`,
		Example: `  ackn4th brief
  ackn4th brief ~/Git/ackn4th | less`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := a.projectDir(args)
			if err != nil {
				return err
			}
			ws, err := workspace.Open(dir, nil, a.cfg.Limits.MaxFileBytes)
			if err != nil {
				return err
			}
			defer ws.Close()
			brief := onboard.Brief(ws, onboard.BriefBytes)
			fmt.Print(brief)
			a.term.Println(a.term.Gray(fmt.Sprintf("\n── %s, about %s tokens", ui.Bytes(len(brief)), ui.Tokens(int64(len(brief)/4)))))
			return nil
		},
	}
}

// trustable checks dir can be a workspace root and returns its real path.
// The filesystem root and the home directory are refused: trusting them
// would let agents read everything.
func trustable(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("%s: %w", dir, err)
	}
	if info, err := os.Stat(real); err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	home, _ := os.UserHomeDir()
	if hr, err := filepath.EvalSymlinks(home); err == nil {
		home = hr
	}
	// Compared as files, not strings: on macOS and Windows another case
	// (/users/me) names the same directory.
	same := func(a, b string) bool {
		ai, err1 := os.Stat(a)
		bi, err2 := os.Stat(b)
		return err1 == nil && err2 == nil && os.SameFile(ai, bi)
	}
	if real == filepath.Dir(real) || same(real, home) || same(real, filepath.Dir(home)) || same(real, filepath.VolumeName(real)+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing to trust %s: too broad, it would expose every file under it; trust a project directory instead", real)
	}
	return real, nil
}

// trust adds dir to workspace_roots in config.yaml.
func (a *app) trust(dir string) (string, error) {
	real, err := trustable(dir)
	if err != nil {
		return "", err
	}
	allowed, err := a.cfg.AllowedRoots()
	if err != nil {
		return "", err
	}
	if _, err := workspace.Resolve(real, allowed, false); err == nil {
		return real, nil // already inside a trusted root
	}
	a.cfg.WorkspaceRoots = append(a.cfg.WorkspaceRoots, real)
	if _, err := config.Save(a.cfg); err != nil {
		return "", err
	}
	return real, nil
}

// askTrust asks the operator, on a terminal, whether agents may work in dir.
func (a *app) askTrust(dir string) error {
	t := a.term
	real, err := trustable(dir)
	if err != nil {
		return err
	}
	if !a.interactive() {
		return fmt.Errorf("%s is not trusted yet: run `ackn4th trust %s` first", real, dir)
	}
	t.Box("TRUST THIS DIRECTORY?", []string{
		t.Bold(t.Lime(real)),
		"",
		"Agents working here can read its files (except credential files such",
		"as .env and keys) and send them to their model provider, and change",
		"the files their roles allow. They can run builds and tests here in",
		"an OS sandbox: commands may change files in this directory, but",
		"nothing outside it, and can't read your credentials.",
		"",
		t.Gray("Saved to workspace_roots in config.yaml; undo with ") + t.Lime("ackn4th untrust"),
	})
	if a.confirm(nil, "trust it?") != nil {
		return errors.New("not trusted; nothing was started")
	}
	if _, err := a.trust(real); err != nil {
		return err
	}
	t.Println(t.Green("✓ "), "trusted ", t.Lime(real))
	return nil
}

func trustCmd(a *app) *cobra.Command {
	var list bool
	cmd := &cobra.Command{
		Use:     "trust [dir]",
		GroupID: "project",
		Short:   "Let agents work in a directory (default: the current one)",
		Long: `Add a directory to workspace_roots so tasks can run in it and everything
below it. ` + "`start-task`" + ` asks the first time you use a new directory; trust it
ahead of time for background or scripted runs. The filesystem root and your
home directory are refused.`,
		Example: `  cd ~/Git/my-project && ackn4th trust
  ackn4th trust ~/Git
  ackn4th trust --list`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t := a.term
			if list {
				roots, err := a.cfg.AllowedRoots()
				if err != nil {
					return err
				}
				for i, r := range roots {
					tag := ""
					if i == 0 {
						tag = t.Gray("  (default_workspace_root, for --scratch)")
					}
					t.Println("  ", t.Lime(r), tag)
				}
				return nil
			}
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			real, err := a.trust(dir)
			if err != nil {
				return err
			}
			t.Println(t.Green("✓ "), "trusted ", t.Lime(real))
			return nil
		},
	}
	cmd.Flags().BoolVarP(&list, "list", "l", false, "list trusted directories")
	return cmd
}

func untrustCmd(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "untrust [dir]",
		GroupID: "project",
		Short:   "Remove a directory from the trusted workspace roots",
		Example: `  ackn4th untrust
  ackn4th untrust ~/Git/old-project`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			abs, err := config.ExpandPath(dir)
			if err != nil {
				return err
			}
			real := abs
			if r, err := filepath.EvalSymlinks(abs); err == nil {
				real = r
			}
			var keep []string
			removed := false
			for _, r := range a.cfg.WorkspaceRoots {
				rabs, _ := config.ExpandPath(r)
				if rr, err := filepath.EvalSymlinks(rabs); err == nil {
					rabs = rr
				}
				if rabs == real || rabs == abs {
					removed = true
					continue
				}
				keep = append(keep, r)
			}
			if !removed {
				return fmt.Errorf("%s is not in workspace_roots (see `ackn4th trust --list`)", real)
			}
			a.cfg.WorkspaceRoots = keep
			if _, err := config.Save(a.cfg); err != nil {
				return err
			}
			a.term.Println(a.term.Green("✓ "), "no longer trusted: ", a.term.Lime(real))
			return nil
		},
	}
}
