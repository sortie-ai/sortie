package qualification

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// declarationEntryJSON renders one raw declaration entry.
func declarationEntryJSON(capability, caseID, reason string) string {
	return fmt.Sprintf(`{"capability":%q,"case":%q,"reason":%q}`, capability, caseID, reason)
}

// declarationSetJSON renders a declaration document from raw entry
// bodies.
func declarationSetJSON(entries ...string) string {
	return fmt.Sprintf(`{"schema_version":1,"declarations":[%s]}`, strings.Join(entries, ","))
}

// validRefusalPeerEntries are the two entries the refusal/non-retryable
// peer pair requires.
func validRefusalPeerEntries() []string {
	return []string{
		declarationEntryJSON(string(CapabilityTurnDisposition), string(CaseRuntimeRefusal), DeclaredGapNeverProduced),
		declarationEntryJSON(string(CapabilityRetryClassification), string(CaseNonRetryableRefusal), DeclaredGapNeverProduced),
	}
}

// TestDecodeDeclaredGapSet confirms the strict declaration decoder:
// the accepting control for a valid peer pair, and one rejection per
// clause the decoder enforces.
func TestDecodeDeclaredGapSet(t *testing.T) {
	t.Parallel()

	t.Run("valid two-entry peer pair decodes", func(t *testing.T) {
		t.Parallel()

		doc := declarationSetJSON(validRefusalPeerEntries()...)
		got, err := DecodeDeclaredGapSet([]byte(doc))
		if err != nil {
			t.Fatalf("DecodeDeclaredGapSet(%s) error = %v, want nil", doc, err)
		}
		want := DeclaredGapSet{
			SchemaVersion: 1,
			Declarations: []DeclaredGap{
				{Capability: CapabilityTurnDisposition, Case: CaseRuntimeRefusal, Reason: DeclaredGapNeverProduced},
				{Capability: CapabilityRetryClassification, Case: CaseNonRetryableRefusal, Reason: DeclaredGapNeverProduced},
			},
		}
		if got.SchemaVersion != want.SchemaVersion || len(got.Declarations) != len(want.Declarations) {
			t.Fatalf("DecodeDeclaredGapSet() = %+v, want %+v", got, want)
		}
		for i := range want.Declarations {
			if got.Declarations[i] != want.Declarations[i] {
				t.Errorf("DecodeDeclaredGapSet() entry %d = %+v, want %+v", i, got.Declarations[i], want.Declarations[i])
			}
		}
	})

	t.Run("unknown field at the set level is rejected", func(t *testing.T) {
		t.Parallel()

		doc := fmt.Sprintf(`{"schema_version":1,"declarations":[%s],"extra":1}`, validRefusalPeerEntries()[0])
		_, err := DecodeDeclaredGapSet([]byte(doc))
		requireDeclarationError(t, doc, err, `unknown field "extra"`)
	})

	t.Run("missing schema_version at the set level is rejected", func(t *testing.T) {
		t.Parallel()

		doc := fmt.Sprintf(`{"declarations":[%s]}`, validRefusalPeerEntries()[0])
		_, err := DecodeDeclaredGapSet([]byte(doc))
		requireDeclarationError(t, doc, err, `missing field "schema_version"`)
	})

	t.Run("missing declarations at the set level is rejected", func(t *testing.T) {
		t.Parallel()

		doc := `{"schema_version":1}`
		_, err := DecodeDeclaredGapSet([]byte(doc))
		requireDeclarationError(t, doc, err, `missing field "declarations"`)
	})

	t.Run("unknown field at the entry level is rejected", func(t *testing.T) {
		t.Parallel()

		entry := fmt.Sprintf(`{"capability":%q,"case":%q,"reason":%q,"extra":1}`,
			CapabilityTurnDisposition, CaseRuntimeRefusal, DeclaredGapNeverProduced)
		doc := declarationSetJSON(entry, validRefusalPeerEntries()[1])
		_, err := DecodeDeclaredGapSet([]byte(doc))
		requireDeclarationError(t, doc, err, `unknown field "extra"`)
	})

	t.Run("missing field at the entry level is rejected", func(t *testing.T) {
		t.Parallel()

		tests := []struct {
			name  string
			entry string
		}{
			{"missing capability", fmt.Sprintf(`{"case":%q,"reason":%q}`, CaseRuntimeRefusal, DeclaredGapNeverProduced)},
			{"missing case", fmt.Sprintf(`{"capability":%q,"reason":%q}`, CapabilityTurnDisposition, DeclaredGapNeverProduced)},
			{"missing reason", fmt.Sprintf(`{"capability":%q,"case":%q}`, CapabilityTurnDisposition, CaseRuntimeRefusal)},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				doc := declarationSetJSON(tt.entry)
				_, err := DecodeDeclaredGapSet([]byte(doc))
				requireDeclarationError(t, doc, err, `missing field "`)
			})
		}
	})

	t.Run("schema_version other than 1 is rejected", func(t *testing.T) {
		t.Parallel()

		doc := `{"schema_version":2,"declarations":[` + validRefusalPeerEntries()[0] + `]}`
		_, err := DecodeDeclaredGapSet([]byte(doc))
		requireDeclarationError(t, doc, err, "schema_version = 2, want 1")
	})

	t.Run("empty declarations list is rejected", func(t *testing.T) {
		t.Parallel()

		doc := `{"schema_version":1,"declarations":[]}`
		_, err := DecodeDeclaredGapSet([]byte(doc))
		requireDeclarationError(t, doc, err, "declarations is empty")
	})

	t.Run("capability outside CapabilityCases is rejected", func(t *testing.T) {
		t.Parallel()

		entry := declarationEntryJSON(string(CapabilityTokenCeiling), string(CaseSuccess), DeclaredGapNeverProduced)
		doc := declarationSetJSON(entry)
		_, err := DecodeDeclaredGapSet([]byte(doc))
		requireDeclarationError(t, doc, err, "outside CapabilityCases")
	})

	t.Run("case outside the capability's own case set is rejected", func(t *testing.T) {
		t.Parallel()

		entry := declarationEntryJSON(string(CapabilityTurnDisposition), string(CaseRetryableTransport), DeclaredGapNeverProduced)
		doc := declarationSetJSON(entry)
		_, err := DecodeDeclaredGapSet([]byte(doc))
		requireDeclarationError(t, doc, err, "outside capability")
	})

	t.Run("reason outside DeclaredGapReasons is rejected", func(t *testing.T) {
		t.Parallel()

		entry := declarationEntryJSON(string(CapabilityTurnDisposition), string(CaseRuntimeRefusal), "bogus_reason")
		doc := declarationSetJSON(entry)
		_, err := DecodeDeclaredGapSet([]byte(doc))
		requireDeclarationError(t, doc, err, "outside the closed value set")
	})

	t.Run("duplicate capability and case pair is rejected", func(t *testing.T) {
		t.Parallel()

		entry := declarationEntryJSON(string(CapabilityTurnDisposition), string(CaseRuntimeRefusal), DeclaredGapNeverProduced)
		doc := declarationSetJSON(entry, entry)
		_, err := DecodeDeclaredGapSet([]byte(doc))
		requireDeclarationError(t, doc, err, "duplicate capability")
	})

	t.Run("peer declared alone is rejected", func(t *testing.T) {
		t.Parallel()

		entry := declarationEntryJSON(string(CapabilityTurnDisposition), string(CaseRuntimeRefusal), DeclaredGapNeverProduced)
		doc := declarationSetJSON(entry)
		_, err := DecodeDeclaredGapSet([]byte(doc))
		requireDeclarationError(t, doc, err, "declared without its peer")
	})

	t.Run("peer declared with a differing reason is rejected", func(t *testing.T) {
		t.Parallel()

		entries := []string{
			declarationEntryJSON(string(CapabilityTurnDisposition), string(CaseRuntimeRefusal), DeclaredGapNeverProduced),
			declarationEntryJSON(string(CapabilityRetryClassification), string(CaseNonRetryableRefusal), DeclaredGapFolded),
		}
		doc := declarationSetJSON(entries...)
		_, err := DecodeDeclaredGapSet([]byte(doc))
		requireDeclarationError(t, doc, err, "differing reasons")
	})
}

// requireDeclarationError fails t unless err is non-nil and its message
// contains wantSub.
func requireDeclarationError(t *testing.T, doc string, err error, wantSub string) {
	t.Helper()
	if err == nil {
		t.Fatalf("DecodeDeclaredGapSet(%s) = nil error, want error naming %q", doc, wantSub)
	}
	if !strings.Contains(err.Error(), wantSub) {
		t.Errorf("DecodeDeclaredGapSet() error = %v, want it to mention %q", err, wantSub)
	}
}

// TestReadDeclaredGapFile confirms the file reader surfaces both a
// read error for a missing path and a decode error for invalid
// content, unwrapped rather than swallowed.
func TestReadDeclaredGapFile(t *testing.T) {
	t.Parallel()

	t.Run("missing path surfaces the read error", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "does-not-exist.json")
		_, err := ReadDeclaredGapFile(path)
		if err == nil {
			t.Fatalf("ReadDeclaredGapFile(%s) = nil error, want a read error", path)
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("ReadDeclaredGapFile() error = %v, want it to wrap os.ErrNotExist", err)
		}
	})

	t.Run("invalid JSON content surfaces the decode error", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "declared-gaps.json")
		if err := os.WriteFile(path, []byte("not valid json"), 0o600); err != nil {
			t.Fatalf("write declaration file: %v", err)
		}
		_, err := ReadDeclaredGapFile(path)
		requireDeclarationError(t, path, err, "decode declaration document")
	})

	t.Run("valid file decodes", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "declared-gaps.json")
		doc := declarationSetJSON(validRefusalPeerEntries()...)
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatalf("write declaration file: %v", err)
		}
		set, err := ReadDeclaredGapFile(path)
		if err != nil {
			t.Fatalf("ReadDeclaredGapFile(%s) error = %v, want nil", path, err)
		}
		if len(set.Declarations) != 2 {
			t.Errorf("ReadDeclaredGapFile() declarations = %d, want 2", len(set.Declarations))
		}
	})
}

// TestDeclaredGapSetDeclared confirms the linear-scan lookup reports
// the declared reason and found flag exactly for a declared pair.
func TestDeclaredGapSetDeclared(t *testing.T) {
	t.Parallel()

	set := DeclaredGapSet{
		SchemaVersion: 1,
		Declarations: []DeclaredGap{
			{Capability: CapabilityTurnDisposition, Case: CaseRuntimeRefusal, Reason: DeclaredGapNeverProduced},
		},
	}

	if reason, found := set.Declared(CapabilityTurnDisposition, CaseRuntimeRefusal); !found || reason != DeclaredGapNeverProduced {
		t.Errorf("Declared(turn_disposition, runtime_refusal) = %q, %v, want %q, true", reason, found, DeclaredGapNeverProduced)
	}
	if _, found := set.Declared(CapabilityRetryClassification, CaseNonRetryableRefusal); found {
		t.Error("Declared(retry_classification, non_retryable_refusal) = true, want false for an undeclared pair")
	}
}
