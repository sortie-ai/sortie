package sshutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"unicode"
)

func TestIsEnvName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"underscore", "_", true},
		{"single letter", "a", true},
		{"letter digit underscore mix", "A1_b", true},
		{"empty", "", false},
		{"leading digit", "1A", false},
		{"hyphen", "A-B", false},
		{"equals sign", "A=B", false},
		{"dollar sign", "$A", false},
		{"embedded space", "A B", false},
		{"non-ASCII letter", "Aé", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := IsEnvName(tt.input); got != tt.want {
				t.Errorf("IsEnvName(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestIsReservedEnvName asserts that the carrier reserves the marker
// its import step tests and nothing else. The preamble's other
// internal name stays carryable: the preamble's leading unset frees it
// before the exports run.
func TestIsReservedEnvName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"completion marker", "_sortie_complete", true},
		{"preamble scratch name", "_sortie_env", false},
		{"ordinary name", "EXAMPLE_TOKEN", false},
		{"empty", "", false},
		{"marker with a suffix", "_sortie_complete_x", false},
		{"marker uppercased", "_SORTIE_COMPLETE", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := IsReservedEnvName(tt.input); got != tt.want {
				t.Errorf("IsReservedEnvName(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// envVarRenderers exercises every rendering path EnvVar must redact
// through: fmt with several verbs, alone and inside a slice, a slog
// text handler, and encoding/json.Marshal.
func envVarRenderers(v EnvVar) map[string]string {
	renderings := map[string]string{
		"%v":       fmt.Sprintf("%v", v),
		"%+v":      fmt.Sprintf("%+v", v),
		"%#v":      fmt.Sprintf("%#v", v),
		"%s":       fmt.Sprintf("%s", v),
		"%q":       fmt.Sprintf("%q", v),
		"slice %v": fmt.Sprintf("%v", []EnvVar{v}),
	}

	var textBuf bytes.Buffer
	textLogger := slog.New(slog.NewTextHandler(&textBuf, nil))
	textLogger.Info("carrying", slog.Any("var", v))
	renderings["slog text"] = textBuf.String()

	jsonBytes, err := json.Marshal(v)
	if err == nil {
		renderings["json.Marshal"] = string(jsonBytes)
	}

	return renderings
}

// TestEnvVar_RedactsValueEverywhere asserts that every rendering of an
// EnvVar carries Name and never Value or Value's shell-quoted form.
func TestEnvVar_RedactsValueEverywhere(t *testing.T) {
	t.Parallel()

	const name = "EXAMPLE_TOKEN"
	const value = "occurs-nowhere-else-in-any-rendering-9f3c"
	v := EnvVar{Name: name, Value: value}

	shellQuoted := shellQuote(value)

	for label, rendering := range envVarRenderers(v) {
		if strings.Contains(rendering, value) {
			t.Errorf("rendering %s = %q, contains the carried value %q", label, rendering, value)
		}
		if strings.Contains(rendering, shellQuoted) {
			t.Errorf("rendering %s = %q, contains the shell-quoted value %q", label, rendering, shellQuoted)
		}
		if !strings.Contains(rendering, name) {
			t.Errorf("rendering %s = %q, want it to contain Name %q", label, rendering, name)
		}
	}
}

// TestEnvVar_JSONHandlerRedactsValue exercises the JSON slog handler
// separately from the text handler above, since the two handlers
// serialize a slog.LogValuer differently enough to be worth pinning on
// their own.
func TestEnvVar_JSONHandlerRedactsValue(t *testing.T) {
	t.Parallel()

	const name = "EXAMPLE_TOKEN"
	const value = "occurs-nowhere-else-json-handler-2b7e"
	v := EnvVar{Name: name, Value: value}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("carrying", slog.Any("var", v))
	output := buf.String()

	if strings.Contains(output, value) {
		t.Errorf("slog JSON handler output = %q, contains the carried value %q", output, value)
	}
	if !strings.Contains(output, name) {
		t.Errorf("slog JSON handler output = %q, want it to contain Name %q", output, name)
	}
}

// redactionSubject names one value that a test drives through
// [renderEverywhere] and the value(s) that must never appear in any
// resulting rendering.
type redactionSubject struct {
	name    string
	value   any
	secrets []string
}

// renderEverywhere renders v alone and as a slice element through the
// %v, %+v, %#v, %s, and %q fmt verbs, through both slog handlers, and
// through encoding/json.Marshal, keyed by a label identifying the
// rendering path. sliceOf wraps v in a single-element slice of v's own
// type, since a %v over []any loses the type-specific Format method a
// slice of the concrete type would use.
func renderEverywhere(v any, sliceOf func(any) any) map[string]string {
	renderings := map[string]string{
		"%v":  fmt.Sprintf("%v", v),
		"%+v": fmt.Sprintf("%+v", v),
		"%#v": fmt.Sprintf("%#v", v),
		"%s":  fmt.Sprintf("%s", v),
		"%q":  fmt.Sprintf("%q", v),
	}

	slice := sliceOf(v)
	renderings["slice %v"] = fmt.Sprintf("%v", slice)
	renderings["slice %+v"] = fmt.Sprintf("%+v", slice)
	renderings["slice %#v"] = fmt.Sprintf("%#v", slice)
	renderings["slice %s"] = fmt.Sprintf("%s", slice)
	renderings["slice %q"] = fmt.Sprintf("%q", slice)

	var textBuf bytes.Buffer
	slog.New(slog.NewTextHandler(&textBuf, nil)).Info("carrying", slog.Any("subject", v))
	renderings["slog text"] = textBuf.String()

	var jsonLogBuf bytes.Buffer
	slog.New(slog.NewJSONHandler(&jsonLogBuf, nil)).Info("carrying", slog.Any("subject", v))
	renderings["slog json"] = jsonLogBuf.String()

	if jsonBytes, err := json.Marshal(v); err == nil {
		renderings["json.Marshal"] = string(jsonBytes)
	}

	return renderings
}

// assertRedacted fails t for any rendering in got that contains one of
// subject's secrets, and reports a rendering set that redacts every
// secret but never mentions any of them as diagnosable on its own.
func assertRedacted(t *testing.T, subject redactionSubject, got map[string]string) {
	t.Helper()

	for label, rendering := range got {
		for _, secret := range subject.secrets {
			if strings.Contains(rendering, secret) {
				t.Errorf("%s: rendering %s = %q, contains a carried secret", subject.name, label, rendering)
			}
		}
	}
}

// TestRedaction_AllCarrierTypesAcrossAllRenderings drives every type
// that carries a secret through the ssh launch path - EnvVar,
// SSHOptions, SSHLaunch, the reader StdinReader returns, and the
// writer PrefixStdin returns - through every fmt verb, both slog
// handlers, and encoding/json.Marshal, alone, as a slice element, and
// as an exported struct field, with a value occurring nowhere else in
// the test binary, and asserts no rendering contains it or its
// shell-quoted form.
func TestRedaction_AllCarrierTypesAcrossAllRenderings(t *testing.T) {
	t.Parallel()

	const secretValue = "v9-matrix-secret-9d2f1a7c"
	quoted := shellQuote(secretValue)

	envVar := EnvVar{Name: "V9_ENV_VAR", Value: secretValue}
	opts := SSHOptions{StrictHostKeyChecking: "accept-new", Env: []EnvVar{envVar}}
	launch := BuildSSHLaunch("host.example", "/workspace", "run --acp", nil, opts)

	reader := launch.StdinReader()
	if reader == nil {
		t.Fatal("StdinReader() = nil, want a reader carrying the preamble")
	}

	var recordedWrite []byte
	rec := &captureWriteCloser{onWrite: func(p []byte) { recordedWrite = append(recordedWrite, p...) }}
	writer := launch.PrefixStdin(rec)
	if _, err := writer.Write([]byte("payload")); err != nil {
		t.Fatalf("PrefixStdin(w).Write: %v", err)
	}
	if !strings.Contains(string(recordedWrite), secretValue) {
		t.Fatal("test fixture error: the wrapped writer never received the secret value")
	}

	subjects := []struct {
		subject   redactionSubject
		sliceOf   func(any) any
		render    func() map[string]string
		wantField bool
	}{
		{
			subject: redactionSubject{name: "EnvVar", value: envVar, secrets: []string{secretValue, quoted}},
			sliceOf: func(v any) any { return []EnvVar{v.(EnvVar)} },
		},
		{
			subject: redactionSubject{name: "SSHOptions", value: opts, secrets: []string{secretValue, quoted}},
			sliceOf: func(v any) any { return []SSHOptions{v.(SSHOptions)} },
		},
		{
			subject: redactionSubject{name: "SSHLaunch", value: launch, secrets: []string{secretValue, quoted}},
			sliceOf: func(v any) any { return []SSHLaunch{v.(SSHLaunch)} },
		},
		{
			subject: redactionSubject{name: "StdinReader() result", value: reader, secrets: []string{secretValue, quoted}},
			sliceOf: func(v any) any { return []io.Reader{v.(io.Reader)} },
		},
		{
			subject: redactionSubject{name: "PrefixStdin() result", value: writer, secrets: []string{secretValue, quoted}},
			sliceOf: func(v any) any { return []io.WriteCloser{v.(io.WriteCloser)} },
		},
	}

	for _, tt := range subjects {
		got := renderEverywhere(tt.subject.value, tt.sliceOf)
		assertRedacted(t, tt.subject, got)
	}

	type wrapper struct {
		Field SSHLaunch
	}
	wrapped := wrapper{Field: launch}
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		var rendering string
		switch verb {
		case "%v":
			rendering = fmt.Sprintf("%v", wrapped)
		case "%+v":
			rendering = fmt.Sprintf("%+v", wrapped)
		case "%#v":
			rendering = fmt.Sprintf("%#v", wrapped)
		}
		if strings.Contains(rendering, secretValue) || strings.Contains(rendering, quoted) {
			t.Errorf("exported struct field %s = %q, contains a carried secret", verb, rendering)
		}
	}
}

// TestRedaction_ReaderAndWriterTypesUnexported asserts that
// StdinReader and PrefixStdin return values of unexported concrete
// types, so no caller outside this package can hold a type whose
// default reflection-based formatting would render the preamble.
func TestRedaction_ReaderAndWriterTypesUnexported(t *testing.T) {
	t.Parallel()

	launch := BuildSSHLaunch("h", "/w", "cmd", nil, SSHOptions{Env: []EnvVar{{Name: "A", Value: "v"}}})

	readerType := reflect.TypeOf(launch.StdinReader())
	if isExportedType(readerType) {
		t.Errorf("StdinReader() concrete type = %v, want an unexported type", readerType)
	}

	writerType := reflect.TypeOf(launch.PrefixStdin(&recordingWriteCloser{}))
	if isExportedType(writerType) {
		t.Errorf("PrefixStdin() concrete type = %v, want an unexported type", writerType)
	}
}

// isExportedType reports whether t names an exported type: a pointer
// or named type whose own name starts with an uppercase letter, or any
// type with no name at all (built-ins, unnamed composites), which
// carries no unexported-name guarantee either way.
func isExportedType(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	name := t.Name()
	if name == "" {
		return false
	}
	return unicode.IsUpper(rune(name[0]))
}

// captureWriteCloser is a minimal io.WriteCloser that hands every
// Write's bytes to onWrite before reporting success.
type captureWriteCloser struct {
	onWrite func(p []byte)
}

func (c *captureWriteCloser) Write(p []byte) (int, error) {
	c.onWrite(p)
	return len(p), nil
}

func (c *captureWriteCloser) Close() error { return nil }
