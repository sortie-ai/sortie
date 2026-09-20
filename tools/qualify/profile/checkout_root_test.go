package profile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckoutRootFromTheToolModule(t *testing.T) {
	root, err := CheckoutRoot()
	if err != nil {
		t.Fatalf("CheckoutRoot() = _, %v, want nil", err)
	}
	if !filepath.IsAbs(root) {
		t.Errorf("CheckoutRoot() = %q, want an absolute path", root)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Errorf("CheckoutRoot() = %q, want a directory carrying go.mod: %v", root, err)
	}
	if _, err := os.Stat(filepath.Join(root, "tools", "qualify", "go.mod")); err != nil {
		t.Errorf("CheckoutRoot() = %q, want the tool module to sit at tools/qualify below it: %v", root, err)
	}
}

// TestCheckoutRootRejectsAWrongModule cannot run in parallel: it changes the
// process working directory, which os.Getwd()-based resolution reads.
func TestCheckoutRootRejectsAWrongModule(t *testing.T) {
	toolRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(toolRoot, "go.mod"), []byte("module github.com/example/other-tool\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatalf("write fake tool go.mod: %v", err)
	}
	checkoutRoot := filepath.Dir(filepath.Dir(toolRoot))
	if err := os.WriteFile(filepath.Join(checkoutRoot, "go.mod"), []byte("module github.com/example/other-checkout\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatalf("write fake checkout go.mod: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(filepath.Join(checkoutRoot, "go.mod")) })

	t.Chdir(toolRoot)

	if _, err := CheckoutRoot(); err == nil {
		t.Error("CheckoutRoot() = _, nil, want rejection: the resolved directory declares a module other than github.com/sortie-ai/sortie")
	}
}

// TestCheckoutRootRejectsNoAncestorGoMod cannot run in parallel: it changes
// the process working directory.
func TestCheckoutRootRejectsNoAncestorGoMod(t *testing.T) {
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("filepath.EvalSymlinks(%q): %v", dir, err)
	}

	t.Chdir(resolved)

	if _, err := CheckoutRoot(); err == nil {
		t.Error("CheckoutRoot() = _, nil, want rejection when no ancestor directory carries go.mod")
	}
}
