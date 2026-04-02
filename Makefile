# Makefile — build, test, and quality targets for Sortie.
#
# Run "make" or "make help" to see all targets and configurable variables.
# Requires GNU make >= 3.81.

include default.mk

# ── Building ──────────────────────────────────────────────────────────────────

##@ Building

.PHONY: build
build: ## Compile the sortie binary into the repo root
	$(GO) build $(BUILDFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/sortie

.PHONY: clean
clean: ## Remove the binary and all coverage files
	$(RM) $(BIN) $(COVERAGE_OUT) $(COVERAGE_HTML)

# ── Testing ───────────────────────────────────────────────────────────────────

##@ Testing

.PHONY: test
test: ## Run tests with the race detector (PKG=./path/to/pkg  RUN=TestFoo)
	$(GO) test -race -count=1 \
		$(if $(PKG),$(PKG),./...) \
		$(if $(RUN),-run $(RUN),)

.PHONY: test-coverage
test-coverage: ## Run tests with coverage and print per-package percentages
	$(GO) test -race -count=1 -covermode=atomic -coverprofile=$(COVERAGE_OUT) \
		$(if $(PKG),$(PKG),./...) \
		$(if $(RUN),-run $(RUN),)
	$(GO) tool cover -func=$(COVERAGE_OUT)

.PHONY: test-coverage-html
test-coverage-html: test-coverage ## Generate an HTML coverage report
	$(GO) tool cover -html=$(COVERAGE_OUT) -o $(COVERAGE_HTML)
	@printf '$(GREEN)Coverage report written to $(BOLD)$(COVERAGE_HTML)$(RESET)\n'

# ── Quality ───────────────────────────────────────────────────────────────────

##@ Quality

.PHONY: fmt
fmt: ## Format all Go source files
	$(LINT) fmt ./...

.PHONY: vet
vet: ## Run go vet on all packages
	$(GO) vet ./...

.PHONY: lint
lint: ## Run golangci-lint on all packages
	$(LINT) run ./...

.PHONY: tidy
tidy: ## Tidy go.sum and prune stale entries from go.mod
	$(GO) mod tidy

.PHONY: style
style: ## Enforce style guidelines via Copilot CLI  (FLAGS=…  FILE=path)
	sh scripts/enforce-style.sh $(FLAGS) $(if $(FILE),'$(FILE)',)

# ── Utilities ─────────────────────────────────────────────────────────────────

##@ Utilities

.PHONY: version
version: ## Print the current version string
	@printf '$(BOLD)$(BIN)$(RESET) version $(CYAN)$(VERSION)$(RESET)\n'

.PHONY: help
help: ## Show this help message and exit
ifdef _COLORS_OK
	@printf '\n'
	@printf '$(CYAN)  ███████╗ ██████╗ ██████╗ ████████╗██╗███████╗$(RESET)\n'
	@printf '$(CYAN)  ██╔════╝██╔═══██╗██╔══██╗╚══██╔══╝██║██╔════╝$(RESET)\n'
	@printf '$(CYAN)  ███████╗██║   ██║██████╔╝   ██║   ██║█████╗  $(RESET)\n'
	@printf '$(CYAN)  ╚════██║██║   ██║██╔══██╗   ██║   ██║██╔══╝  $(RESET)\n'
	@printf '$(CYAN)  ███████║╚██████╔╝██║  ██║   ██║   ██║███████╗$(RESET)\n'
	@printf '$(CYAN)  ╚══════╝ ╚═════╝ ╚═╝  ╚═╝   ╚═╝   ╚═╝╚══════╝$(RESET)\n'
else
	@printf '\n  Sortie\n'
endif
	@printf '  $(DIM)Turns issue tickets into autonomous sessions$(RESET)\n\n'
	@printf '$(BOLD)Usage$(RESET)\n'
	@printf '  make $(CYAN)<target>$(RESET) [$(CYAN)VAR=value$(RESET) ...]\n'
	@awk -v bold='$(BOLD)' -v cmd='$(BOLD)$(YELLOW)' -v reset='$(RESET)' \
		'BEGIN { FS = ":.*##" } \
		/^##@/  { printf "\n%s%s%s\n", bold, substr($$0, 5), reset } \
		/^[a-zA-Z0-9_][a-zA-Z0-9_-]*:.*## / \
		        { printf "  %s%-24s%s %s\n", cmd, $$1, reset, $$2 }' \
		$(MAKEFILE_LIST)
	@printf '\n$(BOLD)Variables$(RESET)\n'
	@printf '  $(CYAN)%-24s$(RESET) %s\n' \
		'PKG'      ' Package filter for test targets (default: ./...)' \
		'RUN'      ' Test name filter, passed to -run' \
		'FLAGS'    ' Extra flags for the style target' \
		'FILE'     ' Single file target for style' \
		'VERSION'  ' Override version string (default: git describe)' \
		'NO_COLOR' ' Disable color output (https://no-color.org/)'
	@printf '\n'
