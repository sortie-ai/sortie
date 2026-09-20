// Package profile decodes and loads a runtime profile document, and resolves
// the checkout-relative paths it names. It imports tools/qualify/evidence for
// the vocabulary a profile's declarations are built from.
package profile

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// checkoutRootOffset is the fixed path from the tool module root to the
// checkout root.
const checkoutRootOffset = "../.."

// wantCheckoutModule is the module path CheckoutRoot requires the resolved
// directory's go.mod to declare.
const wantCheckoutModule = "github.com/sortie-ai/sortie"

const goModFileName = "go.mod"

// CheckoutRoot returns the checkout root: the tool module root, ascended
// from the current working directory to the nearest go.mod, joined with
// checkoutRootOffset. It returns an error unless the resolved directory
// carries a go.mod declaring github.com/sortie-ai/sortie.
func CheckoutRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	toolRoot, ok := ascendToGoMod(dir)
	if !ok {
		return "", fmt.Errorf("no ancestor of %s carries %s", dir, goModFileName)
	}

	root, err := filepath.Abs(filepath.Join(toolRoot, checkoutRootOffset))
	if err != nil {
		return "", fmt.Errorf("resolve checkout root from %s: %w", toolRoot, err)
	}
	declared, err := modulePath(filepath.Join(root, goModFileName))
	if err != nil {
		return "", fmt.Errorf("resolve checkout root %s: %w", root, err)
	}
	if declared != wantCheckoutModule {
		return "", fmt.Errorf("resolved directory %s declares module %q, want %q", root, declared, wantCheckoutModule)
	}
	return root, nil
}

func ascendToGoMod(dir string) (string, bool) {
	for {
		if _, err := os.Stat(filepath.Join(dir, goModFileName)); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// modulePath reads the module directive from the go.mod at path.
func modulePath(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the caller resolves the path from its own ancestor search
	if err != nil {
		return "", err
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if after, found := strings.CutPrefix(line, "module "); found {
			return strings.TrimSpace(after), nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s carries no module directive", path)
}
