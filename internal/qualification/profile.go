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

// The four capability-gap labels a profile's capability_gap_labels member may
// name. They mirror internal/agent/clientprotocol's own unexported labels;
// this package stays a leaf and cannot import that package, so the strings are
// pinned here and cross-checked by the capability-record drift check.
const (
	capabilityGapLabelToolServers         = "tool servers"
	capabilityGapLabelTokenCounts         = "token counts"
	capabilityGapLabelSessionContinuation = "session continuation"
	capabilityGapLabelAgentVersion        = "agent version"
)

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

var declaredGapFields = map[string]bool{
	"capability": true, "case": true, "reason": true,
}

var absentSurfaceFields = map[string]bool{
	"surface": true, "reason": true,
}

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

// EntryPoint is one surface's argument vector for a graded launch, appended to
// the resolved command path. Placeholders "{model}", "{policy}", and "{prompt}"
// are substituted positionally; any other brace-delimited token is rejected at
// decode time.
type EntryPoint struct {
	Args []string `json:"args"`

	// AskingArgs is the same launch under the posture that asks before running
	// a tool. It is stated rather than derived from Args: which element is the
	// posture switch is not recoverable from an argument vector. Empty leaves
	// permission handling unmeasured rather than induced against an unverified
	// launch.
	AskingArgs []string `json:"asking_args,omitempty"`

	// SeedArgs, appended to Args, launches a session naming its own generated
	// identifier, for the native continuation seed. It carries "{session_id}"
	// exactly once and is rejected on SurfaceProtocol. Empty leaves native
	// continuation unmeasured rather than induced against a guessed identifier.
	SeedArgs []string `json:"seed_args,omitempty"`

	// ResumeArgs, appended to Args, launches a fresh process against a seeded
	// session, carrying an optional "{session_id}" substituted with the seed's
	// confirmed identifier so the recall names the seed's session. It is
	// rejected on SurfaceProtocol. SeedArgs without ResumeArgs is rejected;
	// ResumeArgs without SeedArgs requires the recognizer's session_id_path.
	ResumeArgs []string `json:"resume_args,omitempty"`
}

// TerminalLocator selects the terminal object out of a native surface's decoded
// top-level JSON values.
type TerminalLocator struct {
	// Mode is "first_value" or "discriminated".
	Mode               string `json:"mode"`
	DiscriminatorKey   string `json:"discriminator_key"`
	DiscriminatorValue string `json:"discriminator_value"`

	// EnvelopePath descends from the located value to the object carrying the
	// terminal members, for a runtime that wraps its payload below the located
	// value. Empty leaves the located value as the terminal object, so it is
	// omitted from the encoded form and never moves such a profile's digest.
	EnvelopePath []string `json:"envelope_path,omitempty"`
}

// Recognizer maps one structured native surface's output onto a terminal
// outcome. A surface whose output carries no terminal member carries no
// recognizer.
type Recognizer struct {
	Locator       TerminalLocator `json:"locator"`
	ErrorMembers  []string        `json:"error_members"`
	SuccessMember string          `json:"success_member"`
	StatusMember  string          `json:"status_member"`
	StatusCases   map[string]Case `json:"status_cases"`
	StatusEndTurn []string        `json:"status_end_turn"`

	// ModelRequestPath is a nested key sequence into the terminal object, read
	// for the model-request reading. It stays an explicit key sequence so no
	// path-expression grammar enters the schema for a single flat lookup.
	ModelRequestPath []string `json:"model_request_path"`

	// SessionIDPath is a nested key sequence into the terminal object, read to
	// resolve the surface's actual session identifier for native continuation.
	// Empty falls the seed session id back to the run's generated identifier
	// and leaves the recall arm uninducible.
	SessionIDPath []string `json:"session_id_path,omitempty"`

	// TokenPaths names the nested key sequences the token-inventory reading
	// resolves out of the terminal object, each paired with its contract kind.
	TokenPaths []TokenPath `json:"token_paths,omitempty"`
}

// TokenPath is one nested key sequence the token-inventory reading resolves out
// of a native surface's recognized terminal object.
type TokenPath struct {
	// Path is the nested key sequence into the terminal object. A segment may
	// be the wildcard "*" naming the one dynamic key at that level (such as a
	// per-run model name); that level must carry exactly one key. A path with
	// more than one wildcard is rejected.
	Path []string `json:"path"`
	// Kind is "spend", graded usable, or "occupancy", graded
	// corroboration_only.
	Kind string `json:"kind"`
}

// The two TokenPath.Kind values DecodeRuntimeProfile admits.
const (
	tokenPathKindSpend     = "spend"
	tokenPathKindOccupancy = "occupancy"
)

var tokenPathFields = map[string]bool{
	"path": true, "kind": true,
}

// SurfaceNotInducible is one profile-declared claim that a surface's catalog
// cannot induce a case, scoped to that surface alone, unlike the catalog-wide
// CatalogNotInducibleCases.
type SurfaceNotInducible struct {
	Surface Surface `json:"surface"`
	Case    Case    `json:"case"`
	Reason  string  `json:"reason"` // a member of NotInducibleReasons
}

var surfaceNotInducibleFields = map[string]bool{
	"surface": true, "case": true, "reason": true,
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

// RuntimeProfile is the operator's runtime profile document: everything about
// one runtime the live probe needs, and nothing about the host it runs on.
// Declarations and AbsentSurfaces carry the declaration document it supersedes.
type RuntimeProfile struct {
	SchemaVersion   int      `json:"schema_version"` // exactly 4
	RuntimeID       string   `json:"runtime_id"`     // e.g. "gemini-cli"
	IdentityTokens  []string `json:"identity_tokens"`
	NotesPath       string   `json:"notes_path"`
	MeasurementPath string   `json:"measurement_path"`
	PublishedSample string   `json:"published_sample"`

	ToolNameFormat string `json:"tool_name_format"`

	// ToolPolicyFormat names the file format this runtime reads a tool policy
	// in; a harness writing one selects its writer by this format. Empty when
	// the launch asks for no policy file.
	ToolPolicyFormat string `json:"tool_policy_format,omitempty"`

	ProjectConfigPaths  []string `json:"project_config_paths"`
	VersionArgs         []string `json:"version_args"`
	ModelArgs           []string `json:"model_args"`
	CapabilityGapLabels []string `json:"capability_gap_labels"`

	// ConfigRootEnvNames are the environment variable names, in declared order,
	// whose value points the runtime at a configuration root. A native launch
	// forwards these names when the parent environment carries them; values
	// never travel through this member. Empty leaves the launch environment
	// unset.
	ConfigRootEnvNames []string `json:"config_root_env_names,omitempty"`

	// ProbePrompts carries the five fixed prompt templates the live probe
	// substitutes for every induced case: success, runtime_refusal, tool_call,
	// continuation_seed, and continuation_recall.
	ProbePrompts map[string]string `json:"probe_prompts"`

	EntryPoints map[Surface]EntryPoint `json:"entry_points"`
	Recognizers map[Surface]Recognizer `json:"recognizers"`

	Declarations   []DeclaredGap   `json:"declarations"`
	AbsentSurfaces []AbsentSurface `json:"absent_surfaces"`

	// NotInducibleCases lists every (surface, case) pair this profile declares
	// its catalog cannot induce, scoped to that surface alone, unlike
	// CatalogNotInducibleCases which excludes a case on every measured surface.
	NotInducibleCases []SurfaceNotInducible `json:"not_inducible_cases"`
}

// The five ProbePrompts keys DecodeRuntimeProfile requires.
const (
	probePromptSuccess            = "success"
	probePromptRuntimeRefusal     = "runtime_refusal"
	probePromptToolCall           = "tool_call"
	probePromptContinuationSeed   = "continuation_seed"
	probePromptContinuationRecall = "continuation_recall"
)

var probePromptKeys = []string{
	probePromptSuccess, probePromptRuntimeRefusal, probePromptToolCall,
	probePromptContinuationSeed, probePromptContinuationRecall,
}

// Measurement is the tracked artifact one live run produces: the notes
// expectation, the digest of the profile used, the run's UTC measurement date,
// and a link to its provenance. The date is machine-read data rather than notes
// prose because ValidateNotes rejects a notes line carrying a date.
type Measurement struct {
	SchemaVersion int              `json:"schema_version"` // exactly 1
	ProfileDigest string           `json:"profile_digest"`
	MeasuredAt    string           `json:"measured_at"`
	Expectation   NotesExpectation `json:"expectation"`
}

var entryPointPlaceholders = []string{"{model}", "{policy}", "{prompt}"}

// ToolPolicyFormatTOMLRuleList names the tool-policy file format whose document
// is a TOML list of rules, each naming one tool and the decision for a call of
// it.
const ToolPolicyFormatTOMLRuleList = "toml_rule_list"

// ToolPolicyFormats is the closed set a profile's tool_policy_format may name.
// A format is added only alongside a writer that produces it.
var ToolPolicyFormats = []string{ToolPolicyFormatTOMLRuleList}

var runtimeProfileFieldOrder = []string{
	"schema_version", "runtime_id", "identity_tokens",
	"notes_path", "measurement_path", "published_sample",
	"tool_name_format", "project_config_paths", "version_args",
	"model_args", "capability_gap_labels", "probe_prompts",
	"entry_points", "recognizers",
	"declarations", "absent_surfaces", "not_inducible_cases",
}

// runtimeProfileOptionalFields carries the members a document may omit.
var runtimeProfileOptionalFields = []string{"config_root_env_names", "tool_policy_format"}

// runtimeProfileFields is the exact set of member names a runtime
// profile document may carry at the top level.
var runtimeProfileFields = func() map[string]bool {
	fields := make(map[string]bool, len(runtimeProfileFieldOrder)+len(runtimeProfileOptionalFields))
	for _, name := range runtimeProfileFieldOrder {
		fields[name] = true
	}
	for _, name := range runtimeProfileOptionalFields {
		fields[name] = true
	}
	return fields
}()

var entryPointFields = map[string]bool{
	"args": true, "asking_args": true,
	"seed_args": true, "resume_args": true,
}

var terminalLocatorFields = map[string]bool{
	"mode": true, "discriminator_key": true, "discriminator_value": true,
	"envelope_path": true,
}

var recognizerFieldOrder = []string{
	"locator", "error_members", "success_member",
	"status_member", "status_cases", "status_end_turn",
	"model_request_path",
}

// recognizerOptionalFields carries the recognizer members a discovery pass may
// leave unstated.
var recognizerOptionalFields = []string{"session_id_path", "token_paths"}

var recognizerFields = func() map[string]bool {
	fields := make(map[string]bool, len(recognizerFieldOrder)+len(recognizerOptionalFields))
	for _, name := range recognizerFieldOrder {
		fields[name] = true
	}
	for _, name := range recognizerOptionalFields {
		fields[name] = true
	}
	return fields
}()

// DecodeRuntimeProfile strictly decodes a runtime profile document, rejecting
// unknown and missing fields at every level and enforcing every validation
// rule on schema_version, identity_tokens, tool_name_format, model_args,
// capability_gap_labels, entry_points, recognizers, status_cases/status_end_turn,
// and the declarations/absent_surfaces rules.
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
	if profile.SchemaVersion != 4 {
		return RuntimeProfile{}, fmt.Errorf("schema_version = %d, want 4", profile.SchemaVersion)
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

	if toolPolicyFormatRaw, has := top["tool_policy_format"]; has {
		if profile.ToolPolicyFormat, err = decodeString(toolPolicyFormatRaw); err != nil {
			return RuntimeProfile{}, fmt.Errorf("tool_policy_format: %w", err)
		}
		if !slices.Contains(ToolPolicyFormats, profile.ToolPolicyFormat) {
			return RuntimeProfile{}, fmt.Errorf("tool_policy_format %q is outside the closed value set %v", profile.ToolPolicyFormat, ToolPolicyFormats)
		}
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

	if configRootEnvNamesRaw, has := top["config_root_env_names"]; has {
		if err := json.Unmarshal(configRootEnvNamesRaw, &profile.ConfigRootEnvNames); err != nil {
			return RuntimeProfile{}, fmt.Errorf("config_root_env_names: %w", err)
		}
		if err := validateConfigRootEnvNames(profile.ConfigRootEnvNames); err != nil {
			return RuntimeProfile{}, fmt.Errorf("config_root_env_names: %w", err)
		}
	}

	if profile.ProbePrompts, err = decodeProbePrompts(top["probe_prompts"]); err != nil {
		return RuntimeProfile{}, fmt.Errorf("probe_prompts: %w", err)
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
	if err := checkContinuationCoordinates(profile.EntryPoints, profile.Recognizers); err != nil {
		return RuntimeProfile{}, err
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

	// The evidence fixture seeds a baseline row for every measurable surface a
	// profile does not declare absent, while the summary sizes that set out of
	// entry_points. A surface left out of both is counted by one rule and not
	// the other, surfacing far downstream as a baseline-count mismatch.
	for _, surface := range measurableSurfaces {
		if _, ok := profile.EntryPoints[surface]; !ok {
			return RuntimeProfile{}, fmt.Errorf("entry_points is missing %q: every measurable surface needs one, and a surface the runtime does not offer carries an entry point plus an absent_surfaces declaration rather than being left out", surface)
		}
	}

	if err := checkMeasuredEntryPointShape(profile); err != nil {
		return RuntimeProfile{}, err
	}

	if profile.NotInducibleCases, err = decodeNotInducibleCases(top["not_inducible_cases"], profile); err != nil {
		return RuntimeProfile{}, fmt.Errorf("not_inducible_cases: %w", err)
	}

	return profile, nil
}

// decodeProbePrompts strictly decodes the probe_prompts member: exactly the
// five probePromptKeys, each non-empty, each carrying only the placeholder its
// own row allows.
func decodeProbePrompts(raw json.RawMessage) (map[string]string, error) {
	var prompts map[string]string
	if err := json.Unmarshal(raw, &prompts); err != nil {
		return nil, err
	}
	for key := range prompts {
		if !slices.Contains(probePromptKeys, key) {
			return nil, fmt.Errorf("unknown key %q", key)
		}
	}
	for _, key := range probePromptKeys {
		value, ok := prompts[key]
		if !ok {
			return nil, fmt.Errorf("missing key %q", key)
		}
		if value == "" {
			return nil, fmt.Errorf("%q must be non-empty", key)
		}
		switch key {
		case probePromptToolCall:
			if err := validatePlaceholders(value, []string{"{tool}"}, []string{"{tool}"}); err != nil {
				return nil, fmt.Errorf("%q: %w", key, err)
			}
		case probePromptContinuationSeed:
			if err := validatePlaceholders(value, []string{"{nonce}"}, []string{"{nonce}"}); err != nil {
				return nil, fmt.Errorf("%q: %w", key, err)
			}
		default:
			if err := rejectUnknownPlaceholders(value, nil); err != nil {
				return nil, fmt.Errorf("%q: %w", key, err)
			}
		}
	}
	return prompts, nil
}

// checkContinuationCoordinates enforces that a surface stating seed_args or
// resume_args also states a session_id_path to resolve the recall arm from.
func checkContinuationCoordinates(entryPoints map[Surface]EntryPoint, recognizers map[Surface]Recognizer) error {
	for surface, entry := range entryPoints {
		if len(entry.SeedArgs) == 0 && len(entry.ResumeArgs) == 0 {
			continue
		}
		recognizer, ok := recognizers[surface]
		if !ok || len(recognizer.SessionIDPath) == 0 {
			return fmt.Errorf("entry_points[%s]: seed_args and resume_args require a non-empty recognizers[%s].session_id_path", surface, surface)
		}
	}
	return nil
}

// checkMeasuredEntryPointShape enforces the per-surface argument-vector rules
// for measured surfaces: asking_args is required, and args and asking_args
// carry "{prompt}" exactly once on a native surface and never on the protocol
// surface.
func checkMeasuredEntryPointShape(profile RuntimeProfile) error {
	for _, surface := range profile.MeasuredSurfaces() {
		entry := profile.EntryPoints[surface]
		if len(entry.AskingArgs) == 0 {
			return fmt.Errorf("entry_points[%s]: asking_args is required", surface)
		}
		wantPrompt := surface != SurfaceProtocol
		if err := checkPromptPlaceholder(entry.Args, wantPrompt); err != nil {
			return fmt.Errorf("entry_points[%s].args: %w", surface, err)
		}
		if err := checkPromptPlaceholder(entry.AskingArgs, wantPrompt); err != nil {
			return fmt.Errorf("entry_points[%s].asking_args: %w", surface, err)
		}
	}
	return nil
}

// checkPromptPlaceholder requires args to carry "{prompt}" exactly once
// when want is true, and to carry it not at all otherwise.
func checkPromptPlaceholder(args []string, want bool) error {
	count := strings.Count(strings.Join(args, "\x00"), "{prompt}")
	switch {
	case want && count != 1:
		return fmt.Errorf("must carry {prompt} exactly once, found %d", count)
	case !want && count != 0:
		return errors.New("must not carry {prompt}")
	}
	return nil
}

// notInducibleReasonCase pins each not_inducible_cases reason to the one case
// it names, so a profile cannot pair a reason with a case its meaning does not
// describe.
var notInducibleReasonCase = map[string]Case{
	NotInducibleChannelTooSmall:          CaseLimitReached,
	NotInducibleOutputSilentOnFailure:    CaseRuntimeFailure,
	NotInducibleTerminalAtExitOnly:       CaseCancellation,
	NotInducibleTerminalVocabularyClosed: CaseHumanInput,
}

// checkObjectFields rejects a decoded JSON object carrying a member outside
// allowed, or missing one of them. A schema object decoded straight into its
// struct would drop an unknown member silently, leaving an intended setting
// with no effect and no trace in RuntimeProfile.Digest.
func checkObjectFields(fields map[string]json.RawMessage, allowed map[string]bool) error {
	for name := range fields {
		if !allowed[name] {
			return fmt.Errorf("unknown field %q", name)
		}
	}
	for name := range allowed {
		if _, ok := fields[name]; !ok {
			return fmt.Errorf("missing field %q", name)
		}
	}
	return nil
}

// decodeNotInducibleCases strictly decodes the not_inducible_cases member and
// rejects an entry whose reason names a case other than its paired case. The
// cases a profile names are its own statement about one runtime, so no case is
// required of every profile.
func decodeNotInducibleCases(raw json.RawMessage, profile RuntimeProfile) ([]SurfaceNotInducible, error) {
	var rawEntries []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawEntries); err != nil {
		return nil, err
	}
	for i, fields := range rawEntries {
		if err := checkObjectFields(fields, surfaceNotInducibleFields); err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
	}

	var entries []SurfaceNotInducible
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}

	seen := map[[2]string]bool{}
	for i, entry := range entries {
		if !slices.Contains(Surfaces, entry.Surface) || entry.Surface == SurfaceAggregate {
			return nil, fmt.Errorf("[%d]: surface %q is outside the measured surface set", i, entry.Surface)
		}
		if !slices.Contains(Cases, entry.Case) {
			return nil, fmt.Errorf("[%d]: case %q is outside qualification.Cases", i, entry.Case)
		}
		if slices.Contains(CatalogNotInducibleCases, entry.Case) {
			return nil, fmt.Errorf("[%d]: case %q is already catalog-wide not-inducible", i, entry.Case)
		}
		if !slices.Contains(NotInducibleReasons, entry.Reason) {
			return nil, fmt.Errorf("[%d]: reason %q is outside the closed value set", i, entry.Reason)
		}
		if wantCase := notInducibleReasonCase[entry.Reason]; entry.Case != wantCase {
			return nil, fmt.Errorf("[%d]: reason %q pairs only with case %s, got %s", i, entry.Reason, wantCase, entry.Case)
		}
		key := [2]string{string(entry.Surface), string(entry.Case)}
		if seen[key] {
			return nil, fmt.Errorf("[%d]: duplicate (surface, case) pair (%s, %s)", i, entry.Surface, entry.Case)
		}
		seen[key] = true
		if _, declared := profile.Declared(capabilityOwning(entry.Case), entry.Case); declared {
			return nil, fmt.Errorf("[%d]: (%s, %s) is also named by declarations", i, entry.Surface, entry.Case)
		}
	}

	return entries, nil
}

// CaseExclusion reports what one surface's silence on caseID means, keeping
// applicability, induction status, and observed behavior three answers. A
// declaration excludes the case on every surface; a not_inducible_cases entry
// is scoped to one surface and read through NotInducibleExclusion.
func (p RuntimeProfile) CaseExclusion(surface Surface, capability Capability, caseID Case) ExclusionKind {
	if slices.Contains(CatalogNotInducibleCases, caseID) {
		return ExclusionNotInduced
	}
	if _, declared := p.Declared(capability, caseID); declared {
		return ExclusionNotApplicable
	}
	if reason, ok := p.NotInducibleDeclared(surface, caseID); ok {
		return NotInducibleExclusion(reason)
	}
	return ExclusionNone
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

// validateConfigRootEnvNames rejects an entry that is not a bare environment
// variable name, or a name repeated in the list.
func validateConfigRootEnvNames(names []string) error {
	for i, name := range names {
		switch {
		case name == "":
			return fmt.Errorf("entry %d is empty", i)
		case strings.ContainsAny(name, " \t\n\r="):
			return fmt.Errorf("entry %d, %q, is not a bare environment variable name", i, name)
		case slices.Contains(names[:i], name):
			return fmt.Errorf("entry %d, %q, is a duplicate", i, name)
		}
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
		if askingRaw, hasAsking := fields["asking_args"]; hasAsking {
			if err := json.Unmarshal(askingRaw, &entry.AskingArgs); err != nil {
				return nil, fmt.Errorf("%s: asking_args: %w", key, err)
			}
			if len(entry.AskingArgs) == 0 {
				return nil, fmt.Errorf("%s: asking_args must be non-empty when present", key)
			}
			if err := validatePlaceholderArgsAllowed(entry.AskingArgs); err != nil {
				return nil, fmt.Errorf("%s: asking_args: %w", key, err)
			}
		}
		if seedRaw, hasSeed := fields["seed_args"]; hasSeed {
			if surface == SurfaceProtocol {
				return nil, fmt.Errorf("%s: seed_args is rejected on the protocol surface", key)
			}
			if err := json.Unmarshal(seedRaw, &entry.SeedArgs); err != nil {
				return nil, fmt.Errorf("%s: seed_args: %w", key, err)
			}
			if err := validatePlaceholderArgs(entry.SeedArgs, []string{"{session_id}"}, []string{"{session_id}"}); err != nil {
				return nil, fmt.Errorf("%s: seed_args: %w", key, err)
			}
		}
		if resumeRaw, hasResume := fields["resume_args"]; hasResume {
			if surface == SurfaceProtocol {
				return nil, fmt.Errorf("%s: resume_args is rejected on the protocol surface", key)
			}
			if err := json.Unmarshal(resumeRaw, &entry.ResumeArgs); err != nil {
				return nil, fmt.Errorf("%s: resume_args: %w", key, err)
			}
			if len(entry.ResumeArgs) == 0 {
				return nil, fmt.Errorf("%s: resume_args must be non-empty when present", key)
			}
			for _, arg := range entry.ResumeArgs {
				if err := rejectUnknownPlaceholders(arg, []string{"{session_id}"}); err != nil {
					return nil, fmt.Errorf("%s: resume_args carries a placeholder outside {session_id}: %w", key, err)
				}
			}
		}
		if len(entry.SeedArgs) > 0 && len(entry.ResumeArgs) == 0 {
			return nil, fmt.Errorf("%s: seed_args requires resume_args", key)
		}
		entries[surface] = entry
	}
	return entries, nil
}

func validatePlaceholderArgsAllowed(args []string) error {
	for _, arg := range args {
		if err := rejectUnknownPlaceholders(arg, entryPointPlaceholders); err != nil {
			return err
		}
		if arg != "{policy}" && strings.Contains(arg, "{policy}") {
			return fmt.Errorf("%q embeds {policy}: an empty policy drops the token together with the element before it, so {policy} must stand alone as its own argument", arg)
		}
	}
	return nil
}

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

	if sessionIDPathRaw, has := fields["session_id_path"]; has {
		if err := json.Unmarshal(sessionIDPathRaw, &recognizer.SessionIDPath); err != nil {
			return Recognizer{}, fmt.Errorf("session_id_path: %w", err)
		}
		if err := validateKeyPath(recognizer.SessionIDPath); err != nil {
			return Recognizer{}, fmt.Errorf("session_id_path: %w", err)
		}
	}

	if tokenPathsRaw, has := fields["token_paths"]; has {
		var rawPaths []map[string]json.RawMessage
		if err := json.Unmarshal(tokenPathsRaw, &rawPaths); err != nil {
			return Recognizer{}, fmt.Errorf("token_paths: %w", err)
		}
		for i, pathFields := range rawPaths {
			if err := checkObjectFields(pathFields, tokenPathFields); err != nil {
				return Recognizer{}, fmt.Errorf("token_paths[%d]: %w", i, err)
			}
		}
		if err := json.Unmarshal(tokenPathsRaw, &recognizer.TokenPaths); err != nil {
			return Recognizer{}, fmt.Errorf("token_paths: %w", err)
		}
		seen := map[string]bool{}
		for i, path := range recognizer.TokenPaths {
			if err := validateKeyPath(path.Path); err != nil {
				return Recognizer{}, fmt.Errorf("token_paths[%d]: %w", i, err)
			}
			if wildcards := countWildcards(path.Path); wildcards > 1 {
				return Recognizer{}, fmt.Errorf("token_paths[%d]: path %v carries %d wildcard segments, want at most one", i, path.Path, wildcards)
			}
			if path.Kind != tokenPathKindSpend && path.Kind != tokenPathKindOccupancy {
				return Recognizer{}, fmt.Errorf("token_paths[%d]: kind = %q, want %q or %q", i, path.Kind, tokenPathKindSpend, tokenPathKindOccupancy)
			}
			key := strings.Join(path.Path, "\x00")
			if seen[key] {
				return Recognizer{}, fmt.Errorf("token_paths[%d]: path %v is duplicated", i, path.Path)
			}
			seen[key] = true
		}
	}

	return recognizer, nil
}

// validateKeyPath rejects a nested key sequence carrying an empty key. An empty
// sequence is valid: it names no path at all, which the caller's presence rule
// decides.
func validateKeyPath(path []string) error {
	if slices.Contains(path, "") {
		return fmt.Errorf("path %v carries an empty key", path)
	}
	return nil
}

// decodeStringOrEmpty decodes raw as a JSON string. An absent member
// was already rejected by the caller's field-presence check, so this
// only rejects a wrong-typed value; an explicit empty string decodes
// to "", a valid "this member is unused" state for success_member and
// status_member alike.
func decodeStringOrEmpty(raw json.RawMessage, dst *string) error {
	return json.Unmarshal(raw, dst)
}

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

const repositoryRootMarker = "go.mod"

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

// RepositoryRootFromWD ascends from the current working directory to the
// nearest ancestor carrying go.mod, so a reader resolves a profile's paths
// independently of where the process started.
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

// ReadRuntimeProfileFile reads path, decodes it, and enforces the file-level
// rules: runtime_id equals the file's base name without extension;
// notes_path/measurement_path/published_sample each resolve to an existing
// readable file relative to the repository root ascended from path; and
// published_sample decodes as workflow front matter whose agent.kind is
// agent-client-protocol and whose agent.command carries an element past
// element zero.
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

// workflowFrontMatter is the subset of WORKFLOW.md front matter this package
// needs. It is decoded through gopkg.in/yaml.v3 rather than internal/workflow,
// which this package's leaf property forbids importing.
type workflowFrontMatter struct {
	Agent struct {
		Kind    string `yaml:"kind"`
		Command string `yaml:"command"`
	} `yaml:"agent"`
}

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

func validatePublishedSample(resolved string) error {
	_, err := decodePublishedSample(resolved)
	return err
}

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

// DecodeMeasurement strictly decodes a Measurement document: unknown or missing
// top-level fields are rejected, schema_version MUST equal 4, and every
// nullable member is stated as null rather than omitted.
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

// Declared reports the declared reason for one capability and case.
func (p RuntimeProfile) Declared(capability Capability, caseID Case) (string, bool) {
	for _, entry := range p.Declarations {
		if entry.Capability == capability && entry.Case == caseID {
			return entry.Reason, true
		}
	}
	return "", false
}

// NotInducibleDeclared reports the declared reason for one surface and case
// pair.
func (p RuntimeProfile) NotInducibleDeclared(surface Surface, caseID Case) (string, bool) {
	for _, entry := range p.NotInducibleCases {
		if entry.Surface == surface && entry.Case == caseID {
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

// MeasuredSurfaces returns the surfaces a run launches and grades, in the
// closed vocabulary's order: the key set of p.EntryPoints minus every surface p
// declares absent. A declared-absent surface keeps its entry point (launching
// it corroborates the declaration) but writes no record, so returning it would
// over-count a caller sizing an expected record set.
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

// substitutePlaceholders replaces {model}, {policy}, and {prompt} positionally.
//
// An empty policy drops the {policy} token together with the element before it
// rather than substituting the empty string: some launch targets re-split argv
// on whitespace, where a flag with an empty value lets the next token fill the
// gap, so the flag and its value stay either both present or both absent.
func substitutePlaceholders(args []string, model, policy, prompt string) []string {
	replacer := strings.NewReplacer("{model}", model, "{prompt}", prompt)
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "{policy}" {
			if policy == "" {
				if len(out) > 0 {
					out = out[:len(out)-1]
				}
				continue
			}
			out = append(out, policy)
			continue
		}
		out = append(out, replacer.Replace(arg))
	}
	return out
}

// EntryArgs substitutes {model}, {policy}, and {prompt} positionally into
// surface's EntryPoint.Args. The caller appends the result to its resolved
// command path. It returns an error when surface carries no entry point.
func (p RuntimeProfile) EntryArgs(surface Surface, model, policy, prompt string) ([]string, error) {
	entry, ok := p.EntryPoints[surface]
	if !ok {
		return nil, fmt.Errorf("runtime profile %s carries no entry point for surface %s", p.RuntimeID, surface)
	}
	if err := checkSubstitutedPlaceholder(entry.Args, "{model}", model); err != nil {
		return nil, err
	}
	if err := checkSubstitutedPlaceholder(entry.Args, "{prompt}", prompt); err != nil {
		return nil, err
	}
	return substitutePlaceholders(entry.Args, model, policy, prompt), nil
}

// checkSubstitutedPlaceholder returns an error when args carries token and the
// caller substituted it with the empty string: a launch cannot silently drop
// {model} or {prompt} from its argument vector.
func checkSubstitutedPlaceholder(args []string, token, value string) error {
	if value != "" {
		return nil
	}
	if strings.Contains(strings.Join(args, "\x00"), token) {
		return fmt.Errorf("args carry %s but no value was substituted for it", token)
	}
	return nil
}

// AskingArgs substitutes the same placeholders into surface's own
// EntryPoint.AskingArgs. It reports false when the profile states no
// asking posture for that surface, which is the caller's signal to
// record the row unmeasured rather than to launch something else.
func (p RuntimeProfile) AskingArgs(surface Surface, model, policy, prompt string) ([]string, bool) {
	entry, ok := p.EntryPoints[surface]
	if !ok || len(entry.AskingArgs) == 0 {
		return nil, false
	}
	return substitutePlaceholders(entry.AskingArgs, model, policy, prompt), true
}

// PublishedPostureArgs builds the published-posture probe's argv: the sample
// command with element zero replaced by commandPath, and p.ModelArgs appended
// with {model} substituted.
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
// reformatting the tracked file does not move the digest, and a change to any
// member does.
func (p RuntimeProfile) Digest() string {
	encoded, err := json.Marshal(p)
	if err != nil {
		// RuntimeProfile carries only JSON-safe member types, so a marshal
		// failure would be a programming error, not operator-recoverable.
		panic(fmt.Sprintf("digest: marshal runtime profile: %v", err))
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// decodeTopLevelJSONValues decodes output as a sequence of top-level JSON
// values: one per newline-delimited line carrying a JSON object, falling back
// to a streaming decode of the first "{" onward when no line decodes on its
// own. A native surface's output is not necessarily one JSON document.
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

func (r Recognizer) locateTerminal(values []any) (map[string]any, bool) {
	located, ok := r.locateEnvelope(values)
	if !ok {
		return nil, false
	}
	return descend(located, r.Locator.EnvelopePath)
}

// locateEnvelope selects the top-level value the locator's mode picks,
// before any envelope descent.
func (r Recognizer) locateEnvelope(values []any) (map[string]any, bool) {
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

type keyResolver func(object map[string]any, segment string) (string, bool)

func literalKey(_ map[string]any, segment string) (string, bool) {
	return segment, true
}

// tokenPathWildcard is the TokenPath.Path segment naming "the one dynamic key
// at this level" instead of a literal key, for a terminal whose key at that
// level is a per-run value the profile cannot pin.
const tokenPathWildcard = "*"

// resolveTokenKey resolves a TokenPath.Path segment against object: a literal
// segment names its key directly, and the wildcard resolves to object's sole
// key. An object with zero or more than one key fails rather than picking one.
func resolveTokenKey(object map[string]any, segment string) (string, bool) {
	if segment != tokenPathWildcard {
		return segment, true
	}
	if len(object) != 1 {
		return "", false
	}
	for key := range object {
		return key, true
	}
	return "", false
}

func countWildcards(path []string) int {
	count := 0
	for _, segment := range path {
		if segment == tokenPathWildcard {
			count++
		}
	}
	return count
}

func descendWith(object map[string]any, path []string, resolve keyResolver) (map[string]any, bool) {
	for _, segment := range path {
		key, ok := resolve(object, segment)
		if !ok {
			return nil, false
		}
		next, ok := object[key].(map[string]any)
		if !ok {
			return nil, false
		}
		object = next
	}
	return object, true
}

// descend walks path from object, reporting failure at the first key
// that is missing or does not carry a further object.
func descend(object map[string]any, path []string) (map[string]any, bool) {
	return descendWith(object, path, literalKey)
}

func descendValueWith(object map[string]any, path []string, resolve keyResolver) (any, bool) {
	if len(path) == 0 {
		return nil, false
	}
	parent, ok := descendWith(object, path[:len(path)-1], resolve)
	if !ok {
		return nil, false
	}
	key, ok := resolve(parent, path[len(path)-1])
	if !ok {
		return nil, false
	}
	value, exists := parent[key]
	return value, exists
}

func descendValue(object map[string]any, path []string) (any, bool) {
	return descendValueWith(object, path, literalKey)
}

// SessionID resolves r.SessionIDPath from output, reading the recognized
// terminal object first and otherwise the first other top-level value that
// resolves the selector. A streaming surface announces its session in its first
// event and need not repeat it in the terminal, so reading the terminal alone
// would report a named session as unobserved. It reports false when r carries
// no selector or nothing resolves to a string.
func (r Recognizer) SessionID(output string) (string, bool) {
	if len(r.SessionIDPath) == 0 {
		return "", false
	}
	values := decodeTopLevelJSONValues(output)
	if terminal, found := r.locateTerminal(values); found {
		if id, ok := stringAtPath(terminal, r.SessionIDPath); ok {
			return id, true
		}
	}
	for _, value := range values {
		object, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if id, ok := stringAtPath(object, r.SessionIDPath); ok {
			return id, true
		}
	}
	return "", false
}

func stringAtPath(object map[string]any, path []string) (string, bool) {
	value, ok := descendValue(object, path)
	if !ok {
		return "", false
	}
	s, ok := value.(string)
	return s, ok
}

// TokenValue resolves path's nested key sequence from output's recognized
// terminal object, reporting the numeric value found there. A wildcard segment
// resolves against the terminal's dynamic key at that level.
func (r Recognizer) TokenValue(path TokenPath, output string) (float64, bool) {
	terminal, found := r.locateTerminal(decodeTopLevelJSONValues(output))
	if !found {
		return 0, false
	}
	value, ok := descendValueWith(terminal, path.Path, resolveTokenKey)
	if !ok {
		return 0, false
	}
	n, ok := value.(float64)
	return n, ok
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

// RawTerminal resolves the envelope object r.Locator selects out of output,
// which Recognizer.Terminal discards once recognition fails. It lets a caller
// distinguish "no envelope exists" from "an envelope exists but none of its
// members resolved to a known outcome".
func (r Recognizer) RawTerminal(output string) (map[string]any, bool) {
	return r.locateTerminal(decodeTopLevelJSONValues(output))
}

// ModelRequestReading is the three-way state one structured native
// surface's model-request reading resolves to.
type ModelRequestReading int

const (
	// ModelRequestUnreadable reports that the terminal's model-request object is
	// absent or does not decode.
	ModelRequestUnreadable ModelRequestReading = iota
	// ModelRequestNone reports that the object decoded and holds no member.
	ModelRequestNone
	// ModelRequestAtLeastOne reports that the object decoded and holds at least
	// one member.
	ModelRequestAtLeastOne
)

// ModelRequests reads r.ModelRequestPath as a nested key sequence into the
// terminal object located out of output, without reading message text. An
// unreadable object is reported rather than defaulted to ModelRequestNone,
// which would manufacture a false positive out of a truncated terminal. An
// empty path spells a surface whose terminal carries no model-request object
// and reads unreadable for the same reason.
func (r Recognizer) ModelRequests(output string) ModelRequestReading {
	if len(r.ModelRequestPath) == 0 {
		return ModelRequestUnreadable
	}
	terminal, found := r.locateTerminal(decodeTopLevelJSONValues(output))
	if !found {
		return ModelRequestUnreadable
	}
	models, ok := descend(terminal, r.ModelRequestPath)
	if !ok {
		return ModelRequestUnreadable
	}
	if len(models) == 0 {
		return ModelRequestNone
	}
	return ModelRequestAtLeastOne
}
