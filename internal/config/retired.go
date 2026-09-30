package config

import (
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/maputil"
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/typeutil"
)

// RetiredAgentLookup reports the retirement declaration for an agent
// kind, and false for a kind that is not retired.
type RetiredAgentLookup func(kind string) (registry.RetiredAgent, bool)

type serviceConfigOptions struct {
	retiredAgents RetiredAgentLookup
}

// ServiceConfigOption tunes how [NewServiceConfig] builds a
// configuration.
type ServiceConfigOption func(*serviceConfigOptions)

// WithRetiredAgents makes [NewServiceConfig] convert every reference to
// a kind lookup reports as retired onto its replacement kind. A nil
// lookup converts nothing.
func WithRetiredAgents(lookup RetiredAgentLookup) ServiceConfigOption {
	return func(o *serviceConfigOptions) {
		o.retiredAgents = lookup
	}
}

// AgentKindConversion records the conversion of one retired agent kind
// onto its replacement kind.
type AgentKindConversion struct {
	// Kind is the retired kind the configuration named.
	Kind string

	// Replacement is the kind the configuration was converted onto.
	Replacement string

	// Fields lists each reference that named Kind, as a dotted path:
	// agent.kind, dispatch.default.agent, then dispatch.rules[i].agent
	// by ascending i.
	Fields []string

	// Command and RemoteCommand are what the replacement kind's
	// sessions launch locally and over SSH. Both are zero when the
	// replacement kind is already the default agent kind and the
	// conversion governs none of its sessions.
	Command       domain.AgentCommand
	RemoteCommand domain.AgentCommand

	// CredentialEnv lists the credential variable names the retired
	// kind carried, which a remote launch of the replacement kind
	// carries in its place. Nil when the conversion governs no
	// sessions.
	CredentialEnv []string
}

func (c AgentKindConversion) clone() AgentKindConversion {
	c.Fields = slices.Clone(c.Fields)
	c.Command.Argv = slices.Clone(c.Command.Argv)
	c.RemoteCommand.Argv = slices.Clone(c.RemoteCommand.Argv)
	c.CredentialEnv = slices.Clone(c.CredentialEnv)
	return c
}

func (c AgentKindConversion) governs() bool {
	return !c.Command.IsZero() || !c.RemoteCommand.IsZero()
}

// AgentKindConversions returns the conversion records in the order the
// configuration named the retired kinds, newly allocated with their
// slices. A configuration that converted nothing returns an empty,
// non-nil slice.
func (c ServiceConfig) AgentKindConversions() []AgentKindConversion {
	out := make([]AgentKindConversion, len(c.conversions))
	for i, conversion := range c.conversions {
		out[i] = conversion.clone()
	}
	return out
}

// AgentCommand returns the command a session of kind launches in the
// given launch mode: the non-zero command of the conversion record
// whose Replacement is kind; otherwise agent.command in its form when
// kind is the default agent kind (dispatch.default.agent, else
// agent.kind); otherwise zero, so the kind launches its own default
// command. A returned Argv is newly allocated.
func (c ServiceConfig) AgentCommand(kind string, remote bool) domain.AgentCommand {
	for _, conversion := range c.conversions {
		if conversion.Replacement != kind {
			continue
		}
		command := conversion.Command
		if remote {
			command = conversion.RemoteCommand
		}
		if !command.IsZero() {
			return domain.AgentCommand{Line: command.Line, Argv: slices.Clone(command.Argv)}
		}
	}
	if kind != c.defaultAgentKind() {
		return domain.AgentCommand{}
	}
	return domain.AgentCommand{Line: c.Agent.Command, Argv: slices.Clone(c.Agent.CommandArgv)}
}

// defaultAgentKind is the kind every selection without an agent of its
// own runs.
func (c ServiceConfig) defaultAgentKind() string {
	if c.Dispatch.Default.AgentKind != "" {
		return c.Dispatch.Default.AgentKind
	}
	return c.Agent.Kind
}

// WorkerSSHHosts returns the non-empty string elements of the
// ssh_hosts list of the worker extension section, in order and not
// deduplicated, and nil when the section holds no such list.
func WorkerSSHHosts(worker map[string]any) []string {
	list, _ := worker["ssh_hosts"].([]any)
	var hosts []string
	for _, elem := range list {
		if host, ok := elem.(string); ok && host != "" {
			hosts = append(hosts, host)
		}
	}
	return hosts
}

// agentKindReference is one place the raw configuration names an agent
// kind, with the means to point it at another kind.
type agentKindReference struct {
	path   string
	kind   string
	assign func(kind string)
}

// collectAgentKindReferences lists agent.kind, dispatch.default.agent,
// and each dispatch.rules[i].agent whose raw value is a non-empty
// string, in that order. Other shapes stay for the section builders to
// report.
func collectAgentKindReferences(raw map[string]any) []agentKindReference {
	var refs []agentKindReference
	add := func(path string, holder map[string]any, key string) {
		if kind, ok := holder[key].(string); ok && kind != "" {
			refs = append(refs, agentKindReference{path: path, kind: kind, assign: func(k string) { holder[key] = k }})
		}
	}

	add("agent.kind", extractSubMap(raw, "agent"), "kind")
	dispatch := extractSubMap(raw, "dispatch")
	add("dispatch.default.agent", extractSubMap(dispatch, "default"), "agent")
	rules, _ := dispatch["rules"].([]any)
	for i, elem := range rules {
		if rule, ok := elem.(map[string]any); ok {
			add(fmt.Sprintf("dispatch.rules[%d].agent", i), rule, "agent")
		}
	}
	return refs
}

// retiredGroup gathers the references that named one retired kind and
// what converting it produced.
type retiredGroup struct {
	kind     string
	decl     registry.RetiredAgent
	refs     []agentKindReference
	command  domain.AgentCommand
	settings map[string]any
	local    registry.AgentConversion
	remote   registry.AgentConversion
	record   AgentKindConversion

	ruleBlocks []ruleBlockConversion
}

// ruleBlockConversion is one dispatch rule's settings block for a
// retired kind and its conversion.
type ruleBlockConversion struct {
	index    int
	rule     map[string]any
	settings map[string]any
	local    registry.AgentConversion
	remote   registry.AgentConversion
}

func (c ruleBlockConversion) conversionFor(remote bool) registry.AgentConversion {
	if remote {
		return c.remote
	}
	return c.local
}

func (g *retiredGroup) fields() []string {
	fields := make([]string, len(g.refs))
	for i, ref := range g.refs {
		fields[i] = ref.path
	}
	return fields
}

func (g *retiredGroup) conversionFor(remote bool) registry.AgentConversion {
	if remote {
		return g.remote
	}
	return g.local
}

func groupRetiredReferences(refs []agentKindReference, lookup RetiredAgentLookup) []*retiredGroup {
	var groups []*retiredGroup
	byKind := make(map[string]*retiredGroup)
	declined := make(map[string]bool)
	for _, ref := range refs {
		if group, ok := byKind[ref.kind]; ok {
			group.refs = append(group.refs, ref)
			continue
		}
		if declined[ref.kind] {
			continue
		}
		decl, ok := lookup(ref.kind)
		if !ok {
			declined[ref.kind] = true
			continue
		}
		group := &retiredGroup{kind: ref.kind, decl: decl, refs: []agentKindReference{ref}}
		byKind[ref.kind] = group
		groups = append(groups, group)
	}
	return groups
}

// convertRetiredAgents converts every reference to a retired kind in
// raw onto its replacement kind, rewriting raw and extensions in place,
// and returns one conversion record and one advisory per retired kind.
// It leaves both maps untouched when it returns an error.
func convertRetiredAgents(raw, extensions map[string]any, lookup RetiredAgentLookup) ([]AgentKindConversion, []Advisory, error) {
	groups := groupRetiredReferences(collectAgentKindReferences(raw), lookup)
	if len(groups) == 0 {
		return nil, nil, nil
	}

	remote := len(WorkerSSHHosts(extractSubMap(raw, "worker"))) > 0
	defaultKind := rawDefaultAgentKind(raw)
	rawCommand := extractSubMap(raw, "agent")["command"]

	for _, group := range groups {
		if group.kind == defaultKind {
			if parsed, err := parseAgentCommand(rawCommand); err == nil {
				group.command = parsed
			}
		}
		group.settings, _ = raw[group.kind].(map[string]any)
		if err := group.convert(); err != nil {
			return nil, nil, err
		}
		group.record = AgentKindConversion{Kind: group.kind, Replacement: group.decl.Replacement, Fields: group.fields()}
		if defaultKind != group.decl.Replacement {
			group.record.Command = group.local.Command
			group.record.RemoteCommand = group.remote.Command
			group.record.CredentialEnv = group.decl.CredentialEnv.Names()
		}
		if err := group.convertRuleBlocks(raw, defaultKind); err != nil {
			return nil, nil, err
		}
	}

	records := make([]AgentKindConversion, len(groups))
	for i, group := range groups {
		records[i] = group.record
	}
	if err := checkConversionConflicts(records); err != nil {
		return nil, nil, err
	}

	advisories := make([]Advisory, len(groups))
	for i, group := range groups {
		advisories[i] = retiredAgentAdvisory(group, remote, group.kind == defaultKind)
		rewriteRetiredGroup(raw, extensions, group, remote)
		rewriteRuleBlocks(group, remote)
	}
	return records, advisories, nil
}

// convert runs the declaration's conversion for both launch modes and
// reports a fault as the configuration error the operator sees.
func (g *retiredGroup) convert() error {
	var err error
	g.local, g.remote, err = g.convertSettings(g.settings, g.kind)
	return err
}

// convertSettings converts settings for both launch modes, reporting a
// fault under field.
func (g *retiredGroup) convertSettings(settings map[string]any, field string) (local, remote registry.AgentConversion, err error) {
	for _, isRemote := range []bool{false, true} {
		conv, fault := g.decl.Convert(registry.AgentConversionInput{Command: g.command, Settings: settings, Remote: isRemote})
		if fault != nil {
			return local, remote, &ConfigError{
				Field: field + "." + fault.Key,
				Message: fmt.Sprintf("agent kind %q was removed and this configuration cannot be converted to agent kind %q: %s",
					g.kind, g.decl.Replacement, fault.Message),
			}
		}
		if isRemote {
			remote = conv
		} else {
			local = conv
		}
	}
	return local, remote, nil
}

// convertRuleBlocks converts every rule block of the retired kind as it
// applies, laid over the kind's top-level block. A rule cannot set a
// command, so a conversion that changes the replacement's command fails
// the load. It leaves raw untouched.
func (g *retiredGroup) convertRuleBlocks(raw map[string]any, defaultKind string) error {
	for i, rule := range dispatchRuleMaps(raw) {
		kind := defaultKind
		own, fault := typeutil.StringField(rule, "agent")
		if fault != nil {
			continue
		}
		if own != "" {
			kind = own
		}
		block, held := rule[g.kind]
		if kind != g.kind || !held {
			continue
		}

		field := fmt.Sprintf("dispatch.rules[%d].%s", i, g.kind)
		settings, _ := block.(map[string]any)
		effective := maps.Clone(g.settings)
		if effective == nil {
			effective = map[string]any{}
		}
		overlaySettings(effective, settings)
		local, remote, err := g.convertSettings(effective, field)
		if err != nil {
			return err
		}
		if g.record.governs() && (!commandsEqual(local.Command, g.record.Command) || !commandsEqual(remote.Command, g.record.RemoteCommand)) {
			return &ConfigError{
				Field: field,
				Message: fmt.Sprintf("agent kind %q was removed and this rule's settings cannot be converted to agent kind %q: they change the command the replacement kind launches, which a dispatch rule cannot set",
					g.kind, g.decl.Replacement),
			}
		}
		g.ruleBlocks = append(g.ruleBlocks, ruleBlockConversion{index: i, rule: rule, settings: settings, local: local, remote: remote})
	}
	return nil
}

// droppedRuleKeys lists, as "dispatch.rules[i].<kind>.<key>", the rule
// block keys the conversion for the launch mode does not carry.
func (g *retiredGroup) droppedRuleKeys(remote bool) []string {
	var dropped []string
	for _, block := range g.ruleBlocks {
		carried := block.conversionFor(remote).Carried
		for _, key := range maputil.SortedKeys(block.settings) {
			if !slices.Contains(carried, key) {
				dropped = append(dropped, fmt.Sprintf("dispatch.rules[%d].%s.%s", block.index, g.kind, key))
			}
		}
	}
	return dropped
}

// rawDefaultAgentKind is the default agent kind of the raw
// configuration as it stands before any rewrite.
func rawDefaultAgentKind(raw map[string]any) string {
	dispatchDefault := extractSubMap(extractSubMap(raw, "dispatch"), "default")
	if kind, ok := dispatchDefault["agent"].(string); ok && kind != "" {
		return kind
	}
	if kind, ok := extractSubMap(raw, "agent")["kind"].(string); ok && kind != "" {
		return kind
	}
	return builtinAgentKind
}

func checkConversionConflicts(records []AgentKindConversion) error {
	for j, later := range records {
		if !later.governs() {
			continue
		}
		for _, earlier := range records[:j] {
			if earlier.Replacement != later.Replacement || !earlier.governs() {
				continue
			}
			if commandsEqual(earlier.Command, later.Command) && commandsEqual(earlier.RemoteCommand, later.RemoteCommand) {
				continue
			}
			return &ConfigError{
				Field: later.Fields[0],
				Message: fmt.Sprintf("agent kinds %q and %q were removed and convert to agent kind %q with different commands; name agent kind %q in the workflow file with one agent.command",
					earlier.Kind, later.Kind, later.Replacement, later.Replacement),
			}
		}
	}
	return nil
}

func commandsEqual(a, b domain.AgentCommand) bool {
	return a.Line == b.Line && slices.Equal(a.Argv, b.Argv)
}

// rewriteRetiredGroup points every reference of the group at the
// replacement kind, removes the retired kind's settings block, and
// merges the conversion's settings into the replacement block.
func rewriteRetiredGroup(raw, extensions map[string]any, group *retiredGroup, remote bool) {
	replacement := group.decl.Replacement
	for _, ref := range group.refs {
		ref.assign(replacement)
	}

	// The retired block's string leaves leave the tree the credential
	// registration walks at the end of the build, so they register now.
	if block, ok := raw[group.kind].(map[string]any); ok {
		registerCredentialLeaves("", map[string]any{group.kind: block}, false)
	}
	delete(raw, group.kind)
	delete(extensions, group.kind)

	existing, present := raw[replacement]
	if !present || existing == nil {
		existing = map[string]any{}
		raw[replacement] = existing
		extensions[replacement] = existing
	}
	block, isMapping := existing.(map[string]any)
	if !isMapping {
		return
	}
	for key, value := range group.conversionFor(remote).Settings {
		if _, has := block[key]; !has {
			block[key] = value
		}
	}
}

// rewriteRuleBlocks replaces each converted rule's retired-kind block with
// the conversion merged into its replacement-kind block, keeping keys the
// rule already sets.
func rewriteRuleBlocks(group *retiredGroup, remote bool) {
	replacement := group.decl.Replacement
	for _, converted := range group.ruleBlocks {
		rule := converted.rule
		registerCredentialLeaves(fmt.Sprintf("dispatch.rules[%d]", converted.index), map[string]any{group.kind: rule[group.kind]}, false)
		delete(rule, group.kind)

		existing, present := rule[replacement]
		if !present || existing == nil {
			existing = map[string]any{}
			rule[replacement] = existing
		}
		block, isMapping := existing.(map[string]any)
		if !isMapping {
			continue
		}
		for key, value := range converted.conversionFor(remote).Settings {
			if _, has := block[key]; !has {
				block[key] = value
			}
		}
	}
}

const retiredAgentAdvisoryMessage = "agent kind was removed and its configuration was converted to the replacement kind; the conversion will be removed in a later release"

// retiredAgentAdvisory builds the advisory for one converted group,
// describing the conversion for the configuration's own launch mode.
func retiredAgentAdvisory(group *retiredGroup, remote, wasDefault bool) Advisory {
	record := group.record
	conv := group.conversionFor(remote)
	prefix := fmt.Sprintf("agent kind %q was removed, so this configuration was converted to agent kind %q (%s); ",
		record.Kind, record.Replacement, strings.Join(record.Fields, ", "))
	suffix := " This conversion is temporary and will be removed in a later release: "

	var text string
	if !record.governs() {
		text = prefix + fmt.Sprintf("agent kind %q is already the default agent kind, so its sessions keep agent.command and carry none of the %q settings.", record.Replacement, record.Kind) +
			suffix + fmt.Sprintf("name agent kind %q in the workflow file", record.Replacement)
	} else {
		credentials, passenv := "", ""
		if remote && len(record.CredentialEnv) > 0 {
			names := strings.Join(record.CredentialEnv, ", ")
			credentials = fmt.Sprintf(", and a remote launch carries %s as agent kind %q did", names, record.Kind)
			passenv = fmt.Sprintf(" and list %s under worker.ssh_pass_env", names)
		}
		remedy := fmt.Sprintf("agent kind %q launches this invocation only as the default agent kind, with it in agent.command%s", record.Replacement, passenv)
		if wasDefault {
			remedy = fmt.Sprintf("name agent kind %q where the workflow names %q and give it this invocation in agent.command%s", record.Replacement, record.Kind, passenv)
		}
		text = prefix + "its sessions launch " + describeLaunch(conv, group.command) + credentials + describeDropped(record.Kind, group.settings, conv.Carried, group.droppedRuleKeys(remote)) + "." + suffix + remedy
	}

	return Advisory{
		Check:   "agent.kind.retired",
		Text:    text,
		Message: retiredAgentAdvisoryMessage,
		Attrs: []slog.Attr{
			slog.String("agent_kind", record.Kind),
			slog.String("replacement_kind", record.Replacement),
		},
	}
}

func describeLaunch(conv registry.AgentConversion, command domain.AgentCommand) string {
	if conv.DefaultCommand == "" {
		args := quoteDisplayArgs(conv.Args)
		if command.Line != "" && len(conv.Command.Argv) > 0 {
			return "agent.command written as a list, one element per word, followed by the arguments " + args
		}
		return "agent.command with the arguments " + args
	}
	if len(conv.Command.Argv) > 0 {
		quoted := make([]string, len(conv.Command.Argv))
		for i, elem := range conv.Command.Argv {
			quoted[i] = strconv.Quote(elem)
		}
		return "[" + strings.Join(quoted, ", ") + "]"
	}
	return strconv.Quote(conv.Command.Line)
}

func quoteDisplayArgs(args []string) string {
	shown := make([]string, len(args))
	for i, arg := range args {
		shown[i] = arg
		if !displaySafe(arg) {
			shown[i] = strconv.Quote(arg)
		}
	}
	return strings.Join(shown, " ")
}

// displaySafe reports whether arg needs no quoting to read unambiguously:
// it is non-empty and holds only A-Za-z0-9_@%+=:,./-.
func displaySafe(arg string) bool {
	if arg == "" {
		return false
	}
	for _, r := range arg {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("_@%+=:,./-", r):
		default:
			return false
		}
	}
	return true
}

func describeDropped(kind string, settings map[string]any, carried, droppedRuleKeys []string) string {
	var dropped []string
	for _, key := range maputil.SortedKeys(settings) {
		if !slices.Contains(carried, key) {
			dropped = append(dropped, kind+"."+key)
		}
	}
	dropped = append(dropped, droppedRuleKeys...)
	if len(dropped) == 0 {
		return ""
	}
	return "; not carried: " + strings.Join(dropped, ", ")
}
