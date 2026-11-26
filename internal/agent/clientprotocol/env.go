package clientprotocol

import (
	"sort"
	"strconv"

	"github.com/sortie-ai/sortie/internal/agent/sshutil"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

const envKey = "env"

// settingFault is a refusal of one settings key. The same value feeds
// session start and the offline validate verdict, so both carry the same
// check name and the same text. The text names the key and never a value.
type settingFault struct {
	check   string
	message string
}

func (f *settingFault) Error() string { return f.message }

func newTypeFault(check string, fault *typeutil.TypeFault) *settingFault {
	return &settingFault{check: check, message: fault.Error()}
}

// envSetting reads the env map of the settings block as ascending-name
// entries. An absent or null key yields no entries; the first fault in
// name order is returned, so the verdict is stable across runs.
func envSetting(settings map[string]any) ([]sshutil.EnvVar, *settingFault) {
	raw, present := settings[envKey]
	if !present || raw == nil {
		return nil, nil
	}

	table, ok := raw.(map[string]any)
	if !ok {
		return nil, newTypeFault("agent-client-protocol.env.wrong_type",
			&typeutil.TypeFault{Key: envKey, Want: "mapping", Got: typeutil.DescribeYAMLType(raw)})
	}

	names := make([]string, 0, len(table))
	for name := range table {
		names = append(names, name)
	}
	sort.Strings(names)

	entries := make([]sshutil.EnvVar, 0, len(names))
	for _, name := range names {
		if !sshutil.IsEnvName(name) || sshutil.IsReservedEnvName(name) {
			return nil, &settingFault{
				check:   "agent-client-protocol.env.invalid_name",
				message: envKey + ": " + strconv.Quote(name) + " is not a usable environment variable name",
			}
		}
		value, ok := table[name].(string)
		if !ok {
			return nil, newTypeFault("agent-client-protocol.env.wrong_type",
				&typeutil.TypeFault{Key: envKey + "." + name, Want: "string", Got: typeutil.DescribeYAMLType(table[name])})
		}
		entries = append(entries, sshutil.EnvVar{Name: name, Value: value})
	}
	return entries, nil
}
