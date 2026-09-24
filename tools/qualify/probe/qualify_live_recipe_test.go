package probe

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

func readQualifyLiveRecipe(t *testing.T) string {
	t.Helper()

	root, err := profile.CheckoutRoot()
	if err != nil {
		t.Fatalf("CheckoutRoot() error = %v, want nil", err)
	}
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read the root Makefile: %v", err)
	}

	lines := strings.Split(string(makefile), "\n")
	target := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "qualify-live:") {
			target = i
			break
		}
	}
	if target < 0 {
		t.Fatal("Makefile carries no qualify-live target")
	}
	for _, line := range lines[target+1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, "\t") {
			break
		}
		if strings.Contains(line, "test") {
			return strings.TrimSpace(line)
		}
	}
	t.Fatal("qualify-live target carries no test recipe line")
	return ""
}

var (
	qualifyLiveRunPattern = regexp.MustCompile(`-run\s+'([^']*)'`)
	qualifyLivePackageArg = regexp.MustCompile(`(\./\S+)\s*$`)
)

func TestQualifyLiveRecipeListsExactlyTheLiveTest(t *testing.T) {
	t.Parallel()

	recipe := readQualifyLiveRecipe(t)

	match := qualifyLiveRunPattern.FindStringSubmatch(recipe)
	if match == nil {
		t.Fatalf("recipe %q carries no -run pattern", recipe)
	}
	pattern := strings.ReplaceAll(match[1], "$$", "$")

	pkgMatch := qualifyLivePackageArg.FindStringSubmatch(recipe)
	if pkgMatch == nil {
		t.Fatalf("recipe %q carries no trailing package argument", recipe)
	}
	pkg := pkgMatch[1]

	root, err := profile.CheckoutRoot()
	if err != nil {
		t.Fatalf("CheckoutRoot() error = %v, want nil", err)
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("LookPath(go) error = %v, want nil", err)
	}

	cmd := exec.Command(goBin, "test", "-list", pattern, pkg) //nolint:gosec // pattern and pkg come from the repository's own Makefile
	cmd.Dir = filepath.Join(root, "tools", "qualify")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("go test -list %s %s: %v", pattern, pkg, err)
	}

	var listed []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(output)), "\n") {
		if line == "" || strings.HasPrefix(line, "ok") {
			continue
		}
		listed = append(listed, line)
	}
	if len(listed) != 1 || listed[0] != "TestQualificationProfile" {
		t.Errorf("go test -list %s %s listed %v, want exactly [TestQualificationProfile]", pattern, pkg, listed)
	}
}

func TestQualifyLiveRecipeDisablesDefaultTimeoutAndIsVerbose(t *testing.T) {
	t.Parallel()

	recipe := readQualifyLiveRecipe(t)

	if !strings.Contains(recipe, "-v") {
		t.Errorf("recipe %q does not pass -v", recipe)
	}
	if !strings.Contains(recipe, "-timeout 0") {
		t.Errorf("recipe %q does not pass -timeout 0", recipe)
	}
}
