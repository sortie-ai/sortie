package jira

import (
	"strings"
	"testing"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)

// hasCheck reports whether diags carries a diagnostic with the given check name.
func hasCheck(diags []registry.ValidationDiag, check string) bool {
	for _, d := range diags {
		if d.Check == check {
			return true
		}
	}
	return false
}

// configFromFields builds the config map [NewJiraAdapter] accepts from the
// same fields validateConfig reads, so a case can assert the offline
// verdict and the construction verdict agree on one input.
func configFromFields(fields registry.TrackerConfigFields) map[string]any {
	config := map[string]any{
		"endpoint": fields.Endpoint,
		"api_key":  fields.APIKey,
		"project":  fields.Project,
	}
	if fields.ActiveStates != nil {
		config["active_states"] = fields.ActiveStates
	}
	return config
}

// TestValidateConfig_ErrorRows covers the five error-severity rows of the
// R31a table, one case per row (plus a port variant of the Cloud-format
// row proving the check compares against url.URL.Hostname rather than
// Host). Each case also asserts NewJiraAdapter rejects the equivalent
// config, so the offline verdict cannot diverge from the construction
// verdict.
func TestValidateConfig_ErrorRows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		fields            registry.TrackerConfigFields
		wantCheck         string
		wantConstructKind domain.TrackerErrorKind
	}{
		{
			name:              "empty endpoint",
			fields:            registry.TrackerConfigFields{Endpoint: "", APIKey: "user@test.com:tok", Project: "PROJ"},
			wantCheck:         "tracker.endpoint.missing",
			wantConstructKind: domain.ErrTrackerPayload,
		},
		{
			name:              "endpoint does not parse as a URL with a scheme and host",
			fields:            registry.TrackerConfigFields{Endpoint: "not-a-url", APIKey: "user@test.com:tok", Project: "PROJ"},
			wantCheck:         "tracker.endpoint.invalid",
			wantConstructKind: domain.ErrTrackerPayload,
		},
		{
			name:              "trailing-slash-trimmed endpoint still contains rest/api",
			fields:            registry.TrackerConfigFields{Endpoint: "https://x.atlassian.net/rest/api/3", APIKey: "user@test.com:tok", Project: "PROJ"},
			wantCheck:         "tracker.endpoint.api_suffix",
			wantConstructKind: domain.ErrTrackerPayload,
		},
		{
			name:              "api_key colon at first character",
			fields:            registry.TrackerConfigFields{Endpoint: "https://x.atlassian.net", APIKey: ":token", Project: "PROJ"},
			wantCheck:         "tracker.api_key.jira_format",
			wantConstructKind: domain.ErrTrackerAuth,
		},
		{
			name:              "api_key colon at last character",
			fields:            registry.TrackerConfigFields{Endpoint: "https://x.atlassian.net", APIKey: "user@test.com:", Project: "PROJ"},
			wantCheck:         "tracker.api_key.jira_format",
			wantConstructKind: domain.ErrTrackerAuth,
		},
		{
			name:              "colon-free api_key against a Jira Cloud host",
			fields:            registry.TrackerConfigFields{Endpoint: "https://x.atlassian.net", APIKey: "notoken", Project: "PROJ"},
			wantCheck:         "tracker.api_key.jira_cloud_format",
			wantConstructKind: domain.ErrTrackerAuth,
		},
		{
			name:              "colon-free api_key against a Jira Cloud host carrying a port",
			fields:            registry.TrackerConfigFields{Endpoint: "https://x.atlassian.net:8443", APIKey: "notoken", Project: "PROJ"},
			wantCheck:         "tracker.api_key.jira_cloud_format",
			wantConstructKind: domain.ErrTrackerAuth,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := validateConfig(tt.fields)
			if len(got) != 1 {
				t.Fatalf("validateConfig(%+v) = %d diags, want 1; diags: %v", tt.fields, len(got), got)
			}
			if got[0].Check != tt.wantCheck {
				t.Errorf("validateConfig(%+v) diag.Check = %q, want %q", tt.fields, got[0].Check, tt.wantCheck)
			}
			if got[0].Severity != string(registry.SeverityError) {
				t.Errorf("validateConfig(%+v) diag.Severity = %q, want %q", tt.fields, got[0].Severity, registry.SeverityError)
			}

			_, err := NewJiraAdapter(configFromFields(tt.fields))
			if err == nil {
				t.Fatalf("NewJiraAdapter(%+v) = nil error, want the offline verdict's construction-time counterpart", tt.fields)
			}
			assertTrackerErrorKind(t, err, tt.wantConstructKind)
		})
	}
}

// TestValidateConfig_StateListWarnings covers the sixth R31a row: an
// empty or untrimmed tracker.active_states or tracker.terminal_states
// element is a warning, because the fault only costs matches at query
// time rather than blocking Jira construction.
func TestValidateConfig_StateListWarnings(t *testing.T) {
	t.Parallel()

	fields := registry.TrackerConfigFields{
		Endpoint:       "https://x.atlassian.net",
		APIKey:         "user@test.com:tok",
		Project:        "PROJ",
		ActiveStates:   []string{" review", ""},
		TerminalStates: []string{"done "},
	}

	got := validateConfig(fields)

	for _, wantCheck := range []string{
		"tracker.active_states.untrimmed_element",
		"tracker.active_states.empty_element",
		"tracker.terminal_states.untrimmed_element",
	} {
		if !hasCheck(got, wantCheck) {
			t.Errorf("validateConfig(%+v) missing check %q; diags: %v", fields, wantCheck, got)
		}
	}
	for i, d := range got {
		if d.Severity != string(registry.SeverityWarning) {
			t.Errorf("validateConfig(%+v) diag[%d].Severity = %q, want %q (a state-list fault does not abort Jira construction)", fields, i, d.Severity, registry.SeverityWarning)
		}
	}

	if _, err := NewJiraAdapter(configFromFields(fields)); err != nil {
		t.Errorf("NewJiraAdapter(state-list faults) = %v, want nil (construction proceeds despite the warning)", err)
	}
}

// TestValidateConfig_StatesOverlapWarning covers the shared
// registry.DiagStateOverlap wiring: a label configured on both
// tracker.active_states and tracker.terminal_states is a warning, not an
// error, because Jira construction does not reject it.
func TestValidateConfig_StatesOverlapWarning(t *testing.T) {
	t.Parallel()

	fields := registry.TrackerConfigFields{
		Endpoint:       "https://x.atlassian.net",
		APIKey:         "user@test.com:tok",
		Project:        "PROJ",
		ActiveStates:   []string{"backlog", "done"},
		TerminalStates: []string{"done"},
	}

	got := validateConfig(fields)

	if !hasCheck(got, "tracker.states.overlap") {
		t.Errorf("validateConfig(%+v) missing check %q; diags: %v", fields, "tracker.states.overlap", got)
	}
	for i, d := range got {
		if d.Severity != string(registry.SeverityWarning) {
			t.Errorf("validateConfig(%+v) diag[%d].Severity = %q, want %q", fields, i, d.Severity, registry.SeverityWarning)
		}
	}
}

// TestValidateConfig_CleanConfigProducesNoDiagnostics covers the clean-
// config case: a config that constructs successfully today acquires no
// diagnostic from the offline hook.
func TestValidateConfig_CleanConfigProducesNoDiagnostics(t *testing.T) {
	t.Parallel()

	fields := registry.TrackerConfigFields{
		Endpoint: "https://x.atlassian.net",
		APIKey:   "user@test.com:tok",
		Project:  "PROJ",
	}

	got := validateConfig(fields)
	if len(got) != 0 {
		t.Errorf("validateConfig(clean config) = %v, want empty", got)
	}
}

// TestValidateConfig_TrailingSlashRestApiEndpointIsAccepted is the first
// of the two normalization cases: an endpoint that loses its trailing
// slash before the "/rest/api/" check runs no longer contains that
// substring, so both the hook and NewJiraAdapter accept it.
func TestValidateConfig_TrailingSlashRestApiEndpointIsAccepted(t *testing.T) {
	t.Parallel()

	fields := registry.TrackerConfigFields{
		Endpoint: "https://jira.example.com/rest/api/",
		APIKey:   "user@test.com:tok",
		Project:  "PROJ",
	}

	got := validateConfig(fields)
	if len(got) != 0 {
		t.Errorf("validateConfig(trailing-slash rest/api endpoint) = %v, want empty (matches NewJiraAdapter's trim order)", got)
	}

	if _, err := NewJiraAdapter(configFromFields(fields)); err != nil {
		t.Errorf("NewJiraAdapter(trailing-slash rest/api endpoint) = %v, want nil", err)
	}
}

// TestValidateConfig_EmptyAPIKeyAndProjectProduceNoDiagnostic proves the
// hook defers to the required-field checks that already run earlier in
// the preflight pipeline.
func TestValidateConfig_EmptyAPIKeyAndProjectProduceNoDiagnostic(t *testing.T) {
	t.Parallel()

	fields := registry.TrackerConfigFields{
		Endpoint: "https://x.atlassian.net",
	}

	got := validateConfig(fields)
	if len(got) != 0 {
		t.Errorf("validateConfig(empty api_key and project) = %v, want empty", got)
	}
}

// TestValidateConfig_QueryFilterAnyShapeProducesNoDiagnostic proves the
// hook never checks tracker.query_filter: Jira stores the value verbatim
// and parses no grammar at construction, so an offline grammar check
// would make sortie validate stricter than the run.
func TestValidateConfig_QueryFilterAnyShapeProducesNoDiagnostic(t *testing.T) {
	t.Parallel()

	fields := registry.TrackerConfigFields{
		Endpoint:    "https://x.atlassian.net",
		APIKey:      "user@test.com:tok",
		Project:     "PROJ",
		QueryFilter: `status IN ("unterminated`,
	}

	got := validateConfig(fields)
	if len(got) != 0 {
		t.Errorf("validateConfig(malformed query_filter) = %v, want empty", got)
	}
}

// TestValidateConfig_NeverPlacesAPIKeyValueInMessage pins R7: neither
// api_key-shape check may echo the api_key value in its diagnostic
// message.
func TestValidateConfig_NeverPlacesAPIKeyValueInMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		fields registry.TrackerConfigFields
	}{
		{
			name:   "jira_format diagnostic",
			fields: registry.TrackerConfigFields{Endpoint: "https://x.atlassian.net", APIKey: ":super-secret-token-1", Project: "PROJ"},
		},
		{
			name:   "jira_cloud_format diagnostic",
			fields: registry.TrackerConfigFields{Endpoint: "https://x.atlassian.net", APIKey: "super-secret-token-2", Project: "PROJ"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := validateConfig(tt.fields)
			for _, d := range got {
				if strings.Contains(d.Message, tt.fields.APIKey) {
					t.Errorf("validateConfig(...) diag.Message = %q, must not contain the api_key value %q", d.Message, tt.fields.APIKey)
				}
			}
		})
	}
}

// TestValidateTrackerConfigMeta mirrors the meta assertion
// internal/registry/adapter_meta_test.go makes for the sibling adapters
// that already register a validation hook.
func TestValidateTrackerConfigMeta(t *testing.T) {
	t.Parallel()

	meta, ok := registry.Trackers.Meta("jira")
	if !ok {
		t.Fatal(`Trackers.Meta("jira") reported not registered`)
	}
	if meta.ValidateTrackerConfig == nil {
		t.Error(`Trackers.Meta("jira").ValidateTrackerConfig = nil, want non-nil`)
	}
}
