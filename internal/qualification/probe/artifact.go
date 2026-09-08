package probe

import (
	"fmt"
	"os"
	"path/filepath"
)

// repositoryRootFromWD ascends from the current working directory to
// the nearest ancestor holding go.mod, mirroring
// qualification.ReadRuntimeProfileFile's own resolution so every
// reader is independent of the caller's working directory.
func repositoryRootFromWD() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no ancestor of %s carries go.mod", dir)
		}
		dir = parent
	}
}
