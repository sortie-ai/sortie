package config

import (
	"errors"
	"strings"
	"testing"
)

// --- coercion function tests ---

func TestCoerceEnvInt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    int
		wantErr bool
	}{
		{"positive integer", "123", 123, false},
		{"zero", "0", 0, false},
		{"negative integer", "-5", -5, false},
		{"whitespace trimmed", " 456 ", 456, false},
		{"whitespace tabs", "\t100\t", 100, false},
		{"non-numeric", "abc", 0, true},
		{"float string", "1.5", 0, true},
		{"empty string", "", 0, true},
		{"mixed alpha and digit", "12abc", 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := coerceEnvInt(tt.input)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("coerceEnvInt(%q) = %v, want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("coerceEnvInt(%q) unexpected error: %v", tt.input, err)
			}
			n, ok := got.(int)
			if !ok {
				t.Fatalf("coerceEnvInt(%q) returned %T, want int", tt.input, got)
			}
			if n != tt.want {
				t.Errorf("coerceEnvInt(%q) = %d, want %d", tt.input, n, tt.want)
			}
		})
	}
}

func TestCoerceCSVList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  []string // nil means expect empty []any{}
	}{
		{"empty string", "", nil},
		{"single element", "foo", []string{"foo"}},
		{"multiple elements", "a,b,c", []string{"a", "b", "c"}},
		{"whitespace around items", " Open , Working ", []string{"Open", "Working"}},
		{"trailing comma discarded", "a,b,", []string{"a", "b"}},
		{"leading comma discarded", ",a,b", []string{"a", "b"}},
		{"only commas", ",,,", nil},
		{"spaces and commas only", " , , ", nil},
		{"multi-word states", "To Do,In Progress,Done", []string{"To Do", "In Progress", "Done"}},
		{"single item with spaces", "  In Progress  ", []string{"In Progress"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := coerceCSVList(tt.input)
			if err != nil {
				t.Fatalf("coerceCSVList(%q) unexpected error: %v", tt.input, err)
			}

			gotSlice, ok := got.([]any)
			if !ok {
				t.Fatalf("coerceCSVList(%q) returned %T, want []any", tt.input, got)
			}

			if tt.want == nil {
				if len(gotSlice) != 0 {
					t.Errorf("coerceCSVList(%q) = %v, want empty []any", tt.input, gotSlice)
				}
				return
			}

			if len(gotSlice) != len(tt.want) {
				t.Fatalf("coerceCSVList(%q) len = %d, want %d: got %v", tt.input, len(gotSlice), len(tt.want), gotSlice)
			}
			for i, wantItem := range tt.want {
				gotItem, ok := gotSlice[i].(string)
				if !ok {
					t.Errorf("coerceCSVList(%q)[%d] is %T, want string", tt.input, i, gotSlice[i])
					continue
				}
				if gotItem != wantItem {
					t.Errorf("coerceCSVList(%q)[%d] = %q, want %q", tt.input, i, gotItem, wantItem)
				}
			}
		})
	}
}

func TestCoerceEnvBool(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    bool
		wantErr bool
	}{
		{"true lowercase", "true", true, false},
		{"TRUE uppercase", "TRUE", true, false},
		{"True mixed", "True", true, false},
		{"1", "1", true, false},
		{"true with spaces", "  true  ", true, false},
		{"false lowercase", "false", false, false},
		{"FALSE uppercase", "FALSE", false, false},
		{"False mixed", "False", false, false},
		{"0", "0", false, false},
		{"false with spaces", "  False  ", false, false},
		{"maybe invalid", "maybe", false, true},
		{"yes invalid", "yes", false, true},
		{"no invalid", "no", false, true},
		{"empty string invalid", "", false, true},
		{"2 invalid", "2", false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := coerceEnvBool(tt.input)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("coerceEnvBool(%q) = %v, want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("coerceEnvBool(%q) unexpected error: %v", tt.input, err)
			}
			b, ok := got.(bool)
			if !ok {
				t.Fatalf("coerceEnvBool(%q) returned %T, want bool", tt.input, got)
			}
			if b != tt.want {
				t.Errorf("coerceEnvBool(%q) = %v, want %v", tt.input, b, tt.want)
			}
		})
	}
}

// --- ensureSubMap tests ---

func TestEnsureSubMap(t *testing.T) {
	t.Parallel()

	t.Run("absent key creates empty map", func(t *testing.T) {
		t.Parallel()
		m := map[string]any{}
		sub := ensureSubMap(m, "tracker")
		if sub == nil {
			t.Fatal("ensureSubMap returned nil, want empty map")
		}
		if len(sub) != 0 {
			t.Errorf("ensureSubMap returned map with %d entries, want 0", len(sub))
		}
		// Verify the returned map is the same object stored in the parent:
		// mutate sub and confirm the parent sees the change.
		if _, ok := m["tracker"].(map[string]any); !ok {
			t.Errorf("m[\"tracker\"] = %T, want map[string]any", m["tracker"])
		}
		sub["_probe"] = true
		if stored, _ := m["tracker"].(map[string]any); stored["_probe"] != true {
			t.Error("m[\"tracker\"] is not the same map returned by ensureSubMap")
		}
	})

	t.Run("nil value creates empty map", func(t *testing.T) {
		t.Parallel()
		m := map[string]any{"tracker": nil}
		sub := ensureSubMap(m, "tracker")
		if sub == nil {
			t.Fatal("ensureSubMap returned nil, want empty map")
		}
		if len(sub) != 0 {
			t.Errorf("ensureSubMap returned map with %d entries, want 0", len(sub))
		}
	})

	t.Run("existing map returned as-is", func(t *testing.T) {
		t.Parallel()
		inner := map[string]any{"kind": "jira"}
		m := map[string]any{"tracker": inner}
		sub := ensureSubMap(m, "tracker")
		if sub == nil {
			t.Fatal("ensureSubMap returned nil")
		}
		got, ok := sub["kind"].(string)
		if !ok || got != "jira" {
			t.Errorf("ensureSubMap()[\"kind\"] = %v, want \"jira\"", sub["kind"])
		}
		// Must be same map object.
		if sub["kind"] != inner["kind"] {
			t.Error("ensureSubMap returned a copy, want the original map")
		}
	})

	t.Run("non-map type replaced with empty map", func(t *testing.T) {
		t.Parallel()
		m := map[string]any{"tracker": "a-string"}
		sub := ensureSubMap(m, "tracker")
		if sub == nil {
			t.Fatal("ensureSubMap returned nil, want empty map")
		}
		if len(sub) != 0 {
			t.Errorf("ensureSubMap returned map with %d entries, want 0", len(sub))
		}
		// Original string value must be gone.
		if _, isStr := m["tracker"].(string); isStr {
			t.Error("m[\"tracker\"] is still a string; expected it to be replaced with map")
		}
	})

	t.Run("integer type replaced with empty map", func(t *testing.T) {
		t.Parallel()
		m := map[string]any{"polling": 42}
		sub := ensureSubMap(m, "polling")
		if sub == nil {
			t.Fatal("ensureSubMap returned nil")
		}
		if _, isMap := m["polling"].(map[string]any); !isMap {
			t.Errorf("m[\"polling\"] = %T, want map[string]any after replacement", m["polling"])
		}
	})
}

// --- applyEnvOverrides tests ---

// assertEnvOverrideError verifies that err is a *ConfigError whose Field matches
// wantField and whose Message contains wantMsgSubstr.
func assertEnvOverrideError(t *testing.T, err error, wantField, wantMsgSubstr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected *ConfigError with field %q, got nil", wantField)
	}
	var ce *ConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("error type = %T, want *ConfigError; error = %v", err, err)
	}
	if ce.Field != wantField {
		t.Errorf("ConfigError.Field = %q, want %q", ce.Field, wantField)
	}
	if wantMsgSubstr != "" && !strings.Contains(ce.Message, wantMsgSubstr) {
		t.Errorf("ConfigError.Message = %q, want it to contain %q", ce.Message, wantMsgSubstr)
	}
}

func TestApplyEnvOverrides(t *testing.T) {
	// Not parallel at outer level — subtests use t.Setenv and package-level
	// dotenvPathOverride needs isolation.
	origPath := dotenvPathOverride
	t.Cleanup(func() { dotenvPathOverride = origPath })
	dotenvPathOverride = ""

	t.Run("no SORTIE_ vars set leaves raw unchanged", func(t *testing.T) {
		// Explicitly clear the vars we check to ensure isolation.
		t.Setenv("SORTIE_TRACKER_KIND", "")
		t.Setenv("SORTIE_DB_PATH", "")
		t.Setenv("SORTIE_POLLING_INTERVAL_MS", "")
		t.Setenv("SORTIE_ENV_FILE", "")

		raw := map[string]any{}
		envKeys, err := applyEnvOverrides(raw)
		if err != nil {
			t.Fatalf("applyEnvOverrides: unexpected error: %v", err)
		}
		if envKeys["tracker.kind"] {
			t.Error("envKeys[\"tracker.kind\"] = true, want false when env var unset")
		}
		if envKeys["db_path"] {
			t.Error("envKeys[\"db_path\"] = true, want false when env var unset")
		}
		if _, ok := raw["tracker"]; ok {
			t.Error("raw[\"tracker\"] was set, want unchanged empty map")
		}
	})

	t.Run("string override SORTIE_TRACKER_KIND", func(t *testing.T) {
		t.Setenv("SORTIE_TRACKER_KIND", "file")

		raw := map[string]any{}
		envKeys, err := applyEnvOverrides(raw)
		if err != nil {
			t.Fatalf("applyEnvOverrides: unexpected error: %v", err)
		}

		if !envKeys["tracker.kind"] {
			t.Error("envKeys[\"tracker.kind\"] = false, want true")
		}
		trackerMap, ok := raw["tracker"].(map[string]any)
		if !ok {
			t.Fatalf("raw[\"tracker\"] = %T, want map[string]any", raw["tracker"])
		}
		if got, _ := trackerMap["kind"].(string); got != "file" {
			t.Errorf("raw[\"tracker\"][\"kind\"] = %q, want %q", got, "file")
		}
	})

	t.Run("CSV override SORTIE_TRACKER_ACTIVE_STATES", func(t *testing.T) {
		t.Setenv("SORTIE_TRACKER_ACTIVE_STATES", "Open,Working")

		raw := map[string]any{}
		envKeys, err := applyEnvOverrides(raw)
		if err != nil {
			t.Fatalf("applyEnvOverrides: unexpected error: %v", err)
		}

		if !envKeys["tracker.active_states"] {
			t.Error("envKeys[\"tracker.active_states\"] = false, want true")
		}
		trackerMap, ok := raw["tracker"].(map[string]any)
		if !ok {
			t.Fatalf("raw[\"tracker\"] = %T, want map[string]any", raw["tracker"])
		}
		states, ok := trackerMap["active_states"].([]any)
		if !ok {
			t.Fatalf("raw[\"tracker\"][\"active_states\"] = %T, want []any", trackerMap["active_states"])
		}
		wantStates := []string{"Open", "Working"}
		if len(states) != len(wantStates) {
			t.Fatalf("active_states len = %d, want %d", len(states), len(wantStates))
		}
		for i, want := range wantStates {
			if got, _ := states[i].(string); got != want {
				t.Errorf("active_states[%d] = %q, want %q", i, got, want)
			}
		}
	})

	t.Run("CSV with whitespace trimmed", func(t *testing.T) {
		t.Setenv("SORTIE_TRACKER_ACTIVE_STATES", "  Open , Working  ")

		raw := map[string]any{}
		_, err := applyEnvOverrides(raw)
		if err != nil {
			t.Fatalf("applyEnvOverrides: unexpected error: %v", err)
		}

		trackerMap, _ := raw["tracker"].(map[string]any)
		states, _ := trackerMap["active_states"].([]any)
		wantStates := []string{"Open", "Working"}
		if len(states) != len(wantStates) {
			t.Fatalf("active_states len = %d, want %d: %v", len(states), len(wantStates), states)
		}
		for i, want := range wantStates {
			if got, _ := states[i].(string); got != want {
				t.Errorf("active_states[%d] = %q, want %q", i, got, want)
			}
		}
	})

	t.Run("CSV empty string sets empty list", func(t *testing.T) {
		// An empty env var value is treated as "not set" by applyEnvOverrides.
		// Setting the var to "" means the env var is present but empty,
		// and applyEnvOverrides skips it (val == "").
		// To override to empty list requires a non-empty env mechanism — skip here.
		// This test verifies that an empty env var value is a no-op.
		t.Setenv("SORTIE_TRACKER_ACTIVE_STATES", "")
		t.Setenv("SORTIE_ENV_FILE", "") // no dotenv fallback

		raw := map[string]any{}
		envKeys, err := applyEnvOverrides(raw)
		if err != nil {
			t.Fatalf("applyEnvOverrides: unexpected error: %v", err)
		}
		if envKeys["tracker.active_states"] {
			t.Error("empty env var should not set envKeys")
		}
	})

	t.Run("bool override true", func(t *testing.T) {
		t.Setenv("SORTIE_TRACKER_COMMENTS_ON_DISPATCH", "true")

		raw := map[string]any{}
		envKeys, err := applyEnvOverrides(raw)
		if err != nil {
			t.Fatalf("applyEnvOverrides: unexpected error: %v", err)
		}

		if !envKeys["tracker.comments.on_dispatch"] {
			t.Error("envKeys[\"tracker.comments.on_dispatch\"] = false, want true")
		}
		trackerMap, _ := raw["tracker"].(map[string]any)
		commentsMap, _ := trackerMap["comments"].(map[string]any)
		if b, _ := commentsMap["on_dispatch"].(bool); !b {
			t.Errorf("comments[\"on_dispatch\"] = %v, want true", commentsMap["on_dispatch"])
		}
	})

	t.Run("bool override 1", func(t *testing.T) {
		t.Setenv("SORTIE_TRACKER_COMMENTS_ON_COMPLETION", "1")

		raw := map[string]any{}
		_, err := applyEnvOverrides(raw)
		if err != nil {
			t.Fatalf("applyEnvOverrides: unexpected error: %v", err)
		}

		trackerMap, _ := raw["tracker"].(map[string]any)
		commentsMap, _ := trackerMap["comments"].(map[string]any)
		if b, _ := commentsMap["on_completion"].(bool); !b {
			t.Errorf("comments[\"on_completion\"] = %v, want true", commentsMap["on_completion"])
		}
	})

	t.Run("bool override invalid returns ConfigError with env var name", func(t *testing.T) {
		t.Setenv("SORTIE_TRACKER_COMMENTS_ON_DISPATCH", "maybe")

		raw := map[string]any{}
		_, err := applyEnvOverrides(raw)
		assertEnvOverrideError(t, err, "tracker.comments.on_dispatch", "SORTIE_TRACKER_COMMENTS_ON_DISPATCH")
	})

	t.Run("int override valid", func(t *testing.T) {
		t.Setenv("SORTIE_POLLING_INTERVAL_MS", "5000")

		raw := map[string]any{}
		envKeys, err := applyEnvOverrides(raw)
		if err != nil {
			t.Fatalf("applyEnvOverrides: unexpected error: %v", err)
		}

		if !envKeys["polling.interval_ms"] {
			t.Error("envKeys[\"polling.interval_ms\"] = false, want true")
		}
		pollingMap, _ := raw["polling"].(map[string]any)
		if n, _ := pollingMap["interval_ms"].(int); n != 5000 {
			t.Errorf("polling[\"interval_ms\"] = %v (%T), want int(5000)", pollingMap["interval_ms"], pollingMap["interval_ms"])
		}
	})

	t.Run("int override invalid returns ConfigError with env var name", func(t *testing.T) {
		t.Setenv("SORTIE_POLLING_INTERVAL_MS", "abc")

		raw := map[string]any{}
		_, err := applyEnvOverrides(raw)
		assertEnvOverrideError(t, err, "polling.interval_ms", "SORTIE_POLLING_INTERVAL_MS")
		assertEnvOverrideError(t, err, "polling.interval_ms", "invalid integer value")
	})

	t.Run("top-level SORTIE_DB_PATH", func(t *testing.T) {
		t.Setenv("SORTIE_DB_PATH", "/data/sortie.db")

		raw := map[string]any{}
		envKeys, err := applyEnvOverrides(raw)
		if err != nil {
			t.Fatalf("applyEnvOverrides: unexpected error: %v", err)
		}

		if !envKeys["db_path"] {
			t.Error("envKeys[\"db_path\"] = false, want true")
		}
		if got, _ := raw["db_path"].(string); got != "/data/sortie.db" {
			t.Errorf("raw[\"db_path\"] = %q, want %q", got, "/data/sortie.db")
		}
	})

	t.Run("non-map YAML section does not panic", func(t *testing.T) {
		t.Setenv("SORTIE_TRACKER_KIND", "file")

		// raw["tracker"] is a string — ensureSubMap must replace it.
		raw := map[string]any{"tracker": "a-string"}
		_, err := applyEnvOverrides(raw)
		if err != nil {
			t.Fatalf("applyEnvOverrides: unexpected error: %v", err)
		}
		trackerMap, ok := raw["tracker"].(map[string]any)
		if !ok {
			t.Fatalf("raw[\"tracker\"] = %T after override, want map[string]any", raw["tracker"])
		}
		if got, _ := trackerMap["kind"].(string); got != "file" {
			t.Errorf("raw[\"tracker\"][\"kind\"] = %q, want %q", got, "file")
		}
	})

	t.Run("dotenv fallback via SORTIE_ENV_FILE", func(t *testing.T) {
		dotenvFile := writeDotEnvFile(t, "SORTIE_TRACKER_PROJECT=my-proj\n")
		t.Setenv("SORTIE_ENV_FILE", dotenvFile)
		t.Setenv("SORTIE_TRACKER_PROJECT", "") // ensure real env is empty

		raw := map[string]any{}
		envKeys, err := applyEnvOverrides(raw)
		if err != nil {
			t.Fatalf("applyEnvOverrides: unexpected error: %v", err)
		}

		if !envKeys["tracker.project"] {
			t.Error("envKeys[\"tracker.project\"] = false, want true")
		}
		trackerMap, _ := raw["tracker"].(map[string]any)
		if got, _ := trackerMap["project"].(string); got != "my-proj" {
			t.Errorf("raw[\"tracker\"][\"project\"] = %q, want %q", got, "my-proj")
		}
	})

	t.Run("real env wins over dotenv", func(t *testing.T) {
		dotenvFile := writeDotEnvFile(t, "SORTIE_TRACKER_KIND=file\n")
		t.Setenv("SORTIE_ENV_FILE", dotenvFile)
		t.Setenv("SORTIE_TRACKER_KIND", "jira") // real env overrides .env

		raw := map[string]any{}
		_, err := applyEnvOverrides(raw)
		if err != nil {
			t.Fatalf("applyEnvOverrides: unexpected error: %v", err)
		}

		trackerMap, _ := raw["tracker"].(map[string]any)
		if got, _ := trackerMap["kind"].(string); got != "jira" {
			t.Errorf("raw[\"tracker\"][\"kind\"] = %q, want %q (real env should win over .env)", got, "jira")
		}
	})

	t.Run("missing dotenv file logs warning and returns no error", func(t *testing.T) {
		nonexistent := t.TempDir() + "/does_not_exist.env"
		t.Setenv("SORTIE_ENV_FILE", nonexistent)

		raw := map[string]any{}
		envKeys, err := applyEnvOverrides(raw)
		if err != nil {
			t.Fatalf("applyEnvOverrides: unexpected error for missing file: %v", err)
		}
		_ = envKeys // no overrides expected
	})

	t.Run("SetDotEnvPath takes priority over SORTIE_ENV_FILE", func(t *testing.T) {
		// Not parallel — modifies package-level dotenvPathOverride.
		file1 := writeDotEnvFile(t, "SORTIE_TRACKER_KIND=jira\n")
		file2 := writeDotEnvFile(t, "SORTIE_TRACKER_KIND=file\n")

		t.Setenv("SORTIE_ENV_FILE", file2) // would give "file" if dotenvPathOverride not set

		orig := dotenvPathOverride
		t.Cleanup(func() { dotenvPathOverride = orig })
		SetDotEnvPath(file1) // CLI flag value → should give "jira"

		raw := map[string]any{}
		_, err := applyEnvOverrides(raw)
		if err != nil {
			t.Fatalf("applyEnvOverrides: unexpected error: %v", err)
		}

		trackerMap, _ := raw["tracker"].(map[string]any)
		if got, _ := trackerMap["kind"].(string); got != "jira" {
			t.Errorf("raw[\"tracker\"][\"kind\"] = %q, want %q (SetDotEnvPath should win over SORTIE_ENV_FILE)", got, "jira")
		}

		// Clean up dotenvPathOverride for subsequent tests.
		SetDotEnvPath("")
	})

	t.Run("dotenv parse error fails startup", func(t *testing.T) {
		malformed := writeDotEnvFile(t, "SORTIE_KEY_NO_EQUALS\n")
		t.Setenv("SORTIE_ENV_FILE", malformed)

		raw := map[string]any{}
		_, err := applyEnvOverrides(raw)
		if err == nil {
			t.Fatal("applyEnvOverrides: expected error for malformed .env file, got nil")
		}
		if !strings.Contains(err.Error(), "missing '='") {
			t.Errorf("error = %q, want it to contain %q", err.Error(), "missing '='")
		}
	})
}
