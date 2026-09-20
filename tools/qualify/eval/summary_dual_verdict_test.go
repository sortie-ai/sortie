package eval

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/tools/qualify/evidence"
	"github.com/sortie-ai/sortie/tools/qualify/evidencetest"
	"github.com/sortie-ai/sortie/tools/qualify/profile"
)

// publishedSummary walks the collection's whole path and returns the summary
// text a reader sees. Every control here grades the published text, because the
// defect it closes was a report holding both answers in memory and printing one.
func publishedSummary(t *testing.T, records []evidence.Record, p profile.RuntimeProfile) string {
	t.Helper()

	evidencePath := evidencetest.WriteEvidenceFile(t, records)
	published, err := evidence.ReadEvidenceFile(evidencePath)
	if err != nil {
		t.Fatalf("read back the published evidence: %v", err)
	}
	verdict, err := ValidateObservations(evidencePath, p)
	if err != nil {
		t.Fatalf("validate the published evidence: %v", err)
	}
	conclusions, err := conclusionsFromRecords(published, verdict, p)
	if err != nil {
		t.Fatalf("conclusionsFromRecords(...) = _, %v, want nil", err)
	}
	summaryPath := filepath.Join(t.TempDir(), "summary.txt")
	if err := os.WriteFile(summaryPath, []byte(formatSummary(conclusions, false)), 0o600); err != nil {
		t.Fatalf("write the summary artifact: %v", err)
	}
	data, err := os.ReadFile(summaryPath) //nolint:gosec // this test's own temporary directory
	if err != nil {
		t.Fatalf("read back the summary artifact: %v", err)
	}
	return string(data)
}

// summarySection returns the entries under one heading, up to the next.
// Headings are the only lines ending in a colon, so a section is delimited by
// the text itself rather than by a count a change would move.
func summarySection(t *testing.T, summary, heading string) []string {
	t.Helper()

	lines := strings.Split(strings.TrimRight(summary, "\n"), "\n")
	start := -1
	for i, line := range lines {
		if line == heading {
			if start >= 0 {
				t.Fatalf("summary carries the heading %q more than once", heading)
			}
			start = i
		}
	}
	if start < 0 {
		t.Fatalf("summary carries no %q heading:\n%s", heading, summary)
	}
	var entries []string
	for _, line := range lines[start+1:] {
		if strings.HasSuffix(line, ":") {
			break
		}
		entries = append(entries, line)
	}
	return entries
}

func summaryValue(t *testing.T, summary, label string) string {
	t.Helper()

	var values []string
	for line := range strings.SplitSeq(summary, "\n") {
		if after, found := strings.CutPrefix(line, label); found {
			values = append(values, after)
		}
	}
	if len(values) != 1 {
		t.Fatalf("summary carries %d %q lines, want exactly 1:\n%s", len(values), label, summary)
	}
	return values[0]
}

func summaryProfile(fixture *evidencetest.Fixture) profile.RuntimeProfile {
	p := fixture.Declarations()
	p.EntryPoints = map[evidence.Surface]profile.EntryPoint{
		evidence.SurfaceProtocol:         {},
		evidence.SurfaceNativeJSON:       {},
		evidence.SurfaceNativeStreamJSON: {},
	}
	return p
}

// tokenCeilingWorksOnNoRoute is the shape behind the defect: no token spend over
// protocol or either native surface, so transport parity qualifies while the
// operator has no token ceiling at all.
func tokenCeilingWorksOnNoRoute(t *testing.T) *evidencetest.Fixture {
	t.Helper()

	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
	for _, surface := range []evidence.Surface{
		evidence.SurfaceProtocol,
		evidence.SurfaceNativeJSON,
		evidence.SurfaceNativeStreamJSON,
	} {
		fixture.SetTokenSentinel(surface, false)
	}
	fixture.Finalize()
	return fixture
}

func TestPublishedSummaryStatesBothVerdicts(t *testing.T) {
	t.Parallel()

	fixture := tokenCeilingWorksOnNoRoute(t)
	summary := publishedSummary(t, fixture.Records, summaryProfile(fixture))

	if got := summaryValue(t, summary, "Eligibility: "); got != string(evidence.VerdictQualified) {
		t.Errorf("published transport verdict = %q, want %q", got, evidence.VerdictQualified)
	}
	if got := summaryValue(t, summary, "Product conformance: "); got != string(evidence.VerdictNotQualified) {
		t.Errorf("published product verdict = %q, want %q", got, evidence.VerdictNotQualified)
	}
	for _, want := range []string{
		QuestionRationale(evidence.QuestionTransportParity, evidence.VerdictQualified),
		QuestionRationale(evidence.QuestionProductConformance, evidence.VerdictNotQualified),
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("published summary carries no rationale line %q:\n%s", want, summary)
		}
	}
}

func TestPublishedSummarySeparatesTheBlockingLists(t *testing.T) {
	t.Parallel()

	fixture := tokenCeilingWorksOnNoRoute(t)
	summary := publishedSummary(t, fixture.Records, summaryProfile(fixture))

	if parity := summarySection(t, summary, "Blocking rows:"); len(parity) != 1 || parity[0] != "none" {
		t.Errorf("transport blocking rows = %v, want [none]: no native reference is above the protocol surface here", parity)
	}
	product := summarySection(t, summary, "Conformance-blocking rows:")
	if len(product) != 1 {
		t.Fatalf("product blocking rows = %v, want exactly the token ceiling", product)
	}
	if !strings.Contains(product[0], string(evidence.CapabilityTokenCeiling)) {
		t.Errorf("product blocking row = %q, want it to name the token ceiling", product[0])
	}
	if !strings.Contains(product[0], string(evidence.GradeGap)) {
		t.Errorf("product blocking row = %q, want it to carry the cause the row failed on", product[0])
	}
}

func TestPublishedSummaryListsConformanceUnmeasuredRows(t *testing.T) {
	t.Parallel()

	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
	for _, surface := range []evidence.Surface{
		evidence.SurfaceProtocol,
		evidence.SurfaceNativeJSON,
		evidence.SurfaceNativeStreamJSON,
	} {
		fixture.SetSemanticNotObserved(surface, evidence.CapabilityTurnDisposition, evidence.CaseSuccess)
	}
	fixture.Finalize()

	summary := publishedSummary(t, fixture.Records, summaryProfile(fixture))
	if got := summaryValue(t, summary, "Product conformance: "); got != string(evidence.VerdictUnmeasured) {
		t.Errorf("published product verdict = %q, want %q", got, evidence.VerdictUnmeasured)
	}
	product := summarySection(t, summary, "Conformance-unmeasured rows:")
	if len(product) != 1 || !strings.Contains(product[0], string(evidence.CapabilityTurnDisposition)) {
		t.Errorf("product unmeasured rows = %v, want the turn disposition row", product)
	}
}

func TestCompensationReachesOnlyTheProductAnswer(t *testing.T) {
	t.Parallel()

	fixture := evidencetest.NewFixture(evidencetest.FixtureQualified)
	fixture.SetTokenCorroborationOnly(evidence.SurfaceProtocol)
	fixture.Finalize()
	fixture.SetTokenCompensated("sortie/session/turn/usage")

	summary := publishedSummary(t, fixture.Records, summaryProfile(fixture))

	if got := summaryValue(t, summary, "Eligibility: "); got != string(evidence.VerdictNotQualified) {
		t.Errorf("published transport verdict = %q, want %q: the wire still carries nothing", got, evidence.VerdictNotQualified)
	}
	if got := summaryValue(t, summary, "Product conformance: "); got != string(evidence.VerdictUnmeasured) {
		t.Errorf("published product verdict = %q, want %q: the figure arrives and no run crossed a ceiling", got, evidence.VerdictUnmeasured)
	}
	if product := summarySection(t, summary, "Conformance-blocking rows:"); len(product) != 1 || product[0] != "none" {
		t.Errorf("product blocking rows = %v, want [none]", product)
	}
	withheld := summarySection(t, summary, "Conformance-unmeasured rows:")
	if len(withheld) != 1 || !strings.Contains(withheld[0], string(evidence.CapabilityTokenCeiling)) {
		t.Fatalf("product unmeasured rows = %v, want the token ceiling row the figure did not settle", withheld)
	}
	if !strings.Contains(withheld[0], "stop") {
		t.Errorf("product unmeasured row = %q, want it to name the ceiling stop nothing observed", withheld[0])
	}
	parity := summarySection(t, summary, "Blocking rows:")
	if len(parity) != 1 || !strings.Contains(parity[0], string(evidence.CapabilityTokenCeiling)) {
		t.Fatalf("transport blocking rows = %v, want the token ceiling", parity)
	}

	wantRow := strings.Join([]string{
		string(evidence.SurfaceProtocol),
		string(evidence.CapabilityTokenCeiling) + ":",
		evidence.StatusLabel(evidence.GradeGap),
		string(evidence.GradeGap),
	}, " ")
	if !slices.Contains(summarySection(t, summary, "Capability grades:"), wantRow) {
		t.Errorf("published grade table carries no %q row, want the raw reading the surface itself carried:\n%s", wantRow, summary)
	}
}
