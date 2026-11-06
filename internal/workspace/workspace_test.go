package workspace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func open(t *testing.T, writable ...string) (*Workspace, string) {
	t.Helper()
	dir := t.TempDir()
	w, err := Open(dir, writable, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w, dir
}

// Listing and searching find what's there. On Windows they once found
// nothing at all, with the error thrown away by every caller: this says
// why if it happens again.
func TestListAndSearch(t *testing.T) {
	w, dir := open(t, "**")
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("needle\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "b.txt"), []byte("hay\nneedle\n"), 0o644)

	entries, _, err := w.List(".", 5, 50)
	if err != nil || len(entries) != 3 {
		t.Errorf("List = %+v, %v (dir %s, hidden %q)", entries, err, dir, w.hidden)
	}
	hits, _, err := w.Search("needle", ".", "", 10)
	if err != nil || len(hits) != 2 || hits[1].Path != "sub/b.txt" || hits[1].Line != 2 {
		t.Errorf("Search = %+v, %v (dir %s, hidden %q)", hits, err, dir, w.hidden)
	}
	if _, err := w.root.Stat("."); err != nil {
		t.Errorf("stat the root: %v", err)
	}
	if _, err := fs.ReadDir(w.root.FS(), "."); err != nil {
		t.Errorf("read the root: %v", err)
	}
}

func TestEscapesAreRejected(t *testing.T) {
	w, dir := open(t, "**")
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("nope"), 0o644)
	os.Symlink(outside, filepath.Join(dir, "link"))

	for _, p := range []string{"../x", "/etc/passwd", "a/../../x", filepath.Join(outside, "secret.txt")} {
		if err := w.WriteFile(p, "x"); err == nil {
			t.Errorf("WriteFile(%q) escaped the workspace", p)
		}
		if _, err := w.ReadFile(p, 0, 0); err == nil {
			t.Errorf("ReadFile(%q) escaped the workspace", p)
		}
	}
	if _, err := w.ReadFile("link/secret.txt", 0, 0); err == nil {
		t.Error("read through a symlink to outside the workspace")
	}
	if err := w.WriteFile("link/new.txt", "x"); err == nil {
		t.Error("wrote through a symlink to outside the workspace")
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); err == nil {
		t.Error("file created outside the workspace")
	}
}

func TestProtectedPaths(t *testing.T) {
	w, dir := open(t, "**")
	os.MkdirAll(filepath.Join(dir, ".git", "hooks"), 0o755)
	os.Symlink(filepath.Join(dir, ".git", "hooks"), filepath.Join(dir, "hooks"))
	for _, p := range []string{".git/hooks/pre-commit", "sub/.git/config", ".ackn4th/roles/x.md", "hooks/pre-commit"} {
		if err := w.WriteFile(p, "#!/bin/sh"); !errors.Is(err, ErrDenied) {
			t.Errorf("WriteFile(%q) = %v, want ErrDenied", p, err)
		}
	}
}

func TestSecretsAreUnreadable(t *testing.T) {
	w, dir := open(t, "**")
	os.WriteFile(filepath.Join(dir, ".env"), []byte("KEY=1"), 0o644)
	os.WriteFile(filepath.Join(dir, ".env.example"), []byte("KEY="), 0o644)
	if _, err := w.ReadFile(".env", 0, 0); !errors.Is(err, ErrDenied) {
		t.Errorf("read .env: %v", err)
	}
	if _, err := w.ReadFile(".env.example", 0, 0); err != nil {
		t.Errorf("read .env.example: %v", err)
	}
	hits, _, _ := w.Search("KEY", ".", "", 10)
	if len(hits) != 1 || hits[0].Path != ".env.example" {
		t.Errorf("search hits = %+v", hits)
	}
}

func TestRolePermissions(t *testing.T) {
	w, _ := open(t, "docs/**", "*.md")
	if err := w.WriteFile("docs/a/b.txt", "ok"); err != nil {
		t.Error(err)
	}
	if err := w.WriteFile("pkg/README.md", "ok"); err != nil {
		t.Error(err)
	}
	if err := w.WriteFile("main.go", "package main"); !errors.Is(err, ErrDenied) {
		t.Errorf("main.go: %v", err)
	}
	ro, _ := open(t)
	if err := ro.WriteFile("a.md", "x"); !errors.Is(err, ErrDenied) {
		t.Errorf("read-only role wrote: %v", err)
	}
}

func TestEditAndRead(t *testing.T) {
	w, _ := open(t, "**")
	w.WriteFile("f.txt", "one\ntwo\ntwo\nthree\n")
	if _, err := w.EditFile("f.txt", "two", "2", false); err == nil || !strings.Contains(err.Error(), "2 times") {
		t.Errorf("ambiguous edit: %v", err)
	}
	if n, err := w.EditFile("f.txt", "two", "2", true); err != nil || n != 2 {
		t.Errorf("replace_all: %d %v", n, err)
	}
	got, _ := w.ReadFile("f.txt", 2, 2)
	if got != "2\n2\n" {
		t.Errorf("window = %q", got)
	}
}

func TestResolve(t *testing.T) {
	root := t.TempDir()
	if _, err := Resolve(filepath.Join(root, "task"), []string{root}, true); err != nil {
		t.Error(err)
	}
	if _, err := Resolve(t.TempDir(), []string{root}, false); err == nil {
		t.Error("directory outside the allowed roots accepted")
	}
	stray := filepath.Join(t.TempDir(), "stray")
	if _, err := Resolve(stray, []string{root}, true); err == nil {
		t.Error("directory outside the allowed roots accepted")
	}
	if _, err := os.Stat(stray); err == nil {
		t.Error("rejected workspace was created anyway")
	}
	os.Symlink(t.TempDir(), filepath.Join(root, "escape"))
	if _, err := Resolve(filepath.Join(root, "escape"), []string{root}, false); err == nil {
		t.Error("symlink out of the root accepted")
	}
}

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pat, path string
		want      bool
	}{
		{"*.md", "README.md", true},
		{"*.md", "docs/x/y.md", true},
		{"docs/**", "docs/a/b.go", true},
		{"docs/**", "src/docs/a", false},
		{"**/*_test.go", "a_test.go", true},
		{"**/*_test.go", "pkg/a_test.go", true},
		{"tests/", "tests/unit/a.py", true},
		{"Dockerfile*", "build/Dockerfile.dev", true},
		{"cmd/*.go", "cmd/sub/main.go", false},
	}
	for _, c := range cases {
		if got := MatchGlob(c.pat, c.path); got != c.want {
			t.Errorf("MatchGlob(%q, %q) = %v, want %v", c.pat, c.path, got, c.want)
		}
	}
}

func TestAckn4thDirsHidden(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "sandbox", "home")
	t.Setenv("ACKN4TH_HOME", home)
	os.MkdirAll(filepath.Join(home, "config", "oauth"), 0o700)
	os.WriteFile(filepath.Join(home, "config", "oauth", "token.json"), []byte("secret-token"), 0o600)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main // secret-token? no\n"), 0o644)

	ws, err := Open(dir, []string{"**"}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if _, err := ws.ReadFile("sandbox/home/config/oauth/token.json", 0, 0); err == nil {
		t.Fatal("ackn4th's config is readable from the workspace")
	}
	if err := ws.WriteFile("sandbox/home/data/x", "y"); err == nil {
		t.Fatal("ackn4th's data dir is writable from the workspace")
	}
	entries, _, _ := ws.List(".", 10, 100)
	for _, e := range entries {
		if strings.Contains(e.Path, "home") {
			t.Fatalf("listing shows %s", e.Path)
		}
	}
	hits, _, _ := ws.Search("secret-token", ".", "", 10)
	if len(hits) != 1 || hits[0].Path != "main.go" {
		t.Fatalf("search reached hidden files: %+v", hits)
	}
}

// A sandbox whose home IS the workspace (ACKN4TH_HOME pointing at the
// directory the agents work in) must not hide every path in it: hiding
// the root turned read_file into "does not exist" and write_file into
// "belongs to ackn4th" for the whole tree. Its config and data dirs are
// hidden by name instead.
func TestSandboxHomeAtWorkspaceRoot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ACKN4TH_HOME", dir)
	os.MkdirAll(filepath.Join(dir, "config"), 0o700)
	os.WriteFile(filepath.Join(dir, "config", "oauth.json"), []byte("grant"), 0o600)
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "main.go"), []byte("package main\n"), 0o644)

	ws, err := Open(dir, []string{"**"}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	if b, err := ws.ReadFile("sub/main.go", 0, 0); err != nil || !strings.Contains(b, "package main") {
		t.Fatalf("reading an ordinary file when the sandbox home is the workspace: %q, %v", b, err)
	}
	if err := ws.WriteFile("sub/other.go", "package main\n"); err != nil {
		t.Fatalf("writing an ordinary file when the sandbox home is the workspace: %v", err)
	}
	if _, err := ws.ReadFile("config/oauth.json", 0, 0); err == nil {
		t.Fatal("ackn4th's config is readable from the workspace root")
	}
}

func TestSandboxHomeAlwaysHidden(t *testing.T) {
	dir := t.TempDir() // no ACKN4TH_HOME: a normal install working on the repo
	os.MkdirAll(filepath.Join(dir, "sandbox", HomeDirName, "config"), 0o700)
	os.WriteFile(filepath.Join(dir, "sandbox", HomeDirName, "config", "creds.json"), []byte("{}"), 0o600)
	ws, _ := Open(dir, []string{"**"}, 1<<20)
	defer ws.Close()
	if _, err := ws.ReadFile("sandbox/"+HomeDirName+"/config/creds.json", 0, 0); err == nil {
		t.Fatal("a sandbox home is readable")
	}
	if entries, _, _ := ws.List(".", 5, 50); len(entries) != 1 {
		t.Fatalf("listing shows the sandbox home: %+v", entries)
	}
}

// Another spelling of .git or .env must not get around the rules: macOS
// and Windows filesystems ignore case, and Windows has more aliases.
func TestProtectedAliases(t *testing.T) {
	w, dir := open(t, "**")
	os.MkdirAll(filepath.Join(dir, ".git", "hooks"), 0o755)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("TOKEN=hunter2"), 0o600)
	for _, p := range []string{".GIT/hooks/post-checkout", "sub/.Git/config", ".Ackn4th/x"} {
		if err := w.WriteFile(p, "#!/bin/sh\n"); err == nil {
			t.Errorf("wrote %s", p)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "hooks", "post-checkout")); err == nil {
		t.Error("a hook was written through another spelling of .git")
	}
	if got, err := w.ReadFile(".ENV", 0, 0); err == nil || strings.Contains(got, "hunter2") {
		t.Errorf(".ENV read: %q %v", got, err)
	}
	// A hard link elsewhere can't name the directory, but any existing path
	// that is .git is refused whatever it's called.
	if err := os.Symlink(".git", filepath.Join(dir, "hooks-link")); err == nil {
		if err := w.WriteFile("hooks-link/hooks/x", "x"); err == nil {
			t.Error("wrote through a link to .git")
		}
	}
}

func TestWinAlias(t *testing.T) {
	for _, seg := range []string{"x:stream", "GIT~1", ".git.", ".git ", "PROGRA~2", "NUL", "con.txt", "Com1", "LPT9.log", "aux", "CONOUT$"} {
		if !winAlias(seg) {
			t.Errorf("%q should count as an alias", seg)
		}
	}
	for _, seg := range []string{".git", "main.go", "a~b", "v1.2", "file~", "console.go", "nullable", "com10", "auxiliary.txt", "lpt"} {
		if winAlias(seg) {
			t.Errorf("%q is a plain name", seg)
		}
	}
}

// TestWindowsNames runs Windows' name rules on every OS: the root is
// fine, names Windows would read as another file are not.
func TestWindowsNames(t *testing.T) {
	for _, p := range []string{"", ".", "./", "/", "src", "src/main.go", "a/./b"} {
		if _, err := cleanFor(p, true); err != nil {
			t.Errorf("%q refused: %v", p, err)
		}
	}
	for _, p := range []string{".git.", "src/GIT~1/config", "a.txt:stream", "NUL", "con.txt", "dir /x"} {
		if _, err := cleanFor(p, true); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
}
