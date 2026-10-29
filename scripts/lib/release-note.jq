# The announcement part of a release body: GoReleaser appends the New
# Contributors section and the links footer after it.
def release_note:
	((. // "") | gsub("\r"; "") | split("\n")) as $lines
	| ([$lines | index("---"), index("## New Contributors")] | map(select(. != null)) | min // ($lines | length)) as $end
	| $lines[:$end] | join("\n") | sub("\\s+$"; "");
