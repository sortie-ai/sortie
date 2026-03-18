package main

import "fmt"

// Version is set at build time via -ldflags.
var Version = "dev"

func versionBanner() string {
	return fmt.Sprintf(`sortie %s
Copyright (C) 2026 Serghei Iakovlev <oss@serghei.pl>

This is free software; see the source for copying conditions.  There is NO
warranty; not even for MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.
`, Version)
}
