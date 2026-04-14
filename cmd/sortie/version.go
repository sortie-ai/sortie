package main

import (
	"fmt"
	"runtime"
)

// Version, Commit, and Date are set at build time via -ldflags.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

func versionBanner() string {
	return fmt.Sprintf("sortie %s (commit: %s, built: %s, %s, %s/%s)\n",
		Version, shortCommit(Commit), Date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

func shortCommit(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
