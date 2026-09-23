package main

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// reportTitle is the report's identity key across runs; it must never vary
// with data.
const reportTitle = "Review the pinned Agent Client Protocol schema against publisher releases"

var safeTagRE = regexp.MustCompile(`^[A-Za-z0-9._/+-]+$`)

// safeTag wraps tag in a Markdown code span, escaping it first when it
// carries characters outside a safe set, so a publisher-chosen tag can
// never break out of the span to create Markdown structure or a mention.
func safeTag(tag string) string {
	if safeTagRE.MatchString(tag) {
		return "`" + tag + "`"
	}
	return "`" + url.PathEscape(tag) + "`"
}

// releaseLink is always constructed from repository and tag, never copied
// from a publisher response.
func releaseLink(repository, tag string) string {
	return fmt.Sprintf("https://github.com/%s/releases/tag/%s", repository, url.PathEscape(tag))
}

func renderTagLink(repository, tag string) string {
	return "[" + safeTag(tag) + "](" + releaseLink(repository, tag) + ")"
}

func renderTitle() string {
	return reportTitle
}

func renderRecordedPin(pin recordedPin) string {
	var b strings.Builder
	b.WriteString("Recorded pin:\n\n")
	fmt.Fprintf(&b, "- Repository: `%s`\n", pin.Repository)
	fmt.Fprintf(&b, "- Tag: %s\n", safeTag(pin.Tag))
	fmt.Fprintf(&b, "- Commit: `%s`\n", pin.Commit)
	for _, a := range pin.Assets {
		fmt.Fprintf(&b, "- %s: `sha256:%s`\n", a.Name, a.SHA256)
	}
	return b.String()
}

func renderNewerSection(d drift) string {
	var b strings.Builder
	b.WriteString("Newer stable releases:\n\n| Tag | Published |")
	for _, a := range d.Pin.Assets {
		fmt.Fprintf(&b, " %s |", a.Name)
	}
	b.WriteString("\n|---|---|")
	for range d.Pin.Assets {
		b.WriteString("---|")
	}
	b.WriteString("\n")
	for _, r := range d.Newer {
		fmt.Fprintf(&b, "| %s | %s |", renderTagLink(d.Pin.Repository, r.Tag), r.Published)
		for _, a := range r.Assets {
			fmt.Fprintf(&b, " %s |", a.Status)
		}
		b.WriteString("\n")
	}

	newest := d.Newer[len(d.Newer)-1]
	fmt.Fprintf(&b, "\nThe newest is %s.\n", renderTagLink(d.Pin.Repository, newest.Tag))

	for _, r := range d.Newer {
		if r.NewMajor {
			fmt.Fprintf(&b, "- %s is a new major schema release; adopting it is a deliberate, separate decision.\n", renderTagLink(d.Pin.Repository, r.Tag))
		}
	}
	return b.String()
}

func renderIntegrityLine(f integrityFinding) string {
	switch f.Kind {
	case "tag_absent":
		return fmt.Sprintf("tag_absent: the publisher no longer lists a ref for tag %s", safeTag(f.Recorded))
	case "tag_moved":
		return fmt.Sprintf("tag_moved: recorded commit `%s`, the tag now resolves to `%s`", f.Recorded, f.Published)
	case "release_absent":
		return fmt.Sprintf("release_absent: the publisher lists no release for tag %s", safeTag(f.Recorded))
	case "asset_absent":
		return fmt.Sprintf("asset_absent: `%s`, recorded digest `%s`, the pinned release no longer carries this asset", f.Subject, f.Recorded)
	case "asset_replaced":
		return fmt.Sprintf("asset_replaced: `%s`, recorded digest `%s`, the pinned release now carries `%s`", f.Subject, f.Recorded, f.Published)
	default:
		return fmt.Sprintf("%s: recorded %q, published %q", f.Kind, f.Recorded, f.Published)
	}
}

func fingerprintMarkerLine(fp string) string {
	return "<!-- protocol-pin-fingerprint: " + fp + " -->"
}

// renderBody omits the fingerprint marker for close, and never writes a run
// URL, a run time, or a value naming what the pin should change to.
func renderBody(action string, d drift, fp string) string {
	var b strings.Builder

	if action == "close" {
		b.WriteString("A scheduled comparison of the pinned Agent Client Protocol schema against the publisher's releases found no difference of any kind, and this report is closed.\n\n")
		b.WriteString(renderRecordedPin(d.Pin))
		return strings.TrimRight(b.String(), "\n")
	}

	b.WriteString("A scheduled comparison of the pinned Agent Client Protocol schema against the publisher's releases found the difference described below. This report only ever describes the difference; it never changes the pin. See the \"Pinned schema artifact\" section of the Agent Client Protocol adapter notes.\n\n")
	b.WriteString(renderRecordedPin(d.Pin))

	if len(d.Newer) > 0 {
		b.WriteString("\n")
		b.WriteString(renderNewerSection(d))
	}

	if len(d.Integrity) > 0 {
		b.WriteString("\nIntegrity findings:\n\n")
		for _, f := range d.Integrity {
			b.WriteString("- " + renderIntegrityLine(f) + "\n")
		}
	}

	if len(d.Unrecognized) > 0 {
		b.WriteString("\nUnrecognized releases:\n\n")
		for _, u := range d.Unrecognized {
			fmt.Fprintf(&b, "- %s, published %s\n", renderTagLink(d.Pin.Repository, u.Tag), u.Published)
		}
	}

	b.WriteString("\nThis report closes itself on the first run that finds no difference of any kind. Closing it by hand keeps this exact difference quiet until it changes.\n\n")
	b.WriteString(fingerprintMarkerLine(fp))

	return b.String()
}

// renderComment excludes the full difference, which the body already holds.
// hadMarker distinguishes, for reopen, a difference returning after the
// report carried no marker from one that changed after a person closed it.
func renderComment(action string, d drift, hadMarker bool, runURL, now string) string {
	if action == "close" {
		return fmt.Sprintf("A scheduled comparison found no difference of any kind. Run: %s, at %s.", runURL, now)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Recorded tag %s, %s. %d integrity finding(s), %d unrecognized release(s).",
		safeTag(d.Pin.Tag), newestPhrase(d), len(d.Integrity), len(d.Unrecognized))

	if action == "reopen" {
		if hadMarker {
			b.WriteString(" This difference changed since a person closed this report.")
		} else {
			b.WriteString(" This difference returned after the report was resolved.")
		}
	}

	fmt.Fprintf(&b, " Run: %s, at %s.", runURL, now)
	return b.String()
}

// renderNotify carries no URL: the shell appends the issue URL itself.
func renderNotify(action string, d drift) string {
	var verb string
	switch action {
	case "open":
		verb = "opened"
	case "update":
		verb = "updated"
	case "reopen":
		verb = "reopened"
	default:
		return ""
	}

	return fmt.Sprintf("Protocol schema pin report %s: recorded tag %s, %s.", verb, safeTag(d.Pin.Tag), newestPhrase(d))
}

func newestPhrase(d drift) string {
	if len(d.Newer) == 0 {
		return "no newer stable release"
	}
	return "newest stable release " + safeTag(d.Newer[len(d.Newer)-1].Tag)
}

// renderSummary treats reportNumber 0 as ambiguous between a brand-new
// report and no report at all, resolving it from action instead.
func renderSummary(d drift, action string, reportNumber int) string {
	var issueRef string
	switch {
	case action == "open":
		issueRef = "new"
	case reportNumber == 0:
		issueRef = "none"
	default:
		issueRef = fmt.Sprintf("#%d", reportNumber)
	}

	newest := "none newer"
	if len(d.Newer) > 0 {
		newest = safeTag(d.Newer[len(d.Newer)-1].Tag)
	}

	return fmt.Sprintf(
		"| Recorded tag | Newest stable | Integrity findings | Unrecognized releases | Action | Report |\n"+
			"|---|---|---|---|---|---|\n"+
			"| %s | %s | %d | %d | %s | %s |\n",
		safeTag(d.Pin.Tag), newest, len(d.Integrity), len(d.Unrecognized), action, issueRef)
}
