package sqlite

import (
	"context"
	"database/sql"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/storage"
)

type costState struct {
	Runs  map[int64]storage.ModelCost
	Parts map[int64]map[string]storage.ModelCost
}

func readCostState(t *testing.T, s *Store) costState {
	t.Helper()
	state := costState{Runs: map[int64]storage.ModelCost{}, Parts: map[int64]map[string]storage.ModelCost{}}

	runs, err := s.db.QueryContext(context.Background(), `SELECT id, estimated_cost_usd, cost_status FROM run_history`)
	if err != nil {
		t.Fatalf("query run costs: %v", err)
	}
	defer runs.Close() //nolint:errcheck // test helper
	for runs.Next() {
		var id int64
		var cost sql.NullFloat64
		var status sql.NullString
		if err := runs.Scan(&id, &cost, &status); err != nil {
			t.Fatalf("scan run cost: %v", err)
		}
		state.Runs[id] = storage.ModelCost{EstimatedCostUSD: floatPtr(cost), CostStatus: status.String}
	}
	if err := runs.Err(); err != nil {
		t.Fatalf("iterate run costs: %v", err)
	}

	parts, err := s.db.QueryContext(context.Background(),
		`SELECT run_id, model, estimated_cost_usd, cost_status, price_source, price_table_version, unrated_classes FROM run_model_usage`)
	if err != nil {
		t.Fatalf("query part costs: %v", err)
	}
	defer parts.Close() //nolint:errcheck // test helper
	for parts.Next() {
		var id int64
		var model, source, version string
		var cost sql.NullFloat64
		var status, unrated sql.NullString
		if err := parts.Scan(&id, &model, &cost, &status, &source, &version, &unrated); err != nil {
			t.Fatalf("scan part cost: %v", err)
		}
		if state.Parts[id] == nil {
			state.Parts[id] = map[string]storage.ModelCost{}
		}
		part := storage.ModelCost{EstimatedCostUSD: floatPtr(cost), CostStatus: status.String, PriceSource: source, PriceTableVersion: version}
		if unrated.Valid {
			part.UnratedClasses = []string{}
			if unrated.String != "" {
				part.UnratedClasses = strings.Split(unrated.String, ",")
			}
		}
		state.Parts[id][model] = part
	}
	if err := parts.Err(); err != nil {
		t.Fatalf("iterate part costs: %v", err)
	}
	return state
}

func floatPtr(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	return &v.Float64
}

func operatorCost(v float64) storage.ModelCost {
	return storage.ModelCost{EstimatedCostUSD: &v, CostStatus: "estimated", PriceSource: "operator", PriceTableVersion: "v1", UnratedClasses: []string{}}
}

func TestRecordUnrecordedRunCosts(t *testing.T) {
	t.Parallel()

	partsA := map[string]storage.ModelTokens{"a": {InputTokens: 1_000_000, CacheReadTokens: 2_000_000}, "b": {InputTokens: 3_000_000, OutputTokens: 1_000_000}}
	partS := map[string]storage.ModelTokens{"m1": {InputTokens: 1_000_000, OutputTokens: 1_000_000}}
	recordedPart := storage.ModelCost{EstimatedCostUSD: new(5.0), CostStatus: "partial", PriceSource: "operator", PriceTableVersion: "v0", UnratedClasses: []string{"input"}}
	pricedPart := storage.ModelCost{EstimatedCostUSD: new(16.0), CostStatus: "partial", PriceSource: "operator", PriceTableVersion: "v1", UnratedClasses: []string{"cache_read", "cache_write"}}

	t.Run("records NULL-status rows only and a second fill records none", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		recorded := newTestRun(1)
		recorded.TokensMeasured, recorded.TotalTokens = true, 2_000_000
		recorded.EstimatedCostUSD, recorded.CostStatus = new(12.0), "estimated"
		recorded.ModelUsage = partS
		recorded.ModelCosts = map[string]storage.ModelCost{"m1": operatorCost(12)}
		recordedRun := appendWithPartsOrFatal(t, s, recorded)

		withParts := newTestRun(2)
		withParts.TokensMeasured, withParts.TotalTokens = true, 7_000_000
		withParts.ModelUsage = partsA
		withParts.ModelCosts = map[string]storage.ModelCost{"a": recordedPart}
		withPartsRun := appendWithPartsOrFatal(t, s, withParts)

		bare := newTestRun(3)
		bare.TokensMeasured, bare.TotalTokens = true, 2_000_000
		bareRun := appendOrFatal(t, s, bare)

		before := readCostState(t, s)
		var priced []storage.UnrecordedRun
		price := func(run storage.UnrecordedRun) storage.RunCostRecord {
			priced = append(priced, run)
			switch run.ID {
			case withPartsRun.ID:
				return storage.RunCostRecord{EstimatedCostUSD: new(15.0), CostStatus: "estimated", PartCosts: map[string]storage.ModelCost{
					"a": operatorCost(1), "b": pricedPart,
				}}
			case bareRun.ID:
				return storage.RunCostRecord{EstimatedCostUSD: new(12.0), CostStatus: "estimated", NewParts: partS,
					PartCosts: map[string]storage.ModelCost{"m1": operatorCost(12)}}
			}
			return storage.RunCostRecord{EstimatedCostUSD: new(99.0), CostStatus: "estimated"}
		}

		n, err := s.RecordUnrecordedRunCosts(context.Background(), price)
		if err != nil {
			t.Fatalf("RecordUnrecordedRunCosts() unexpected error: %v", err)
		}

		if n != 2 {
			t.Errorf("RecordUnrecordedRunCosts() = %d, want 2", n)
		}
		ids := []int64{}
		for _, run := range priced {
			ids = append(ids, run.ID)
		}
		if want := []int64{withPartsRun.ID, bareRun.ID}; !slices.Equal(ids, want) {
			t.Errorf("price called for runs %v, want %v", ids, want)
		}
		if !reflect.DeepEqual(priced[0].Parts, partsA) {
			t.Errorf("UnrecordedRun.Parts = %v, want %v", priced[0].Parts, partsA)
		}
		after := readCostState(t, s)
		if !reflect.DeepEqual(after.Runs[recordedRun.ID], before.Runs[recordedRun.ID]) ||
			!reflect.DeepEqual(after.Parts[recordedRun.ID], before.Parts[recordedRun.ID]) {
			t.Errorf("recorded run %d changed: got %+v %+v, want %+v %+v",
				recordedRun.ID, after.Runs[recordedRun.ID], after.Parts[recordedRun.ID], before.Runs[recordedRun.ID], before.Parts[recordedRun.ID])
		}
		wantParts := map[string]storage.ModelCost{"a": recordedPart, "b": pricedPart}
		if !reflect.DeepEqual(after.Parts[withPartsRun.ID], wantParts) {
			t.Errorf("parts of run %d = %+v, want %+v", withPartsRun.ID, after.Parts[withPartsRun.ID], wantParts)
		}
		wantNew := map[string]storage.ModelCost{"m1": operatorCost(12)}
		if !reflect.DeepEqual(after.Parts[bareRun.ID], wantNew) {
			t.Errorf("parts of run %d = %+v, want %+v", bareRun.ID, after.Parts[bareRun.ID], wantNew)
		}

		priced = nil
		again, err := s.RecordUnrecordedRunCosts(context.Background(), price)
		if err != nil {
			t.Fatalf("second RecordUnrecordedRunCosts() unexpected error: %v", err)
		}
		if again != 0 || len(priced) != 0 {
			t.Errorf("second RecordUnrecordedRunCosts() = %d with %d price calls, want 0 and 0", again, len(priced))
		}
	})

	t.Run("a rejected part rolls back every row and part", func(t *testing.T) {
		t.Parallel()
		s := openTestStore(t)
		migrateOrFatal(t, s)

		first := newTestRun(1)
		first.TokensMeasured, first.TotalTokens = true, 2_000_000
		firstRun := appendOrFatal(t, s, first)

		second := newTestRun(2)
		second.TokensMeasured, second.TotalTokens = true, 2_000_000
		second.ModelUsage = partS
		appendWithPartsOrFatal(t, s, second)

		before := readCostState(t, s)
		price := func(run storage.UnrecordedRun) storage.RunCostRecord {
			record := storage.RunCostRecord{EstimatedCostUSD: new(12.0), CostStatus: "estimated"}
			if run.ID == firstRun.ID {
				return record
			}
			record.NewParts = partS
			record.PartCosts = map[string]storage.ModelCost{"m1": operatorCost(12)}
			return record
		}

		n, err := s.RecordUnrecordedRunCosts(context.Background(), price)

		if err == nil {
			t.Fatalf("RecordUnrecordedRunCosts() = %d, want error", n)
		}
		if n != 0 {
			t.Errorf("RecordUnrecordedRunCosts() = %d, want 0", n)
		}
		if after := readCostState(t, s); !reflect.DeepEqual(after, before) {
			t.Errorf("cost state after failed fill = %+v, want %+v", after, before)
		}
	})
}
