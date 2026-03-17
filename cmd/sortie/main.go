package main

import (
	"fmt"
	"os"
)

// Version is set at build time via -ldflags.
var Version = "dev"

func main() {
	fmt.Printf("sortie %s\n", Version)
	os.Exit(0)
}
