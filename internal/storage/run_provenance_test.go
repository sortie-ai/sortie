package storage

import (
	"reflect"
	"testing"
)

func TestUsageCompletenessOf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		measured bool
		turns    int
		want     string
	}{
		{"no figure is unmeasured", false, 0, UsageUnmeasured},
		{"no figure stays unmeasured whatever the turns", false, 2, UsageUnmeasured},
		{"measured with an uncounted turn is a lower bound", true, 1, UsageLowerBound},
		{"measured with every turn counted is complete", true, 0, UsageComplete},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := UsageCompletenessOf(tt.measured, tt.turns)

			if got != tt.want {
				t.Errorf("UsageCompletenessOf(%v, %d) = %q, want %q", tt.measured, tt.turns, got, tt.want)
			}
		})
	}
}

func TestUnmeasuredCauseOf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		status        string
		completeness  string
		reportedUnits []string
		want          UnmeasuredCause
	}{
		{"pre-provenance run is unknown even with units", "failed", "", []string{"credits"}, UnmeasuredUnknown},
		{"units reported by a succeeded run", "succeeded", UsageUnmeasured, []string{"credits"}, UnmeasuredAmountsOnly},
		{"a US dollar unit alone is amounts only", "succeeded", UsageUnmeasured, []string{"USD"}, UnmeasuredAmountsOnly},
		{"units win over a failed status", "failed", UsageUnmeasured, []string{"credits"}, UnmeasuredAmountsOnly},
		{"non-succeeded run without units ended early", "stopped", UsageUnmeasured, nil, UnmeasuredEndedEarly},
		{"succeeded run without units sent no usage", "succeeded", UsageUnmeasured, nil, UnmeasuredNoUsage},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := UnmeasuredCauseOf(tt.status, tt.completeness, tt.reportedUnits)

			if got != tt.want {
				t.Errorf("UnmeasuredCauseOf(%q, %q, %v) = %q, want %q", tt.status, tt.completeness, tt.reportedUnits, got, tt.want)
			}
		})
	}
}

func TestUnmeasuredCauses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		adds      []causeKey
		wantCause UnmeasuredCause
		wantUnits []string
	}{
		{"nothing added", nil, "", []string{}},
		{
			"one key serves its distinct sorted units",
			[]causeKey{{cause: UnmeasuredAmountsOnly, units: []string{"credits", "USD", "credits"}}},
			UnmeasuredAmountsOnly, []string{"USD", "credits"},
		},
		{
			"the same key twice is not mixed",
			[]causeKey{
				{cause: UnmeasuredAmountsOnly, units: []string{"credits", "USD"}},
				{cause: UnmeasuredAmountsOnly, units: []string{"USD", "credits"}},
			},
			UnmeasuredAmountsOnly, []string{"USD", "credits"},
		},
		{
			"a cause other than amounts only serves no units",
			[]causeKey{{cause: UnmeasuredNoUsage}, {cause: UnmeasuredNoUsage}},
			UnmeasuredNoUsage, []string{},
		},
		{
			"the same cause over different units is mixed",
			[]causeKey{
				{cause: UnmeasuredAmountsOnly, units: []string{"credits"}},
				{cause: UnmeasuredAmountsOnly, units: []string{"USD"}},
			},
			UnmeasuredMixed, []string{},
		},
		{
			"different causes are mixed",
			[]causeKey{{cause: UnmeasuredNoUsage}, {cause: UnmeasuredEndedEarly}},
			UnmeasuredMixed, []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var causes UnmeasuredCauses
			for _, add := range tt.adds {
				causes.Add(add.cause, add.units)
			}

			gotCause, gotUnits := causes.Result()

			if gotCause != tt.wantCause || !reflect.DeepEqual(gotUnits, tt.wantUnits) {
				t.Errorf("UnmeasuredCauses.Result() = %q, %#v, want %q, %#v", gotCause, gotUnits, tt.wantCause, tt.wantUnits)
			}
		})
	}
}

func TestParseUsageProvenance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want []UsagePair
	}{
		{"empty text is an empty non-nil slice", "", []UsagePair{}},
		{
			"live first, then by source, then by strategy; unknown values pass through",
			"output:records,journal:records,zeta:live,alpha:pull,alpha:live",
			[]UsagePair{
				{Source: "alpha", Strategy: "live"},
				{Source: "zeta", Strategy: "live"},
				{Source: "alpha", Strategy: "pull"},
				{Source: "journal", Strategy: "records"},
				{Source: "output", Strategy: "records"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ParseUsageProvenance(tt.text)

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseUsageProvenance(%q) = %+v, want %+v", tt.text, got, tt.want)
			}
		})
	}
}
