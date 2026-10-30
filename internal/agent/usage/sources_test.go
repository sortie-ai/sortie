package usage_test

import (
	"errors"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/usage"
)

var (
	mappedLive = usage.Source{
		Category: usage.SourceOutput,
		Strategy: usage.StrategyLive,
		Spend:    usage.SpendDeclaration{Shape: usage.SpendAdapterMapped},
	}
	vendorLive = usage.Source{
		Category: usage.SourceOutput,
		Strategy: usage.StrategyLive,
		Spend:    usage.SpendDeclaration{VendorUnit: "premium requests"},
	}
	journal = usage.Source{
		Category: usage.SourceJournal,
		Strategy: usage.StrategyRecords,
		Spend:    usage.SpendDeclaration{Shape: usage.SpendAdapterMapped},
	}
)

func withSpend(src usage.Source, spend usage.SpendDeclaration) usage.Source {
	src.Spend = spend
	return src
}

func TestSources_Check(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		sources    usage.Sources
		wantErr    bool
		wantNoLive bool
	}{
		{name: "protocol live source", sources: usage.Sources{Live: withSpend(mappedLive, usage.SpendDeclaration{Shape: usage.SpendProcessCumulative})}},
		{name: "output live source with a journal", sources: usage.Sources{Live: mappedLive, Records: journal}},
		{name: "vendor-only live source", sources: usage.Sources{Live: vendorLive}},
		{name: "vendor-only live source with a token-counting journal", sources: usage.Sources{Live: vendorLive, Records: journal}},
		{name: "zero sources", wantErr: true, wantNoLive: true},
		{name: "live category is not live-capable", sources: usage.Sources{Live: usage.Source{Category: usage.SourceJournal, Strategy: usage.StrategyLive, Spend: mappedLive.Spend}}, wantErr: true, wantNoLive: true},
		{name: "live strategy is records", sources: usage.Sources{Live: usage.Source{Category: usage.SourceOutput, Strategy: usage.StrategyRecords, Spend: mappedLive.Spend}}, wantErr: true, wantNoLive: true},
		{name: "live source reports no spend", sources: usage.Sources{Live: withSpend(mappedLive, usage.SpendDeclaration{})}, wantErr: true, wantNoLive: true},
		{name: "live defect wins over a records defect", sources: usage.Sources{Live: withSpend(mappedLive, usage.SpendDeclaration{}), Records: withSpend(journal, usage.SpendDeclaration{})}, wantErr: true, wantNoLive: true},
		{name: "records category is not a journal", sources: usage.Sources{Live: mappedLive, Records: usage.Source{Category: usage.SourceOutput, Strategy: usage.StrategyRecords, Spend: journal.Spend}}, wantErr: true},
		{name: "records strategy is live", sources: usage.Sources{Live: mappedLive, Records: usage.Source{Category: usage.SourceJournal, Strategy: usage.StrategyLive, Spend: journal.Spend}}, wantErr: true},
		{name: "records source counts no tokens", sources: usage.Sources{Live: mappedLive, Records: withSpend(journal, usage.SpendDeclaration{VendorUnit: "credits"})}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.sources.Check()

			if (err != nil) != tt.wantErr {
				t.Fatalf("Check() = %v, want error %v", err, tt.wantErr)
			}
			if got := errors.Is(err, usage.ErrNoLiveSource); got != tt.wantNoLive {
				t.Errorf("errors.Is(Check(), ErrNoLiveSource) = %v, want %v", got, tt.wantNoLive)
			}
		})
	}
}

func TestSources_Disposition(t *testing.T) {
	t.Parallel()

	duringTurn := withSpend(mappedLive, usage.SpendDeclaration{Shape: usage.SpendAdapterMapped, DuringTurn: true, ServedModel: true})
	split := withSpend(journal, usage.SpendDeclaration{Shape: usage.SpendAdapterMapped, ServedModel: true})

	tests := []struct {
		name            string
		sources         usage.Sources
		wantArrival     usage.Arrival
		wantAttribution usage.Attribution
		wantCarries     bool
	}{
		{name: "live counts during the turn and names the model", sources: usage.Sources{Live: duringTurn}, wantArrival: usage.ArrivalIncremental, wantAttribution: usage.AttributionPerModel, wantCarries: true},
		{name: "live counts at turn end without a model", sources: usage.Sources{Live: mappedLive}, wantArrival: usage.ArrivalTurnEnd, wantAttribution: usage.AttributionSessionTotal, wantCarries: true},
		{name: "records split gives a model-naming live source", sources: usage.Sources{Live: mappedLive, Records: split}, wantArrival: usage.ArrivalTurnEnd, wantAttribution: usage.AttributionPerModel, wantCarries: true},
		{name: "records without a split leave the live attribution", sources: usage.Sources{Live: mappedLive, Records: journal}, wantArrival: usage.ArrivalTurnEnd, wantAttribution: usage.AttributionSessionTotal, wantCarries: true},
		{name: "vendor-only live source with split records carries a token figure from its records", sources: usage.Sources{Live: vendorLive, Records: split}, wantArrival: usage.ArrivalTurnEnd, wantAttribution: usage.AttributionPerModel, wantCarries: true},
		{name: "vendor-only live source with unsplit records", sources: usage.Sources{Live: vendorLive, Records: journal}, wantArrival: usage.ArrivalTurnEnd, wantAttribution: usage.AttributionSessionTotal, wantCarries: true},
		{name: "vendor-only live source alone", sources: usage.Sources{Live: vendorLive}, wantArrival: usage.ArrivalNone, wantAttribution: usage.AttributionNone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			arrival, attribution := tt.sources.Disposition()

			if arrival != tt.wantArrival || attribution != tt.wantAttribution {
				t.Errorf("Disposition() = %q, %q, want %q, %q", arrival, attribution, tt.wantArrival, tt.wantAttribution)
			}
			if got := tt.sources.CarriesTokenFigure(); got != tt.wantCarries {
				t.Errorf("CarriesTokenFigure() = %v, want %v", got, tt.wantCarries)
			}
		})
	}
}

func TestArrivalAndAttributionPredicates(t *testing.T) {
	t.Parallel()

	arrivals := []struct {
		arrival        usage.Arrival
		wantDuringTurn bool
		wantAnyFigure  bool
	}{
		{usage.ArrivalIncremental, true, true},
		{usage.ArrivalTurnEnd, false, true},
		{usage.ArrivalNone, false, false},
		{usage.ArrivalUndeclared, false, false},
		{"unknown", false, false},
	}
	for _, tt := range arrivals {
		if got := tt.arrival.ReportsDuringTurn(); got != tt.wantDuringTurn {
			t.Errorf("Arrival(%q).ReportsDuringTurn() = %v, want %v", tt.arrival, got, tt.wantDuringTurn)
		}
		if got := tt.arrival.ReportsAnyFigure(); got != tt.wantAnyFigure {
			t.Errorf("Arrival(%q).ReportsAnyFigure() = %v, want %v", tt.arrival, got, tt.wantAnyFigure)
		}
	}

	attributions := []struct {
		attribution usage.Attribution
		want        bool
	}{
		{usage.AttributionPerModel, true},
		{usage.AttributionSessionTotal, false},
		{usage.AttributionNone, false},
		{usage.AttributionUndeclared, false},
		{"unknown", false},
	}
	for _, tt := range attributions {
		if got := tt.attribution.NamesModel(); got != tt.want {
			t.Errorf("Attribution(%q).NamesModel() = %v, want %v", tt.attribution, got, tt.want)
		}
	}
}
