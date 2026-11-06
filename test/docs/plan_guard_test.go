package docs_test

// A test plan that cites tests which do not exist is worse than no plan: it
// tells a reader the behaviour is covered when nothing checks it, and the
// names it quotes look authoritative. That is exactly what happened to
// `docs/qa/test-plan.md` when the suite it described was deleted - and nothing
// caught it, because the only guards were for links, anchors and environment
// variables.
//
// So: every `pkg.TestName` the plan cites must resolve to a `func TestName` in
// that package's own tests, and every bare path it cites must exist in the
// checkout. Both are checked from the checkout alone, like the other guards.

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// planFile is the document under guard: the QA plan shipped with the tree.
const planFile = "docs/qa/test-plan.md"

// citeRE matches the three shapes the plan uses to name a test:
// `task.TestFoo`, `cli.TestFoo`, `docs.TestFoo`, and a backticked path.
var (
	pkgTestRE = regexp.MustCompile("`(task|cli|dash|docs|arch)\\.(Test[A-Za-z0-9_]+)`")
	pathRE    = regexp.MustCompile("`([a-z0-9_./-]+\\.go)`")
	funcRE    = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
)

// pkgDirs maps a plan's package alias to the directory whose tests it means.
var pkgDirs = map[string]string{
	"task": "internal/task",
	"cli":  "internal/cli",
	"dash": "internal/dash",
	"docs": "test/docs",
	"arch": "test/arch",
}

func planText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), planFile))
	if err != nil {
		t.Fatalf("read %s: %v", planFile, err)
	}
	return string(b)
}

// testFuncsIn returns every Test* function declared under dir.
func testFuncsIn(t *testing.T, root, dir string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	base := filepath.Join(root, dir)
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err // a missing package is the caller's problem to report
		}
		if d.IsDir() || !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		for _, m := range funcRE.FindAllStringSubmatch(string(b), -1) {
			out[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

func TestPlanCitesTestsThatExist(t *testing.T) {
	root := repoRoot(t)
	text := planText(t)

	byPkg := map[string]map[string]bool{}
	var missing []string
	for _, m := range pkgTestRE.FindAllStringSubmatch(text, -1) {
		pkg, name := m[1], m[2]
		dir, ok := pkgDirs[pkg]
		if !ok {
			continue
		}
		if byPkg[pkg] == nil {
			byPkg[pkg] = testFuncsIn(t, root, dir)
		}
		if !byPkg[pkg][name] {
			missing = append(missing, pkg+"."+name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%s cites %d test(s) that do not exist: %s\n\ta plan naming tests nobody wrote claims coverage that is not there; add the test or drop the citation",
			planFile, len(missing), strings.Join(missing, ", "))
	}
}

func TestPlanCitesFilesThatExist(t *testing.T) {
	root := repoRoot(t)
	text := planText(t)

	seen := map[string]bool{}
	var missing []string
	for _, m := range pathRE.FindAllStringSubmatch(text, -1) {
		p := m[1]
		if seen[p] {
			continue
		}
		seen[p] = true
		if _, err := os.Stat(filepath.Join(root, p)); err != nil {
			missing = append(missing, p)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%s cites %d file(s) that are not in the tree: %s\n\ta reader who follows the plan lands nowhere; cite what ships",
			planFile, len(missing), strings.Join(missing, ", "))
	}
}

// The guard is only worth having if the plan actually cites something: an
// empty citation set would make both tests above pass while checking nothing.
func TestPlanCitesSomething(t *testing.T) {
	text := planText(t)
	if n := len(pkgTestRE.FindAllString(text, -1)); n == 0 {
		t.Errorf("%s cites no tests at all: either the plan stopped mapping acceptance criteria to tests, or the citation format changed and this guard now checks nothing", planFile)
	}
	if n := len(pathRE.FindAllString(text, -1)); n == 0 {
		t.Errorf("%s cites no files at all: the guard above is checking an empty set", planFile)
	}
}
