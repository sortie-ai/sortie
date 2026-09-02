// Command schemagen generates internal/agent/clientprotocol/wire_gen.go from
// the pinned Agent Client Protocol schema artifact. It is never imported and
// never enters the shipped binary; it is invoked only through go generate.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: schemagen <assets-dir> <destination>")
		os.Exit(1)
	}
	assetsDir, dest := os.Args[1], os.Args[2]

	out, err := Generate(assetsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "schemagen: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(dest, out, 0o600); err != nil { //nolint:gosec // G703: dest is the operator-supplied destination positional argument
		fmt.Fprintf(os.Stderr, "schemagen: write %s: %v\n", dest, err)
		os.Exit(1)
	}
}
