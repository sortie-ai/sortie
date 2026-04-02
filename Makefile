.POSIX:

MODULE   := github.com/sortie-ai/sortie
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -ldflags "-s -w -X main.Version=$(VERSION)"

GO           := go
LINT         := golangci-lint
COVERAGE_OUT  ?= coverage.out
COVERAGE_HTML  = $(COVERAGE_OUT:.out=.html)

.PHONY: fmt lint style test test-coverage test-coverage-html build clean

## fmt: format all Go source files
fmt:
	$(LINT) fmt ./...

## lint: run golangci-lint on all packages
lint:
	$(LINT) run ./...

## style: enforce code style and documentation rules (FILE=path to target a single file)
style:
	sh scripts/enforce-style.sh $(FILE)

## test: run tests (use PKG=./path/to/pkg and/or RUN=TestName to filter)
test:
	$(GO) test -race -count=1 $(if $(PKG),$(PKG),./...) $(if $(RUN),-run $(RUN),)

## test-coverage: run tests with coverage profile and print coverage percentage
test-coverage:
	$(GO) test -race -count=1 -coverprofile=$(COVERAGE_OUT) $(if $(PKG),$(PKG),./...) $(if $(RUN),-run $(RUN),)
	$(GO) tool cover -func=$(COVERAGE_OUT)

## test-coverage-html: generate HTML coverage report
test-coverage-html: test-coverage
	$(GO) tool cover -html=$(COVERAGE_OUT) -o $(COVERAGE_HTML)

## build: compile the sortie binary
build:
	$(GO) build $(LDFLAGS) -o sortie ./cmd/sortie

## clean: remove build artifacts
clean:
	rm -f sortie $(COVERAGE_OUT) $(COVERAGE_HTML)
