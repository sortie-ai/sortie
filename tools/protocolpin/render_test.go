package main

import (
	"strings"
	"testing"
)

func driftWithNewerAndFindings() drift {
	pin := testPin()
	return drift{
		Pin: pin,
		Newer: []newerRelease{
			{
				Tag: "schema-v1.22.0", Published: "2026-09-01",
				Assets: []assetComparison{{Name: "meta.json", Status: "unchanged"}, {Name: "schema.json", Status: "changed"}},
			},
			{
				Tag: "schema-v2.0.0", Published: "2026-09-10", NewMajor: true,
				Assets: []assetComparison{{Name: "meta.json", Status: "unchanged"}, {Name: "schema.json", Status: "not published"}},
			},
		},
		Integrity: []integrityFinding{
			{Kind: "asset_replaced", Subject: "schema.json", Recorded: "sha256:" + strings.Repeat("1", 64), Published: "sha256:" + strings.Repeat("9", 64)},
		},
		Unrecognized: []unrecognizedRelease{
			{Tag: "v2.0.0-custom", Published: "2026-09-05"},
		},
	}
}

func TestRenderBodyContent(t *testing.T) {
	t.Parallel()

	fp := "sha256:" + strings.Repeat("a", 64)

	t.Run("open, update, and reopen carry the marker and no run data", func(t *testing.T) {
		t.Parallel()

		for _, action := range []string{"open", "update", "reopen"} {
			d := driftWithNewerAndFindings()
			body := renderBody(action, d, fp)

			if !strings.HasSuffix(strings.TrimRight(body, "\n"), "<!-- protocol-pin-fingerprint: "+fp+" -->") {
				t.Errorf("renderBody(%s, ...) does not end with the fingerprint marker:\n%s", action, body)
			}
			if !strings.Contains(body, d.Pin.Repository) || !strings.Contains(body, d.Pin.Commit) {
				t.Errorf("renderBody(%s, ...) does not name the recorded pin's repository or commit:\n%s", action, body)
			}
			if !strings.Contains(body, "schema-v1.22.0") || !strings.Contains(body, "schema-v2.0.0") {
				t.Errorf("renderBody(%s, ...) does not name every newer release:\n%s", action, body)
			}
			if !strings.Contains(body, "asset_replaced") {
				t.Errorf("renderBody(%s, ...) does not carry the integrity finding:\n%s", action, body)
			}
			if !strings.Contains(body, "v2.0.0-custom") {
				t.Errorf("renderBody(%s, ...) does not carry the unrecognized release:\n%s", action, body)
			}
			if strings.Contains(body, "http://") && strings.Contains(body, "actions/runs") {
				t.Errorf("renderBody(%s, ...) carries a run URL:\n%s", action, body)
			}
			for _, forbidden := range []string{"Run:", "run_url", "replace the pin with", "new tag:"} {
				if strings.Contains(strings.ToLower(body), strings.ToLower(forbidden)) {
					t.Errorf("renderBody(%s, ...) contains forbidden text %q:\n%s", action, forbidden, body)
				}
			}
		}
	})

	t.Run("close carries no marker and no run data", func(t *testing.T) {
		t.Parallel()

		d := driftWithNewerAndFindings()
		body := renderBody("close", d, "")

		if strings.Contains(body, "protocol-pin-fingerprint") {
			t.Errorf("renderBody(close, ...) carries a fingerprint marker:\n%s", body)
		}
		if !strings.Contains(body, d.Pin.Repository) || !strings.Contains(body, d.Pin.Commit) {
			t.Errorf("renderBody(close, ...) does not name the recorded pin:\n%s", body)
		}
		if strings.Contains(body, "Run:") {
			t.Errorf("renderBody(close, ...) carries run data:\n%s", body)
		}
	})
}

func TestRenderCommentAndNotify(t *testing.T) {
	t.Parallel()

	d := driftWithNewerAndFindings()
	runURL := "https://github.com/sortie-ai/sortie/actions/runs/123"
	now := "2026-09-23 12:00 UTC"

	t.Run("update and reopen name the counts, the newest tag, and the run, but not the full difference", func(t *testing.T) {
		t.Parallel()

		for _, action := range []string{"update", "reopen"} {
			comment := renderComment(action, d, false, runURL, now)
			if !strings.Contains(comment, runURL) || !strings.Contains(comment, now) {
				t.Errorf("renderComment(%s, ...) = %q, want the run URL and now", action, comment)
			}
			if !strings.Contains(comment, "schema-v2.0.0") {
				t.Errorf("renderComment(%s, ...) = %q, want the newest stable tag", action, comment)
			}
			if strings.Contains(comment, "asset_replaced") || strings.Contains(comment, "v2.0.0-custom") {
				t.Errorf("renderComment(%s, ...) = %q, want no full difference: the body already holds it", action, comment)
			}
		}
	})

	t.Run("reopen distinguishes an unmarked return from a changed one", func(t *testing.T) {
		t.Parallel()

		unmarked := renderComment("reopen", d, false, runURL, now)
		changed := renderComment("reopen", d, true, runURL, now)
		if unmarked == changed {
			t.Error("renderComment(reopen, hadMarker=false) equals renderComment(reopen, hadMarker=true), want distinct wording")
		}
	})

	t.Run("close states no difference and carries the run", func(t *testing.T) {
		t.Parallel()

		comment := renderComment("close", d, false, runURL, now)
		if !strings.Contains(comment, runURL) || !strings.Contains(comment, now) {
			t.Errorf("renderComment(close, ...) = %q, want the run URL and now", comment)
		}
	})

	t.Run("Notify names the action, the recorded tag, and the newest, with no mention or URL", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			action string
			verb   string
		}{
			{"open", "opened"},
			{"update", "updated"},
			{"reopen", "reopened"},
		}
		for _, tt := range tests {
			notify := renderNotify(tt.action, d)
			if notify == "" {
				t.Fatalf("renderNotify(%s, ...) = empty, want non-empty", tt.action)
			}
			if !strings.Contains(notify, tt.verb) {
				t.Errorf("renderNotify(%s, ...) = %q, want it to name the action %q", tt.action, notify, tt.verb)
			}
			if !strings.Contains(notify, d.Pin.Tag) || !strings.Contains(notify, "schema-v2.0.0") {
				t.Errorf("renderNotify(%s, ...) = %q, want the recorded tag and the newest stable tag", tt.action, notify)
			}
			if strings.Contains(notify, "http://") || strings.Contains(notify, "https://") {
				t.Errorf("renderNotify(%s, ...) = %q, want no URL: the shell appends it", tt.action, notify)
			}
			if strings.Contains(notify, "@") {
				t.Errorf("renderNotify(%s, ...) = %q, want no mention", tt.action, notify)
			}
		}

		for _, action := range []string{"close", "none"} {
			if got := renderNotify(action, d); got != "" {
				t.Errorf("renderNotify(%s, ...) = %q, want empty", action, got)
			}
		}
	})
}

func TestRenderSummaryAlwaysNonEmpty(t *testing.T) {
	t.Parallel()

	d := driftWithNewerAndFindings()
	for _, tt := range []struct {
		action       string
		reportNumber int
	}{
		{"none", 0},
		{"none", 42},
		{"open", 0},
		{"update", 7},
		{"reopen", 7},
		{"close", 7},
	} {
		summary := renderSummary(d, tt.action, tt.reportNumber)
		if summary == "" {
			t.Errorf("renderSummary(..., %q, %d) = empty, want non-empty", tt.action, tt.reportNumber)
		}
		if !strings.Contains(summary, d.Pin.Tag) {
			t.Errorf("renderSummary(..., %q, %d) = %q, want the recorded tag", tt.action, tt.reportNumber, summary)
		}
	}

	if got := renderSummary(d, "open", 0); !strings.Contains(got, "new") {
		t.Errorf("renderSummary(..., open, 0) = %q, want it to name the report as new", got)
	}
	if got := renderSummary(d, "none", 0); !strings.Contains(got, "none") {
		t.Errorf("renderSummary(..., none, 0) = %q, want it to name the report as none", got)
	}
	if got := renderSummary(d, "update", 42); !strings.Contains(got, "#42") {
		t.Errorf("renderSummary(..., update, 42) = %q, want it to name the report number", got)
	}
}

func TestSafeTagRendering(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		tag  string
	}{
		{"a conforming tag renders literally", "schema-v1.22.0"},
		{"a tag with a space is escaped", "schema v1.22.0"},
		{"a tag with a backtick is escaped", "schema-v1.22.0`injected`"},
		{"a tag with markdown link syntax is escaped", "evil](https://attacker.example)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := safeTag(tt.tag)
			if !strings.HasPrefix(got, "`") || !strings.HasSuffix(got, "`") {
				t.Fatalf("safeTag(%q) = %q, want a Markdown code span", tt.tag, got)
			}
			inner := got[1 : len(got)-1]
			if strings.Contains(inner, "`") {
				t.Errorf("safeTag(%q) = %q, want no backtick inside the code span", tt.tag, got)
			}
		})
	}
}

func TestReleaseLinkNeverCopiedFromAPI(t *testing.T) {
	t.Parallel()

	got := releaseLink("agentclientprotocol/agent-client-protocol", "schema-v1.22.0")
	want := "https://github.com/agentclientprotocol/agent-client-protocol/releases/tag/schema-v1.22.0"
	if got != want {
		t.Errorf("releaseLink(...) = %q, want %q: built only from repository and tag", got, want)
	}

	escaped := releaseLink("agentclientprotocol/agent-client-protocol", "schema v1.22.0")
	if strings.Contains(escaped, "schema v1.22.0") {
		t.Errorf("releaseLink(...) = %q, want the tag percent-escaped", escaped)
	}
}

func TestPublisherTextCannotInjectMarkdownOrMentions(t *testing.T) {
	t.Parallel()

	adversarialTag := "evil](https://attacker.example) @everyone http://attacker.example"

	d := drift{
		Pin: testPin(),
		Newer: []newerRelease{
			{Tag: adversarialTag, Published: "2026-09-01", Assets: []assetComparison{{Name: "meta.json", Status: "unchanged"}, {Name: "schema.json", Status: "changed"}}},
		},
	}

	body := renderBody("open", d, "sha256:"+strings.Repeat("a", 64))

	wantSpan := safeTag(adversarialTag)
	if !strings.Contains(body, wantSpan) {
		t.Fatalf("renderBody(...) does not confine the adversarial tag to one code span %q:\n%s", wantSpan, body)
	}
	if strings.Contains(body, "](https://attacker.example)") {
		t.Errorf("renderBody(...) lets an adversarial tag form a live Markdown link outside its code span:\n%s", body)
	}
}
