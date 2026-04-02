package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// validKeyRe matches valid .env key names: starts with a letter or
// underscore, followed by letters, digits, or underscores.
var validKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseDotEnv reads a .env file and returns a map of SORTIE_* key-value
// pairs. Returns (nil, nil) when path is empty or the file does not
// exist. Returns an error with file path and line number on parse
// failures. Only keys with the "SORTIE_" prefix are included.
func parseDotEnv(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}

	f, err := os.Open(path) //nolint:gosec // path is operator-provided via CLI flag or env var, not user input
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("dotenv %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only file, close error is harmless

	pairs := make(map[string]string)
	scanner := bufio.NewScanner(f)
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("dotenv %s:%d: missing '=' in line", path, lineNum)
		}

		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)

		if !validKeyRe.MatchString(key) {
			return nil, fmt.Errorf("dotenv %s:%d: invalid key %q", path, lineNum, key)
		}

		// Strip one layer of matching outer quotes (no escape processing).
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') ||
				(val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}

		if strings.HasPrefix(key, "SORTIE_") {
			pairs[key] = val
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("dotenv %s: %w", path, err)
	}

	return pairs, nil
}
