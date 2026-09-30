package registry

import "github.com/sortie-ai/sortie/internal/typeutil"

// EffortKey is the settings-block key that carries a reasoning level to an
// agent runtime.
const EffortKey = "effort"

// EffortForwarding declares what an agent adapter does with the [EffortKey]
// key of its own settings block, not what its runtime could accept. The
// empty value means undeclared.
type EffortForwarding string

const (
	// EffortForwardingUndeclared is the zero value: the adapter has not
	// declared what it does with the key.
	EffortForwardingUndeclared EffortForwarding = ""

	// EffortForwarded declares that the adapter reads the key through
	// [EffortSetting] and delivers a non-empty value to its runtime on
	// every turn of every session it starts.
	EffortForwarded EffortForwarding = "forwarded"

	// EffortNotForwarded declares that the adapter reads no such key and
	// nothing carries a value to its runtime.
	EffortNotForwarded EffortForwarding = "not_forwarded"
)

// EffortSetting returns the level passthrough carries under [EffortKey]:
// "" when the key is absent, null, or the empty string; the string exactly
// as written otherwise; a [*typeutil.TypeFault] keyed by [EffortKey] for any
// other YAML type. It never mutates passthrough.
func EffortSetting(passthrough map[string]any) (string, *typeutil.TypeFault) {
	return typeutil.StringField(passthrough, EffortKey)
}
