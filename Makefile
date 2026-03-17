.POSIX:

MODULE   := github.com/sergeyklay/sortie
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -ldflags "-s -w -X main.Version=$(VERSION)"

GO       := go
LINT     := golangci-lint

.PHONY: fmt lint test build clean

## fmt: format all Go source files
fmt:
	$(GO) fmt ./...

## lint: run golangci-lint on all packages
lint:
	$(LINT) run ./...

## test: run all tests
test:
	$(GO) test -race -count=1 ./...

## build: compile the sortie binary
build:
	$(GO) build $(LDFLAGS) -o sortie ./cmd/sortie

## clean: remove build artifacts
clean:
	rm -f sortie
