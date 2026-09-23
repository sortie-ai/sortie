#!/bin/sh
# Check comments for spec labels, internal references and decorations.
# Usage: comments.sh [FILE...]
# Without arguments, checks the files changed since the merge base with
# origin/main (or main), including uncommitted and untracked ones.

set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/lib/common.sh
. "${SCRIPT_DIR}/lib/common.sh"

changed_files() {
	base=$(git merge-base HEAD origin/main 2>/dev/null || git merge-base HEAD main)
	{
		git diff --name-only --diff-filter=d "$base"
		git ls-files --others --exclude-standard
	} | sort -u
}

# This file is made of the patterns it forbids.
self="scripts/comments.sh"

check() {
	file=$1
	case "$file" in
	*"$self") return 0 ;;
	*.go) marker='//' ;;
	*.sh | *.py | *.mk | Makefile | */Makefile) marker='#' ;;
	*) return 0 ;;
	esac
	[ -f "$file" ] || return 0

	# Only test names and assertion messages are checked outside comments; in
	# shipped code a path inside a string is the payload of a help text.
	case "$file" in
	*_test.go) in_test=1 ;;
	*) in_test=0 ;;
	esac

	awk -v marker="$marker" -v in_test="$in_test" '
# No apostrophe may appear in this awk source: it lives in a single-quoted
# shell string, and one apostrophe would end that string.
BEGIN {
  SQ = sprintf("%c", 39)
  FENCE_DOUBLE = "\"\"\""
  FENCE_SINGLE = SQ SQ SQ
  QUOTES = "\"" SQ
  if (marker == "//") QUOTES = QUOTES "`"

  SEQ_LABEL = "(^|[^[:alnum:]])(step|phase|check|rule|section|part|case|pass|stage|round|scenario)[[:space:]]+[0-9]"
  SPEC_NOUN = "(^|[^[:alnum:]])(Table|Tables|Appendix|Figure|Diagram|Criterion|Criteria|Requirement|Spec)[-[:space:]]+[0-9]"
  DOC_REF   = "(docs/architecture|docs/decisions|architecture\\.md|architecture-digest|\\.specs/|\\.plans/|ADR-?[0-9])"
  SPEC_PREFIX = "(^|[^[:alnum:]])(AC|FR|NFR|REQ|US)-[0-9]"
  AGENT_CONTEXT = "(^|[^[:alnum:]_])(AGENTS|CLAUDE|CURSOR|GEMINI)\\.md"

  # A letter-and-digits token is as often a register or a standard (R0, C99)
  # as a plan label, so only an attesting verb before it and a continuation a
  # plan label takes after it mark the citation.
  ATTESTED_ID = "(^|[^[:alnum:]_])([Vv]erifies|[Cc]overs|[Pp]ins|[Pp]roves|[Pp]er)[[:space:]]+([A-Z]|[A-Z][A-Z][0-9])[0-9][0-9]?(:|,|/|" SQ "s|\\.([[:space:]]|$)|[[:space:]]+(and|for)([^[:alnum:]]|$)|[[:space:]]*$)"

  # A fixture row shares the plan-label shape (head H1), so only a label noun
  # confirms a family for the file; a range alone does not.
  PLAN_ID = "[A-Z][0-9][0-9]?"
  BARE_PLAN_ID = "(^|[^[:alnum:]_])" PLAN_ID "([^[:alnum:]_]|$)"
  PLAN_LABEL = "(^|[^[:alnum:]_])([Pp]ropert(y|ies)|[Ii]nvariants?|[Rr]ules?|[Ss]teps?|[Pp]hases?|[Ss]tages?)[[:space:]]+[(]?" PLAN_ID "([^[:alnum:]_]|$)"
  PLAN_LABEL = PLAN_LABEL "|(^|[^[:alnum:]_])" PLAN_ID "[[:space:]]+(step|phase|stage|reconciliation|probe|property|invariant|rule)s?([^[:alnum:]_]|$)"
  PLAN_RANGE = "(^|[^[:alnum:]_])" PLAN_ID "[[:space:]]+(to|through)[[:space:]]+" PLAN_ID "([^[:alnum:]_]|$)"

  VERIFICATION_PROPERTY = "(verification[[:space:]]+property[[:space:]]+[0-9]|property[[:space:]]+[0-9][[:space:]]+of[[:space:]]+the[[:space:]]+verification)"

  # Every other single letter is fixture issue data (C-1, D-1).
  TEST_TYPE = "(^|[^[:alnum:]])(I|U|Q|S)-[0-9]"

  # Three characters, spelled out rather than as an interval so BSD awk applies
  # it too; two would catch "// == false" and the flag "--no-ask-user".
  FRAME = "^[[:space:]]*[-=*#~_+][-=*#~_+][-=*#~_+]"
  HLINE = "(─|━|═|┄|┅|┈|┉|╌|╍)"
  DRAWN_FRAME = "^[[:space:]]*" HLINE "|" HLINE HLINE HLINE "[[:space:]]*$"

  PREFORMATTED     = "^\t"
  EDITOR_DIRECTIVE = "^[[:space:]]*-\\*-.*-\\*-"
  BOX_BORDER       = "^[[:space:]]*\\+-+\\+"

  # The separator before the number keeps time.RFC3339 out of the exemption.
  RFC_CITATION = "(^|[^[:alnum:]])RFC[[:space:]-]+[0-9]"

  # A leading zero or a trailing hex letter marks a color, not an issue number.
  ISSUE_REF = "(^|[^[:alnum:]_./-])#[1-9][0-9]*($|[^0-9a-fA-F])"

  EM_DASH      = "—"
  SECTION_MARK = "§[[:space:]]*[0-9]"

  # Spelled out: under the C locale a bracket range matches single bytes, and
  # an arrow shares its lead byte with the em-dash and the curly quotes.
  ARROW = "(←|↑|→|↓|↔|↕|↖|↗|↘|↙|↚|↛|↜|↝|↞|↟|↠|↡|↢|↣|↤|↥|↦|↧|↨|↩|↪|↫|↬|↭|↮|↯"
  ARROW = ARROW "|↰|↱|↲|↳|↴|↵|↶|↷|↸|↹|↺|↻|↼|↽|↾|↿|⇀|⇁|⇂|⇃|⇄|⇅|⇆|⇇|⇈|⇉|⇊|⇋|⇌|⇍|⇎|⇏"
  ARROW = ARROW "|⇐|⇑|⇒|⇓|⇔|⇕|⇖|⇗|⇘|⇙|⇚|⇛|⇜|⇝|⇞|⇟|⇠|⇡|⇢|⇣|⇤|⇥|⇦|⇧|⇨|⇩|⇪|⇫|⇬|⇭|⇮|⇯"
  ARROW = ARROW "|⇰|⇱|⇲|⇳|⇴|⇵|⇶|⇷|⇸|⇹|⇺|⇻|⇼|⇽|⇾|⇿|⟰|⟱|⟲|⟳|⟴|⟵|⟶|⟷|⟸|⟹|⟺|⟻|⟼|⟽|⟾|⟿)"
}
# The file is read twice: a label may be confirmed below its first mention.
FNR == 1 { open_span = "" }
NR == FNR {
  scan($0)
  for (i = 1; i <= comment_count; i++) confirm_plan_families(comment_text[i])
  next
}
{
  scan($0)
  for (i = 1; i <= comment_count; i++) {
    kind = classify(comment_text[i])
    if (kind != "") { report(kind); next }
  }
  if (in_test == 1 && (code ~ SPEC_NOUN || code ~ DOC_REF))
    report("spec reference outside a comment")
}

# A block comment, a raw string and a docstring fence stay open into the next
# record; a plain quote closes at the end of its own.
function scan(s,   i, n, ch, three) {
  comment_count = 0
  code = ""
  i = 1
  n = length(s)
  while (i <= n) {
    if (open_span != "") { i = close_span(s, i); continue }
    ch = substr(s, i, 1)
    three = substr(s, i, 3)
    if (marker == "#" && (three == FENCE_DOUBLE || three == FENCE_SINGLE)) {
      open_span = three
      span_kind = "prose"
      i += 3
    } else if (substr(s, i, length(marker)) == marker) {
      add_comment(substr(s, i + length(marker)))
      i = n + 1
    } else if (marker == "//" && substr(s, i, 2) == "/*") {
      open_span = "*/"
      span_kind = "comment"
      i += 2
    } else if (index(QUOTES, ch) > 0) {
      open_span = ch
      span_kind = "code"
      code = code ch
      i++
    } else {
      code = code ch
      i++
    }
  }
  if (open_span == "\"" || open_span == SQ) open_span = ""
}

function close_span(s, from,   at) {
  at = span_end(s, from, open_span)
  collect(at > 0 ? substr(s, from, at - from) : substr(s, from), from == 1)
  if (at == 0) return length(s) + 1
  if (span_kind == "code") code = code open_span
  at += length(open_span)
  open_span = ""
  return at
}

function collect(text, block_continuation) {
  if (span_kind == "comment") {
    if (block_continuation) sub(/^[[:space:]]*\*/, "", text)
    add_comment(text)
  } else if (span_kind == "code") {
    code = code text
  }
}

function add_comment(text) { comment_text[++comment_count] = text }

# A backslash escapes the next character inside a quote, never inside a comment.
function span_end(s, from, delim,   i, last, width) {
  width = length(delim)
  last = length(s) - width + 1
  for (i = from; i <= last; i++) {
    if (delim != "*/" && substr(s, i, 1) == "\\") { i++; continue }
    if (substr(s, i, width) == delim) return i
  }
  return 0
}

function report(kind) { printf("%s:%d: [%s] %s\n", FILENAME, FNR, kind, $0) }

function confirm_plan_families(c,   label) {
  while (match(c, PLAN_LABEL)) {
    label = substr(c, RSTART, RLENGTH)
    c = substr(c, RSTART + RLENGTH)
    while (match(label, PLAN_ID)) {
      plan_family[substr(label, RSTART, 1)] = 1
      label = substr(label, RSTART + RLENGTH)
    }
  }
}

function cites_plan_family(c,   id) {
  while (match(c, BARE_PLAN_ID)) {
    id = substr(c, RSTART, RLENGTH)
    c = substr(c, RSTART + RLENGTH)
    match(id, PLAN_ID)
    if (substr(id, RSTART, 1) in plan_family) return 1
  }
  return 0
}

function classify(c) {
  if (c ~ RFC_CITATION) return ""
  if (tolower(c) ~ SEQ_LABEL) return "sequence/section label"
  if (c ~ SPEC_NOUN) return "spec-criteria reference"
  if (c ~ SPEC_PREFIX) return "spec-criteria reference"
  if (c ~ ATTESTED_ID) return "spec-criteria reference"
  if (c ~ PLAN_LABEL || c ~ PLAN_RANGE || cites_plan_family(c)) return "spec-criteria reference"
  if (tolower(c) ~ VERIFICATION_PROPERTY) return "spec-criteria reference"
  if (c ~ TEST_TYPE) return "test-type reference"
  if (c ~ DOC_REF) return "internal doc/ADR reference"
  if (c ~ AGENT_CONTEXT) return "agent context-file reference"
  if ((c ~ FRAME || c ~ DRAWN_FRAME) && c !~ PREFORMATTED && c !~ EDITOR_DIRECTIVE && c !~ BOX_BORDER) return "banner decoration"
  if (index(c, EM_DASH) > 0) return "em-dash"
  if (c ~ ARROW) return "unicode arrow"
  if (c ~ SECTION_MARK) return "section-mark reference"
  if (c ~ ISSUE_REF) return "internal issue number"
  return ""
}
' "$file" "$file"
}

explain() {
	log "Comments must not carry what rots or points outside the tree:"
	log "  - labels: Step 2, Phase 1, AC-7, FR-1, I-1, property P5, D1 to D3, Table 3"
	log "  - references: docs/architecture, docs/decisions, ADR-3, .specs/, .plans/,"
	log "    a section sign with a number, #811, AGENTS.md or CLAUDE.md"
	log "  - decorations: banner lines, em-dashes, unicode arrows (write ->)"
	log "Delete the label or reference and keep the plain-language reason."
	log "Allowed: data IDs (PROJ-42, C-1), ISO-8601, SHA-256, RFC 7231, a spaced"
	log "en-dash, golang/go#22315, a hex color."
}

if [ "$#" -eq 0 ]; then
	files=$(changed_files)
else
	files=$(printf '%s\n' "$@")
fi

found=$(printf '%s\n' "$files" | while IFS= read -r file; do check "$file"; done)
if [ -n "$found" ]; then
	printf '%s\n' "$found"
	explain
	exit 1
fi
