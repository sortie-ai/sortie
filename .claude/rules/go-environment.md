# Go Environment

Go is managed by asdf. The `go` binary resolves through `~/.asdf/shims/go`.

## Commands

Use Makefile targets for all build, test, and lint operations:

- Format: `make fmt`
- Lint: `make lint`
- Build: `make build`
- Run tests: `make test`
- Run package tests: `make test PKG=./internal/persistence`
- Run single test: `make test RUN=TestOpenStore`
- Run single test in package: `make test PKG=./internal/persistence RUN=TestOpenStore`


Read the Makefile to discover available targets before running any Go toolchain commands directly.

## Constraints

<constraint>
NEVER prefix commands with `GOPATH=...`, `GOMODCACHE=...`, or any Go environment overrides. The asdf shim configures everything. Adding these overrides breaks the toolchain resolution.
</constraint>

<constraint>
NEVER run `go test`, `go build`, `go vet`, or `golangci-lint` directly. Use the corresponding `make` target. The Makefile sets flags, tags, and environment correctly.
</constraint>

<constraint>
NEVER use `/usr/local/go/bin/go`, `/usr/bin/go`, or any absolute path to a Go binary. If `go` is not found, the problem is asdf configuration, not a missing binary. Do not attempt to fix it.
</constraint>

<constraint>
NEVER downgrade the `go` directive in `go.mod`. NEVER add or modify `toolchain` directives in `go.mod` unless explicitly asked.
</constraint>

## CI and Dockerfiles

When writing CI workflows or Dockerfiles, pin the Go version to match `.tool-versions`. Do not use `latest` or `1.x`.

## Examples

<example type="correct">
```bash
make test
make lint
make build
```
</example>

<example type="incorrect">
```bash
GOPATH=$HOME/go GOMODCACHE=$HOME/go/pkg/mod go test -race -count=1 ./... 2>&1
go test ./...
/usr/local/go/bin/go build ./cmd/sortie
```
</example>
