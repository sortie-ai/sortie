package usagesource

import (
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
)

// ClaimVerification returns the registered addition that claims target, with
// its switches, and closes every other claimant; the caller closes the one
// returned. Each merge in the switches is resolved into an Env entry carrying
// the addition merged into the value the launch gives its variable, so the
// caller applies Env alone.
//
// It returns nothing when none or several claim, when target or environ already
// sets one of the claimant's switches, and when a merge is not in effect on the
// value the launch gives it: replacing an operator's setting could change the
// model, provider, or credential the runtime resolves. environ is read through
// [agentcore.LaunchTarget.RuntimeEnv], so a remote launch sees what it carries.
func ClaimVerification(target agentcore.LaunchTarget, environ []string) (VerificationAddition, VerificationSwitches, bool) {
	var claimants []VerificationAddition
	var switches []VerificationSwitches
	for _, kind := range VerificationAdditions.Kinds() {
		constructor, _ := VerificationAdditions.Get(kind) // a listed kind always resolves
		addition := constructor()
		claimed, ok := addition.Claim(target)
		if !ok {
			addition.Close()
			continue
		}
		claimants = append(claimants, addition)
		switches = append(switches, claimed)
	}

	if len(claimants) == 1 {
		if resolved, ok := resolveMerges(target, environ, switches[0]); ok {
			return claimants[0], resolved, true
		}
	}
	for _, claimant := range claimants {
		claimant.Close()
	}
	return nil, VerificationSwitches{}, false
}

// CommandAssigns reports whether the configured agent command assigns name in
// a word of the form NAME=value, as an env program in front of the runtime
// does. Such an assignment replaces any value the launch environment carries,
// so a merge into that value would not reach the runtime. A word may open with
// a shell quote, which a string-form remote command keeps in its words.
func CommandAssigns(target agentcore.LaunchTarget, name string) bool {
	return slices.ContainsFunc(target.CommandWords(), func(word string) bool {
		return agentcore.IsEnvEntryNamed(strings.TrimLeft(word, `'"`), name)
	})
}

func resolveMerges(target agentcore.LaunchTarget, environ []string, s VerificationSwitches) (VerificationSwitches, bool) {
	if setsSwitch(target, environ, s) {
		return VerificationSwitches{}, false
	}
	env := slices.Clone(s.Env)
	for _, merge := range s.Merges {
		current, set := target.RuntimeEnv(environ, merge.Name)
		value, inEffect := merge.Merge(current, set)
		if !inEffect {
			return VerificationSwitches{}, false
		}
		env = append(env, merge.Name+"="+value)
	}
	return VerificationSwitches{Env: env, Args: s.Args}, true
}

func setsSwitch(target agentcore.LaunchTarget, environ []string, s VerificationSwitches) bool {
	tokens := append([]string{target.Command}, target.Args...)
	for _, arg := range s.Args {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		for _, token := range tokens {
			if token == arg || strings.HasPrefix(token, arg+"=") {
				return true
			}
		}
	}
	for _, entry := range s.Env {
		name, _, _ := strings.Cut(entry, "=")
		if _, set := target.RuntimeEnv(environ, name); set || CommandAssigns(target, name) {
			return true
		}
	}
	return slices.ContainsFunc(s.Merges, func(merge LaunchAddition) bool {
		return CommandAssigns(target, merge.Name)
	})
}
