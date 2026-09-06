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

// AbsentSurface is one operator claim that the runtime exposes no
// entry point for a measured surface.
type AbsentSurface struct {
	Surface Surface `json:"surface"`
	Reason  string  `json:"reason"`
}

// DeclarationSet is the operator's declaration document: the
// capability gaps and the absent surfaces the run was collected
// under.
type DeclarationSet struct {
	SchemaVersion  int             `json:"schema_version"`
	Declarations   []DeclaredGap   `json:"declarations"`
	AbsentSurfaces []AbsentSurface `json:"absent_surfaces"`
}

// declarationSetFieldOrder is the exact set of member names a
// declaration document may carry at the set level, in the fixed order
// a missing-field check reports them, so that order stays stable
// across runs rather than following Go's randomized map iteration.
var declarationSetFieldOrder = []string{"schema_version", "declarations", "absent_surfaces"}

// declarationSetFields is declarationSetFieldOrder as a set, for the
// unordered "is this member allowed at all" check.
var declarationSetFields = map[string]bool{
	"schema_version": true, "declarations": true, "absent_surfaces": true,
}

// declaredGapFields is the exact set of member names one declaration
// entry may carry.
var declaredGapFields = map[string]bool{
	"capability": true, "case": true, "reason": true,
}

// absentSurfaceFields is the exact set of member names one
// absent-surface entry may carry.
var absentSurfaceFields = map[string]bool{
	"surface": true, "reason": true,
}

// DecodeDeclarationSet strictly decodes a declaration document. It
// rejects unknown and missing fields at every level, a schema version
// other than 2, a document whose declarations and absent_surfaces both
// decode to empty, a capability outside CapabilityCases, a case
// outside its capability's own case set, a reason outside
// DeclaredGapReasons, a duplicate capability and case pair, a peer
// violation under DeclaredGapPeers, a surface outside
// DeclarableAbsentSurfaces, an absent-surface reason outside
// AbsentSurfaceReasons, and a surface declared absent twice.
func DecodeDeclarationSet(data []byte) (DeclarationSet, error) {
	var setFields map[string]json.RawMessage
	if err := json.Unmarshal(data, &setFields); err != nil {
		return DeclarationSet{}, fmt.Errorf("decode declaration document: %w", err)
	}
	for name := range setFields {
		if !declarationSetFields[name] {
			return DeclarationSet{}, fmt.Errorf("unknown field %q", name)
		}
	}
	for _, name := range declarationSetFieldOrder {
		if _, ok := setFields[name]; !ok {
			return DeclarationSet{}, fmt.Errorf("missing field %q", name)
		}
	}

	var schemaVersion int
	if err := json.Unmarshal(setFields["schema_version"], &schemaVersion); err != nil {
		return DeclarationSet{}, fmt.Errorf("schema_version: %w", err)
	}
	if schemaVersion != 2 {
		return DeclarationSet{}, fmt.Errorf("schema_version = %d, want 2 (absent_surfaces)", schemaVersion)
	}

	var rawEntries []map[string]json.RawMessage
	if err := json.Unmarshal(setFields["declarations"], &rawEntries); err != nil {
		return DeclarationSet{}, fmt.Errorf("declarations: %w", err)
	}
	var rawAbsent []map[string]json.RawMessage
	if err := json.Unmarshal(setFields["absent_surfaces"], &rawAbsent); err != nil {
		return DeclarationSet{}, fmt.Errorf("absent_surfaces: %w", err)
	}
	if len(rawEntries) == 0 && len(rawAbsent) == 0 {
		return DeclarationSet{}, fmt.Errorf("declarations and absent_surfaces are both empty, want at least one entry in either")
	}

	set := DeclarationSet{SchemaVersion: schemaVersion}
	seen := map[[2]string]bool{}
	reasons := map[[2]string]string{}
	for i, raw := range rawEntries {
		entry, err := decodeDeclaredGapEntry(raw)
		if err != nil {
			return DeclarationSet{}, fmt.Errorf("declarations[%d]: %w", i, err)
		}
		key := [2]string{string(entry.Capability), string(entry.Case)}
		if seen[key] {
			return DeclarationSet{}, fmt.Errorf("declarations[%d]: duplicate capability %s and case %s", i, entry.Capability, entry.Case)
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
			return DeclarationSet{}, fmt.Errorf("declarations[%d]: case %s is declared without its peer %s", i, entry.Case, peer)
		}
		if peerReason != entry.Reason {
			return DeclarationSet{}, fmt.Errorf("declarations[%d]: case %s and its peer %s carry differing reasons", i, entry.Case, peer)
		}
	}

	seenAbsent := map[Surface]bool{}
	for i, raw := range rawAbsent {
		entry, err := decodeAbsentSurfaceEntry(raw)
		if err != nil {
			return DeclarationSet{}, fmt.Errorf("absent_surfaces[%d]: %w", i, err)
		}
		if seenAbsent[entry.Surface] {
			return DeclarationSet{}, fmt.Errorf("absent_surfaces[%d]: surface %s is declared absent twice", i, entry.Surface)
		}
		seenAbsent[entry.Surface] = true
		set.AbsentSurfaces = append(set.AbsentSurfaces, entry)
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

// decodeAbsentSurfaceEntry strictly decodes one absent-surface entry.
func decodeAbsentSurfaceEntry(raw map[string]json.RawMessage) (AbsentSurface, error) {
	for name := range raw {
		if !absentSurfaceFields[name] {
			return AbsentSurface{}, fmt.Errorf("unknown field %q", name)
		}
	}
	for name := range absentSurfaceFields {
		if _, ok := raw[name]; !ok {
			return AbsentSurface{}, fmt.Errorf("missing field %q", name)
		}
	}

	var surface Surface
	if err := json.Unmarshal(raw["surface"], &surface); err != nil {
		return AbsentSurface{}, fmt.Errorf("surface: %w", err)
	}
	if !slices.Contains(DeclarableAbsentSurfaces, surface) {
		return AbsentSurface{}, fmt.Errorf("surface %q is outside DeclarableAbsentSurfaces", surface)
	}

	var reason string
	if err := json.Unmarshal(raw["reason"], &reason); err != nil {
		return AbsentSurface{}, fmt.Errorf("reason: %w", err)
	}
	if !slices.Contains(AbsentSurfaceReasons, reason) {
		return AbsentSurface{}, fmt.Errorf("reason %q is outside the closed value set", reason)
	}

	return AbsentSurface{Surface: surface, Reason: reason}, nil
}

// ReadDeclarationFile reads and strictly decodes a declaration file.
// A read error and a decode error are both returned, unwrapped rather
// than swallowed.
func ReadDeclarationFile(path string) (DeclarationSet, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the caller supplies an operator-named path resolved at coordinate resolution
	if err != nil {
		return DeclarationSet{}, err
	}
	return DecodeDeclarationSet(data)
}

// Declared reports the declared reason for one capability and case. It
// performs a linear scan of s.Declarations, a small, bounded list.
func (s DeclarationSet) Declared(capability Capability, caseID Case) (string, bool) {
	for _, entry := range s.Declarations {
		if entry.Capability == capability && entry.Case == caseID {
			return entry.Reason, true
		}
	}
	return "", false
}

// AbsentSurfaceDeclared reports the declared reason for one surface. It
// performs a linear scan of s.AbsentSurfaces, a small, bounded list.
func (s DeclarationSet) AbsentSurfaceDeclared(surface Surface) (string, bool) {
	for _, entry := range s.AbsentSurfaces {
		if entry.Surface == surface {
			return entry.Reason, true
		}
	}
	return "", false
}
