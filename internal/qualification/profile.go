package qualification

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// The four capability-gap labels a profile's capability_gap_labels
// member may name. They mirror internal/agent/clientprotocol's own
// unexported capability-record labels; this package stays a leaf and
// cannot import that package, so the four strings are pinned here as
// their own source of truth and cross-checked against the adapter's
// labels by the capability-record drift check.
const (
	capabilityGapLabelToolServers         = "tool servers"
	capabilityGapLabelTokenCounts         = "token counts"
	capabilityGapLabelSessionContinuation = "session continuation"
	capabilityGapLabelAgentVersion        = "agent version"
)

// capabilityGapLabels is the closed set capability_gap_labels draws
// from.
var capabilityGapLabels = []string{
	capabilityGapLabelToolServers, capabilityGapLabelTokenCounts,
	capabilityGapLabelSessionContinuation, capabilityGapLabelAgentVersion,
}

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

// EntryPoint is one surface's argument vector for a graded launch,
// appended to the resolved command path. Placeholders "{model}",
// "{policy}", and "{prompt}" are substituted positionally; any other
// brace-delimited token is rejected at decode time. The
// published-posture probe is launched from a profile's PublishedSample
// instead and carries none of these args.
type EntryPoint struct {
	Args []string `json:"args"`
}

// TerminalLocator selects the terminal object out of a native
// surface's decoded top-level JSON values.
type TerminalLocator struct {
	// Mode is "first_value" or "discriminated".
	Mode               string `json:"mode"`
	DiscriminatorKey   string `json:"discriminator_key"`
	DiscriminatorValue string `json:"discriminator_value"`
}

// Recognizer maps one structured native surface's own output onto a
// terminal outcome. A surface whose output carries no terminal member
// carries no recognizer.
type Recognizer struct {
	Locator       TerminalLocator `json:"locator"`
	ErrorMembers  []string        `json:"error_members"`
	SuccessMember string          `json:"success_member"`
	StatusMember  string          `json:"status_member"`
	StatusCases   map[string]Case `json:"status_cases"`
	StatusEndTurn []string        `json:"status_end_turn"`

	// ModelRequestPath is a nested key sequence into the terminal
	// object, read for the model-request reading. It stays an explicit
	// key sequence rather than a path-expression string, so no
	// path-expression grammar enters the schema for what is otherwise a
	// single flat lookup.
	ModelRequestPath []string `json:"model_request_path"`
}

// Terminal is one native surface's recognized terminal outcome:
// exactly one of EndTurn, Error, or a non-empty Case is set. The zero
// value paired with a false Recognizer.Terminal result means no
// terminal member was recognized at all.
type Terminal struct {
	EndTurn bool
	Error   bool
	Case    Case
}

// RuntimeProfile is the operator's runtime profile document: everything
// about one runtime the live probe needs, and nothing about the host it
// runs on. It absorbs the declaration document it supersedes:
// Declarations and AbsentSurfaces carry that document's own fields.
type RuntimeProfile struct {
	SchemaVersion   int      `json:"schema_version"` // exactly 3
	RuntimeID       string   `json:"runtime_id"`     // e.g. "gemini-cli"
	IdentityTokens  []string `json:"identity_tokens"`
	NotesPath       string   `json:"notes_path"`
	MeasurementPath string   `json:"measurement_path"`
	PublishedSample string   `json:"published_sample"`

	ToolNameFormat      string   `json:"tool_name_format"`
	ProjectConfigPaths  []string `json:"project_config_paths"`
	VersionArgs         []string `json:"version_args"`
	ModelArgs           []string `json:"model_args"`
	CapabilityGapLabels []string `json:"capability_gap_labels"`

	EntryPoints map[Surface]EntryPoint `json:"entry_points"`
	Recognizers map[Surface]Recognizer `json:"recognizers"`

	Declarations   []DeclaredGap   `json:"declarations"`
	AbsentSurfaces []AbsentSurface `json:"absent_surfaces"`
}

// Measurement is the tracked artifact one live run produces: the notes
// expectation, the digest of the profile the run used, and the run's
// UTC measurement date. The date is machine-read data rather than
// notes prose because qualification.ValidateNotes rejects a notes line
// carrying a date.
type Measurement struct {
	SchemaVersion int              `json:"schema_version"` // exactly 1
	ProfileDigest string           `json:"profile_digest"`
	MeasuredAt    string           `json:"measured_at"`
	Expectation   NotesExpectation `json:"expectation"`
}

// entryPointPlaceholders is the closed set of brace-delimited tokens
// an EntryPoint.Args or PublishedSample model_args entry may carry.
var entryPointPlaceholders = []string{"{model}", "{policy}", "{prompt}"}

// runtimeProfileFields is the exact set of member names a runtime
// profile document may carry at the top level.
var runtimeProfileFieldOrder = []string{
	"schema_version", "runtime_id", "identity_tokens",
	"notes_path", "measurement_path", "published_sample",
	"tool_name_format", "project_config_paths", "version_args",
	"model_args", "capability_gap_labels",
	"entry_points", "recognizers",
	"declarations", "absent_surfaces",
}

var runtimeProfileFields = func() map[string]bool {
	fields := make(map[string]bool, len(runtimeProfileFieldOrder))
	for _, name := range runtimeProfileFieldOrder {
		fields[name] = true
	}
	return fields
}()

var entryPointFields = map[string]bool{"args": true}

var terminalLocatorFields = map[string]bool{
	"mode": true, "discriminator_key": true, "discriminator_value": true,
}

var recognizerFieldOrder = []string{
	"locator", "error_members", "success_member",
	"status_member", "status_cases", "status_end_turn",
	"model_request_path",
}

var recognizerFields = func() map[string]bool {
	fields := make(map[string]bool, len(recognizerFieldOrder))
	for _, name := range recognizerFieldOrder {
		fields[name] = true
	}
	return fields
}()

// DecodeRuntimeProfile strictly decodes a runtime profile document. It
// rejects unknown and missing fields at every level, mirroring
// its own strict discipline, and enforces every stage-D
// validation rule: schema_version, identity_tokens, tool_name_format,
// model_args, capability_gap_labels, entry_points, recognizers,
// status_cases/status_end_turn, and the declarations/absent_surfaces
// rules the declaration document already enforced.
func DecodeRuntimeProfile(data []byte) (RuntimeProfile, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return RuntimeProfile{}, fmt.Errorf("decode runtime profile: %w", err)
	}
	for name := range top {
		if !runtimeProfileFields[name] {
			return RuntimeProfile{}, fmt.Errorf("unknown field %q", name)
		}
	}
	for _, name := range runtimeProfileFieldOrder {
		if _, ok := top[name]; !ok {
			return RuntimeProfile{}, fmt.Errorf("missing field %q", name)
		}
	}

	var profile RuntimeProfile
	var err error

	if profile.SchemaVersion, err = decodeInt(top["schema_version"]); err != nil {
		return RuntimeProfile{}, fmt.Errorf("schema_version: %w", err)
	}
	if profile.SchemaVersion != 3 {
		return RuntimeProfile{}, fmt.Errorf("schema_version = %d, want 3", profile.SchemaVersion)
	}

	if profile.RuntimeID, err = decodeString(top["runtime_id"]); err != nil {
		return RuntimeProfile{}, fmt.Errorf("runtime_id: %w", err)
	}
	if profile.RuntimeID == "" || profile.RuntimeID != strings.ToLower(profile.RuntimeID) {
		return RuntimeProfile{}, fmt.Errorf("runtime_id %q must be non-empty and lowercase", profile.RuntimeID)
	}

	if err := json.Unmarshal(top["identity_tokens"], &profile.IdentityTokens); err != nil {
		return RuntimeProfile{}, fmt.Errorf("identity_tokens: %w", err)
	}
	if len(profile.IdentityTokens) == 0 {
		return RuntimeProfile{}, errors.New("identity_tokens must carry at least one token")
	}
	for _, token := range profile.IdentityTokens {
		if token == "" || token != strings.ToLower(token) {
			return RuntimeProfile{}, fmt.Errorf("identity_tokens entry %q must be a non-empty lowercase token", token)
		}
	}

	if profile.NotesPath, err = decodeString(top["notes_path"]); err != nil {
		return RuntimeProfile{}, fmt.Errorf("notes_path: %w", err)
	}
	if profile.MeasurementPath, err = decodeString(top["measurement_path"]); err != nil {
		return RuntimeProfile{}, fmt.Errorf("measurement_path: %w", err)
	}
	if profile.PublishedSample, err = decodeString(top["published_sample"]); err != nil {
		return RuntimeProfile{}, fmt.Errorf("published_sample: %w", err)
	}
	if profile.NotesPath == "" || profile.MeasurementPath == "" || profile.PublishedSample == "" {
		return RuntimeProfile{}, errors.New("notes_path, measurement_path, and published_sample must each be non-empty")
	}

	if profile.ToolNameFormat, err = decodeString(top["tool_name_format"]); err != nil {
		return RuntimeProfile{}, fmt.Errorf("tool_name_format: %w", err)
	}
	if err := validatePlaceholders(profile.ToolNameFormat, []string{"{server}", "{tool}"}, []string{"{server}", "{tool}"}); err != nil {
		return RuntimeProfile{}, fmt.Errorf("tool_name_format: %w", err)
	}

	if err := json.Unmarshal(top["project_config_paths"], &profile.ProjectConfigPaths); err != nil {
		return RuntimeProfile{}, fmt.Errorf("project_config_paths: %w", err)
	}
	if err := json.Unmarshal(top["version_args"], &profile.VersionArgs); err != nil {
		return RuntimeProfile{}, fmt.Errorf("version_args: %w", err)
	}

	if err := json.Unmarshal(top["model_args"], &profile.ModelArgs); err != nil {
		return RuntimeProfile{}, fmt.Errorf("model_args: %w", err)
	}
	if len(profile.ModelArgs) == 0 {
		return RuntimeProfile{}, errors.New("model_args must be non-empty")
	}
	if err := validatePlaceholderArgs(profile.ModelArgs, []string{"{model}"}, []string{"{model}"}); err != nil {
		return RuntimeProfile{}, fmt.Errorf("model_args: %w", err)
	}

	if err := json.Unmarshal(top["capability_gap_labels"], &profile.CapabilityGapLabels); err != nil {
		return RuntimeProfile{}, fmt.Errorf("capability_gap_labels: %w", err)
	}
	if err := validateCapabilityGapLabels(profile.CapabilityGapLabels); err != nil {
		return RuntimeProfile{}, fmt.Errorf("capability_gap_labels: %w", err)
	}

	if profile.EntryPoints, err = decodeEntryPoints(top["entry_points"]); err != nil {
		return RuntimeProfile{}, fmt.Errorf("entry_points: %w", err)
	}
	if _, hasProtocol := profile.EntryPoints[SurfaceProtocol]; !hasProtocol {
		return RuntimeProfile{}, fmt.Errorf("entry_points must carry %q", SurfaceProtocol)
	}

	if profile.Recognizers, err = decodeRecognizers(top["recognizers"], profile.EntryPoints); err != nil {
		return RuntimeProfile{}, fmt.Errorf("recognizers: %w", err)
	}

	declarations, absentSurfaces, err := decodeDeclarationFields(top)
	if err != nil {
		return RuntimeProfile{}, err
	}
	profile.Declarations = declarations
	profile.AbsentSurfaces = absentSurfaces
	for _, absent := range profile.AbsentSurfaces {
		if _, ok := profile.EntryPoints[absent.Surface]; !ok {
			return RuntimeProfile{}, fmt.Errorf("absent_surfaces: %q is declared absent but carries no entry point, so its absence cannot be corroborated by a launch", absent.Surface)
		}
	}

	return profile, nil
}

// validatePlaceholders rejects a string that does not carry every
// member of required exactly once, or that carries a brace-delimited
// token outside allowed.
func validatePlaceholders(value string, required, allowed []string) error {
	for _, token := range required {
		if strings.Count(value, token) != 1 {
			return fmt.Errorf("%q must carry %s exactly once", value, token)
		}
	}
	return rejectUnknownPlaceholders(value, allowed)
}

// rejectUnknownPlaceholders reports an error naming the first
// brace-delimited token in value that is not a member of allowed.
func rejectUnknownPlaceholders(value string, allowed []string) error {
	rest := value
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			return nil
		}
		closeIdx := strings.IndexByte(rest[open:], '}')
		if closeIdx < 0 {
			return fmt.Errorf("placeholder value %q carries an unterminated token at %q", value, rest[open:])
		}
		token := rest[open : open+closeIdx+1]
		if !slices.Contains(allowed, token) {
			return fmt.Errorf("%q carries placeholder %s outside the allowed set %v", value, token, allowed)
		}
		rest = rest[open+closeIdx+1:]
	}
}

// validatePlaceholderArgs applies validatePlaceholders across every
// element of an argument vector, so a single string need not carry
// every required placeholder itself.
func validatePlaceholderArgs(args, required, allowed []string) error {
	joined := strings.Join(args, "\x00")
	for _, token := range required {
		if strings.Count(joined, token) != 1 {
			return fmt.Errorf("%v must carry %s exactly once", args, token)
		}
	}
	for _, arg := range args {
		if err := rejectUnknownPlaceholders(arg, allowed); err != nil {
			return err
		}
	}
	return nil
}

// validateCapabilityGapLabels rejects a label list that is not sorted,
// carries a duplicate, names a label outside capabilityGapLabels, or
// omits capabilityGapLabelTokenCounts.
func validateCapabilityGapLabels(labels []string) error {
	if !slices.IsSorted(labels) {
		return fmt.Errorf("%v must be sorted", labels)
	}
	seen := map[string]bool{}
	for _, label := range labels {
		if !slices.Contains(capabilityGapLabels, label) {
			return fmt.Errorf("%q is outside the closed capability-gap label set", label)
		}
		if seen[label] {
			return fmt.Errorf("%q is duplicated", label)
		}
		seen[label] = true
	}
	if !slices.Contains(labels, capabilityGapLabelTokenCounts) {
		return fmt.Errorf("must contain %q", capabilityGapLabelTokenCounts)
	}
	return nil
}

// decodeEntryPoints strictly decodes the entry_points member.
func decodeEntryPoints(raw json.RawMessage) (map[Surface]EntryPoint, error) {
	var rawEntries map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawEntries); err != nil {
		return nil, err
	}
	entries := make(map[Surface]EntryPoint, len(rawEntries))
	for key, fields := range rawEntries {
		surface := Surface(key)
		if !slices.Contains(Surfaces, surface) {
			return nil, fmt.Errorf("%q is outside qualification.Surfaces", key)
		}
		if surface == SurfaceAggregate {
			return nil, fmt.Errorf("%q carries no entry point: it names a cross-surface observation rather than a launch", key)
		}
		for name := range fields {
			if !entryPointFields[name] {
				return nil, fmt.Errorf("%s: unknown field %q", key, name)
			}
		}
		argsRaw, ok := fields["args"]
		if !ok {
			return nil, fmt.Errorf("%s: missing field \"args\"", key)
		}
		var entry EntryPoint
		if err := json.Unmarshal(argsRaw, &entry.Args); err != nil {
			return nil, fmt.Errorf("%s: args: %w", key, err)
		}
		if len(entry.Args) == 0 {
			return nil, fmt.Errorf("%s: args must be non-empty", key)
		}
		if err := validatePlaceholderArgsAllowed(entry.Args); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		entries[surface] = entry
	}
	return entries, nil
}

// validatePlaceholderArgsAllowed rejects an argument vector carrying a
// brace-delimited token outside entryPointPlaceholders. Every
// placeholder is optional here: a surface's own args need carry only
// the ones its launch actually substitutes.
func validatePlaceholderArgsAllowed(args []string) error {
	for _, arg := range args {
		if err := rejectUnknownPlaceholders(arg, entryPointPlaceholders); err != nil {
			return err
		}
	}
	return nil
}

// structuredNativeSurfaces returns the members of entryPoints that are
// neither the protocol surface nor the cross-surface aggregate: the
// surfaces a recognizer may exist for.
func structuredNativeSurfaces(entryPoints map[Surface]EntryPoint) []Surface {
	var surfaces []Surface
	for surface := range entryPoints {
		if surface == SurfaceProtocol || surface == SurfaceAggregate {
			continue
		}
		surfaces = append(surfaces, surface)
	}
	slices.Sort(surfaces)
	return surfaces
}

// decodeRecognizers strictly decodes the recognizers member, requiring
// its key set to equal the structured native surfaces present in
// entryPoints.
func decodeRecognizers(raw json.RawMessage, entryPoints map[Surface]EntryPoint) (map[Surface]Recognizer, error) {
	var rawEntries map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawEntries); err != nil {
		return nil, err
	}

	want := structuredNativeSurfaces(entryPoints)
	recognizers := make(map[Surface]Recognizer, len(rawEntries))
	for key, fields := range rawEntries {
		surface := Surface(key)
		if surface == SurfaceProtocol {
			return nil, fmt.Errorf("%q must not carry a recognizer: the protocol's stop reason is the terminal", key)
		}
		if !slices.Contains(want, surface) {
			return nil, fmt.Errorf("%q is not a structured native surface present in entry_points", key)
		}
		recognizer, err := decodeRecognizerEntry(fields)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		recognizers[surface] = recognizer
	}
	if len(recognizers) != len(want) {
		return nil, fmt.Errorf("recognizers key set %v does not equal the structured native surfaces %v present in entry_points", slices.Sorted(maps.Keys(recognizers)), want)
	}
	return recognizers, nil
}

// decodeRecognizerEntry strictly decodes one recognizer entry.
func decodeRecognizerEntry(fields map[string]json.RawMessage) (Recognizer, error) {
	for name := range fields {
		if !recognizerFields[name] {
			return Recognizer{}, fmt.Errorf("unknown field %q", name)
		}
	}
	for _, name := range recognizerFieldOrder {
		if _, ok := fields[name]; !ok {
			return Recognizer{}, fmt.Errorf("missing field %q", name)
		}
	}

	var recognizer Recognizer
	locatorFields := map[string]json.RawMessage{}
	if err := json.Unmarshal(fields["locator"], &locatorFields); err != nil {
		return Recognizer{}, fmt.Errorf("locator: %w", err)
	}
	for name := range locatorFields {
		if !terminalLocatorFields[name] {
			return Recognizer{}, fmt.Errorf("locator: unknown field %q", name)
		}
	}
	if err := json.Unmarshal(fields["locator"], &recognizer.Locator); err != nil {
		return Recognizer{}, fmt.Errorf("locator: %w", err)
	}
	if recognizer.Locator.Mode != "first_value" && recognizer.Locator.Mode != "discriminated" {
		return Recognizer{}, fmt.Errorf("locator.mode = %q, want first_value or discriminated", recognizer.Locator.Mode)
	}
	if recognizer.Locator.Mode == "discriminated" && (recognizer.Locator.DiscriminatorKey == "" || recognizer.Locator.DiscriminatorValue == "") {
		return Recognizer{}, errors.New("locator.discriminator_key and locator.discriminator_value are required in discriminated mode")
	}

	if err := json.Unmarshal(fields["error_members"], &recognizer.ErrorMembers); err != nil {
		return Recognizer{}, fmt.Errorf("error_members: %w", err)
	}
	if err := decodeStringOrEmpty(fields["success_member"], &recognizer.SuccessMember); err != nil {
		return Recognizer{}, fmt.Errorf("success_member: %w", err)
	}
	if err := decodeStringOrEmpty(fields["status_member"], &recognizer.StatusMember); err != nil {
		return Recognizer{}, fmt.Errorf("status_member: %w", err)
	}

	var rawStatusCases map[string]Case
	if err := json.Unmarshal(fields["status_cases"], &rawStatusCases); err != nil {
		return Recognizer{}, fmt.Errorf("status_cases: %w", err)
	}
	for status, caseID := range rawStatusCases {
		if !slices.Contains(Cases, caseID) {
			return Recognizer{}, fmt.Errorf("status_cases[%q] = %q is outside qualification.Cases", status, caseID)
		}
	}
	recognizer.StatusCases = rawStatusCases

	if err := json.Unmarshal(fields["status_end_turn"], &recognizer.StatusEndTurn); err != nil {
		return Recognizer{}, fmt.Errorf("status_end_turn: %w", err)
	}
	for _, status := range recognizer.StatusEndTurn {
		if _, overlaps := rawStatusCases[status]; overlaps {
			return Recognizer{}, fmt.Errorf("status_end_turn member %q also appears as a status_cases key", status)
		}
	}

	if err := json.Unmarshal(fields["model_request_path"], &recognizer.ModelRequestPath); err != nil {
		return Recognizer{}, fmt.Errorf("model_request_path: %w", err)
	}

	return recognizer, nil
}

// decodeStringOrEmpty decodes raw as a JSON string. An absent member
// was already rejected by the caller's field-presence check, so this
// only rejects a wrong-typed value; an explicit empty string decodes
// to "", a valid "this member is unused" state for success_member and
// status_member alike.
func decodeStringOrEmpty(raw json.RawMessage, dst *string) error {
	return json.Unmarshal(raw, dst)
}

// decodeDeclarationFields decodes the declarations and absent_surfaces
// members with the rules the declaration document this type absorbed
// used to enforce: capability and case membership, reason membership,
// no duplicate capability-and-case pair, DeclaredGapPeers satisfied in
// both directions, surface membership in DeclarableAbsentSurfaces, and
// no surface declared absent twice.
func decodeDeclarationFields(top map[string]json.RawMessage) ([]DeclaredGap, []AbsentSurface, error) {
	var rawEntries []map[string]json.RawMessage
	if err := json.Unmarshal(top["declarations"], &rawEntries); err != nil {
		return nil, nil, fmt.Errorf("declarations: %w", err)
	}
	var rawAbsent []map[string]json.RawMessage
	if err := json.Unmarshal(top["absent_surfaces"], &rawAbsent); err != nil {
		return nil, nil, fmt.Errorf("absent_surfaces: %w", err)
	}

	var declarations []DeclaredGap
	seen := map[[2]string]bool{}
	reasons := map[[2]string]string{}
	for i, raw := range rawEntries {
		entry, err := decodeDeclaredGapEntry(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("declarations[%d]: %w", i, err)
		}
		key := [2]string{string(entry.Capability), string(entry.Case)}
		if seen[key] {
			return nil, nil, fmt.Errorf("declarations[%d]: duplicate capability %s and case %s", i, entry.Capability, entry.Case)
		}
		seen[key] = true
		reasons[key] = entry.Reason
		declarations = append(declarations, entry)
	}
	for i, entry := range declarations {
		peer, hasPeer := DeclaredGapPeers[entry.Case]
		if !hasPeer {
			continue
		}
		peerKey := [2]string{string(capabilityOwning(peer)), string(peer)}
		peerReason, peerDeclared := reasons[peerKey]
		if !peerDeclared {
			return nil, nil, fmt.Errorf("declarations[%d]: case %s is declared without its peer %s", i, entry.Case, peer)
		}
		if peerReason != entry.Reason {
			return nil, nil, fmt.Errorf("declarations[%d]: case %s and its peer %s carry differing reasons", i, entry.Case, peer)
		}
	}

	var absentSurfaces []AbsentSurface
	seenAbsent := map[Surface]bool{}
	for i, raw := range rawAbsent {
		entry, err := decodeAbsentSurfaceEntry(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("absent_surfaces[%d]: %w", i, err)
		}
		if seenAbsent[entry.Surface] {
			return nil, nil, fmt.Errorf("absent_surfaces[%d]: surface %s is declared absent twice", i, entry.Surface)
		}
		seenAbsent[entry.Surface] = true
		absentSurfaces = append(absentSurfaces, entry)
	}
	return declarations, absentSurfaces, nil
}

// repositoryRootMarker is the file DecodeRuntimeProfile's location
// resolver ascends toward.
const repositoryRootMarker = "go.mod"

// resolveRepositoryRoot ascends from the directory holding path to the
// nearest ancestor carrying repositoryRootMarker.
func resolveRepositoryRoot(path string) (string, error) {
	dir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return "", fmt.Errorf("resolve absolute path for %s: %w", path, err)
	}
	root, ok := ascendToRepositoryRoot(dir)
	if !ok {
		return "", fmt.Errorf("no ancestor of %s carries %s; searched up to %s", path, repositoryRootMarker, dir)
	}
	return root, nil
}

// RepositoryRootFromWD ascends from the current working directory to
// the nearest ancestor carrying go.mod, so a reader resolves the paths
// a profile names independently of where the process was started.
func RepositoryRootFromWD() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root, ok := ascendToRepositoryRoot(dir)
	if !ok {
		return "", fmt.Errorf("no ancestor of %s carries %s", dir, repositoryRootMarker)
	}
	return root, nil
}

// ascendToRepositoryRoot walks dir and its ancestors, reporting the
// first that carries repositoryRootMarker.
func ascendToRepositoryRoot(dir string) (string, bool) {
	for {
		if _, err := os.Stat(filepath.Join(dir, repositoryRootMarker)); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// ReadRuntimeProfileFile reads path, decodes it with DecodeRuntimeProfile,
// and enforces the stage-R rules: runtime_id equals the file's own base
// name without extension, notes_path/measurement_path/published_sample
// each resolve to an existing readable file relative to the repository
// root ascended from path's own location, and published_sample decodes
// as workflow front matter whose agent.kind is agent-client-protocol
// and whose agent.command carries at least one element past element
// zero.
func ReadRuntimeProfileFile(path string) (RuntimeProfile, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the caller supplies an operator-named or repository-tracked path
	if err != nil {
		return RuntimeProfile{}, err
	}
	profile, err := DecodeRuntimeProfile(data)
	if err != nil {
		return RuntimeProfile{}, err
	}

	base := filepath.Base(path)
	wantID := strings.TrimSuffix(base, filepath.Ext(base))
	if profile.RuntimeID != wantID {
		return RuntimeProfile{}, fmt.Errorf("runtime_id %q does not match the file's own base name %q", profile.RuntimeID, wantID)
	}

	root, err := resolveRepositoryRoot(path)
	if err != nil {
		return RuntimeProfile{}, err
	}

	for _, member := range []struct {
		name  string
		value string
	}{
		{"notes_path", profile.NotesPath},
		{"measurement_path", profile.MeasurementPath},
		{"published_sample", profile.PublishedSample},
	} {
		resolved := filepath.Join(root, member.value)
		if info, statErr := os.Stat(resolved); statErr != nil || info.IsDir() {
			return RuntimeProfile{}, fmt.Errorf("%s %q does not resolve to an existing readable file under %s", member.name, member.value, root)
		}
	}

	if err := validatePublishedSample(filepath.Join(root, profile.PublishedSample)); err != nil {
		return RuntimeProfile{}, fmt.Errorf("published_sample %q: %w", profile.PublishedSample, err)
	}

	return profile, nil
}

// workflowFrontMatter is the subset of WORKFLOW.md front matter
// ReadRuntimeProfileFile and ReadPublishedSampleCommand need. It is
// decoded directly through gopkg.in/yaml.v3 rather than through
// internal/workflow, which internal/qualification's leaf property
// forbids importing.
type workflowFrontMatter struct {
	Agent struct {
		Kind    string `yaml:"kind"`
		Command string `yaml:"command"`
	} `yaml:"agent"`
}

// extractFrontMatter returns the YAML front matter body between the
// document's opening and closing "---" delimiters.
func extractFrontMatter(raw []byte) (string, error) {
	content := strings.ReplaceAll(string(raw), "\r\n", "\n")
	rest, found := strings.CutPrefix(content, "---\n")
	if !found {
		return "", errors.New("carries no front matter")
	}
	frontMatter, _, found := strings.Cut(rest, "\n---")
	if !found {
		return "", errors.New("carries no closing front-matter delimiter")
	}
	return frontMatter, nil
}

// validatePublishedSample enforces the published_sample stage-R rule
// on the file at resolved.
func validatePublishedSample(resolved string) error {
	_, err := decodePublishedSample(resolved)
	return err
}

// decodePublishedSample reads and decodes the workflow front matter at
// resolved, enforcing the published_sample stage-R rule.
func decodePublishedSample(resolved string) (workflowFrontMatter, error) {
	raw, err := os.ReadFile(resolved) //nolint:gosec // a repository-relative path resolved from the tracked profile
	if err != nil {
		return workflowFrontMatter{}, err
	}
	frontMatter, err := extractFrontMatter(raw)
	if err != nil {
		return workflowFrontMatter{}, err
	}
	var parsed workflowFrontMatter
	if err := yaml.Unmarshal([]byte(frontMatter), &parsed); err != nil {
		return workflowFrontMatter{}, fmt.Errorf("decode front matter: %w", err)
	}
	if parsed.Agent.Kind != "agent-client-protocol" {
		return workflowFrontMatter{}, fmt.Errorf("agent.kind = %q, want agent-client-protocol", parsed.Agent.Kind)
	}
	if len(strings.Fields(parsed.Agent.Command)) < 2 {
		return workflowFrontMatter{}, errors.New("agent.command carries no element past element zero")
	}
	return parsed, nil
}

// ReadPublishedSampleCommand reads the workflow front matter at path
// and returns its agent.command split into an argument vector.
func ReadPublishedSampleCommand(path string) ([]string, error) {
	parsed, err := decodePublishedSample(path)
	if err != nil {
		return nil, err
	}
	return strings.Fields(parsed.Agent.Command), nil
}

// notesExpectationFields is the exact set of member names a
// Measurement's expectation object may carry.
var measurementFieldOrder = []string{"schema_version", "profile_digest", "measured_at", "expectation"}

var measurementFields = func() map[string]bool {
	fields := make(map[string]bool, len(measurementFieldOrder))
	for _, name := range measurementFieldOrder {
		fields[name] = true
	}
	return fields
}()

// DecodeMeasurement strictly decodes a Measurement document: unknown
// or missing top-level fields are rejected, and schema_version MUST
// equal 1.
func DecodeMeasurement(data []byte) (Measurement, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return Measurement{}, fmt.Errorf("decode measurement: %w", err)
	}
	for name := range top {
		if !measurementFields[name] {
			return Measurement{}, fmt.Errorf("unknown field %q", name)
		}
	}
	for _, name := range measurementFieldOrder {
		if _, ok := top[name]; !ok {
			return Measurement{}, fmt.Errorf("missing field %q", name)
		}
	}

	var measurement Measurement
	if err := json.Unmarshal(top["schema_version"], &measurement.SchemaVersion); err != nil {
		return Measurement{}, fmt.Errorf("schema_version: %w", err)
	}
	if measurement.SchemaVersion != 1 {
		return Measurement{}, fmt.Errorf("schema_version = %d, want 1", measurement.SchemaVersion)
	}
	if err := json.Unmarshal(top["profile_digest"], &measurement.ProfileDigest); err != nil {
		return Measurement{}, fmt.Errorf("profile_digest: %w", err)
	}
	if measurement.ProfileDigest == "" {
		return Measurement{}, errors.New("profile_digest must be non-empty")
	}
	if err := json.Unmarshal(top["measured_at"], &measurement.MeasuredAt); err != nil {
		return Measurement{}, fmt.Errorf("measured_at: %w", err)
	}
	if measurement.MeasuredAt == "" {
		return Measurement{}, errors.New("measured_at must be non-empty")
	}
	if err := json.Unmarshal(top["expectation"], &measurement.Expectation); err != nil {
		return Measurement{}, fmt.Errorf("expectation: %w", err)
	}
	return measurement, nil
}

// ReadMeasurementFile reads and strictly decodes a Measurement file.
func ReadMeasurementFile(path string) (Measurement, error) {
	data, err := os.ReadFile(path) //nolint:gosec // a repository-relative path resolved from the tracked profile
	if err != nil {
		return Measurement{}, err
	}
	return DecodeMeasurement(data)
}

// Declared reports the declared reason for one capability and case,
// performing a linear scan of p.Declarations, a small, bounded list.
func (p RuntimeProfile) Declared(capability Capability, caseID Case) (string, bool) {
	for _, entry := range p.Declarations {
		if entry.Capability == capability && entry.Case == caseID {
			return entry.Reason, true
		}
	}
	return "", false
}

// AbsentSurfaceDeclared reports the declared reason for one surface,
// performing a linear scan of p.AbsentSurfaces, a small, bounded list.
func (p RuntimeProfile) AbsentSurfaceDeclared(surface Surface) (string, bool) {
	for _, entry := range p.AbsentSurfaces {
		if entry.Surface == surface {
			return entry.Reason, true
		}
	}
	return "", false
}

// MeasuredSurfaces returns the surfaces a run launches and grades, in
// the closed vocabulary's own order: the key set of p.EntryPoints minus
// every surface p declares absent. SurfaceAggregate carries no entry
// point and so is never returned.
//
// A declared-absent surface keeps its entry point, because corroborating
// the declaration launches it, but that launch writes no record. Callers
// sizing an expected record set would over-count if it were returned.
func (p RuntimeProfile) MeasuredSurfaces() []Surface {
	var surfaces []Surface
	for _, surface := range Surfaces {
		if _, ok := p.EntryPoints[surface]; !ok {
			continue
		}
		if _, absent := p.AbsentSurfaceDeclared(surface); absent {
			continue
		}
		surfaces = append(surfaces, surface)
	}
	return surfaces
}

// substitutePlaceholders replaces the {model}, {policy}, and {prompt}
// placeholders in args positionally.
func substitutePlaceholders(args []string, model, policy, prompt string) []string {
	replacer := strings.NewReplacer("{model}", model, "{policy}", policy, "{prompt}", prompt)
	out := make([]string, len(args))
	for i, arg := range args {
		out[i] = replacer.Replace(arg)
	}
	return out
}

// EntryArgs substitutes {model}, {policy}, and {prompt} positionally
// into surface's own EntryPoint.Args. The caller appends the result to
// its own resolved command path; EntryArgs carries no host coordinate.
// It returns an error when surface carries no entry point.
func (p RuntimeProfile) EntryArgs(surface Surface, model, policy, prompt string) ([]string, error) {
	entry, ok := p.EntryPoints[surface]
	if !ok {
		return nil, fmt.Errorf("runtime profile %s carries no entry point for surface %s", p.RuntimeID, surface)
	}
	return substitutePlaceholders(entry.Args, model, policy, prompt), nil
}

// PublishedPostureArgs builds the published-posture probe's argv: the
// sample command with element zero replaced by commandPath, and
// p.ModelArgs appended with {model} substituted.
func (p RuntimeProfile) PublishedPostureArgs(sampleCommand []string, commandPath, model string) ([]string, error) {
	if len(sampleCommand) == 0 {
		return nil, fmt.Errorf("published sample %q carries an empty agent.command", p.PublishedSample)
	}
	if len(sampleCommand) == 1 {
		return nil, fmt.Errorf("published sample %q carries no launch posture past element zero", p.PublishedSample)
	}
	argv := slices.Clone(sampleCommand)
	argv[0] = commandPath
	return append(argv, substitutePlaceholders(p.ModelArgs, model, "", "")...), nil
}

// Digest computes a hex SHA-256 over p re-encoded by encoding/json:
// reformatting the tracked file does not move the digest, and a change
// to any member does.
func (p RuntimeProfile) Digest() string {
	encoded, err := json.Marshal(p)
	if err != nil {
		// RuntimeProfile carries only JSON-safe member types; a
		// marshal failure here would be a programming error, not an
		// operator-recoverable condition.
		panic(fmt.Sprintf("digest: marshal runtime profile: %v", err))
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// decodeTopLevelJSONValues decodes output as a sequence of top-level
// JSON values: one per newline-delimited line carrying a JSON object,
// falling back to a streaming decode of the first "{" onward when no
// line decodes on its own. A native surface's output is not
// necessarily one JSON document; this mirrors how the shipped
// recognizers already read it.
func decodeTopLevelJSONValues(output string) []any {
	values := []any{}
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var value any
		if err := json.Unmarshal([]byte(line), &value); err == nil {
			values = append(values, value)
		}
	}
	if len(values) > 0 {
		return values
	}
	if idx := strings.Index(output, "{"); idx >= 0 {
		decoder := json.NewDecoder(strings.NewReader(output[idx:]))
		for {
			var value any
			if err := decoder.Decode(&value); err != nil {
				break
			}
			values = append(values, value)
		}
	}
	return values
}

// locateTerminal applies r.Locator to values, returning the selected
// terminal object and whether one was found.
func (r Recognizer) locateTerminal(values []any) (map[string]any, bool) {
	switch r.Locator.Mode {
	case "first_value":
		if len(values) == 0 {
			return nil, false
		}
		object, ok := values[0].(map[string]any)
		return object, ok
	case "discriminated":
		for _, value := range values {
			object, ok := value.(map[string]any)
			if !ok {
				continue
			}
			if fmt.Sprint(object[r.Locator.DiscriminatorKey]) == r.Locator.DiscriminatorValue {
				return object, true
			}
		}
	}
	return nil, false
}

// Terminal recognizes one native surface's terminal outcome from its
// raw output, following the profile-driven recognition algorithm:
// locate the terminal object by Locator.Mode, check ErrorMembers
// before StatusMember, resolve StatusCases and StatusEndTurn, and fall
// back to SuccessMember.
func (r Recognizer) Terminal(output string) (Terminal, bool) {
	terminal, found := r.locateTerminal(decodeTopLevelJSONValues(output))
	if !found {
		return Terminal{}, false
	}
	for _, member := range r.ErrorMembers {
		if _, has := terminal[member]; has {
			return Terminal{Error: true}, true
		}
	}
	if r.StatusMember != "" {
		status, ok := terminal[r.StatusMember].(string)
		if !ok {
			return Terminal{}, false
		}
		if caseID, ok := r.StatusCases[status]; ok {
			return Terminal{Case: caseID}, true
		}
		if slices.Contains(r.StatusEndTurn, status) {
			return Terminal{EndTurn: true}, true
		}
		return Terminal{}, false
	}
	if r.SuccessMember != "" {
		if _, has := terminal[r.SuccessMember]; has {
			return Terminal{EndTurn: true}, true
		}
	}
	return Terminal{}, false
}

// ModelRequestReading is the three-way state one structured native
// surface's model-request reading resolves to.
type ModelRequestReading int

const (
	// ModelRequestUnreadable reports that the terminal's model-request
	// object is absent or does not decode.
	ModelRequestUnreadable ModelRequestReading = iota
	// ModelRequestNone reports that the object decoded and holds no
	// member.
	ModelRequestNone
	// ModelRequestAtLeastOne reports that the object decoded and holds
	// at least one member.
	ModelRequestAtLeastOne
)

// ModelRequests reads r.ModelRequestPath as a nested key sequence into
// the terminal object located out of output, without reading any
// message text. An unreadable object is reported rather than defaulted
// to ModelRequestNone: reading it as "no model request" would
// manufacture a false positive out of a truncated terminal.
func (r Recognizer) ModelRequests(output string) ModelRequestReading {
	terminal, found := r.locateTerminal(decodeTopLevelJSONValues(output))
	if !found {
		return ModelRequestUnreadable
	}
	var cursor any = terminal
	for _, key := range r.ModelRequestPath {
		object, ok := cursor.(map[string]any)
		if !ok {
			return ModelRequestUnreadable
		}
		cursor, ok = object[key]
		if !ok {
			return ModelRequestUnreadable
		}
	}
	models, ok := cursor.(map[string]any)
	if !ok {
		return ModelRequestUnreadable
	}
	if len(models) == 0 {
		return ModelRequestNone
	}
	return ModelRequestAtLeastOne
}
