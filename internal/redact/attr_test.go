package redact

import (
	"errors"
	"log/slog"
	"testing"
)

func TestReplaceAttr_LeavesBuiltinKeysUntouched(t *testing.T) {
	t.Parallel()

	value := randomSecret(t)
	Add("test.ReplaceAttr builtin", value)

	for _, key := range []string{slog.TimeKey, slog.LevelKey, slog.SourceKey} {
		a := slog.String(key, "carries "+value)
		got := ReplaceAttr(nil, a)
		if got.Value.String() != a.Value.String() {
			t.Errorf("ReplaceAttr(%q) = %v, want unchanged", key, got)
		}
	}
}

func TestReplaceAttr_MasksStringAttribute(t *testing.T) {
	t.Parallel()

	value := randomSecret(t)
	Add("test.ReplaceAttr string", value)

	a := slog.String("message", "token="+value)
	got := ReplaceAttr(nil, a)
	if got.Key != "message" {
		t.Errorf("ReplaceAttr() key = %q, want %q", got.Key, "message")
	}
	if got.Value.String() != "token="+Marker {
		t.Errorf("ReplaceAttr() value = %q, want %q", got.Value.String(), "token="+Marker)
	}
}

func TestReplaceAttr_UnchangedRecordIsByteIdentical(t *testing.T) {
	t.Parallel()

	value := randomSecret(t)
	Add("test.ReplaceAttr unchanged", value)

	a := slog.String("message", "nothing secret here")
	got := ReplaceAttr(nil, a)
	if got.Key != a.Key || !got.Value.Equal(a.Value) {
		t.Errorf("ReplaceAttr(%v) = %v, want the identical attribute for text with no registered value", a, got)
	}
}

func TestReplaceAttr_MasksErrorAttribute(t *testing.T) {
	t.Parallel()

	value := randomSecret(t)
	Add("test.ReplaceAttr error", value)

	a := slog.Any("error", errors.New("failed with credential "+value))
	got := ReplaceAttr(nil, a)
	if got.Value.Kind() != slog.KindString {
		t.Fatalf("ReplaceAttr() value kind = %v, want KindString", got.Value.Kind())
	}
	if s := got.Value.String(); s == a.Value.String() {
		t.Errorf("ReplaceAttr(%v) = %q, want the credential masked", a, s)
	}
}

func TestReplaceAttr_MasksTextMarshaler(t *testing.T) {
	t.Parallel()

	value := randomSecret(t)
	Add("test.ReplaceAttr textmarshaler", value)

	a := slog.Any("endpoint", marshalerValue("carries "+value))
	got := ReplaceAttr(nil, a)
	if s := got.Value.String(); s != "carries "+Marker {
		t.Errorf("ReplaceAttr(%v) = %q, want %q", a, s, "carries "+Marker)
	}
}

func TestReplaceAttr_MasksByteSlice(t *testing.T) {
	t.Parallel()

	value := randomSecret(t)
	Add("test.ReplaceAttr byteslice", value)

	a := slog.Any("payload", []byte("blob "+value))
	got := ReplaceAttr(nil, a)
	if s := got.Value.String(); s != "blob "+Marker {
		t.Errorf("ReplaceAttr(%v) = %q, want %q", a, s, "blob "+Marker)
	}
}

func TestReplaceAttr_MasksFallbackFormattedValue(t *testing.T) {
	t.Parallel()

	value := randomSecret(t)
	Add("test.ReplaceAttr fallback", value)

	a := slog.Any("detail", struct{ Token string }{Token: value})
	got := ReplaceAttr(nil, a)
	if s := got.Value.String(); s == a.Value.String() {
		t.Errorf("ReplaceAttr(%v) = %q, want the token masked via the %%+v fallback", a, s)
	}
}

func TestReplaceAttr_OtherKindsCarryNoText(t *testing.T) {
	t.Parallel()

	a := slog.Bool("flag", true)
	got := ReplaceAttr(nil, a)
	if got.Key != a.Key || !got.Value.Equal(a.Value) {
		t.Errorf("ReplaceAttr(%v) = %v, want unchanged for a bool attribute", a, got)
	}

	a = slog.Int("count", 7)
	got = ReplaceAttr(nil, a)
	if got.Key != a.Key || !got.Value.Equal(a.Value) {
		t.Errorf("ReplaceAttr(%v) = %v, want unchanged for an int attribute", a, got)
	}
}

type marshalerValue string

func (m marshalerValue) MarshalText() ([]byte, error) {
	return []byte(m), nil
}
