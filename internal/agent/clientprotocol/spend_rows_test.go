package clientprotocol

import (
	"maps"
	"slices"
	"testing"

	"github.com/sortie-ai/sortie/internal/agent/clientprotocol/usagesource"
	"github.com/sortie-ai/sortie/internal/agent/usage"
)

const (
	spendDeclarationEnv    = "SORTIE_CLIENTPROTOCOL_SPEND_DECLARATION"
	spendDeclarationColumn = "clientprotocol_spend_declaration"
)

func declarationsNamedByRows(doc nightlyWorkflowDocument) map[string]bool {
	named := map[string]bool{}
	for _, r := range nightlyMatrixRowsNamingThisPackage(doc) {
		step, found := nightlyGoTestStep(r.job.Steps)
		if !found {
			continue
		}
		env := map[string]string{}
		maps.Copy(env, r.job.Env)
		maps.Copy(env, step.Env)
		named[nightlyMatrixValue(r.row, env[spendDeclarationEnv])] = true
	}
	return named
}

func unnamedSpendDeclarations(t *testing.T, doc nightlyWorkflowDocument) []string {
	t.Helper()

	named := declarationsNamedByRows(doc)
	declarations := map[string]usagesource.Declaration{usagesource.DefaultName: usagesource.Default}
	for _, name := range usagesource.Declarations.Kinds() {
		declaration, err := usagesource.Declarations.Get(name)
		if err != nil {
			t.Fatalf("Declarations.Get(%q) error = %v", name, err)
		}
		declarations[name] = declaration
	}

	var unnamed []string
	for _, name := range slices.Sorted(maps.Keys(declarations)) {
		declaration := declarations[name]
		declaresSpend := declaration.Live.Spend.Shape != usage.SpendUndeclared ||
			declaration.Records.Spend.Shape != usage.SpendUndeclared
		if declaresSpend && !named[name] {
			unnamed = append(unnamed, name)
		}
	}
	return unnamed
}

func TestEverySpendDeclarationHasScriptedModelRow(t *testing.T) {
	t.Parallel()

	doc, err := readNightlyWorkflow()
	if err != nil {
		t.Fatalf("read the nightly workflow: %v", err)
	}

	for _, name := range unnamedSpendDeclarations(t, doc) {
		t.Errorf("usage declaration %q declares spend and no nightly row names it in %s", name, spendDeclarationEnv)
	}
}

func TestUnnamedSpendDeclarationsNamesDeclarationWithoutRow(t *testing.T) {
	t.Parallel()

	shipped, err := readNightlyWorkflow()
	if err != nil {
		t.Fatalf("read the nightly workflow: %v", err)
	}
	withoutVariable := withMatrixRows(shipped, func(row map[string]string) bool {
		delete(row, spendDeclarationColumn)
		return true
	})

	named := declarationsNamedByRows(shipped)
	delete(named, "")
	want := slices.Sorted(maps.Keys(named))
	if len(want) == 0 {
		t.Fatalf("no nightly row names a spend declaration in %s", spendDeclarationEnv)
	}

	got := unnamedSpendDeclarations(t, withoutVariable)
	slices.Sort(got)

	if !slices.Equal(got, want) {
		t.Errorf("unnamedSpendDeclarations(rows without %s) = %q, want %q", spendDeclarationColumn, got, want)
	}
}
