package qualification

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
)

// DeclaredGap is one operator claim that the runtime cannot produce a
// semantic case.
type DeclaredGap struct {
	Capability Capability `json:"capability"`
	Case       Case       `json:"case"`
	Reason     string     `json:"reason"`
}

// DeclaredGapSet is the operator's declaration document: every
// capability gap the run was collected under.
type DeclaredGapSet struct {
	SchemaVersion int           `json:"schema_version"`
	Declarations  []DeclaredGap `json:"declarations"`
}

// declaredGapSetFields is the exact set of member names a declaration
// document may carry at the set level.
var declaredGapSetFields = map[string]bool{
	"schema_version": true, "declarations": true,
}

// declaredGapFields is the exact set of member names one declaration
// entry may carry.
var declaredGapFields = map[string]bool{
	"capability": true, "case": true, "reason": true,
}

// DecodeDeclaredGapSet strictly decodes a declaration document. It
// rejects unknown and missing fields at both levels, a schema version
// other than 1, an empty declaration list, a capability outside
// CapabilityCases, a case outside its capability's own case set, a
// reason outside DeclaredGapReasons, a duplicate capability and case
// pair, and a peer violation under DeclaredGapPeers.
func DecodeDeclaredGapSet(data []byte) (DeclaredGapSet, error) {
	var setFields map[string]json.RawMessage
	if err := json.Unmarshal(data, &setFields); err != nil {
		return DeclaredGapSet{}, fmt.Errorf("decode declaration document: %w", err)
	}
	for name := range setFields {
		if !declaredGapSetFields[name] {
			return DeclaredGapSet{}, fmt.Errorf("unknown field %q", name)
		}
	}
	for name := range declaredGapSetFields {
		if _, ok := setFields[name]; !ok {
			return DeclaredGapSet{}, fmt.Errorf("missing field %q", name)
		}
	}

	var schemaVersion int
	if err := json.Unmarshal(setFields["schema_version"], &schemaVersion); err != nil {
		return DeclaredGapSet{}, fmt.Errorf("schema_version: %w", err)
	}
	if schemaVersion != 1 {
		return DeclaredGapSet{}, fmt.Errorf("schema_version = %d, want 1", schemaVersion)
	}

	var rawEntries []map[string]json.RawMessage
	if err := json.Unmarshal(setFields["declarations"], &rawEntries); err != nil {
		return DeclaredGapSet{}, fmt.Errorf("declarations: %w", err)
	}
	if len(rawEntries) == 0 {
		return DeclaredGapSet{}, fmt.Errorf("declarations is empty, want at least one entry")
	}

	set := DeclaredGapSet{SchemaVersion: schemaVersion}
	seen := map[[2]string]bool{}
	reasons := map[[2]string]string{}
	for i, raw := range rawEntries {
		entry, err := decodeDeclaredGapEntry(raw)
		if err != nil {
			return DeclaredGapSet{}, fmt.Errorf("declarations[%d]: %w", i, err)
		}
		key := [2]string{string(entry.Capability), string(entry.Case)}
		if seen[key] {
			return DeclaredGapSet{}, fmt.Errorf("declarations[%d]: duplicate capability %s and case %s", i, entry.Capability, entry.Case)
		}
		seen[key] = true
		reasons[key] = entry.Reason
		set.Declarations = append(set.Declarations, entry)
	}

	for i, entry := range set.Declarations {
		peer, hasPeer := DeclaredGapPeers[entry.Case]
		if !hasPeer {
			continue
		}
		peerKey := [2]string{string(capabilityOwning(peer)), string(peer)}
		peerReason, peerDeclared := reasons[peerKey]
		if !peerDeclared {
			return DeclaredGapSet{}, fmt.Errorf("declarations[%d]: case %s is declared without its peer %s", i, entry.Case, peer)
		}
		if peerReason != entry.Reason {
			return DeclaredGapSet{}, fmt.Errorf("declarations[%d]: case %s and its peer %s carry differing reasons", i, entry.Case, peer)
		}
	}

	return set, nil
}

// decodeDeclaredGapEntry strictly decodes one declaration entry.
func decodeDeclaredGapEntry(raw map[string]json.RawMessage) (DeclaredGap, error) {
	for name := range raw {
		if !declaredGapFields[name] {
			return DeclaredGap{}, fmt.Errorf("unknown field %q", name)
		}
	}
	for name := range declaredGapFields {
		if _, ok := raw[name]; !ok {
			return DeclaredGap{}, fmt.Errorf("missing field %q", name)
		}
	}

	var capability Capability
	if err := json.Unmarshal(raw["capability"], &capability); err != nil {
		return DeclaredGap{}, fmt.Errorf("capability: %w", err)
	}
	cases, known := CapabilityCases[capability]
	if !known {
		return DeclaredGap{}, fmt.Errorf("capability %q is outside CapabilityCases", capability)
	}

	var caseID Case
	if err := json.Unmarshal(raw["case"], &caseID); err != nil {
		return DeclaredGap{}, fmt.Errorf("case: %w", err)
	}
	if !slices.Contains(cases, caseID) {
		return DeclaredGap{}, fmt.Errorf("case %q is outside capability %s's own case set", caseID, capability)
	}

	var reason string
	if err := json.Unmarshal(raw["reason"], &reason); err != nil {
		return DeclaredGap{}, fmt.Errorf("reason: %w", err)
	}
	if !slices.Contains(DeclaredGapReasons, reason) {
		return DeclaredGap{}, fmt.Errorf("reason %q is outside the closed value set", reason)
	}

	return DeclaredGap{Capability: capability, Case: caseID, Reason: reason}, nil
}

// ReadDeclaredGapFile reads and strictly decodes a declaration file.
// A read error and a decode error are both returned, unwrapped rather
// than swallowed.
func ReadDeclaredGapFile(path string) (DeclaredGapSet, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the caller supplies an operator-named path resolved at coordinate resolution
	if err != nil {
		return DeclaredGapSet{}, err
	}
	return DecodeDeclaredGapSet(data)
}

// Declared reports the declared reason for one capability and case. It
// performs a linear scan of s.Declarations, a small, bounded list.
func (s DeclaredGapSet) Declared(capability Capability, caseID Case) (string, bool) {
	for _, entry := range s.Declarations {
		if entry.Capability == capability && entry.Case == caseID {
			return entry.Reason, true
		}
	}
	return "", false
}
