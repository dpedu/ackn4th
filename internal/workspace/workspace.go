// Package workspace confines agent file access to a single directory.
//
// Every operation goes through an *os.Root, which rejects absolute paths,
// `..` traversal, and symlinks that resolve outside the workspace, so even
// a confused or adversarial model cannot touch files elsewhere on disk. On
// top of that, writes are limited to the role's writable globs, and a small
// set of paths (.git internals, ackn4th's own metadata) can never be written.
package workspace

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/dethmasque/ackn4th/internal/config"
)

// protectedDirs are never writable at any depth: git hooks would let an
// agent run code on the user's next git command, and .ackn4th holds
// ackn4th's own metadata.
var protectedDirs = map[string]bool{".git": true, ".ackn4th": true}

// secretGlobs are never readable, so credentials sitting in a workspace are
// not shipped off to a model provider.
var secretGlobs = []string{".env", ".env.*", "*.pem", "*.key", "*.p12", "*.pfx", "id_rsa*", "id_ed25519*", ".netrc", ".npmrc", ".pypirc"}

// skipDirs are hidden from listings and searches to save tokens.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, ".venv": true, "__pycache__": true}

// ErrDenied is returned when a write falls outside the role's permissions.
var ErrDenied = errors.New("permission denied")

// Workspace is an open, confined directory.
type Workspace struct {
	dir      string
	root     *os.Root
	writable []string
	maxBytes int64
	// hidden are workspace-relative paths agents can neither see nor
	// touch: ackn4th's own config (OAuth grants) and data, when they lie
	// inside the workspace, as a sandbox inside the repo does.
	hidden []string
}

// Resolve turns dir into an absolute, symlink-free path and checks that it
// lies inside one of the allowed roots. With create set, a missing
// directory is created, but only after its path is known to be inside a
// root, so a bad --workspace never leaves anything behind.
func Resolve(dir string, allowedRoots []string, create bool) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if create {
		// Check before creating anything, comparing with symlinks resolved
		// as far as the path exists (macOS's /var is /private/var).
		if !within(realPath(abs), allowedRoots, false) {
			return "", outside(abs, allowedRoots)
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return "", err
		}
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("workspace %s: %w", dir, err)
	}
	if info, err := os.Stat(real); err != nil {
		return "", err
	} else if !info.IsDir() {
		return "", fmt.Errorf("workspace %s is not a directory", dir)
	}
	if !within(real, allowedRoots, true) {
		return "", outside(real, allowedRoots)
	}
	return real, nil
}

// within reports whether p is inside (or equal to) one of roots. With
// resolve set, roots are compared by their symlink-free paths.
func within(p string, roots []string, resolve bool) bool {
	for _, root := range roots {
		candidates := []string{filepath.Clean(root)}
		if rr, err := filepath.EvalSymlinks(root); err == nil {
			candidates = append(candidates, rr)
		} else if resolve {
			continue // a root that does not exist cannot contain anything
		}
		for _, c := range candidates {
			if p == c || strings.HasPrefix(p, c+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

// ErrUntrusted is returned when a workspace lies outside every allowed root.
var ErrUntrusted = errors.New("not a trusted directory")

func outside(p string, roots []string) error {
	return fmt.Errorf("workspace %s: %w (trust it with `ackn4th trust %s`)", p, ErrUntrusted, p)
}

// Open confines access to dir. writable lists the globs the caller may
// modify; nil means read-only. maxBytes caps reads and writes.
func Open(dir string, writable []string, maxBytes int64) (*Workspace, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	w := &Workspace{dir: dir, root: root, writable: writable, maxBytes: maxBytes}
	for _, f := range []func() (string, error){config.Dir, config.DataDir} {
		w.hide(f())
	}
	if h, ok := config.Sandbox(); ok {
		w.hide(h, nil)
	}
	return w, nil
}

// hide makes an absolute path inaccessible if it lies inside the workspace.
func (w *Workspace) hide(abs string, err error) {
	if err != nil || abs == "" {
		return
	}
	rel, err := filepath.Rel(realPath(w.dir), realPath(abs))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return
	}
	// When the sandbox home is the workspace itself (ACKN4TH_HOME with the
	// data and config dirs inside it), rel is ".": hiding it would hide
	// every path in the workspace. config and data are hidden by their own
	// names below.
	if rel == "." {
		return
	}
	w.hidden = append(w.hidden, filepath.ToSlash(rel))
}

// realPath resolves symlinks in the longest existing prefix of p, so
// paths that do not exist yet compare correctly with resolved ones.
func realPath(p string) string {
	rest := ""
	for {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, rest)
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// HomeDirName is the name sandbox homes use (ACKN4TH_HOME, holding OAuth
// grants). A directory with this name is hidden from agents wherever it
// is, even when no sandbox is active.
const HomeDirName = ".ackn4th-home"

// isHidden reports whether the clean relative path c is hidden. Names
// are compared ignoring case, as macOS and Windows filesystems do.
func (w *Workspace) isHidden(c string) bool {
	c = strings.ToLower(c)
	if c == HomeDirName || strings.HasPrefix(c, HomeDirName+"/") || strings.Contains(c, "/"+HomeDirName+"/") || strings.HasSuffix(c, "/"+HomeDirName) {
		return true
	}
	for _, h := range w.hidden {
		h = strings.ToLower(h)
		if h == "." || c == h || strings.HasPrefix(c, h+"/") {
			return true
		}
	}
	return false
}

// Close releases the workspace.
func (w *Workspace) Close() error { return w.root.Close() }

// Dir is the workspace's absolute path.
func (w *Workspace) Dir() string { return w.dir }

// ReadOnly reports whether no writes are permitted.
func (w *Workspace) ReadOnly() bool { return len(w.writable) == 0 }

// Writable returns the write globs.
func (w *Workspace) Writable() []string { return w.writable }

// clean normalizes an agent-supplied path to a slash-separated relative path.
func clean(p string) (string, error) { return cleanFor(p, runtime.GOOS == "windows") }

// cleanFor is clean, with Windows' extra name rules when windows is set,
// so they are tested on every OS.
func cleanFor(p string, windows bool) (string, error) {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" || p == "/" {
		return ".", nil
	}
	if path.IsAbs(p) || filepath.IsAbs(p) {
		return "", fmt.Errorf("%q: use a path relative to the workspace", p)
	}
	c := path.Clean(p)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("%q escapes the workspace", p)
	}
	// The root itself: path.Clean leaves no other "." segment, and this
	// one isn't a trailing-dot name.
	if c == "." {
		return c, nil
	}
	if windows {
		for _, seg := range strings.Split(c, "/") {
			if winAlias(seg) {
				return "", fmt.Errorf("%q: Windows would read %q as another name (a stream, short name or trailing dot); use the file's real name", p, seg)
			}
		}
	}
	return c, nil
}

var shortName = regexp.MustCompile(`~[0-9]`)

// winAlias reports whether a path segment could name a different file on
// Windows than it appears to: an alternate data stream (name:stream), an
// 8.3 short name (GIT~1 for .git), or a name Windows trims (".git.").
func winAlias(seg string) bool {
	return strings.Contains(seg, ":") || shortName.MatchString(seg) || strings.TrimRight(seg, ". ") != seg || winDevice.MatchString(seg)
}

// winDevice matches the reserved device names (CON, NUL, COM1, ...), with
// or without an extension: opening one reaches a device, not a file in the
// workspace.
var winDevice = regexp.MustCompile(`(?i)^(con|prn|aux|nul|conin\$|conout\$|com[0-9¹²³]|lpt[0-9¹²³])(\..*)?$`)

// CanWrite reports whether rel may be modified.
func (w *Workspace) CanWrite(rel string) error {
	c, err := clean(rel)
	if err != nil {
		return err
	}
	if c == "." {
		return fmt.Errorf("%w: cannot modify the workspace root", ErrDenied)
	}
	if w.isHidden(c) {
		return fmt.Errorf("%w: %s belongs to ackn4th", ErrDenied, c)
	}
	for _, seg := range strings.Split(c, "/") {
		if protectedDirs[strings.ToLower(seg)] {
			return fmt.Errorf("%w: %s is protected", ErrDenied, c)
		}
	}
	if w.ReadOnly() {
		return fmt.Errorf("%w: this role is read-only", ErrDenied)
	}
	allowed := false
	for _, g := range w.writable {
		if MatchGlob(g, c) {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("%w: %s is outside this role's writable paths %v", ErrDenied, c, w.writable)
	}
	// os.Root already stops symlinks from leaving the workspace, but a link
	// inside it could still route a write around the checks above (say,
	// docs -> .git/hooks), so never write through one.
	// Nor through another name for a protected directory: whatever the
	// spelling, an existing path that is the same file as a .git or
	// .ackn4th beside it is refused.
	parts := strings.Split(c, "/")
	for i := range parts {
		info, err := w.root.Lstat(strings.Join(parts[:i+1], "/"))
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err == nil && info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s passes through a symlink", ErrDenied, c)
		}
		if err == nil {
			parent := path.Join(append([]string{"."}, parts[:i]...)...)
			for name := range protectedDirs {
				if pi, err := w.root.Lstat(path.Join(parent, name)); err == nil && os.SameFile(info, pi) {
					return fmt.Errorf("%w: %s is protected", ErrDenied, c)
				}
			}
		}
	}
	return nil
}

// IsSecret reports whether a workspace-relative path looks like a
// credential file, which agents may not read by any means.
func IsSecret(rel string) bool { return secret(rel) }

func secret(rel string) bool {
	rel = strings.ToLower(rel)
	switch path.Ext(rel) {
	case ".example", ".sample", ".template", ".dist":
		return false
	}
	for _, g := range secretGlobs {
		if MatchGlob(g, rel) {
			return true
		}
	}
	return false
}

// ReadFile returns a file's contents, optionally limited to a line window.
// offset is 1-based; limit 0 means to the end.
func (w *Workspace) ReadFile(rel string, offset, limit int) (string, error) {
	c, err := clean(rel)
	if err != nil {
		return "", err
	}
	if w.isHidden(c) {
		return "", fmt.Errorf("%s: %w", c, fs.ErrNotExist)
	}
	if secret(c) {
		return "", fmt.Errorf("%w: %s may contain credentials and is not readable by agents", ErrDenied, c)
	}
	info, err := w.root.Stat(c)
	if err != nil {
		return "", w.relErr(err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", c)
	}
	if info.Size() > w.maxBytes && offset == 0 && limit == 0 {
		return "", fmt.Errorf("%s is %d bytes (limit %d): read it in parts with offset/limit", c, info.Size(), w.maxBytes)
	}
	f, err := w.root.Open(c)
	if err != nil {
		return "", w.relErr(err)
	}
	defer f.Close()
	if offset <= 0 {
		offset = 1
	}
	var b strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), int(w.maxBytes))
	for n := 1; sc.Scan(); n++ {
		if n < offset {
			continue
		}
		if limit > 0 && n >= offset+limit {
			break
		}
		if int64(b.Len()) > w.maxBytes {
			return "", fmt.Errorf("%s: window exceeds %d bytes; use a smaller limit", c, w.maxBytes)
		}
		b.Write(sc.Bytes())
		b.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	if bytes.IndexByte([]byte(b.String()), 0) >= 0 {
		return "", fmt.Errorf("%s looks like a binary file", c)
	}
	return b.String(), nil
}

// WriteFile creates or replaces a file, creating parent directories.
func (w *Workspace) WriteFile(rel, content string) error {
	if err := w.CanWrite(rel); err != nil {
		return err
	}
	if int64(len(content)) > w.maxBytes {
		return fmt.Errorf("content is %d bytes (limit %d)", len(content), w.maxBytes)
	}
	c, _ := clean(rel)
	if dir := path.Dir(c); dir != "." {
		if err := w.root.MkdirAll(dir, 0o755); err != nil {
			return w.relErr(err)
		}
	}
	return w.relErr(w.root.WriteFile(c, []byte(content), 0o644))
}

// EditFile replaces old with new in a file. Unless all is set, old must
// occur exactly once. It returns the number of replacements.
func (w *Workspace) EditFile(rel, old, new string, all bool) (int, error) {
	if err := w.CanWrite(rel); err != nil {
		return 0, err
	}
	if old == "" {
		return 0, errors.New("old_string must not be empty")
	}
	c, _ := clean(rel)
	if secret(c) {
		return 0, fmt.Errorf("%w: %s may contain credentials; replace it with write_file instead", ErrDenied, c)
	}
	data, err := w.root.ReadFile(c)
	if err != nil {
		return 0, w.relErr(err)
	}
	s := string(data)
	n := strings.Count(s, old)
	switch {
	case n == 0:
		return 0, fmt.Errorf("old_string not found in %s", c)
	case n > 1 && !all:
		return 0, fmt.Errorf("old_string occurs %d times in %s: add context to make it unique, or set replace_all", n, c)
	}
	if !all {
		n = 1
	}
	s = strings.Replace(s, old, new, n)
	if int64(len(s)) > w.maxBytes {
		return 0, fmt.Errorf("result would be %d bytes (limit %d)", len(s), w.maxBytes)
	}
	return n, w.relErr(w.root.WriteFile(c, []byte(s), 0o644))
}

// Remove deletes a file or an empty directory.
func (w *Workspace) Remove(rel string) error {
	if err := w.CanWrite(rel); err != nil {
		return err
	}
	c, _ := clean(rel)
	return w.relErr(w.root.Remove(c))
}

// Entry is one item in a directory listing.
type Entry struct {
	Path    string
	IsDir   bool
	Size    int64
	ModTime time.Time
}

// List walks rel up to depth levels (1 = immediate children), skipping
// dependency and VCS directories. At most max entries are returned.
func (w *Workspace) List(rel string, depth, max int) ([]Entry, bool, error) {
	c, err := clean(rel)
	if err != nil {
		return nil, false, err
	}
	if depth <= 0 {
		depth = 2
	}
	var out []Entry
	truncated := false
	base := strings.Count(c, "/")
	if c == "." {
		base = -1
	}
	err = fs.WalkDir(w.root.FS(), c, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return skipUnreadable(p, c, d, err)
		}
		if p == c {
			return nil
		}
		if d.IsDir() && (skipDirs[d.Name()] || w.isHidden(p)) {
			return fs.SkipDir
		}
		if w.isHidden(p) {
			return nil
		}
		if strings.Count(p, "/")-base > depth {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if len(out) >= max {
			truncated = true
			return fs.SkipAll
		}
		e := Entry{Path: p, IsDir: d.IsDir()}
		if info, err := d.Info(); err == nil && !d.IsDir() {
			e.Size, e.ModTime = info.Size(), info.ModTime()
		}
		out = append(out, e)
		return nil
	})
	return out, truncated, w.relErr(err)
}

// skipUnreadable handles an error during a walk from c: one unreadable
// file or folder (a junction, a permission) is skipped instead of ending
// the walk with nothing, but failing to read c itself is an error.
func skipUnreadable(p, c string, d fs.DirEntry, err error) error {
	if p == c {
		return err
	}
	if d != nil && d.IsDir() {
		return fs.SkipDir
	}
	return nil
}

// Match is one search hit.
type Match struct {
	Path string
	Line int
	Text string
}

// Search finds lines matching the regular expression under rel, optionally
// only in files whose path matches glob. At most max hits are returned.
func (w *Workspace) Search(pattern, rel, glob string, max int) ([]Match, bool, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, false, fmt.Errorf("bad pattern: %w", err)
	}
	c, err := clean(rel)
	if err != nil {
		return nil, false, err
	}
	var out []Match
	truncated := false
	err = fs.WalkDir(w.root.FS(), c, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return skipUnreadable(p, c, d, err)
		}
		if d.IsDir() {
			if p != c && (skipDirs[d.Name()] || w.isHidden(p)) {
				return fs.SkipDir
			}
			return nil
		}
		if (glob != "" && !MatchGlob(glob, p)) || secret(p) || w.isHidden(p) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > w.maxBytes || !d.Type().IsRegular() {
			return nil
		}
		data, err := w.root.ReadFile(p)
		if err != nil || bytes.IndexByte(data, 0) >= 0 {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				if len(out) >= max {
					truncated = true
					return fs.SkipAll
				}
				out = append(out, Match{Path: p, Line: i + 1, Text: strings.TrimSpace(line)})
			}
		}
		return nil
	})
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, truncated, w.relErr(err)
}

// relErr strips the absolute workspace path from errors so models only ever
// see relative paths.
func (w *Workspace) relErr(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ReplaceAll(err.Error(), w.dir+string(filepath.Separator), "")
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: %w", msg, fs.ErrNotExist)
	}
	return errors.New(msg)
}
