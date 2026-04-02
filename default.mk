# default.mk — project variables, tool detection, and output formatting.
#
# Included by the root Makefile.  Every variable is defined with ?= so any of
# them can be overridden on the command line or in the environment:
#
#   make build VERSION=v1.2.3
#   make lint  LINT=/usr/local/bin/golangci-lint
#
# Requires GNU make >= 3.81 (the version shipped with macOS Xcode CLT).

# The directory that contains this file, which is also the repository root.
TOP := $(dir $(lastword $(MAKEFILE_LIST)))

# ── Project metadata ──────────────────────────────────────────────────────────

MODULE := github.com/sortie-ai/sortie
BIN    := sortie

.DEFAULT_GOAL := help

# ── Versioning ────────────────────────────────────────────────────────────────
#
# Derived from the nearest reachable git tag.  Falls back to "dev" in shallow
# clones, detached HEADs without tags, or non-git directories.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

# ── Go toolchain ──────────────────────────────────────────────────────────────

GO   ?= go
LINT ?= golangci-lint

# ── Build flags ───────────────────────────────────────────────────────────────
#
# -trimpath   strips local file-system paths for reproducible builds.
# -s -w       strip the symbol table and DWARF info to shrink the binary.
# -X          embeds the version string at link time.

GOFLAGS    ?=
LDFLAGS    := -s -w -X main.Version=$(VERSION)
BUILDFLAGS ?= -trimpath $(GOFLAGS)

# ── Test and coverage ─────────────────────────────────────────────────────────

COVERAGE_OUT  ?= coverage.out
COVERAGE_HTML  = $(COVERAGE_OUT:.out=.html)

# ── Color / formatting ────────────────────────────────────────────────────────
#
# Honors three opt-out signals:
#   NO_COLOR  — set to any value to disable (https://no-color.org/)
#   CI        — set to any value (most CI/CD platforms set this automatically)
#   TERM=dumb — indicates a terminal with no escape-sequence support
#
# When all conditions are satisfied, colors are enabled by embedding the real
# ESC byte (0x1B) once via printf rather than using \033 literals throughout —
# this keeps every downstream variable self-contained and portable across both
# GNU awk and BSD awk (macOS).
#
# _COLORS_OK is the single gate: downstream code checks only this variable.

_COLORS_OK :=
ifeq  ($(NO_COLOR),)
ifeq  ($(CI),)
ifneq ($(TERM),)
ifneq ($(TERM),dumb)
  _COLORS_OK := yes
endif
endif
endif
endif

ifeq ($(_COLORS_OK),yes)
  _ESC   := $(shell printf '\033')
  BOLD   := $(_ESC)[1m
  DIM    := $(_ESC)[2m
  RED    := $(_ESC)[31m
  GREEN  := $(_ESC)[32m
  YELLOW := $(_ESC)[33m
  CYAN   := $(_ESC)[36m
  RESET  := $(_ESC)[0m
else
  _ESC   :=
  BOLD   :=
  DIM    :=
  RED    :=
  GREEN  :=
  YELLOW :=
  CYAN   :=
  RESET  :=
endif
