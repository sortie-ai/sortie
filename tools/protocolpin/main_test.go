package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
)

const (
	fixtureCommit121 = "272bf799f35a258c6a4107a0410ed361e83683d3"
	fixtureCommit123 = "6d08f412a7a1370d3cc9a124e3be3d6acf92641e"
	fixtureMeta      = "061edb6efa8fb2aa2792459a86ec7268de5fe665bba48b2ffe7939df01481f88"
	fixtureSchema121 = "caf62ff962ada396878372ced11efb2c6764e59d90919a38583c319948931a42"
	fixtureSchema123 = "3c17bd6385d90cf672d8a661fddc359d73422cf8b8ce6865213d25cfd4c0eca7"
)

func fixtureGenerateGoNamed(tag string) string {
	return "package clientprotocol\n\n//go:generate go run " + generatorPackagePath + " testdata/" + tag + " wire_gen.go\n"
}

func buildFixtureRepoNamed(t *testing.T, tag, generateGo, provenanceTxt, schemagenGo string) string {
	t.Helper()
	root := t.TempDir()
	mustWriteFile(t, root+"/"+adapterPackageDir+"/generate.go", generateGo)
	mustWriteFile(t, root+"/"+adapterPackageDir+"/testdata/"+tag+"/PROVENANCE.txt", provenanceTxt)
	mustWriteFile(t, root+"/"+adapterPackageDir+"/schemagen/generate.go", schemagenGo)
	return root
}

func loadReleasesFixture(t *testing.T) []release {
	t.Helper()
	data, err := os.ReadFile("testdata/releases.json")
	if err != nil {
		t.Fatalf("reading testdata/releases.json: %v", err)
	}
	var releases []release
	if err := json.Unmarshal(data, &releases); err != nil {
		t.Fatalf("Unmarshal(testdata/releases.json): %v", err)
	}
	return releases
}

func releasesUpTo(t *testing.T, all []release, maxTag string) []release {
	t.Helper()
	max, ok := parseStableVersion(maxTag)
	if !ok {
		t.Fatalf("releasesUpTo: %q does not parse as a stable version", maxTag)
	}
	var out []release
	for _, r := range all {
		if v, ok := parseStableVersion(r.TagName); ok && compareVersions(v, max) <= 0 {
			out = append(out, r)
			continue
		}
		if _, ok := parseStableVersion(r.TagName); !ok {
			out = append(out, r)
		}
	}
	return out
}

func runDecideWith(t *testing.T, repoRoot string, in decideInput) (decision, error) {
	t.Helper()
	encoded, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal(decideInput): %v", err)
	}
	var stdout bytes.Buffer
	if err := runDecide(repoRoot, bytes.NewReader(encoded), &stdout); err != nil {
		return decision{}, err
	}
	var dec decision
	if err := json.Unmarshal(stdout.Bytes(), &dec); err != nil {
		t.Fatalf("Unmarshal(decision) from %s: %v", stdout.String(), err)
	}
	return dec, nil
}

func TestLocateSubcommand(t *testing.T) {
	t.Parallel()

	root := liveTaggedFixtureRepoOverride(t, "schema-v1.21.0", fixtureCommit121, fixtureSchema121)
	var stdout bytes.Buffer
	if err := runLocate(root, &stdout); err != nil {
		t.Fatalf("runLocate(...) returned error: %v", err)
	}

	var out locateOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("Unmarshal(runLocate output) from %s: %v", stdout.String(), err)
	}
	if out.Repository != "agentclientprotocol/agent-client-protocol" {
		t.Errorf("runLocate(...).Repository = %q, want %q", out.Repository, "agentclientprotocol/agent-client-protocol")
	}
	if out.Tag != "schema-v1.21.0" {
		t.Errorf("runLocate(...).Tag = %q, want %q", out.Tag, "schema-v1.21.0")
	}
	if out.Title != renderTitle() {
		t.Errorf("runLocate(...).Title = %q, want %q", out.Title, renderTitle())
	}
}

func liveTaggedFixtureRepoOverride(t *testing.T, tag, commit, schemaDigest string) string {
	t.Helper()
	return buildFixtureRepoNamed(t, tag,
		fixtureGenerateGoNamed(tag),
		fixtureProvenanceTxt(tag, commit,
			"schema.json 247168 sha256:"+schemaDigest,
			"meta.json 1159 sha256:"+fixtureMeta),
		fixtureSchemagenGo(tag, commit))
}

func refsFor(tag, commit string) []gitRef {
	return []gitRef{{Ref: "refs/tags/" + tag, Object: gitObject{Type: "commit", SHA: commit}}}
}

func TestDecideSubcommandSupersededPin(t *testing.T) {
	t.Parallel()

	root := liveTaggedFixtureRepoOverride(t, "schema-v1.21.0", fixtureCommit121, fixtureSchema121)
	dec, err := runDecideWith(t, root, decideInput{
		Releases:      loadReleasesFixture(t),
		PinnedTagRefs: refsFor("schema-v1.21.0", fixtureCommit121),
		Report:        reportIssue{State: "absent"},
		RunURL:        "https://github.com/sortie-ai/sortie/actions/runs/1",
		Now:           "2026-09-23 12:00 UTC",
	})
	if err != nil {
		t.Fatalf("runDecide(superseded pin) returned error: %v", err)
	}

	if dec.Action != "open" {
		t.Fatalf("runDecide(superseded pin).Action = %q, want %q", dec.Action, "open")
	}
	if !strings.Contains(dec.Body, "schema-v1.21.0") || !strings.Contains(dec.Body, "schema-v1.23.0") {
		t.Errorf("runDecide(superseded pin).Body does not name both the recorded and the newest release:\n%s", dec.Body)
	}
	if !strings.Contains(dec.Body, "schema-v1.22.0") {
		t.Errorf("runDecide(superseded pin).Body does not list the intermediate release:\n%s", dec.Body)
	}

	lines := strings.Split(dec.Body, "\n")
	var row121Newer, row123Newer string
	for _, line := range lines {
		if strings.Contains(line, "schema-v1.22.0") && strings.HasPrefix(line, "|") {
			row121Newer = line
		}
		if strings.Contains(line, "schema-v1.23.0") && strings.HasPrefix(line, "|") {
			row123Newer = line
		}
	}
	for _, row := range []string{row121Newer, row123Newer} {
		if row == "" {
			t.Fatalf("runDecide(superseded pin).Body missing a table row for a newer release:\n%s", dec.Body)
		}
		if !strings.HasSuffix(row, "| unchanged | changed |") {
			t.Errorf("row %q does not report meta.json unchanged and schema.json changed", row)
		}
	}
}

func TestDecideSubcommandPinAtNewest(t *testing.T) {
	t.Parallel()

	root := liveTaggedFixtureRepoOverride(t, "schema-v1.23.0", fixtureCommit123, fixtureSchema123)
	baseInput := decideInput{
		Releases:      loadReleasesFixture(t),
		PinnedTagRefs: refsFor("schema-v1.23.0", fixtureCommit123),
		RunURL:        "https://github.com/sortie-ai/sortie/actions/runs/1",
		Now:           "2026-09-23 12:00 UTC",
	}

	t.Run("no report: none, with no Body", func(t *testing.T) {
		t.Parallel()
		in := baseInput
		in.Report = reportIssue{State: "absent"}
		dec, err := runDecideWith(t, root, in)
		if err != nil {
			t.Fatalf("runDecide(pin at newest, no report) returned error: %v", err)
		}
		if dec.Action != "none" {
			t.Errorf("runDecide(pin at newest, no report).Action = %q, want %q", dec.Action, "none")
		}
		if dec.Body != "" {
			t.Errorf("runDecide(pin at newest, no report).Body = %q, want empty", dec.Body)
		}
	})

	t.Run("an open report: close, with a marker-free Body", func(t *testing.T) {
		t.Parallel()
		in := baseInput
		in.Report = reportIssue{State: "open", Number: 7}
		dec, err := runDecideWith(t, root, in)
		if err != nil {
			t.Fatalf("runDecide(pin at newest, open report) returned error: %v", err)
		}
		if dec.Action != "close" {
			t.Errorf("runDecide(pin at newest, open report).Action = %q, want %q", dec.Action, "close")
		}
		if strings.Contains(dec.Body, "protocol-pin-fingerprint") {
			t.Errorf("runDecide(pin at newest, open report).Body carries a marker:\n%s", dec.Body)
		}
	})
}

func TestDecideNegativeControl(t *testing.T) {
	t.Parallel()

	root := liveTaggedFixtureRepoOverride(t, "schema-v1.21.0", fixtureCommit121, fixtureSchema121)
	all := loadReleasesFixture(t)
	refs := refsFor("schema-v1.21.0", fixtureCommit121)

	t.Run("removing every newer release turns open into none", func(t *testing.T) {
		t.Parallel()
		dec, err := runDecideWith(t, root, decideInput{
			Releases:      releasesUpTo(t, all, "schema-v1.21.0"),
			PinnedTagRefs: refs,
			Report:        reportIssue{State: "absent"},
			RunURL:        "https://github.com/sortie-ai/sortie/actions/runs/1",
			Now:           "2026-09-23 12:00 UTC",
		})
		if err != nil {
			t.Fatalf("runDecide(no newer releases) returned error: %v", err)
		}
		if dec.Action != "none" {
			t.Errorf("runDecide(no newer releases).Action = %q, want %q", dec.Action, "none")
		}
	})

	faultCases := []struct {
		name     string
		releases []release
	}{
		{"an empty listing", nil},
		{
			"a listing holding only crate releases and prereleases",
			[]release{
				{TagName: "v1.9.1", Draft: false, Prerelease: false, PublishedAt: "2026-01-01T00:00:00Z"},
				{TagName: "schema-v2.0.0-alpha.5", Draft: false, Prerelease: true, PublishedAt: "2026-01-01T00:00:00Z"},
			},
		},
	}
	for _, tt := range faultCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dec, err := runDecideWith(t, root, decideInput{
				Releases:      tt.releases,
				PinnedTagRefs: refs,
				Report:        reportIssue{State: "absent"},
				RunURL:        "https://github.com/sortie-ai/sortie/actions/runs/1",
				Now:           "2026-09-23 12:00 UTC",
			})
			if err == nil {
				t.Errorf("runDecide(%s) = %+v, want a fault, never none", tt.name, dec)
			}
		})
	}

	t.Run("undecodable stdin is a fault, never none", func(t *testing.T) {
		t.Parallel()
		var stdout bytes.Buffer
		err := runDecide(root, strings.NewReader("{"), &stdout)
		if err == nil {
			t.Error("runDecide(undecodable stdin) error = nil, want a fault")
		}
		if stdout.Len() != 0 {
			t.Errorf("runDecide(undecodable stdin) wrote %q to stdout, want nothing", stdout.String())
		}
	})
}

func TestDecideNewInformation(t *testing.T) {
	t.Parallel()

	root := liveTaggedFixtureRepoOverride(t, "schema-v1.21.0", fixtureCommit121, fixtureSchema121)
	all := loadReleasesFixture(t)
	refs := refsFor("schema-v1.21.0", fixtureCommit121)
	runURL := "https://github.com/sortie-ai/sortie/actions/runs/1"
	now := "2026-09-23 12:00 UTC"

	opened, err := runDecideWith(t, root, decideInput{
		Releases:      releasesUpTo(t, all, "schema-v1.22.0"),
		PinnedTagRefs: refs,
		Report:        reportIssue{State: "absent"},
		RunURL:        runURL,
		Now:           now,
	})
	if err != nil {
		t.Fatalf("runDecide(opening) returned error: %v", err)
	}
	if opened.Action != "open" {
		t.Fatalf("runDecide(opening).Action = %q, want %q", opened.Action, "open")
	}

	updated, err := runDecideWith(t, root, decideInput{
		Releases:      all,
		PinnedTagRefs: refs,
		Report:        reportIssue{State: "open", Number: 7, Body: opened.Body},
		RunURL:        runURL,
		Now:           now,
	})
	if err != nil {
		t.Fatalf("runDecide(with a newer release added) returned error: %v", err)
	}
	if updated.Action != "update" {
		t.Fatalf("runDecide(with a newer release added).Action = %q, want %q", updated.Action, "update")
	}
	if !strings.Contains(updated.Comment, "schema-v1.23.0") {
		t.Errorf("runDecide(with a newer release added).Comment = %q, want it to name the new newest release", updated.Comment)
	}
	if !strings.Contains(updated.Notify, "schema-v1.23.0") {
		t.Errorf("runDecide(with a newer release added).Notify = %q, want it to name the new newest release", updated.Notify)
	}
	oldFP, _ := extractFingerprint(opened.Body)
	newFP, hadMarker := extractFingerprint(updated.Body)
	if !hadMarker || newFP == oldFP {
		t.Errorf("runDecide(with a newer release added).Body fingerprint = %q, want a changed marker distinct from %q", newFP, oldFP)
	}
}

func TestDecideUsageAndInputErrors(t *testing.T) {
	t.Parallel()

	root := liveTaggedFixtureRepoOverride(t, "schema-v1.21.0", fixtureCommit121, fixtureSchema121)

	t.Run("missing -repo-root exits 2", func(t *testing.T) {
		t.Parallel()
		var stdout, stderr bytes.Buffer
		code := run([]string{"locate"}, strings.NewReader(""), &stdout, &stderr)
		if code != 2 {
			t.Errorf("run(locate, no -repo-root) = %d, want 2", code)
		}
	})

	t.Run("unrecognized subcommand exits 2", func(t *testing.T) {
		t.Parallel()
		var stdout, stderr bytes.Buffer
		code := run([]string{"delete", "-repo-root", root}, strings.NewReader(""), &stdout, &stderr)
		if code != 2 {
			t.Errorf("run(unrecognized subcommand) = %d, want 2", code)
		}
	})

	validInput := func() decideInput {
		return decideInput{
			Releases:      loadReleasesFixture(t),
			PinnedTagRefs: refsFor("schema-v1.21.0", fixtureCommit121),
			Report:        reportIssue{State: "absent"},
			RunURL:        "https://github.com/sortie-ai/sortie/actions/runs/1",
			Now:           "2026-09-23 12:00 UTC",
		}
	}

	t.Run("undecodable stdin exits 2", func(t *testing.T) {
		t.Parallel()
		var stdout, stderr bytes.Buffer
		code := run([]string{"decide", "-repo-root", root}, strings.NewReader("not json"), &stdout, &stderr)
		if code != 2 {
			t.Errorf("run(decide, undecodable stdin) = %d, want 2", code)
		}
	})

	invalidCases := []struct {
		name   string
		mutate func(*decideInput)
	}{
		{"invalid report.state", func(in *decideInput) { in.Report.State = "sometimes" }},
		{"report.number 0 for an open report", func(in *decideInput) { in.Report = reportIssue{State: "open", Number: 0} }},
		{"empty run_url", func(in *decideInput) { in.RunURL = "" }},
		{"malformed now", func(in *decideInput) { in.Now = "not-a-time" }},
	}
	for _, tt := range invalidCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := validInput()
			tt.mutate(&in)
			encoded, err := json.Marshal(in)
			if err != nil {
				t.Fatalf("Marshal(decideInput): %v", err)
			}
			var stdout, stderr bytes.Buffer
			code := run([]string{"decide", "-repo-root", root}, bytes.NewReader(encoded), &stdout, &stderr)
			if code != 2 {
				t.Errorf("run(decide, %s) = %d, want 2", tt.name, code)
			}
		})
	}

	t.Run("a locate fault exits 1", func(t *testing.T) {
		t.Parallel()
		encoded, err := json.Marshal(validInput())
		if err != nil {
			t.Fatalf("Marshal(decideInput): %v", err)
		}
		var stdout, stderr bytes.Buffer
		code := run([]string{"decide", "-repo-root", t.TempDir()}, bytes.NewReader(encoded), &stdout, &stderr)
		if code != 1 {
			t.Errorf("run(decide, bad repo root) = %d, want 1", code)
		}
	})

	t.Run("a compute fault exits 1", func(t *testing.T) {
		t.Parallel()
		in := validInput()
		in.Releases = nil
		encoded, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("Marshal(decideInput): %v", err)
		}
		var stdout, stderr bytes.Buffer
		code := run([]string{"decide", "-repo-root", root}, bytes.NewReader(encoded), &stdout, &stderr)
		if code != 1 {
			t.Errorf("run(decide, no releases) = %d, want 1", code)
		}
	})
}

func TestDecideOperationSurface(t *testing.T) {
	t.Parallel()

	root := liveTaggedFixtureRepoOverride(t, "schema-v1.21.0", fixtureCommit121, fixtureSchema121)
	all := loadReleasesFixture(t)
	refs := refsFor("schema-v1.21.0", fixtureCommit121)
	runURL := "https://github.com/sortie-ai/sortie/actions/runs/1"
	now := "2026-09-23 12:00 UTC"

	opened, err := runDecideWith(t, root, decideInput{
		Releases: all, PinnedTagRefs: refs, Report: reportIssue{State: "absent"}, RunURL: runURL, Now: now,
	})
	if err != nil {
		t.Fatalf("runDecide(open) returned error: %v", err)
	}

	updated, err := runDecideWith(t, root, decideInput{
		Releases: all, PinnedTagRefs: refs, Report: reportIssue{State: "open", Number: 7, Body: ""}, RunURL: runURL, Now: now,
	})
	if err != nil {
		t.Fatalf("runDecide(update) returned error: %v", err)
	}

	newestRoot := liveTaggedFixtureRepoOverride(t, "schema-v1.23.0", fixtureCommit123, fixtureSchema123)
	newestRefs := refsFor("schema-v1.23.0", fixtureCommit123)

	closed, err := runDecideWith(t, newestRoot, decideInput{
		Releases: all, PinnedTagRefs: newestRefs, Report: reportIssue{State: "open", Number: 7}, RunURL: runURL, Now: now,
	})
	if err != nil {
		t.Fatalf("runDecide(close) returned error: %v", err)
	}

	none, err := runDecideWith(t, newestRoot, decideInput{
		Releases: all, PinnedTagRefs: newestRefs, Report: reportIssue{State: "absent"}, RunURL: runURL, Now: now,
	})
	if err != nil {
		t.Fatalf("runDecide(none) returned error: %v", err)
	}

	tests := []struct {
		name         string
		dec          decision
		wantNotify   bool
		wantBodyFull bool
	}{
		{"open", opened, true, true},
		{"update", updated, true, true},
		{"close", closed, false, true},
		{"none", none, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotNotify := tt.dec.Notify != ""
			if gotNotify != tt.wantNotify {
				t.Errorf("runDecide(%s).Notify empty = %v, want empty = %v", tt.name, !gotNotify, !tt.wantNotify)
			}
			gotBody := tt.dec.Body != ""
			if gotBody != tt.wantBodyFull {
				t.Errorf("runDecide(%s).Body empty = %v, want empty = %v", tt.name, !gotBody, !tt.wantBodyFull)
			}
		})
	}
}

func TestInterruptedActionsConverge(t *testing.T) {
	t.Parallel()

	pin := testPin()
	refs := []gitRef{matchingRef(pin)}
	present := mustComputeDrift(t, pin, []release{
		mkRelease(pin.Tag, false, false, "2026-08-20T00:00:00Z", pinAssetsAsReleaseAssets(pin)...),
		mkRelease("schema-v1.22.0", false, false, "2026-09-01T00:00:00Z", pinAssetsAsReleaseAssets(pin)...),
	}, refs, nil, fixedNow)
	presentFP, err := fingerprint(present)
	if err != nil {
		t.Fatalf("fingerprint(present) returned error: %v", err)
	}
	staleFP := "sha256:" + strings.Repeat("0", 64)
	staleBody := renderBody("open", present, staleFP)
	closeBody := renderBody("close", present, "")

	tests := []struct {
		name         string
		driftPresent bool
		reportState  string
		reportBody   string
		want         string
	}{
		{"open: before any operation ran, still absent", true, "absent", "", "open"},
		{"update: comment posted but body not yet edited, still the old stale-fingerprint body", true, "open", staleBody, "update"},
		{"reopen: type set but not yet reopened, report still closed with the old marker-free close body", true, "closed", closeBody, "reopen"},
		{"reopen: reopened but comment and body not yet edited, decides update instead of none", true, "open", closeBody, "update"},
		{"close: commented but body not yet edited or closed", false, "open", staleBody, "close"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			bodyFP, hadMarker := extractFingerprint(tt.reportBody)
			bodyMatches := hadMarker && bodyFP == presentFP
			got := decideAction(tt.driftPresent, tt.reportState, bodyMatches)
			if got != tt.want {
				t.Errorf("decideAction(%v, %q, bodyMatches=%v) = %q, want %q", tt.driftPresent, tt.reportState, bodyMatches, got, tt.want)
			}
			if got == "none" {
				t.Errorf("decideAction(%s) = none, want a report never lost mid-interruption", tt.name)
			}
		})
	}
}

func TestValidateDecideInputRejectsEveryInvalidCombination(t *testing.T) {
	t.Parallel()

	valid := decideInput{Report: reportIssue{State: "absent"}, RunURL: "https://x", Now: "2026-09-23 12:00 UTC"}
	if err := validateDecideInput(valid); err != nil {
		t.Fatalf("validateDecideInput(valid) returned error: %v", err)
	}

	for i, mutate := range []func(decideInput) decideInput{
		func(in decideInput) decideInput { in.Report.State = "weird"; return in },
		func(in decideInput) decideInput { in.Report = reportIssue{State: "open", Number: 0}; return in },
		func(in decideInput) decideInput { in.Report = reportIssue{State: "closed", Number: 0}; return in },
		func(in decideInput) decideInput { in.RunURL = ""; return in },
		func(in decideInput) decideInput { in.Now = "2026/09/23"; return in },
	} {
		t.Run("case "+strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()
			if err := validateDecideInput(mutate(valid)); err == nil {
				t.Errorf("validateDecideInput(invalid case %d) error = nil, want a fault", i)
			}
		})
	}
}
