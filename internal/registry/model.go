package registry

import "github.com/sortie-ai/sortie/internal/typeutil"

// ModelKey is the settings-block key that names the model.
const ModelKey = "model"

// ModelSetting returns the model passthrough carries under [ModelKey], ""
// when absent, null, or empty, and a [*typeutil.TypeFault] for any
// non-string value.
func ModelSetting(passthrough map[string]any) (string, *typeutil.TypeFault) {
	return typeutil.StringField(passthrough, ModelKey)
}
