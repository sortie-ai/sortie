package sqlite

import (
	"context"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/storage"
)

func TestListLivePasses_MergesRetryEntriesInIssueThenKindOrder(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()
	for _, pass := range []storage.LivePass{
		{IssueID: "ISS-2", Kind: storage.LivePassBudget, ChainID: "chain-r", RuleName: "plan", StagePrevious: "specify"},
		{IssueID: "ISS-1", Kind: storage.LivePassBudget, ChainID: "chain-b", RuleName: "implement", StagePrevious: "plan"},
		{IssueID: "ISS-1", Kind: storage.LivePassRunning, ChainID: "chain-a", RuleName: "specify", StagePrevious: ""},
	} {
		if err := s.UpsertLivePass(ctx, pass); err != nil {
			t.Fatalf("UpsertLivePass(%v) = %v, want nil", pass, err)
		}
	}
	retry := storage.RetryEntry{
		IssueID: "ISS-1", Identifier: "PROJ-1", Attempt: 1, DueAtMs: 1000,
		RuleName: "plan", ChainID: "chain-q", StagePrevious: "specify",
	}
	if err := s.SaveRetryEntry(ctx, retry); err != nil {
		t.Fatalf("SaveRetryEntry: %v", err)
	}

	got, err := s.ListLivePasses(ctx)

	want := []storage.LivePass{
		{IssueID: "ISS-1", Kind: storage.LivePassBudget, ChainID: "chain-b", RuleName: "implement", StagePrevious: "plan"},
		{IssueID: "ISS-1", Kind: storage.LivePassRetry, ChainID: "chain-q", RuleName: "plan", StagePrevious: "specify"},
		{IssueID: "ISS-1", Kind: storage.LivePassRunning, ChainID: "chain-a", RuleName: "specify", StagePrevious: ""},
		{IssueID: "ISS-2", Kind: storage.LivePassBudget, ChainID: "chain-r", RuleName: "plan", StagePrevious: "specify"},
	}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("ListLivePasses() = %v, %v, want %v, nil", got, err, want)
	}
}

func TestLivePassWrites(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	migrateOrFatal(t, s)
	ctx := context.Background()
	check := func(step string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s = %v, want nil", step, err)
		}
	}
	check("UpsertLivePass first", s.UpsertLivePass(ctx, storage.LivePass{IssueID: "ISS-1", Kind: storage.LivePassRunning, ChainID: "chain-a", RuleName: "specify", StagePrevious: ""}))
	check("UpsertLivePass replacing", s.UpsertLivePass(ctx, storage.LivePass{IssueID: "ISS-1", Kind: storage.LivePassRunning, ChainID: "chain-a", RuleName: "plan", StagePrevious: "specify"}))
	check("UpsertLivePass other kind", s.UpsertLivePass(ctx, storage.LivePass{IssueID: "ISS-1", Kind: storage.LivePassBudget, ChainID: "chain-a", RuleName: "plan", StagePrevious: "specify"}))
	check("DeleteLivePass missing row", s.DeleteLivePass(ctx, "ISS-9", storage.LivePassRunning))

	replaced, err := s.ListLivePasses(ctx)
	check("ListLivePasses after upsert", err)
	wantReplaced := []storage.LivePass{
		{IssueID: "ISS-1", Kind: storage.LivePassBudget, ChainID: "chain-a", RuleName: "plan", StagePrevious: "specify"},
		{IssueID: "ISS-1", Kind: storage.LivePassRunning, ChainID: "chain-a", RuleName: "plan", StagePrevious: "specify"},
	}
	if !slices.Equal(replaced, wantReplaced) {
		t.Errorf("ListLivePasses() after replacing upsert = %v, want %v", replaced, wantReplaced)
	}

	check("DeleteLivePass", s.DeleteLivePass(ctx, "ISS-1", storage.LivePassRunning))
	afterDelete, err := s.ListLivePasses(ctx)
	check("ListLivePasses after delete", err)
	if want := wantReplaced[:1]; !slices.Equal(afterDelete, want) {
		t.Errorf("ListLivePasses() after DeleteLivePass of one kind = %v, want %v", afterDelete, want)
	}

	check("DeleteAllLivePasses", s.DeleteAllLivePasses(ctx))
	afterAll, err := s.ListLivePasses(ctx)
	check("ListLivePasses after delete all", err)
	if len(afterAll) != 0 {
		t.Errorf("ListLivePasses() after DeleteAllLivePasses = %v, want none", afterAll)
	}
}
