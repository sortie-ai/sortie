package server

import (
	"reflect"
	"testing"

	"github.com/sortie-ai/sortie/internal/orchestrator"
)

func TestIssueCost(t *testing.T) {
	t.Parallel()

	type usage = orchestrator.SnapshotIssueUsage
	priced15 := usage{RecordedCostUSD: 15, PricedSessions: 1}
	tests := []struct {
		name       string
		completed  usage
		session    orchestrator.SnapshotRunningEntry
		wantCost   *float64
		wantStatus string
	}{
		{"priced rows", usage{RecordedCostUSD: 27, PricedSessions: 2}, orchestrator.SnapshotRunningEntry{}, new(27.0), costStatusEstimated},
		{"unpriced rows alone", usage{UnpricedSessions: 2}, orchestrator.SnapshotRunningEntry{}, nil, costStatusUnpriced},
		{"partial row", usage{RecordedCostUSD: 15, PricedSessions: 1, PartialSessions: 1}, orchestrator.SnapshotRunningEntry{}, new(15.0), costStatusPartial},
		{"session that spent", priced15, orchestrator.SnapshotRunningEntry{AgentInputTokens: 1, EstimatedCostUSD: new(12.0), CostStatus: costStatusEstimated}, new(27.0), costStatusEstimated},
		{"session that spent nothing", priced15, orchestrator.SnapshotRunningEntry{CostStatus: costStatusUnpriced}, new(15.0), costStatusEstimated},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotCost, gotStatus := issueCost(&tt.completed, new(int64(1)), true, sessionPartOf(tt.session))

			if gotStatus != tt.wantStatus || !reflect.DeepEqual(gotCost, tt.wantCost) {
				t.Errorf("issueCost(%+v, %+v) = %v, %q, want %v, %q", tt.completed, tt.session, gotCost, gotStatus, tt.wantCost, tt.wantStatus)
			}
		})
	}

	if cost, status := issueCost(&priced15, nil, true, sessionCostPart{}); cost != nil || status != costStatusUnmeasured {
		t.Errorf("issueCost(tokens not measured) = %v, %q, want nil, %q", cost, status, costStatusUnmeasured)
	}
}

func TestActiveCostTotal(t *testing.T) {
	t.Parallel()

	total, anySet, unpriced := activeCostTotal([]orchestrator.SnapshotRunningEntry{
		{EstimatedCostUSD: new(12.0), CostStatus: costStatusEstimated},
		{EstimatedCostUSD: new(3.0), CostStatus: costStatusPartial},
		{CostStatus: costStatusUnpriced},
		{CostStatus: ""},
		{CostStatus: costStatusUnmeasured},
	})

	if total != 15 || !anySet || unpriced != 2 {
		t.Errorf("activeCostTotal() = %v, %v, %d, want 15, true, 2", total, anySet, unpriced)
	}
}

func TestFormatCost(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input float64
		want  string
	}{
		{"zero", 0.00, "$0.00"},
		{"rounds up to 1000 from below", 999.999, "$1,000.00"},
		{"mid four digits", 1234.56, "$1,234.56"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := FormatCost(tt.input)
			if got != tt.want {
				t.Errorf("FormatCost(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestFormatCostCut(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input float64
		want  string
	}{
		{"zero", 0, "$0.00"},
		{"cuts instead of rounding", 1.239, "$1.23"},
		{"float hazard: 0.29 times 100 is below 29", 0.29, "$0.29"},
		{"does not round up to 1000", 999.999, "$999.99"},
		{"cuts a four digit amount", 1234.569, "$1,234.56"},
		{"whole cents unchanged", 1234.56, "$1,234.56"},
		{"one decimal digit", 0.5, "$0.50"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := FormatCostCut(tt.input)
			if got != tt.want {
				t.Errorf("FormatCostCut(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
