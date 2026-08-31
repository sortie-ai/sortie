#!/bin/sh
# Fail if an actions/checkout step omits persist-credentials, so the
# token's disposition is always explicit rather than the action's default.
# Usage: checkout-creds.sh [DIR]   (DIR defaults to .github/workflows)

set -eu

dir=${1:-.github/workflows}
checked=0
failed=0

# POSIX sh cannot return two values; sets IND and LT.
split_indent() {
	_lead=${1%%[! 	]*}
	LT=${1#"$_lead"}
	IND=${#_lead}
}

# Fails closed: an open anchor that never saw the key is reported.
close_anchor() {
	if [ -n "$anchor_line" ] && [ "$anchor_compliant" -eq 0 ]; then
		printf '%s:%s: no persist-credentials key in step\n' "$file" "$anchor_line"
		failed=$((failed + 1))
	fi
	anchor_line=""
	anchor_indent=0
	anchor_compliant=0
	with_indent=-1
	with_child=-1
}

# An anchor is a uses: key naming actions/checkout@; a mention inside a
# comment or a run: body is not one. The key counts only as a direct
# child of the step's with: mapping, never nested deeper. A step region spans from that anchor
# to the next sequence
# item at or above its indentation, the next non-blank line below it, or
# EOF. Blank lines never end a region. Only a persist-credentials under
# the step's own with: mapping is an actions/checkout input; under env:
# it is an unrelated variable and the input keeps its default.
scan_file() {
	file=$1
	anchor_line=""
	anchor_indent=0
	anchor_compliant=0
	with_indent=-1
	with_child=-1
	lineno=0

	while IFS= read -r raw || [ -n "$raw" ]; do
		lineno=$((lineno + 1))
		split_indent "$raw"

		if [ -n "$anchor_line" ] && [ -n "$LT" ]; then
			case "$LT" in
			-\ *)
				if [ "$IND" -le "$anchor_indent" ]; then
					close_anchor
				fi
				;;
			*)
				if [ "$IND" -lt "$anchor_indent" ]; then
					close_anchor
				fi
				;;
			esac
		fi

		if [ "$with_indent" -ge 0 ] && [ -n "$LT" ] && [ "$IND" -le "$with_indent" ]; then
			with_indent=-1
			with_child=-1
		fi

		if [ "$with_indent" -ge 0 ] && [ "$with_child" -lt 0 ] && [ -n "$LT" ]; then
			with_child=$IND
		fi

		case "$LT" in
		uses:*actions/checkout@* | -\ uses:*actions/checkout@*)
			close_anchor
			checked=$((checked + 1))
			anchor_line=$lineno
			anchor_indent=$IND
			;;
		with:*)
			if [ -n "$anchor_line" ] && [ "$IND" -ge "$anchor_indent" ]; then
				with_indent=$IND
			fi
			;;
		persist-credentials:*)
			if [ "$with_child" -ge 0 ] && [ "$IND" -eq "$with_child" ]; then
				_val=${LT#persist-credentials:}
				_val=${_val%% #*}
				_val=$(printf '%s' "$_val" | tr -d ' 	')
				case "$_val" in
				true | false)
					anchor_compliant=1
					;;
				esac
			fi
			;;
		esac
	done <"$file"

	close_anchor
}

for candidate in "$dir"/*.yml "$dir"/*.yaml; do
	[ -e "$candidate" ] || continue
	scan_file "$candidate"
done

[ "$failed" -eq 0 ] || exit 1
printf 'checkout-creds: %s step(s) checked, all compliant\n' "$checked"
