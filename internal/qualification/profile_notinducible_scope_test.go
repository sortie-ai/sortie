package qualification

import (
	"strings"
	"testing"
)

func TestNotInducibleCasesAreTheProfilesOwnStatement(T *testing.T) {
	T.Parallel()

	tests := []struct {
		name    string
		entries []any
	}{
		{name: "no entry at all", entries: []any{}},
		{
			name: "an entry on one native surface only",
			entries: []any{
				map[string]any{"surface": "native_json", "case": "limit_reached", "reason": NotInducibleChannelTooSmall},
			},
		},
		{
			name: "an entry naming another case entirely",
			entries: []any{
				map[string]any{"surface": "native_json", "case": "cancellation", "reason": NotInducibleTerminalAtExitOnly},
			},
		},
	}

	for _, tc := range tests {
		T.Run(tc.name, func(T *testing.T) {
			T.Parallel()
			doc := validProfileDoc()
			doc["not_inducible_cases"] = tc.entries
			data := marshalProfileDoc(T, doc)
			if _, err := DecodeRuntimeProfile(data); err != nil {
				T.Errorf("DecodeRuntimeProfile() error = %v, want nil: the profile states what its own runtime cannot reach, and no case is required of every profile", err)
			}
		})
	}
}

func TestNotInducibleReasonStillPinsItsCase(T *testing.T) {
	T.Parallel()

	doc := validProfileDoc()
	doc["not_inducible_cases"] = []any{
		map[string]any{"surface": "native_json", "case": "cancellation", "reason": NotInducibleChannelTooSmall},
	}
	data := marshalProfileDoc(T, doc)
	_, err := DecodeRuntimeProfile(data)
	if err == nil {
		T.Fatal("DecodeRuntimeProfile() = nil error, want rejection of a reason paired with a case it does not describe")
	}
	if !strings.Contains(err.Error(), "pairs only with case") {
		T.Errorf("DecodeRuntimeProfile() error = %v, want it to name the mismatched pairing", err)
	}
}
