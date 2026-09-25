package redact

import (
	"encoding"
	"fmt"
	"log/slog"
)

// ReplaceAttr is a [slog.HandlerOptions.ReplaceAttr] hook that masks
// every registered secret value in an attribute's text, leaving the
// built-in "time", "level", and "source" keys untouched. A record
// holding no registered value is returned byte-identical.
func ReplaceAttr(_ []string, a slog.Attr) slog.Attr {
	switch a.Key {
	case slog.TimeKey, slog.LevelKey, slog.SourceKey:
		return a
	}

	text, ok := attrText(a.Value)
	if !ok {
		return a
	}

	masked := Mask(text)
	if masked == text {
		return a
	}
	return slog.Attr{Key: a.Key, Value: slog.StringValue(masked)}
}

// attrText resolves an attribute's value to the text Mask should
// inspect, and reports whether the value carries any.
func attrText(v slog.Value) (string, bool) {
	v = v.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return v.String(), true
	case slog.KindAny:
		return anyText(v.Any())
	default:
		return "", false
	}
}

func anyText(v any) (string, bool) {
	switch t := v.(type) {
	case error:
		return t.Error(), true
	case encoding.TextMarshaler:
		b, err := t.MarshalText()
		if err != nil {
			return "", false
		}
		return string(b), true
	case []byte:
		return string(t), true
	default:
		return fmt.Sprintf("%+v", v), true
	}
}
