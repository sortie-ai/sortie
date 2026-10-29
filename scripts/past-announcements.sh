#!/bin/sh
# Print the announcements of the latest Sortie releases from the docs changelog.
# Exits non-zero when the download fails or no announcement is found.
#
# Usage: past-announcements.sh [version-to-skip]

set -eu

url=https://raw.githubusercontent.com/sortie-ai/docs/main/content/changelog.md
limit=5
skip=${1:-}

changelog=$(curl -fsSL --retry 3 "$url")

printf '%s\n' "$changelog" | awk -v limit="$limit" -v skip="$skip" '
function emit() {
    if (version != "" && version != skip && note != "") {
        printf "<release version=\"%s\" date=\"%s\">\n%s\n</release>\n", version, date, note
        count++
    }
    version = ""; note = ""; gap = 0
    if (count == limit) exit
}

/^## / {
    emit()
    if ($2 ~ /^\[[0-9]+\.[0-9]+\.[0-9]+\]$/ && $3 == "-" &&
        $4 ~ /^[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]$/) {
        version = substr($2, 2, length($2) - 2)
        date = $4
    }
    next
}

/^### / { emit(); next }

version == "" { next }

/^[[:space:]]*$/ { gap = (note != ""); next }

{
    note = note == "" ? $0 : note (gap ? "\n\n" : "\n") $0
    gap = 0
}

END {
    emit()
    if (count == 0) {
        print "past-announcements: no release announcement found in the changelog" > "/dev/stderr"
        exit 1
    }
}
'
