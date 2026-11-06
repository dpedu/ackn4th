package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/dethmasque/ackn4th/internal/task"
	"github.com/dethmasque/ackn4th/internal/ui"
)

// NoInputEnv disables prompts, like --no-input.
const NoInputEnv = "ACKN4TH_NO_INPUT"

// interactive reports whether ackn4th may ask for missing input: on a
// terminal, unless --no-input, ACKN4TH_NO_INPUT=1 or CI says otherwise.
// Non-interactive runs fail fast with the error instead.
func (a *app) interactive() bool {
	if a.noInput || os.Getenv(NoInputEnv) == "1" || os.Getenv("CI") != "" {
		return false
	}
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
}

// stdin is shared by every prompt, so buffered input isn't lost between
// them.
func (a *app) stdin() *bufio.Reader {
	if a.in == nil {
		a.in = bufio.NewReader(os.Stdin)
	}
	return a.in
}

// cause shows why ackn4th is asking: the error a non-interactive run
// would have failed with.
func (a *app) cause(err error) {
	if err != nil {
		a.term.Println(a.term.Amber("! "), inline(a.term, err.Error()))
	}
}

// ask prompts for a missing value. Non-interactively it returns err.
// Empty input (or EOF) gives up with err; check, if set, rejects bad
// values and asks again.
func (a *app) ask(err error, label string, check func(string) error) (string, error) {
	if !a.interactive() {
		return "", err
	}
	a.cause(err)
	return a.prompt(err, label, check)
}

// prompt reads a value without repeating why it is asked.
func (a *app) prompt(err error, label string, check func(string) error) (string, error) {
	for {
		fmt.Fprint(os.Stdout, a.term.Bold("  "+label+": "))
		line, rerr := a.stdin().ReadString('\n')
		v := strings.TrimSpace(line)
		if v == "" || (rerr != nil && line == "") {
			return "", err
		}
		if check != nil {
			if cerr := check(v); cerr != nil {
				a.term.Println(a.term.Red("  ✗ "), inline(a.term, cerr.Error()))
				continue
			}
		}
		return v, nil
	}
}

// option is one numbered choice.
type option struct {
	name string // returned, and accepted as typed input
	desc string
}

// choose asks for one or more of opts by number or name. Non-interactively
// it returns err.
func (a *app) choose(err error, title string, opts []option, multi bool) ([]string, error) {
	if !a.interactive() {
		return nil, err
	}
	if len(opts) == 0 {
		return nil, err
	}
	t := a.term
	a.cause(err)
	w := 0
	for _, o := range opts {
		w = max(w, len(o.name))
	}
	var lines []string
	for i, o := range opts {
		lines = append(lines, fmt.Sprintf("%s %s  %s", t.Bold(t.Lime(fmt.Sprintf("%2d", i+1))), t.Bold(fmt.Sprintf("%-*s", w, o.name)), t.Gray(o.desc)))
	}
	t.Box(title, lines)
	label := fmt.Sprintf("choose 1-%d", len(opts))
	if len(opts) == 1 {
		label = "choose 1"
	}
	if multi {
		label += ", several with commas"
	}
	var picked []string
	_, err = a.prompt(err, label, func(v string) error {
		picked = picked[:0]
		for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' }) {
			name, ok := pick(f, opts)
			if !ok {
				return fmt.Errorf("%q is not one of the choices", f)
			}
			if !slices.Contains(picked, name) {
				picked = append(picked, name)
			}
		}
		if !multi && len(picked) > 1 {
			return errors.New("choose just one")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return picked, nil
}

func pick(v string, opts []option) (string, bool) {
	if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= len(opts) {
		return opts[n-1].name, true
	}
	v = strings.ReplaceAll(strings.ToLower(v), "-", "_")
	for _, o := range opts {
		if strings.ToLower(o.name) == v {
			return o.name, true
		}
	}
	return "", false
}

// confirm asks a yes/no question, defaulting to no. Non-interactively it
// returns err.
func (a *app) confirm(err error, question string) error {
	if !a.interactive() {
		return err
	}
	a.cause(err)
	fmt.Fprint(os.Stdout, a.term.Bold("  "+question+" [y/N] "))
	line, _ := a.stdin().ReadString('\n')
	if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
		return errors.New("cancelled")
	}
	return nil
}

// taskArg returns the task named by args, or asks which one when it is
// missing. only, if set, limits the choices (e.g. running tasks for
// stop-task).
func (a *app) taskArg(args []string, only func(*task.Task) bool) (*task.Task, error) {
	if len(args) > 0 {
		return a.backend().Find(args[0])
	}
	missing := usageError{errors.New("missing <task>: its name or ID")}
	tasks, err := a.backend().List()
	if err != nil {
		return nil, err
	}
	var opts []option
	for _, tk := range tasks {
		a.backend().Reconcile(tk)
		if only != nil && !only(tk) {
			continue
		}
		opts = append(opts, option{tk.ID, fmt.Sprintf("%-9s %s", tk.Status, ui.Truncate(tk.Title(), 48))})
		if len(opts) == 9 {
			break
		}
	}
	if len(opts) == 0 {
		return nil, missing
	}
	ids, err := a.choose(missing, "WHICH TASK?", opts, false)
	if err != nil {
		return nil, err
	}
	return a.backend().Find(ids[0])
}
