package task

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// NameMax is the longest a task's name may be, in characters.
const NameMax = 60

// A task's name is kept in its own file, tasks/<id>/name, rather than in
// task.json: a background runner rewrites task.json from memory as it
// works, which would undo a rename made while it runs.
func (s *Store) namePath(id string) string { return filepath.Join(s.Dir(id), "name") }

func (s *Store) readName(id string) string {
	data, err := os.ReadFile(s.namePath(id))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// Rename gives a task a new name. It is safe while the task runs.
func (s *Store) Rename(t *Task, name string) error {
	if err := checkID(t.ID); err != nil {
		return err
	}
	name, err := CleanName(name)
	if err != nil {
		return err
	}
	if err := writeAtomic(s.namePath(t.ID), []byte(name+"\n")); err != nil {
		return err
	}
	t.Name = name
	return nil
}

// CleanName checks a name typed by a person and tidies its spacing.
func CleanName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	switch {
	case name == "":
		return "", errors.New("the name is empty")
	case utf8.RuneCountInString(name) > NameMax:
		return "", fmt.Errorf("the name is longer than %d characters", NameMax)
	case strings.ContainsFunc(name, unicode.IsControl):
		return "", errors.New("the name has control characters in it")
	}
	return name, nil
}

// DefaultName suggests a name from a prompt: its first line, cut at a word
// boundary, without links.
func DefaultName(prompt string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(prompt), "\n")
	var words []string
	for _, w := range strings.Fields(line) {
		if strings.Contains(w, "://") {
			continue
		}
		words = append(words, w)
	}
	name := strings.TrimRight(strings.Join(words, " "), " .:;,")
	if name == "" {
		return "untitled"
	}
	const max = 40
	if utf8.RuneCountInString(name) <= max {
		return name
	}
	r := []rune(name)
	cut := string(r[:max])
	if i := strings.LastIndex(cut, " "); i > max/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " .:;,") + "…"
}

// Title is what people see a task as: its name, or, for a task made before
// tasks had names, one drawn from its prompt.
func (t *Task) Title() string {
	if t.Name != "" {
		return t.Name
	}
	return DefaultName(t.Prompt)
}

// Label is the task's title with its ID, for messages: "Rate limiting
// (k3f9qz)".
func (t *Task) Label() string { return t.Title() + " (" + t.ID + ")" }
