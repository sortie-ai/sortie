package adaptertest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode"

	"github.com/sortie-ai/sortie/internal/qualification"
)

// contractRegistryImportPath is the import path the checker resolves the
// "registry" package qualifier from, per file, rather than assuming the
// literal identifier "registry".
const contractRegistryImportPath = "github.com/sortie-ai/sortie/internal/registry"

// contractTrackermetricsImportPath is the import path the checker
// resolves the "trackermetrics" package qualifier from, so a call to
// Track is caught regardless of the local import alias.
const contractTrackermetricsImportPath = "github.com/sortie-ai/sortie/internal/trackermetrics"

// contractProcutilImportPath is the import path the checker resolves
// the "procutil" package qualifier from, per file, so an aliased
// import cannot evade rule STOPGRACE.
const contractProcutilImportPath = "github.com/sortie-ai/sortie/internal/agent/procutil"

// contractBanTable maps a name this work extracted into a shared package
// to the owner that received it. A top-level function re-declaring one of
// these names is a violation; the banned name is the rule. The owner
// records where the extraction landed rather than naming a universal
// requirement: most entries came from a tracker or source-control
// package, so a package in another family may satisfy the rule by
// choosing a different name instead of calling an owner whose contract
// does not fit. The agent-family entries are narrower: an agent kind
// that needs persistent JSON-RPC framing has one owner rather than a
// choice of names.
var contractBanTable = map[string]string{
	"classifyTransportError": "httpkit.ClassifyTransport",
	"withRetry":              "httpkit.RetryWithBackoff",
	"sleepContext":           "httpkit.RetryWithBackoff",
	"isRetryable":            "httpkit.RetryWithBackoff",
	"toLowerSet":             "typeutil.LowerSet",
	"lowerSet":               "typeutil.LowerSet",
	"containsWhitespace":     "typeutil.HasWhitespace",
	"validateStateLabels":    "registry.DiagStateLabelElements",
	"validateStateOverlap":   "registry.DiagStateOverlap",
	"sortableEventID":        "scmcore.SortableEventID",
	"isBotAuthor":            "scmcore.IsBotAuthor",
	"aggregateStatus":        "scmcore.AggregateCIStatus",
	"computeAggregateStatus": "scmcore.AggregateCIStatus",
	"computeFailingCount":    "scmcore.FailingCount",
	"failingCount":           "scmcore.FailingCount",
	"toSCMError":             "scmcore.ToSCMError",
	"giteaToSCMError":        "scmcore.ToSCMError",
	"asSCMError":             "scmcore.AsSCMError",
	"toCIError":              "scmcore.ToCIError",
	"giteaToCIError":         "scmcore.ToCIError",
	"deriveState":            "issuekit.DeriveLabelState",
	"extractState":           "issuekit.DeriveLabelState",
	"findCurrentStateLabel":  "issuekit.CurrentLabelState",
	"paginatePages":          "httpkit.NewPagePaginator",
	"parseUTC":               "scmcore.ParseTimestamp or scmcore.ParseTimestampOrZero",
	"stringFrom":             "typeutil.StringField",
	"StringFrom":             "typeutil.StringField",
	"sendRequest":            "jsonrpc.Conn.SendRequest",
	"sendNotification":       "jsonrpc.Conn.Notify",
	"sendResponse":           "jsonrpc.Conn.Respond",
	"sendErrorResponse":      "jsonrpc.Conn.RespondError",
	"startScannerCh":         "jsonrpc.NewConn",
	"readResponse":           "jsonrpc.Conn.Call",
	"startOpenCodeReader":    "procutil.NewStdoutReader",
	"finishStderrDrain":      "procutil.StderrCollector.FinishAndCollect",
	"release":                "procutil.StartOutputRelease",
	"buildSSHRemoteCmd":      "registry.AgentMeta.CredentialEnv",
	"buildSSHRemoteCommand":  "agentcore.LaunchTarget.SSHOptions",
}

// contractTrackerAdapterMethods are the tracker operation method names
// rule METRICS requires a trackermetrics.Track call inside, when the
// enclosing package registers a tracker kind. Most are
// domain.TrackerAdapter methods; FetchIssueBlockers is a
// domain.BlockerReader method instead.
var contractTrackerAdapterMethods = map[string]bool{
	"FetchCandidateIssues":          true,
	"FetchIssueByID":                true,
	"FetchIssuesByStates":           true,
	"FetchIssueStatesByIDs":         true,
	"FetchIssueStatesByIdentifiers": true,
	"FetchIssueComments":            true,
	"FetchIssueBlockers":            true,
	"TransitionIssue":               true,
	"CommentIssue":                  true,
	"AddLabel":                      true,
}

// contractRule names one of the syntactic rules the checker enforces.
type contractRule string

const (
	ruleBAN       contractRule = "BAN"
	ruleMETRICS   contractRule = "METRICS"
	ruleHOOK      contractRule = "HOOK"
	ruleIMPORT    contractRule = "IMPORT"
	ruleTEARDOWN  contractRule = "TEARDOWN"
	ruleBLOCKER   contractRule = "BLOCKER"
	ruleIDENTITY  contractRule = "IDENTITY"
	ruleSTOPGRACE contractRule = "STOPGRACE"
	ruleCAPTURE   contractRule = "CAPTURE"
	ruleSINK      contractRule = "SINK"
	ruleREAPER    contractRule = "REAPER"
)

// Family roots and the orchestrator path rule IMPORT matches an import
// path against.
const (
	contractTrackerFamilyPath = "github.com/sortie-ai/sortie/internal/tracker"
	contractSCMFamilyPath     = "github.com/sortie-ai/sortie/internal/scm"
	contractOrchestratorPath  = "github.com/sortie-ai/sortie/internal/orchestrator"
)

// The agent and notify family roots that hold kind packages, and the
// module-internal prefix the permit-map guard strips to reach a
// directory.
const (
	contractAgentFamilyPath  = "github.com/sortie-ai/sortie/internal/agent"
	contractNotifyFamilyPath = "github.com/sortie-ai/sortie/internal/notify"
	contractInternalPrefix   = "github.com/sortie-ai/sortie/internal/"
)

// contractFamilyRoots is the ban surface both the adapter-to-adapter arm
// of contractImportBanReason and the core-import rule match an import
// path against.
var contractFamilyRoots = []string{
	contractTrackerFamilyPath,
	contractSCMFamilyPath,
	contractAgentFamilyPath,
	contractNotifyFamilyPath,
}

// contractSharedPackage records why a package under a family root holds
// no adapter, and whether the orchestrator's production code may import
// it. Rule IMPORT consults presence alone; the core-import rule consults
// coreImportable as well.
type contractSharedPackage struct {
	reason         string
	coreImportable bool
}

// contractSharedFamilyPackages names each package under a family root that
// holds no adapter, so it may be imported by a package under a family
// root, and states why. Keys are matched exactly, so a subpackage of a
// permitted package needs its own entry. A package under a family root
// that is absent from this map may be imported only by itself and by
// packages under its own path. Whether the orchestrator may import an
// entry too is a per-entry property: an entry whose coreImportable is
// false stays importable by packages under a family root but not by the
// orchestrator's production code.
var contractSharedFamilyPackages = map[string]contractSharedPackage{
	"github.com/sortie-ai/sortie/internal/scm/scmcore":                     {reason: "shared forge decision core; registers no kind and holds no adapter", coreImportable: true},
	"github.com/sortie-ai/sortie/internal/agent/procutil":                  {reason: "shared subprocess group handling, Windows process containment, and bounded output capture; registers no kind and holds no adapter", coreImportable: true},
	"github.com/sortie-ai/sortie/internal/agent/agentcore":                 {reason: "shared agent session, event, and disposition core; registers no kind and holds no adapter", coreImportable: true},
	"github.com/sortie-ai/sortie/internal/agent/mcpconfig":                 {reason: "shared MCP configuration parsing; registers no kind and holds no adapter", coreImportable: true},
	"github.com/sortie-ai/sortie/internal/agent/sshutil":                   {reason: "shared SSH invocation helpers; registers no kind and holds no adapter", coreImportable: true},
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc":                   {reason: "shared newline-delimited JSON-RPC framing and message delivery; registers no kind and holds no adapter", coreImportable: true},
	"github.com/sortie-ai/sortie/internal/agent/agenttest":                 {reason: "shared agent-adapter test support; registers no kind and holds no adapter; its non-test files import testing, so production code must not reach it", coreImportable: false},
	"github.com/sortie-ai/sortie/internal/agent/agenttest/dispositiontest": {reason: "shared turn-disposition conformance assertion, keyed separately because keys match exactly; registers no kind and holds no adapter; its non-test files import testing, so production code must not reach it", coreImportable: false},
}

// contractPackageBannedImports maps one package's import path to the
// import prefixes that package alone may not reach, with a reason per
// entry.
var contractPackageBannedImports = map[string]map[string]string{
	"github.com/sortie-ai/sortie/internal/scm/gitea": {
		"code.gitea.io/sdk": "the adapter speaks the REST API directly; the vendor SDK is not a dependency of this project",
	},
}

// contractAllowlist exempts a package, named by its directory's base
// name, from one specific rule and states why; the package stays subject
// to every rule not named here. The file adapter performs no HTTP, has
// no credential and no remote project, and registers no validation hook
// because it has no config to validate; it declares none of the banned
// names and already records every operation through trackermetrics.Track,
// so it is not exempt from BAN or METRICS.
var contractAllowlist = map[string]map[contractRule]string{
	"file": {
		ruleHOOK: "no HTTP, no credential, no remote project, and no config to validate",
	},
	"procutil": {
		ruleTEARDOWN:  "owns SetGroupCancel and SetGroupKill, the helpers every other launcher calls",
		ruleSTOPGRACE: "owns DefaultStopGrace, the fallback every other family reaches through StopGrace",
		ruleCAPTURE:   "owns StartCapture and RunCapture, the capture every other launcher calls",
	},
	"agenttest": {
		ruleCAPTURE: "test-support package that cmd/sortie does not link",
	},
	"probe": {
		ruleCAPTURE: "test-support package that cmd/sortie does not link",
	},
	"e2e": {
		ruleCAPTURE: "test-support package that cmd/sortie does not link",
	},
}

// The three outcomes checkContractCoreImports renders as
// "imports " + path + "; " + reason.
const (
	contractCoreRegistryReason     = "the orchestrator resolves an adapter kind through the registry rather than importing its package"
	contractCoreRegistrationReason = "cmd/sortie owns the blank imports that trigger kind registration"
	contractCoreTestSupportReason  = "the orchestrator's production code must not import a test-support package"
)

// contractViolation describes one place a file or package breaks the
// adapter contract.
type contractViolation struct {
	pos  token.Position
	text string
}

// contractWalkedPackage pairs a package the walk found with the directory
// path it was found at. contractPackage carries only the directory's
// base name, which cannot order packages across roots.
type contractWalkedPackage struct {
	dir string
	pkg contractPackage
}

// contractPackage carries one package's identity and its parsed files.
// Non-test files are fully parsed and feed rules BAN, METRICS, and HOOK;
// test files are parsed imports-only and feed rule IMPORT alone.
type contractPackage struct {
	dirName    string
	importPath string
	files      []*ast.File
	testFiles  []*ast.File
}

// resolveContractImportName returns the local identifier a file binds to
// importPath, or "" when the file does not import it. It reads only the
// file's own import declarations, never assumes a literal package name,
// so an aliased import cannot evade the check.
func resolveContractImportName(file *ast.File, importPath string) string {
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != importPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		segments := strings.Split(path, "/")
		return segments[len(segments)-1]
	}
	return ""
}

// unwrapCompositeLit returns the composite literal expr denotes, looking
// through a leading address-of operator so both a value literal and a
// pointer literal are recognized.
func unwrapCompositeLit(expr ast.Expr) (*ast.CompositeLit, bool) {
	switch e := expr.(type) {
	case *ast.CompositeLit:
		return e, true
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			return unwrapCompositeLit(e.X)
		}
	}
	return nil, false
}

// compositeLitKeyValue returns the value expression of the field keyed
// by the given identifier name in the composite literal expr denotes,
// or nil when expr is not such a literal or carries no such key.
func compositeLitKeyValue(expr ast.Expr, key string) ast.Expr {
	lit, ok := unwrapCompositeLit(expr)
	if !ok {
		return nil
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if ident, ok := kv.Key.(*ast.Ident); ok && ident.Name == key {
			return kv.Value
		}
	}
	return nil
}

// compositeLitHasKey reports whether expr is a composite literal
// carrying a field keyed by the given identifier name.
func compositeLitHasKey(expr ast.Expr, key string) bool {
	return compositeLitKeyValue(expr, key) != nil
}

// packageReferencesIdentifier reports whether any file in files
// contains the bare identifier name anywhere in its syntax tree,
// which catches both a plain reference and a selector's trailing
// field name (e.g. issue.BlockersUnresolved).
func packageReferencesIdentifier(files []*ast.File, name string) bool {
	for _, file := range files {
		found := false
		ast.Inspect(file, func(n ast.Node) bool {
			if found {
				return false
			}
			if ident, ok := n.(*ast.Ident); ok && ident.Name == name {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

// contractRegistrationFacts scans every file of one package for a call to
// registry.Trackers.Register or registry.Trackers.RegisterWithMeta,
// resolving the "registry" qualifier from each file's own imports. It
// reports whether the package registers a tracker kind at all, which
// constructor form it used, and, for RegisterWithMeta, whether the meta
// literal supplies ValidateTrackerConfig and BlockerSource, and whether
// BlockerSource is set to registry.BlockersPerIssue.
func contractRegistrationFacts(fset *token.FileSet, files []*ast.File) (registers bool, usedMeta bool, hasHook bool, hasBlockerSource bool, blockerSourceIsPerIssue bool, pos token.Position) {
	for _, file := range files {
		registryIdent := resolveContractImportName(file, contractRegistryImportPath)
		if registryIdent == "" {
			continue
		}

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			outer, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			inner, ok := outer.X.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := inner.X.(*ast.Ident)
			if !ok || ident.Name != registryIdent || inner.Sel.Name != "Trackers" {
				return true
			}

			switch outer.Sel.Name {
			case "Register":
				registers = true
				usedMeta = false
				pos = fset.Position(call.Pos())
			case "RegisterWithMeta":
				registers = true
				usedMeta = true
				pos = fset.Position(call.Pos())
				if len(call.Args) >= 3 {
					hasHook = compositeLitHasKey(call.Args[2], "ValidateTrackerConfig")
					if val := compositeLitKeyValue(call.Args[2], "BlockerSource"); val != nil {
						hasBlockerSource = true
						if sel, ok := val.(*ast.SelectorExpr); ok {
							if valIdent, ok := sel.X.(*ast.Ident); ok && valIdent.Name == registryIdent && sel.Sel.Name == "BlockersPerIssue" {
								blockerSourceIsPerIssue = true
							}
						}
					}
				}
			}
			return true
		})
	}
	return registers, usedMeta, hasHook, hasBlockerSource, blockerSourceIsPerIssue, pos
}

// contractPackageRegistersKind reports whether any file in files calls
// Register or RegisterWithMeta on any selector of the registry package
// qualifier, resolving the qualifier per file through
// resolveContractImportName so an aliased import cannot evade it. It
// generalizes contractRegistrationFacts by dropping that function's
// constraint that the middle selector be literally Trackers.
func contractPackageRegistersKind(files []*ast.File) bool {
	for _, file := range files {
		registryIdent := resolveContractImportName(file, contractRegistryImportPath)
		if registryIdent == "" {
			continue
		}

		registers := false
		ast.Inspect(file, func(n ast.Node) bool {
			if registers {
				return false
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			outer, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			inner, ok := outer.X.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := inner.X.(*ast.Ident)
			if !ok || ident.Name != registryIdent {
				return true
			}
			if outer.Sel.Name == "Register" || outer.Sel.Name == "RegisterWithMeta" {
				registers = true
				return false
			}
			return true
		})
		if registers {
			return true
		}
	}
	return false
}

// contractSharedPackageDirError explains why contractSharedPackageDir
// could not resolve a permit-map key to a directory.
type contractSharedPackageDirError string

func (e contractSharedPackageDirError) Error() string { return string(e) }

// contractSharedPackageDir maps a contractSharedFamilyPackages key to the
// directory it names, relative to this package, joining path segments
// exclusively with filepath.Join so the result carries the platform
// separator throughout.
func contractSharedPackageDir(importPath string) (string, error) {
	if !strings.HasPrefix(importPath, contractInternalPrefix) {
		return "", contractSharedPackageDirError("permit-map key " + importPath + " does not start with " + contractInternalPrefix)
	}
	segments := strings.Split(strings.TrimPrefix(importPath, contractInternalPrefix), "/")
	return filepath.Join(append([]string{".."}, segments...)...), nil
}

// checkContractBan reports a violation for every top-level function
// declaration in file whose name is a contractBanTable entry.
func checkContractBan(fset *token.FileSet, file *ast.File) []contractViolation {
	var violations []contractViolation
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil {
			continue
		}
		owner, banned := contractBanTable[fn.Name.Name]
		if !banned {
			continue
		}
		violations = append(violations, contractViolation{
			pos:  fset.Position(fn.Pos()),
			text: "package re-declares " + fn.Name.Name + "; call " + owner,
		})
	}
	return violations
}

// contractTeardownFields names the exec.Cmd fields a launcher must not
// wire by hand. Assigning either one rebuilds, at a new call site, the
// process-group teardown contractTeardownOwner already owns. The two
// fields have to agree, and a launcher that sets one and forgets the
// other silently keeps the os/exec default for it: a cancelled context
// then force-kills the direct child alone and every descendant it
// started outlives the cancellation.
var contractTeardownFields = map[string]bool{
	"Cancel":    true,
	"WaitDelay": true,
}

// contractTeardownOwner is the helper rule TEARDOWN directs a launcher to.
const contractTeardownOwner = "procutil.SetGroupCancel or procutil.SetGroupKill"

// checkContractTeardown reports a violation for every assignment to an
// exec.Cmd teardown field in file. It reads file only when file imports
// os/exec, so a same-named field on an unrelated type is never matched.
func checkContractTeardown(fset *token.FileSet, file *ast.File) []contractViolation {
	if resolveContractImportName(file, "os/exec") == "" {
		return nil
	}
	var violations []contractViolation
	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range assign.Lhs {
			sel, isSel := lhs.(*ast.SelectorExpr)
			if !isSel || !contractTeardownFields[sel.Sel.Name] {
				continue
			}
			violations = append(violations, contractViolation{
				pos:  fset.Position(sel.Pos()),
				text: "assigns exec.Cmd." + sel.Sel.Name + " directly; call " + contractTeardownOwner,
			})
		}
		return true
	})
	return violations
}

// contractCaptureOwner is the helper rule CAPTURE directs a caller to.
const contractCaptureOwner = "procutil.RunCapture or procutil.StartCapture"

// contractCaptureDotImportReason is the text checkContractCaptureFile
// gives for any dot-imported package, regardless of which one: a dot
// import binds no name contractFileImportAliases or
// resolveContractImportName can key a qualifier to, so a call, a
// constant reference, or a sink type reached through it resolves
// against nothing rather than against the dot-imported package. Rules
// CAPTURE and SINK have no way to tell an innocuous dot import from one
// hiding a process launch or an unbounded sink, so every one is
// unresolvable and this text says so rather than trusting the house
// style ban on dot imports to hold.
const contractCaptureDotImportReason = "which this rule cannot resolve a bound identifier, capture sink, or command constructor through; import it by name"

// contractBoundedSinkTypes names the sink types [procutil.CaptureParams]'s
// own contract admits for Stdout and Stderr: [procutil.Capture.Wait]
// copies into them and [sinkWriter.seal] takes the same lock a Write
// holds, so a Write that blocks holds Wait open past every bound it
// otherwise honours. Each entry here returns from Write immediately
// rather than pushing bytes to a slow consumer - it discards, caps, or
// simply grows in memory - which is what makes it safe. A sink type
// absent from this map is presumed capable of blocking until rule SINK
// is deliberately extended to admit it.
var contractBoundedSinkTypes = map[string]string{
	"bytes.Buffer":  "grows in memory and never blocks on Write",
	"limitedBuffer": "drops the earliest bytes once its cap is exceeded",
	"cappedWriter":  "discards bytes past its cap and always reports success",
}

// contractSinkTypeOwner is the map rule SINK directs a caller to extend
// when a new bounded sink type needs admitting.
const contractSinkTypeOwner = "contractBoundedSinkTypes"

// contractCmdIndex records, for one package's non-test files, every
// top-level function and method whose result list includes *exec.Cmd
// or exec.Cmd: a function is keyed by "importPath.Name", a method by
// its bare name alone, since a call site names a method with no
// receiver-type qualifier. producerPaths holds the import path of
// every indexed function, letting a caller recognize a file as able to
// reach a command through a constructor it never names by declaration,
// only by import.
type contractCmdIndex struct {
	funcs         map[string]bool
	methods       map[string]bool
	producerPaths map[string]bool
}

// contractCmdFields records, for one package's non-test files, every
// struct field name declared with type *exec.Cmd or exec.Cmd.
type contractCmdFields map[string]bool

// contractTypeIsExecCmd reports whether expr, a field or result type
// expression, names exec.Cmd or *exec.Cmd under file's own import name
// for os/exec.
func contractTypeIsExecCmd(execName string, expr ast.Expr) bool {
	if execName == "" {
		return false
	}
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == execName && sel.Sel.Name == "Cmd"
}

// contractTypeIsOSProcess reports whether expr names *os.Process under
// file's own import name for os.
func contractTypeIsOSProcess(osName string, expr ast.Expr) bool {
	if osName == "" {
		return false
	}
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == osName && sel.Sel.Name == "Process"
}

// contractBuildCmdIndex adds every exec.Cmd-returning top-level
// function and method declared in file to idx, and every exec.Cmd-typed
// struct field to fields.
func contractBuildCmdIndex(file *ast.File, importPath string, idx *contractCmdIndex, fields contractCmdFields) {
	execName := resolveContractImportName(file, "os/exec")
	if execName == "" {
		return
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Type.Results == nil {
				continue
			}
			returnsCmd := false
			for _, res := range d.Type.Results.List {
				if contractTypeIsExecCmd(execName, res.Type) {
					returnsCmd = true
					break
				}
			}
			if !returnsCmd {
				continue
			}
			if d.Recv != nil {
				idx.methods[d.Name.Name] = true
			} else {
				idx.funcs[importPath+"."+d.Name.Name] = true
				idx.producerPaths[importPath] = true
			}
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				continue
			}
			for _, spec := range d.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok || st.Fields == nil {
					continue
				}
				for _, f := range st.Fields.List {
					if !contractTypeIsExecCmd(execName, f.Type) {
						continue
					}
					for _, name := range f.Names {
						fields[name.Name] = true
					}
				}
			}
		}
	}
}

// contractFileImportAliases maps each import file binds to the local
// identifier a selector qualifies it with: the explicit alias when one
// is given, or the path's last segment otherwise. contractCmdBoundCall
// uses it to resolve a package-qualified call, such as
// workspace.GitCommand(...), to the import path contractBuildCmdIndex
// keyed that function's idx.funcs entry under, so a call bound to a
// constructor declared in another walked package is recognized exactly
// as a same-package call is. Blank and dot imports are omitted: neither
// binds a name a selector could qualify.
func contractFileImportAliases(file *ast.File) map[string]string {
	aliases := make(map[string]string, len(file.Imports))
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := ""
		switch {
		case imp.Name == nil:
			segments := strings.Split(path, "/")
			name = segments[len(segments)-1]
		case imp.Name.Name != "_" && imp.Name.Name != ".":
			name = imp.Name.Name
		}
		if name != "" {
			aliases[name] = path
		}
	}
	return aliases
}

// contractCmdBoundCall reports whether expr is a call to exec.Command,
// exec.CommandContext, a package-qualified call aliasPaths resolves to a
// function contractBuildCmdIndex indexed under that package's import
// path, or an unqualified call to a function or method it indexed under
// the bare name.
func contractCmdBoundCall(execName string, idx *contractCmdIndex, importPath string, aliasPaths map[string]string, expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		ident, isIdent := fn.X.(*ast.Ident)
		if isIdent && execName != "" && ident.Name == execName &&
			(fn.Sel.Name == "Command" || fn.Sel.Name == "CommandContext") {
			return true
		}
		if isIdent {
			if pkgPath, isPkg := aliasPaths[ident.Name]; isPkg && idx.funcs[pkgPath+"."+fn.Sel.Name] {
				return true
			}
		}
		return idx.methods[fn.Sel.Name]
	case *ast.Ident:
		return idx.funcs[importPath+"."+fn.Name]
	}
	return false
}

// contractProcessBoundExpr reports whether expr is a selector naming
// field Process, or a call to os.StartProcess or os.FindProcess.
func contractProcessBoundExpr(osName string, expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.SelectorExpr:
		return e.Sel.Name == "Process"
	case *ast.CallExpr:
		sel, ok := e.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		ident, ok := sel.X.(*ast.Ident)
		return ok && osName != "" && ident.Name == osName && (sel.Sel.Name == "StartProcess" || sel.Sel.Name == "FindProcess")
	}
	return false
}

// contractCollectBoundNames walks fn once and returns the set of local
// identifiers (parameters, var declarations, and assignment targets)
// bound to an exec.Cmd and the set bound to an *os.Process, per the
// rules contractCmdBoundCall and contractProcessBoundExpr apply to
// their declaration or the value they were last assigned from.
func contractCollectBoundNames(execName, osName string, idx *contractCmdIndex, importPath string, aliasPaths map[string]string, fn *ast.FuncDecl) (cmdNames, procNames map[string]bool) {
	cmdNames = map[string]bool{}
	procNames = map[string]bool{}

	addFieldList := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, f := range fl.List {
			switch {
			case contractTypeIsExecCmd(execName, f.Type):
				for _, n := range f.Names {
					cmdNames[n.Name] = true
				}
			case contractTypeIsOSProcess(osName, f.Type):
				for _, n := range f.Names {
					procNames[n.Name] = true
				}
			}
		}
	}
	if fn.Recv != nil {
		addFieldList(fn.Recv)
	}
	addFieldList(fn.Type.Params)

	if fn.Body == nil {
		return cmdNames, procNames
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.DeclStmt:
			gd, ok := s.Decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				return true
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if vs.Type != nil {
					switch {
					case contractTypeIsExecCmd(execName, vs.Type):
						for _, n2 := range vs.Names {
							cmdNames[n2.Name] = true
						}
					case contractTypeIsOSProcess(osName, vs.Type):
						for _, n2 := range vs.Names {
							procNames[n2.Name] = true
						}
					}
				}
				for i, val := range vs.Values {
					if i >= len(vs.Names) {
						continue
					}
					if contractCmdBoundCall(execName, idx, importPath, aliasPaths, val) {
						cmdNames[vs.Names[i].Name] = true
					}
					if contractProcessBoundExpr(osName, val) {
						procNames[vs.Names[i].Name] = true
					}
				}
			}
		case *ast.AssignStmt:
			for i, rhs := range s.Rhs {
				if i >= len(s.Lhs) {
					continue
				}
				ident, ok := s.Lhs[i].(*ast.Ident)
				if !ok {
					continue
				}
				if contractCmdBoundCall(execName, idx, importPath, aliasPaths, rhs) {
					cmdNames[ident.Name] = true
				}
				if contractProcessBoundExpr(osName, rhs) {
					procNames[ident.Name] = true
				}
			}
		}
		return true
	})
	return cmdNames, procNames
}

// contractExprIsCmdBound reports whether expr, a Start/Run/Wait call's
// receiver, is bound to an exec.Cmd: a direct exec.Command or
// exec.CommandContext call, a call to an indexed function or method, a
// local identifier contractCollectBoundNames marked, or a selector
// whose field fields declares as an exec.Cmd.
func contractExprIsCmdBound(execName string, idx *contractCmdIndex, importPath string, aliasPaths map[string]string, cmdNames map[string]bool, fields contractCmdFields, expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		return cmdNames[e.Name]
	case *ast.CallExpr:
		return contractCmdBoundCall(execName, idx, importPath, aliasPaths, e)
	case *ast.SelectorExpr:
		return fields[e.Sel.Name]
	}
	return false
}

// contractSinkTypeName returns the contractBoundedSinkTypes key a type
// expression names: "bytes.Buffer" for a selector resolving to the
// file's own import of "bytes", or the bare identifier for a
// package-local type such as limitedBuffer or cappedWriter. It returns
// "" for any type this rule does not recognize, so an unrecognized
// type is treated as unbounded rather than silently accepted.
func contractSinkTypeName(bytesName string, expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		if ident, ok := e.X.(*ast.Ident); ok && bytesName != "" && ident.Name == bytesName && e.Sel.Name == "Buffer" {
			return "bytes.Buffer"
		}
	}
	return ""
}

// contractSinkTypeFromValue returns the contractBoundedSinkTypes key
// for a value expression that constructs a sink directly, looking
// through a leading address-of the way unwrapCompositeLit does, so
// both "T{}" and "&T{}" resolve to T's name. It returns "" when expr is
// not a composite literal.
func contractSinkTypeFromValue(bytesName string, expr ast.Expr) string {
	lit, ok := unwrapCompositeLit(expr)
	if !ok {
		return ""
	}
	return contractSinkTypeName(bytesName, lit.Type)
}

// contractCollectSinkVarTypes maps every local variable fn's body binds
// to a recognized sink type - by "var x T" or "var x T = ..." and by
// "x := T{...}" or "x := &T{...}" - to that type's
// contractBoundedSinkTypes key, the same way contractCollectBoundNames
// tracks exec.Cmd- and os.Process-bound names for rule CAPTURE.
func contractCollectSinkVarTypes(bytesName string, fn *ast.FuncDecl) map[string]string {
	sinkTypes := map[string]string{}
	if fn.Body == nil {
		return sinkTypes
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.DeclStmt:
			gd, ok := s.Decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				return true
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if vs.Type != nil {
					if typeName := contractSinkTypeName(bytesName, vs.Type); typeName != "" {
						for _, name := range vs.Names {
							sinkTypes[name.Name] = typeName
						}
					}
				}
				for i, val := range vs.Values {
					if i >= len(vs.Names) {
						continue
					}
					if typeName := contractSinkTypeFromValue(bytesName, val); typeName != "" {
						sinkTypes[vs.Names[i].Name] = typeName
					}
				}
			}
		case *ast.AssignStmt:
			for i, rhs := range s.Rhs {
				if i >= len(s.Lhs) {
					continue
				}
				ident, ok := s.Lhs[i].(*ast.Ident)
				if !ok {
					continue
				}
				if typeName := contractSinkTypeFromValue(bytesName, rhs); typeName != "" {
					sinkTypes[ident.Name] = typeName
				}
			}
		}
		return true
	})
	return sinkTypes
}

// contractResolveSinkType reports the contractBoundedSinkTypes key expr
// resolves to via sinkTypes, and whether expr is the literal nil, which
// [procutil.CaptureParams] accepts unconditionally in place of a
// writer. An expression this function cannot resolve - a call, a
// selector into an unrecognized value such as os.Stdout, or an
// identifier sinkTypes never bound - reports "", false: unresolved is
// treated as unbounded rather than accepted.
func contractResolveSinkType(bytesName string, sinkTypes map[string]string, expr ast.Expr) (typeName string, isNil bool) {
	switch e := expr.(type) {
	case *ast.Ident:
		if e.Name == "nil" {
			return "", true
		}
		return sinkTypes[e.Name], false
	case *ast.UnaryExpr:
		if e.Op != token.AND {
			return "", false
		}
		switch x := e.X.(type) {
		case *ast.Ident:
			return sinkTypes[x.Name], false
		case *ast.CompositeLit:
			return contractSinkTypeName(bytesName, x.Type), false
		}
		return "", false
	case *ast.CompositeLit:
		return contractSinkTypeName(bytesName, e.Type), false
	}
	return "", false
}

// contractIsCaptureParamsLit reports whether lit's type is
// procName.CaptureParams, procName being the local identifier the file
// binds to [procutil]'s import path.
func contractIsCaptureParamsLit(procName string, lit *ast.CompositeLit) bool {
	if procName == "" {
		return false
	}
	sel, ok := lit.Type.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == procName && sel.Sel.Name == "CaptureParams"
}

// checkContractCaptureSinkFields reports a rule SINK violation for each
// of lit's Stdout and Stderr fields that is set and does not resolve,
// via sinkTypes, to nil or a type contractBoundedSinkTypes admits.
func checkContractCaptureSinkFields(fset *token.FileSet, lit *ast.CompositeLit, bytesName string, sinkTypes map[string]string) []contractViolation {
	var violations []contractViolation
	for _, field := range [2]string{"Stdout", "Stderr"} {
		value := compositeLitKeyValue(lit, field)
		if value == nil {
			continue
		}
		typeName, isNil := contractResolveSinkType(bytesName, sinkTypes, value)
		if isNil {
			continue
		}
		if _, ok := contractBoundedSinkTypes[typeName]; ok {
			continue
		}
		violations = append(violations, contractViolation{
			pos:  fset.Position(value.Pos()),
			text: "CaptureParams." + field + " passes a writer not accepted as bounded; extend " + contractSinkTypeOwner + " to admit it deliberately",
		})
	}
	return violations
}

// checkContractCaptureFile reports every rule CAPTURE and rule SINK
// violation in file, using idx and fields already built across the
// whole package file belongs to. Rule SINK is skipped when dirName is
// exempt from it.
//
// Any dot import in file is itself a rule CAPTURE violation, regardless
// of which package it names: a call, constant reference, or sink type
// reached through a dot import binds no local identifier, so the rest
// of this function - which resolves every one of those against file's
// own named and aliased imports - cannot see through it. Reporting the
// import outright, once, keeps a file that hides a command constructor
// or a sink type behind a dot import from silently passing this rule
// the way naming the four import paths this used to check did not.
//
// A file that imports none of os/exec, os, syscall, windows, or
// procutil can still reach an exec.Cmd by calling a constructor
// declared in another walked package - workspace.GitCommand called
// from a file that imports only "workspace", never "os/exec" - so the
// early return below also stays open when idx.producerPaths names one
// of file's own imports. A file naming none of the five imports and no
// producer's import path plainly cannot violate rule CAPTURE or rule
// SINK, since it can neither construct nor receive a command, and is
// skipped at the cost this rule was built to avoid paying.
func checkContractCaptureFile(fset *token.FileSet, file *ast.File, importPath, dirName string, idx *contractCmdIndex, fields contractCmdFields) []contractViolation {
	var violations []contractViolation

	for _, imp := range file.Imports {
		if imp.Name == nil || imp.Name.Name != "." {
			continue
		}
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		violations = append(violations, contractViolation{
			pos:  fset.Position(imp.Pos()),
			text: "dot-imports " + path + ", " + contractCaptureDotImportReason,
		})
	}

	execName := resolveContractImportName(file, "os/exec")
	osName := resolveContractImportName(file, "os")
	syscallName := resolveContractImportName(file, "syscall")
	winName := resolveContractImportName(file, "golang.org/x/sys/windows")
	procName := resolveContractImportName(file, contractProcutilImportPath)
	aliasPaths := contractFileImportAliases(file)
	importsCmdProducer := false
	for _, path := range aliasPaths {
		if idx.producerPaths[path] {
			importsCmdProducer = true
			break
		}
	}
	if execName == "" && osName == "" && syscallName == "" && winName == "" && procName == "" && !importsCmdProducer {
		return violations
	}
	bytesName := resolveContractImportName(file, "bytes")
	checkSinks := procName != "" && !contractExempt(dirName, ruleSINK)
	hasCmd := execName != "" || importsCmdProducer

	if hasCmd {
		ast.Inspect(file, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range assign.Lhs {
				sel, isSel := lhs.(*ast.SelectorExpr)
				if !isSel || (sel.Sel.Name != "Stdout" && sel.Sel.Name != "Stderr") {
					continue
				}
				violations = append(violations, contractViolation{
					pos:  fset.Position(sel.Pos()),
					text: "assigns exec.Cmd." + sel.Sel.Name + " directly; call " + contractCaptureOwner,
				})
			}
			return true
		})
	}

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		cmdNames, procNames := contractCollectBoundNames(execName, osName, idx, importPath, aliasPaths, fn)
		var sinkTypes map[string]string
		if checkSinks {
			sinkTypes = contractCollectSinkVarTypes(bytesName, fn)
		}

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if checkSinks {
				if lit, ok := n.(*ast.CompositeLit); ok && contractIsCaptureParamsLit(procName, lit) {
					violations = append(violations, checkContractCaptureSinkFields(fset, lit, bytesName, sinkTypes)...)
					return true
				}
			}

			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}

			if hasCmd {
				switch sel.Sel.Name {
				case "Output", "CombinedOutput", "StdoutPipe", "StderrPipe":
					violations = append(violations, contractViolation{
						pos:  fset.Position(call.Pos()),
						text: "waits on an exec.Cmd directly; call " + contractCaptureOwner,
					})
					return true
				case "Start", "Run":
					if contractExprIsCmdBound(execName, idx, importPath, aliasPaths, cmdNames, fields, sel.X) {
						violations = append(violations, contractViolation{
							pos:  fset.Position(call.Pos()),
							text: "waits on an exec.Cmd directly; call " + contractCaptureOwner,
						})
					}
					return true
				}
			}

			if sel.Sel.Name == "Wait" {
				if hasCmd && contractExprIsCmdBound(execName, idx, importPath, aliasPaths, cmdNames, fields, sel.X) {
					violations = append(violations, contractViolation{
						pos:  fset.Position(call.Pos()),
						text: "waits on an exec.Cmd directly; call " + contractCaptureOwner,
					})
					return true
				}
				bound := false
				if ident, isIdent := sel.X.(*ast.Ident); isIdent {
					bound = procNames[ident.Name]
				} else if procSel, isSel := sel.X.(*ast.SelectorExpr); isSel {
					bound = procSel.Sel.Name == "Process"
				}
				if bound {
					violations = append(violations, contractViolation{
						pos:  fset.Position(call.Pos()),
						text: "waits on an os.Process directly; call " + contractCaptureOwner,
					})
					return true
				}
			}

			ident, isIdent := sel.X.(*ast.Ident)
			if !isIdent {
				return true
			}
			raw := (osName != "" && ident.Name == osName && sel.Sel.Name == "StartProcess") ||
				(syscallName != "" && ident.Name == syscallName && (sel.Sel.Name == "StartProcess" || sel.Sel.Name == "ForkExec")) ||
				(winName != "" && ident.Name == winName && (sel.Sel.Name == "CreateProcess" || sel.Sel.Name == "CreateProcessAsUser"))
			if raw {
				violations = append(violations, contractViolation{
					pos:  fset.Position(call.Pos()),
					text: "starts a process outside os/exec; call " + contractCaptureOwner,
				})
			}
			return true
		})
	}

	return violations
}

// contractBuildModuleCmdIndex builds one function, method, and
// struct-field index from every non-test file across every package in
// walked, so a caller in one package that binds a *exec.Cmd from a
// constructor declared in another - workspace.GitCommand called from
// internal/orchestrator, for instance - is indexed the same as a
// same-package call site. The cost stays linear in the file count
// contractWalkRoot already parsed: this is one more pass over files
// already held in memory, not a second walk of the tree.
func contractBuildModuleCmdIndex(walked []contractWalkedPackage) (*contractCmdIndex, contractCmdFields) {
	idx := &contractCmdIndex{funcs: map[string]bool{}, methods: map[string]bool{}, producerPaths: map[string]bool{}}
	fields := contractCmdFields{}
	for _, w := range walked {
		for _, file := range w.pkg.files {
			contractBuildCmdIndex(file, w.pkg.importPath, idx, fields)
		}
	}
	return idx, fields
}

// checkContractCapture applies rule CAPTURE and rule SINK to every
// non-test file in pkg, using idx and fields the caller built ahead of
// time with contractBuildModuleCmdIndex. A caller checking one package
// in isolation - a fixture test's single-file package, for instance -
// may build idx and fields from that same package alone; a caller
// checking a real tree builds them from every package the walk found,
// so a cross-package call site resolves against the same index a
// same-package one does. Rule SINK honors its own contractAllowlist
// entry rather than reusing rule CAPTURE's; a caller that skips this
// function entirely for a rule-CAPTURE-exempt package skips rule SINK
// for it too, since nothing here runs for that package at all.
func checkContractCapture(fset *token.FileSet, pkg contractPackage, idx *contractCmdIndex, fields contractCmdFields) []contractViolation {
	var violations []contractViolation
	for _, file := range pkg.files {
		violations = append(violations, checkContractCaptureFile(fset, file, pkg.importPath, pkg.dirName, idx, fields)...)
	}
	return violations
}

// contractStopGraceOwner is the helper rule STOPGRACE directs a family
// to resolve a stop-grace duration through, instead of reading
// [procutil.DefaultStopGrace] directly.
const contractStopGraceOwner = "procutil.StopGrace"

// importPos returns the position of file's import of importPath, or
// the file's own start when the import is absent, so a violation always
// carries a position a reader can open.
func importPos(file *ast.File, importPath string) token.Pos {
	for _, imp := range file.Imports {
		if path, err := strconv.Unquote(imp.Path.Value); err == nil && path == importPath {
			return imp.Pos()
		}
	}
	return file.Pos()
}

// checkContractStopGrace reports a violation for every reference to
// procutil.DefaultStopGrace in file, with the procutil qualifier
// resolved from file's own imports so an aliased import cannot evade
// it.
func checkContractStopGrace(fset *token.FileSet, file *ast.File) []contractViolation {
	procutilIdent := resolveContractImportName(file, contractProcutilImportPath)
	if procutilIdent == "" {
		return nil
	}
	var violations []contractViolation
	// A dot import binds the constant to a bare identifier, so no
	// selector node exists for the qualifier check below to match and
	// the rule would silently pass. Reporting the import itself is the
	// precise answer: chasing bare identifiers instead would also flag
	// a local that shadows the name and the selector half of an
	// unrelated other.DefaultStopGrace, and telling those apart needs
	// type information this check does not have. Nothing here
	// dot-imports a non-test package and no linter forbids it, so the
	// rule states the prohibition rather than resting on a convention.
	if procutilIdent == "." {
		return []contractViolation{{
			pos:  fset.Position(importPos(file, contractProcutilImportPath)),
			text: "dot-imports procutil, which hides a DefaultStopGrace reference from this rule; import it by name and call " + contractStopGraceOwner,
		}}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "DefaultStopGrace" {
			return true
		}
		ident, isIdent := sel.X.(*ast.Ident)
		if !isIdent || ident.Name != procutilIdent {
			return true
		}
		violations = append(violations, contractViolation{
			pos:  fset.Position(sel.Pos()),
			text: "references procutil.DefaultStopGrace directly; call " + contractStopGraceOwner,
		})
		return true
	})
	return violations
}

// contractReaperLoggerHint is what checkContractReaperLogger's message
// tells a caller to pass instead of the two forbidden shapes.
const contractReaperLoggerHint = "a logger the call site already holds, in a local variable or a struct field"

// contractCallsSlogDefault reports whether expr is a call to
// slogIdent.Default, or a call chained onto one (e.g.
// slog.Default().With(...)), by recursing into the receiver of each
// chained call.
func contractCallsSlogDefault(expr ast.Expr, slogIdent string) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == slogIdent && sel.Sel.Name == "Default" {
		return true
	}
	return contractCallsSlogDefault(sel.X, slogIdent)
}

// checkContractReaperLogger reports a violation for every
// procutil.StartReaper call in file whose second argument is the nil
// literal or a call to slog.Default() (with or without a chained
// With), rather than a logger the call site already holds. StartReaper
// logs the one CaptureCleanupWarning record for a reap whose group
// termination cannot prove the process tree gone, and either forbidden
// shape routes that record away from the logger the caller was built
// with: nil falls back to StartReaper's own package-level default, and
// a fresh slog.Default() call reaches the same default directly,
// bypassing whatever component-scoped or session-scoped logger the
// call site actually owns.
func checkContractReaperLogger(fset *token.FileSet, file *ast.File) []contractViolation {
	procutilIdent := resolveContractImportName(file, contractProcutilImportPath)
	if procutilIdent == "" {
		return nil
	}
	// See checkContractStopGrace for why a dot import is reported at the
	// import itself rather than chased through bare identifiers.
	if procutilIdent == "." {
		return []contractViolation{{
			pos:  fset.Position(importPos(file, contractProcutilImportPath)),
			text: "dot-imports procutil, which hides a StartReaper call from this rule; import it by name",
		}}
	}
	slogIdent := resolveContractImportName(file, "log/slog")

	var violations []contractViolation
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, isIdent := sel.X.(*ast.Ident)
		if !isIdent || ident.Name != procutilIdent || sel.Sel.Name != "StartReaper" || len(call.Args) != 2 {
			return true
		}
		arg := call.Args[1]
		if nilIdent, isNilIdent := arg.(*ast.Ident); isNilIdent && nilIdent.Name == "nil" {
			violations = append(violations, contractViolation{
				pos:  fset.Position(call.Pos()),
				text: "calls procutil.StartReaper with a nil logger; pass " + contractReaperLoggerHint,
			})
			return true
		}
		if slogIdent != "" && contractCallsSlogDefault(arg, slogIdent) {
			violations = append(violations, contractViolation{
				pos:  fset.Position(call.Pos()),
				text: "calls procutil.StartReaper with slog.Default() rather than " + contractReaperLoggerHint,
			})
		}
		return true
	})
	return violations
}

// bodyCallsTrackermetricsTrack reports whether body contains a call
// resolving to trackermetrics.Track, with the qualifier resolved from
// file's own imports.
func bodyCallsTrackermetricsTrack(file *ast.File, body *ast.BlockStmt) bool {
	trackermetricsIdent := resolveContractImportName(file, contractTrackermetricsImportPath)
	if trackermetricsIdent == "" {
		return false
	}

	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == trackermetricsIdent && sel.Sel.Name == "Track" {
			found = true
		}
		return true
	})
	return found
}

// checkContractMetrics reports a violation for every domain.TrackerAdapter
// method in file, declared in a tracker-registering package, whose body
// carries no trackermetrics.Track call, and for every call expression
// selecting IncTrackerRequests anywhere in file.
func checkContractMetrics(fset *token.FileSet, file *ast.File, registersTracker bool) []contractViolation {
	var violations []contractViolation

	if registersTracker {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil {
				continue
			}
			if !contractTrackerAdapterMethods[fn.Name.Name] {
				continue
			}
			if fn.Body == nil || !bodyCallsTrackermetricsTrack(file, fn.Body) {
				violations = append(violations, contractViolation{
					pos:  fset.Position(fn.Pos()),
					text: fn.Name.Name + " is not recorded by trackermetrics.Track",
				})
			}
		}
	}

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name == "IncTrackerRequests" {
			violations = append(violations, contractViolation{
				pos:  fset.Position(call.Pos()),
				text: "metrics sink called directly; record through trackermetrics.Track",
			})
		}
		return true
	})

	return violations
}

// contractPathIsUnder reports whether importPath is prefix itself or
// denotes a path below it. It matches on path segments, never on a bare
// substring, so a vanity path that merely embeds prefix does not match.
func contractPathIsUnder(importPath, prefix string) bool {
	if importPath == prefix {
		return true
	}
	return strings.HasPrefix(importPath, prefix+"/")
}

// contractPackageImportPath maps dir, a directory found while walking
// root, to its full import path, given the import path root itself
// resolves to. It normalizes path separators so the result is identical
// on every platform the project's CI runs.
func contractPackageImportPath(root, rootImportPath, dir string) string {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." {
		return rootImportPath
	}
	return rootImportPath + "/" + filepath.ToSlash(rel)
}

// contractImportBanReason returns the reason importPath is banned for
// pkg, or the empty string when it is allowed. Conditions are evaluated
// in order and the function returns on the first match, so one import
// yields at most one reason.
func contractImportBanReason(importPath string, pkg contractPackage) string {
	if contractPathIsUnder(importPath, contractOrchestratorPath) {
		return "an adapter package must not import the orchestrator"
	}

	underFamilyRoot := false
	for _, root := range contractFamilyRoots {
		if contractPathIsUnder(importPath, root) {
			underFamilyRoot = true
			break
		}
	}
	if underFamilyRoot {
		if !contractPathIsUnder(importPath, pkg.importPath) {
			if _, shared := contractSharedFamilyPackages[importPath]; !shared {
				return "an adapter package must not import another adapter package"
			}
		}
	}

	for prefix, reason := range contractPackageBannedImports[pkg.importPath] {
		if contractPathIsUnder(importPath, prefix) {
			return reason
		}
	}

	return ""
}

// contractCoreImportBanReason returns the reason importPath is banned for
// a core file, or the empty string when it is allowed. Conditions are
// evaluated in order and the function returns on the first match, so one
// import yields at most one reason.
func contractCoreImportBanReason(importPath string, blankImport, inTestFile bool) string {
	underFamilyRoot := false
	for _, root := range contractFamilyRoots {
		if contractPathIsUnder(importPath, root) {
			underFamilyRoot = true
			break
		}
	}
	if !underFamilyRoot {
		return ""
	}

	if shared, ok := contractSharedFamilyPackages[importPath]; ok {
		if shared.coreImportable {
			return ""
		}
		if inTestFile {
			return ""
		}
		return contractCoreTestSupportReason
	}

	if blankImport && inTestFile {
		return ""
	}
	if blankImport {
		return contractCoreRegistrationReason
	}
	return contractCoreRegistryReason
}

// checkContractCoreImports reports a violation for every import
// declaration in file that contractCoreImportBanReason rejects.
func checkContractCoreImports(fset *token.FileSet, file *ast.File, inTestFile bool) []contractViolation {
	var violations []contractViolation
	for _, imp := range file.Imports {
		importPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		blankImport := imp.Name != nil && imp.Name.Name == "_"
		reason := contractCoreImportBanReason(importPath, blankImport, inTestFile)
		if reason == "" {
			continue
		}
		violations = append(violations, contractViolation{
			pos:  fset.Position(imp.Pos()),
			text: "imports " + importPath + "; " + reason,
		})
	}
	return violations
}

// checkCoreContractPackage applies the core-import rule to pkg.files with
// inTestFile false and to pkg.testFiles with inTestFile true. It applies
// no other rule and consults contractAllowlist for nothing, because the
// orchestrator root holds exactly one package and any exemption would
// disable the rule outright.
func checkCoreContractPackage(fset *token.FileSet, pkg contractPackage) []contractViolation {
	var violations []contractViolation
	for _, file := range pkg.files {
		violations = append(violations, checkContractCoreImports(fset, file, false)...)
	}
	for _, file := range pkg.testFiles {
		violations = append(violations, checkContractCoreImports(fset, file, true)...)
	}
	return violations
}

// checkContractImports reports a violation for every import declaration
// in file that contractImportBanReason rejects for pkg.
func checkContractImports(fset *token.FileSet, file *ast.File, pkg contractPackage) []contractViolation {
	var violations []contractViolation
	for _, imp := range file.Imports {
		importPath, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		reason := contractImportBanReason(importPath, pkg)
		if reason == "" {
			continue
		}
		violations = append(violations, contractViolation{
			pos:  fset.Position(imp.Pos()),
			text: "imports " + importPath + "; " + reason,
		})
	}
	return violations
}

// contractAgentIdentityFloorTable states the vendor and coding-agent
// runtime names the identity rule always treats as identity tokens, in
// addition to whatever kind strings contractAgentIdentitySnapshotData
// extracts from the tree. The table is a floor, not a closed world: a
// name absent here is still caught once some package registers it as
// a kind.
var contractAgentIdentityFloorTable = []string{
	"claude", "codex", "copilot", "kiro", "opencode", "mock",
	"gemini", "amp", "goose", "zed", "cursor", "aider", "qwen", "crush",
}

// contractAgentIdentitySnapshot is the identity rule's token set,
// gathered once from the files under internal/agent and every runtime
// profile under internal/qualification/profiles: every floor and
// extracted token, and, for each package that registered a kind, the
// kinds it registered, keyed by that package's import path.
// profileDeclaredTokens marks a token as sourced from a profile's
// identity_tokens, which is what makes an unanchored one wide-scoped.
type contractAgentIdentitySnapshot struct {
	tokens                []string
	kindImportPaths       map[string][]string
	profileDeclaredTokens map[string]bool
	extractionErrors      []contractViolation
}

var (
	contractAgentIdentityOnce sync.Once
	contractAgentIdentityData contractAgentIdentitySnapshot
)

// contractAgentIdentitySnapshotData returns the identity rule's token
// set, building it from disk on first use and caching it for the rest
// of the process. Every checked package shares one snapshot, which is
// what lets a package be caught for naming a kind some other package
// registers.
func contractAgentIdentitySnapshotData() contractAgentIdentitySnapshot {
	contractAgentIdentityOnce.Do(func() {
		contractAgentIdentityData = buildContractAgentIdentitySnapshot()
	})
	return contractAgentIdentityData
}

// buildContractAgentIdentitySnapshot walks internal/agent fresh from
// disk, collecting the first argument of every
// registry.Agents.Register and registry.Agents.RegisterWithMeta call
// in a non-test file, resolving the registry qualifier from each
// file's own imports. A call whose kind argument is not a string
// literal contributes an extraction error rather than being skipped,
// so a kind the checker cannot read cannot go unreported.
func buildContractAgentIdentitySnapshot() contractAgentIdentitySnapshot {
	fset := token.NewFileSet()
	tokenSet := map[string]bool{}
	for _, floor := range contractAgentIdentityFloorTable {
		tokenSet[floor] = true
	}
	kindImportPaths := map[string][]string{}
	var extractionErrors []contractViolation

	dir := filepath.Join("..", "agent")
	walkErr := filepath.WalkDir(dir, func(walkPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(walkPath, ".go") || strings.HasSuffix(walkPath, "_test.go") {
			return nil
		}

		file, parseErr := parser.ParseFile(fset, walkPath, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			extractionErrors = append(extractionErrors, contractViolation{text: "parse " + walkPath + ": " + parseErr.Error()})
			return nil
		}
		registryIdent := resolveContractImportName(file, contractRegistryImportPath)
		if registryIdent == "" {
			return nil
		}
		importPath := contractPackageImportPath(dir, contractAgentFamilyPath, filepath.Dir(walkPath))

		ast.Inspect(file, func(n ast.Node) bool {
			call, isCall := n.(*ast.CallExpr)
			if !isCall {
				return true
			}
			outer, isSel := call.Fun.(*ast.SelectorExpr)
			if !isSel {
				return true
			}
			inner, isSel := outer.X.(*ast.SelectorExpr)
			if !isSel {
				return true
			}
			ident, isIdent := inner.X.(*ast.Ident)
			if !isIdent || ident.Name != registryIdent || inner.Sel.Name != "Agents" {
				return true
			}
			if outer.Sel.Name != "Register" && outer.Sel.Name != "RegisterWithMeta" {
				return true
			}
			if len(call.Args) == 0 {
				extractionErrors = append(extractionErrors, contractViolation{
					pos:  fset.Position(call.Pos()),
					text: registryIdent + ".Agents." + outer.Sel.Name + " call has no kind argument",
				})
				return true
			}
			lit, isBasicLit := call.Args[0].(*ast.BasicLit)
			if !isBasicLit || lit.Kind != token.STRING {
				extractionErrors = append(extractionErrors, contractViolation{
					pos:  fset.Position(call.Args[0].Pos()),
					text: registryIdent + ".Agents." + outer.Sel.Name + " call's kind argument is not a string literal",
				})
				return true
			}
			kind, unquoteErr := strconv.Unquote(lit.Value)
			if unquoteErr != nil {
				extractionErrors = append(extractionErrors, contractViolation{
					pos:  fset.Position(lit.Pos()),
					text: "unquote " + registryIdent + ".Agents." + outer.Sel.Name + " kind argument: " + unquoteErr.Error(),
				})
				return true
			}
			tokenSet[strings.ToLower(kind)] = true
			kindImportPaths[importPath] = append(kindImportPaths[importPath], kind)
			return true
		})
		return nil
	})
	if walkErr != nil {
		extractionErrors = append(extractionErrors, contractViolation{text: "walk " + dir + ": " + walkErr.Error()})
	}

	profileDeclaredTokens, profileErrors := contractProfileDeclaredTokens(contractRuntimeProfilesGlob, tokenSet)
	extractionErrors = append(extractionErrors, profileErrors...)

	tokens := make([]string, 0, len(tokenSet))
	for tok := range tokenSet {
		tokens = append(tokens, tok)
	}
	sort.Strings(tokens)

	return contractAgentIdentitySnapshot{
		tokens:                tokens,
		kindImportPaths:       kindImportPaths,
		profileDeclaredTokens: profileDeclaredTokens,
		extractionErrors:      extractionErrors,
	}
}

// contractRuntimeProfilesGlob is the glob pattern
// buildContractAgentIdentitySnapshot reads every runtime profile
// document from. It is a package variable, rather than an inline
// literal, so a staleness-guard test can point it at a scratch
// fixture directory without a live profile under
// internal/qualification/profiles.
var contractRuntimeProfilesGlob = filepath.Join("..", "qualification", "profiles", "*.json")

// contractProfileDeclaredTokens reads every runtime profile document
// matching pattern through qualification.ReadRuntimeProfileFile,
// unioning each profile's identity_tokens into tokenSet and returning
// the lowercased subset marked profile-declared. A profile that fails
// to decode contributes an extraction error rather than being
// skipped, matching how buildContractAgentIdentitySnapshot already
// handles a kind argument it cannot read.
func contractProfileDeclaredTokens(pattern string, tokenSet map[string]bool) (profileDeclaredTokens map[string]bool, extractionErrors []contractViolation) {
	profileDeclaredTokens = map[string]bool{}
	profilePaths, globErr := filepath.Glob(pattern)
	if globErr != nil {
		extractionErrors = append(extractionErrors, contractViolation{text: "glob runtime profiles: " + globErr.Error()})
	}
	for _, profilePath := range profilePaths {
		profile, readErr := qualification.ReadRuntimeProfileFile(profilePath)
		if readErr != nil {
			extractionErrors = append(extractionErrors, contractViolation{text: "read runtime profile " + profilePath + ": " + readErr.Error()})
			continue
		}
		for _, tok := range profile.IdentityTokens {
			lower := strings.ToLower(tok)
			tokenSet[lower] = true
			profileDeclaredTokens[lower] = true
		}
	}
	return profileDeclaredTokens, extractionErrors
}

// contractIdentityWordsFromIdent splits name into lowercase words on
// every case transition and every underscore, which is the identity
// rule's word-splitting rule for an identifier.
func contractIdentityWordsFromIdent(name string) []string {
	var words []string
	var current []rune
	flush := func() {
		if len(current) > 0 {
			words = append(words, strings.ToLower(string(current)))
			current = current[:0]
		}
	}
	runes := []rune(name)
	for i, r := range runes {
		if r == '_' {
			flush()
			continue
		}
		if i > 0 && unicode.IsUpper(r) && !unicode.IsUpper(runes[i-1]) {
			flush()
		}
		// An uppercase run ending in a lowercase rune starts a new word at
		// that run's last uppercase rune, so useCLAUDEFlag yields "use",
		// "claude" and "flag" rather than gluing the token to "flag".
		if i > 0 && unicode.IsUpper(runes[i-1]) && unicode.IsLower(r) && len(current) > 1 {
			last := current[len(current)-1]
			current = current[:len(current)-1]
			flush()
			current = append(current, last)
		}
		current = append(current, r)
	}
	flush()
	return words
}

// contractIdentityWordsFromLiteral splits value into lowercase words on
// every character that is neither a letter nor a digit, which is the
// identity rule's word-splitting rule for a string literal and for a
// token.
func contractIdentityWordsFromLiteral(value string) []string {
	var words []string
	var current []rune
	flush := func() {
		if len(current) > 0 {
			words = append(words, strings.ToLower(string(current)))
			current = current[:0]
		}
	}
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current = append(current, r)
			continue
		}
		flush()
	}
	flush()
	return words
}

// contractIdentityTokenMatches reports whether tokenWords appears as a
// consecutive run inside valueWords, compared word for word. Both
// slices are already lowercased by the splitting functions, so the
// comparison is case-insensitive.
func contractIdentityTokenMatches(valueWords, tokenWords []string) bool {
	if len(tokenWords) == 0 || len(tokenWords) > len(valueWords) {
		return false
	}
	for start := 0; start+len(tokenWords) <= len(valueWords); start++ {
		matched := true
		for i, word := range tokenWords {
			if valueWords[start+i] != word {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// contractPackageUnderKindPackage reports whether checkedImportPath is
// kindPackageImportPath itself or sits under it, comparing on import
// path segments. It backs the identity rule's subtree exclusion: a
// package nested under a kind package's own path may still name the
// kind that package registers.
func contractPackageUnderKindPackage(checkedImportPath, kindPackageImportPath string) bool {
	return contractPathIsUnder(checkedImportPath, kindPackageImportPath)
}

// contractIdentityExcludedTokens returns the tokens the identity rule
// does not enforce against pkg: its own directory name, and the
// directory name and every registered kind of any kind package whose
// import path is pkg.importPath itself or an ancestor of it. The
// ancestor case is what lets a package nested under a kind package,
// such as a code generator that shares its path, name the kind that
// package exists to serve.
func contractIdentityExcludedTokens(pkg contractPackage, kindImportPaths map[string][]string) map[string]bool {
	excluded := map[string]bool{strings.ToLower(pkg.dirName): true}
	for kindImportPath, kinds := range kindImportPaths {
		if !contractPackageUnderKindPackage(pkg.importPath, kindImportPath) {
			continue
		}
		excluded[strings.ToLower(path.Base(kindImportPath))] = true
		for _, kind := range kinds {
			excluded[strings.ToLower(kind)] = true
		}
	}
	return excluded
}

// contractIdentityArm1Violations reports every string literal or
// identifier in file, outside an import declaration, whose word
// sequence carries a token from tokens that excluded does not exempt.
// Import declarations are excluded because rule IMPORT already governs
// which packages one adapter may name; this arm targets identity
// branching in prose and identifiers, not import paths.
func contractIdentityArm1Violations(fset *token.FileSet, file *ast.File, tokens []string, excluded map[string]bool) []contractViolation {
	type activeToken struct {
		token string
		words []string
	}
	active := make([]activeToken, 0, len(tokens))
	for _, tok := range tokens {
		if excluded[strings.ToLower(tok)] {
			continue
		}
		active = append(active, activeToken{token: tok, words: contractIdentityWordsFromLiteral(tok)})
	}

	var violations []contractViolation
	report := func(pos token.Pos, kind, spelling string, valueWords []string) {
		for _, at := range active {
			if contractIdentityTokenMatches(valueWords, at.words) {
				violations = append(violations, contractViolation{
					pos:  fset.Position(pos),
					text: kind + " " + spelling + " names agent identity token " + strconv.Quote(at.token),
				})
			}
		}
	}

	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.ImportSpec:
			return false
		case *ast.BasicLit:
			if v.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(v.Value)
			if err != nil {
				return true
			}
			report(v.Pos(), "string literal", v.Value, contractIdentityWordsFromLiteral(value))
		case *ast.Ident:
			report(v.Pos(), "identifier", v.Name, contractIdentityWordsFromIdent(v.Name))
		}
		return true
	})

	return violations
}

// exprContainsAgentInfo reports whether expr's syntax tree contains an
// identifier spelled exactly agentInfo, case-sensitively, whether bare
// or as part of a selector chain.
func exprContainsAgentInfo(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		if ident, ok := n.(*ast.Ident); ok && ident.Name == "agentInfo" {
			found = true
			return false
		}
		return true
	})
	return found
}

// contractIdentityArm2Violations reports every equality or inequality
// comparison, switch tag, case expression, and map index in file whose
// operand contains agentInfo: the recorded agent identity may be
// logged, but nothing may compare it, switch on it, or use it as a map
// key to decide behavior.
func contractIdentityArm2Violations(fset *token.FileSet, file *ast.File) []contractViolation {
	var violations []contractViolation

	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.BinaryExpr:
			if (v.Op == token.EQL || v.Op == token.NEQ) && (exprContainsAgentInfo(v.X) || exprContainsAgentInfo(v.Y)) {
				violations = append(violations, contractViolation{
					pos:  fset.Position(v.Pos()),
					text: "agentInfo is compared for equality or inequality; branch on an advertised capability or an observed message shape instead",
				})
			}
		case *ast.SwitchStmt:
			if v.Tag != nil && exprContainsAgentInfo(v.Tag) {
				violations = append(violations, contractViolation{
					pos:  fset.Position(v.Pos()),
					text: "switch tag is agentInfo; branch on an advertised capability or an observed message shape instead",
				})
			}
		case *ast.CaseClause:
			for _, expr := range v.List {
				if exprContainsAgentInfo(expr) {
					violations = append(violations, contractViolation{
						pos:  fset.Position(expr.Pos()),
						text: "case expression is agentInfo; branch on an advertised capability or an observed message shape instead",
					})
				}
			}
		case *ast.IndexExpr:
			if exprContainsAgentInfo(v.Index) {
				violations = append(violations, contractViolation{
					pos:  fset.Position(v.Pos()),
					text: "map index is agentInfo; branch on an advertised capability or an observed message shape instead",
				})
			}
		}
		return true
	})

	return violations
}

// checkContractIdentity evaluates both arms of the identity rule
// against pkg.files: the token arm (contractIdentityArm1Violations),
// using the shared identity snapshot minus the tokens pkg excludes for
// itself, and the agentInfo branch arm
// (contractIdentityArm2Violations). Test files are exempt from both
// arms.
func checkContractIdentity(fset *token.FileSet, pkg contractPackage) []contractViolation {
	snapshot := contractAgentIdentitySnapshotData()
	excluded := contractIdentityExcludedTokens(pkg, snapshot.kindImportPaths)

	var violations []contractViolation
	for _, file := range pkg.files {
		violations = append(violations, contractIdentityArm1Violations(fset, file, snapshot.tokens, excluded)...)
		violations = append(violations, contractIdentityArm2Violations(fset, file)...)
	}
	return violations
}

// contractTokenIsKindAnchored reports whether some entry of
// kindImportPaths anchors token: token equals, case-insensitively, that
// kind package's base directory name or one of the kinds it registers.
// That is exactly the set contractIdentityExcludedTokens can ever
// exempt for a package under that kind's own subtree, so an unanchored
// token has no directory anywhere in the tree where its name is legal.
func contractTokenIsKindAnchored(token string, kindImportPaths map[string][]string) bool {
	lower := strings.ToLower(token)
	for kindImportPath, kinds := range kindImportPaths {
		if lower == strings.ToLower(path.Base(kindImportPath)) {
			return true
		}
		for _, kind := range kinds {
			if lower == strings.ToLower(kind) {
				return true
			}
		}
	}
	return false
}

// contractWideScopedTokens returns the members of snapshot.tokens that
// are profile-declared and that no kind package anchors: the set the
// wide scope applies to. Every other token keeps today's narrow scope,
// internal/agent alone.
func contractWideScopedTokens(snapshot contractAgentIdentitySnapshot) []string {
	var wide []string
	for _, tok := range snapshot.tokens {
		if snapshot.profileDeclaredTokens[tok] && !contractTokenIsKindAnchored(tok, snapshot.kindImportPaths) {
			wide = append(wide, tok)
		}
	}
	return wide
}

// contractWideIdentityRoots are the two roots the wide scope walks,
// relative to this package's own directory, beyond the per-family
// walks TestCheckAdapterContract and TestCheckOrchestratorContract
// already cover.
var contractWideIdentityRoots = []string{filepath.Join("..", "..", "cmd"), filepath.Join("..", "..", "internal")}

// contractWideIdentityNonTestViolations walks every non-test .go file
// under root, excluding testdata, and applies contractIdentityArm1Violations
// for the wide-scoped tokens with no exclusion: an unanchored
// profile-declared token has no directory anywhere that exempts it.
// It returns the violations and the number of files scanned, so the
// caller can confirm the walk covered something.
func contractWideIdentityNonTestViolations(t *testing.T, fset *token.FileSet, root string, tokens []string) (violations []contractViolation, scanned []string) {
	t.Helper()

	err := filepath.WalkDir(root, func(walkPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(walkPath, ".go") || strings.HasSuffix(walkPath, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, walkPath, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", walkPath, parseErr)
		}
		scanned = append(scanned, walkPath)
		violations = append(violations, contractIdentityArm1Violations(fset, file, tokens, map[string]bool{})...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return violations, scanned
}

// contractIdentityTestFileIdentViolations reports every *ast.Ident in
// file, outside an import declaration, whose word sequence carries a
// token from tokens. Unlike contractIdentityArm1Violations, it never
// inspects a *ast.BasicLit: a test file's string literals stay legal,
// because a testdata path or a notes file name carries a runtime's
// name by design, and only its bare identifiers do not.
func contractIdentityTestFileIdentViolations(fset *token.FileSet, file *ast.File, tokens []string) []contractViolation {
	var violations []contractViolation
	report := func(pos token.Pos, spelling string, words []string) {
		for _, tok := range tokens {
			if contractIdentityTokenMatches(words, contractIdentityWordsFromLiteral(tok)) {
				violations = append(violations, contractViolation{
					pos:  fset.Position(pos),
					text: "identifier " + spelling + " names agent identity token " + strconv.Quote(tok),
				})
			}
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.ImportSpec:
			return false
		case *ast.Ident:
			report(v.Pos(), v.Name, contractIdentityWordsFromIdent(v.Name))
		}
		return true
	})
	return violations
}

// contractWideIdentityTestViolations walks every _test.go file under
// root, excluding testdata, parses it in full (unlike contractWalkRoot's
// imports-only test-file handling, which the identifier-only arm here
// needs to see past), and applies contractIdentityTestFileIdentViolations
// for the wide-scoped tokens.
func contractWideIdentityTestViolations(t *testing.T, fset *token.FileSet, root string, tokens []string) (violations []contractViolation, scanned []string) {
	t.Helper()

	err := filepath.WalkDir(root, func(walkPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(walkPath, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, walkPath, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", walkPath, parseErr)
		}
		scanned = append(scanned, walkPath)
		violations = append(violations, contractIdentityTestFileIdentViolations(fset, file, tokens)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return violations, scanned
}

// TestContractIdentityWideScope confirms rule IDENTITY's wide scope: an
// unanchored profile-declared token from
// internal/qualification/profiles/*.json carries no identifier or
// string literal in a non-test file, and no bare identifier in a test
// file, anywhere under cmd/ or internal/. It also confirms the wide
// walk reaches the three production files
// TestGeminiQualificationAddsNoProductionIdentityBranch (deleted along
// with the rest of internal/agent/clientprotocol's vendor-shaped
// driver) proved it reached, and pins the mechanism's own logic
// against inline fixtures so it cannot pass vacuously.
func TestContractIdentityWideScope(t *testing.T) {
	fset := token.NewFileSet()
	snapshot := contractAgentIdentitySnapshotData()
	wideTokens := contractWideScopedTokens(snapshot)
	if len(wideTokens) == 0 {
		t.Fatal("contractWideScopedTokens() returned none, want at least one unanchored profile-declared token to check the wide scope against")
	}

	var allScanned []string
	for _, root := range contractWideIdentityRoots {
		violations, scanned := contractWideIdentityNonTestViolations(t, fset, root, wideTokens)
		for _, v := range violations {
			t.Errorf("%s: %s", v.pos, v.text)
		}
		allScanned = append(allScanned, scanned...)

		testViolations, testScanned := contractWideIdentityTestViolations(t, fset, root, wideTokens)
		for _, v := range testViolations {
			t.Errorf("%s: %s", v.pos, v.text)
		}
		allScanned = append(allScanned, testScanned...)
	}

	for _, want := range []string{
		filepath.Join("cmd", "sortie", "main.go"),
		filepath.Join("internal", "agent", "clientprotocol", "pump.go"),
		filepath.Join("internal", "agent", "clientprotocol", "schemagen", "main.go"),
	} {
		if !slices.ContainsFunc(allScanned, func(p string) bool { return strings.HasSuffix(p, want) }) {
			t.Errorf("the wide scan missed the production file %s", want)
		}
	}

	t.Run("a fixture carrying an unanchored token as a non-test identifier fails", func(t *testing.T) {
		fixtureFset := token.NewFileSet()
		file, err := parser.ParseFile(fixtureFset, "fixture.go", `package fixture

func pickGeminiCommand() string { return "" }
`, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse fixture: %v", err)
		}
		violations := contractIdentityArm1Violations(fixtureFset, file, []string{"gemini"}, map[string]bool{})
		if len(violations) == 0 {
			t.Fatal("contractIdentityArm1Violations() found no violation for an unanchored token spelled as a non-test identifier, want at least one")
		}
	})

	t.Run("a fixture carrying an unanchored token as a bare test-file identifier fails", func(t *testing.T) {
		fixtureFset := token.NewFileSet()
		file, err := parser.ParseFile(fixtureFset, "fixture_test.go", `package fixture

func pickGeminiCommand() string { return "" }
`, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse fixture: %v", err)
		}
		violations := contractIdentityTestFileIdentViolations(fixtureFset, file, []string{"gemini"})
		if len(violations) == 0 {
			t.Fatal("contractIdentityTestFileIdentViolations() found no violation for an unanchored token spelled as a bare identifier, want at least one")
		}
	})

	t.Run("a fixture carrying an unanchored token only as a test-file string literal passes", func(t *testing.T) {
		fixtureFset := token.NewFileSet()
		file, err := parser.ParseFile(fixtureFset, "fixture_test.go", `package fixture

const cmd = "gemini --acp"
`, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse fixture: %v", err)
		}
		violations := contractIdentityTestFileIdentViolations(fixtureFset, file, []string{"gemini"})
		if len(violations) != 0 {
			t.Errorf("contractIdentityTestFileIdentViolations() = %v, want none: a test file's string literal stays legal", violations)
		}
	})

	t.Run("a kind-anchored profile-declared token in a package outside internal/agent still passes", func(t *testing.T) {
		anchoredKindImportPaths := map[string][]string{
			"github.com/sortie-ai/sortie/internal/agent/kiro": {"kiro"},
		}
		if !contractTokenIsKindAnchored("kiro", anchoredKindImportPaths) {
			t.Fatal("contractTokenIsKindAnchored(\"kiro\", ...) = false, want true: kiro's own kind package anchors it")
		}

		anchoredSnapshot := contractAgentIdentitySnapshot{
			tokens:                []string{"kiro"},
			kindImportPaths:       anchoredKindImportPaths,
			profileDeclaredTokens: map[string]bool{"kiro": true},
		}
		if wide := contractWideScopedTokens(anchoredSnapshot); len(wide) != 0 {
			t.Errorf("contractWideScopedTokens() = %v, want none: kiro is anchored by its own kind package", wide)
		}
	})
}

// TestContractIdentityWideScope_StalenessGuardCatchesRealBreaks proves
// the three mechanisms TestContractIdentityWideScope depends on are
// themselves capable of failing, not merely capable of passing against
// the current tree: a profile that fails to decode must be reported
// rather than silently skipped, and dropping either the anchoring
// clause or the profile-source clause of rule IDENTITY's wide scope
// must redden against a real collision already present in the tree,
// using the kiro and cursor examples below. The last two subtests
// confirm today's real snapshot keeps both collisions green, for the
// two different reasons the clauses exist.
func TestContractIdentityWideScope_StalenessGuardCatchesRealBreaks(t *testing.T) {
	snapshot := contractAgentIdentitySnapshotData()

	t.Run("a profile file that fails to decode contributes a reported extraction error", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte(`{"schema_version": 3,`), 0o600); err != nil {
			t.Fatalf("write fixture profile: %v", err)
		}

		_, extractionErrors := contractProfileDeclaredTokens(filepath.Join(dir, "*.json"), map[string]bool{})

		if len(extractionErrors) == 0 {
			t.Fatal("contractProfileDeclaredTokens() reported no extraction error for a profile that fails to decode, want at least one")
		}
	})

	t.Run("a profile that decodes cleanly reports no extraction error and contributes its token", func(t *testing.T) {
		tokenSet := map[string]bool{}

		declared, extractionErrors := contractProfileDeclaredTokens(contractRuntimeProfilesGlob, tokenSet)

		if len(extractionErrors) != 0 {
			t.Fatalf("contractProfileDeclaredTokens(%q) reported %v, want none against the tracked profile tree", contractRuntimeProfilesGlob, extractionErrors)
		}
		if !declared["gemini"] {
			t.Errorf("contractProfileDeclaredTokens(%q) = %v, want it to mark %q profile-declared from the tracked gemini-cli profile", contractRuntimeProfilesGlob, declared, "gemini")
		}
	})

	t.Run("dropping the anchoring clause makes an unanchored kiro redden against the real preflight_test.go collision", func(t *testing.T) {
		fset := token.NewFileSet()
		unanchored := contractAgentIdentitySnapshot{
			tokens:                []string{"kiro"},
			kindImportPaths:       map[string][]string{},
			profileDeclaredTokens: map[string]bool{"kiro": true},
		}
		wide := contractWideScopedTokens(unanchored)
		if !slices.Contains(wide, "kiro") {
			t.Fatalf("contractWideScopedTokens() = %v, want it to carry kiro once the anchoring clause is dropped, so this control can prove the clause is load-bearing", wide)
		}

		var violations []contractViolation
		for _, root := range contractWideIdentityRoots {
			v, _ := contractWideIdentityTestViolations(t, fset, root, wide)
			violations = append(violations, v...)
		}
		if len(violations) == 0 {
			t.Fatal("the wide test-file scan found no violation for an unanchored kiro token, want it to catch var kiroErrors in internal/orchestrator/preflight_test.go")
		}
	})

	t.Run("kiro stays kind-anchored against the real snapshot, so today's wide check passes it", func(t *testing.T) {
		if !contractTokenIsKindAnchored("kiro", snapshot.kindImportPaths) {
			t.Fatal("contractTokenIsKindAnchored(\"kiro\", ...) = false against the real snapshot, want true: internal/agent/kiro registers kind kiro")
		}
		if wide := contractWideScopedTokens(snapshot); slices.Contains(wide, "kiro") {
			t.Errorf("contractWideScopedTokens() = %v, want no kiro: it is kind-anchored", wide)
		}
	})

	t.Run("marking cursor profile-declared makes it redden against the real domain and linear collisions", func(t *testing.T) {
		fset := token.NewFileSet()
		hypothetical := contractAgentIdentitySnapshot{
			tokens:                []string{"cursor"},
			kindImportPaths:       snapshot.kindImportPaths,
			profileDeclaredTokens: map[string]bool{"cursor": true},
		}
		if contractTokenIsKindAnchored("cursor", hypothetical.kindImportPaths) {
			t.Fatal("contractTokenIsKindAnchored(\"cursor\", ...) = true against the real snapshot, want false: no kind package anchors cursor")
		}
		wide := contractWideScopedTokens(hypothetical)
		if !slices.Contains(wide, "cursor") {
			t.Fatalf("contractWideScopedTokens() = %v, want it to carry cursor once profile-declared, so this control can prove the profile-source clause is load-bearing", wide)
		}

		var violations []contractViolation
		for _, root := range contractWideIdentityRoots {
			v, _ := contractWideIdentityNonTestViolations(t, fset, root, wide)
			violations = append(violations, v...)
		}
		if len(violations) == 0 {
			t.Fatal("the wide non-test scan found no violation for a profile-declared cursor token, want it to catch the domain.ErrTrackerMissingCursor family in internal/domain and internal/tracker/linear")
		}
	})

	t.Run("cursor stays unprofiled against the real snapshot, so today's wide check passes it", func(t *testing.T) {
		if snapshot.profileDeclaredTokens["cursor"] {
			t.Fatal("the real snapshot marks cursor profile-declared; this control's premise that cursor stays deferred no longer holds")
		}
		if wide := contractWideScopedTokens(snapshot); slices.Contains(wide, "cursor") {
			t.Errorf("contractWideScopedTokens() = %v, want no cursor: it is not profile-declared", wide)
		}
	})
}

// contractExempt reports whether the package named dirName is
// allowlisted for rule.
func contractExempt(dirName string, rule contractRule) bool {
	reasons, ok := contractAllowlist[dirName]
	if !ok {
		return false
	}
	_, ok = reasons[rule]
	return ok
}

// checkAdapterContractPackage evaluates rules BAN, METRICS, HOOK,
// IMPORT, and, for a package under the agent family root, IDENTITY and
// STOPGRACE, against pkg, honoring the allowlist entries for
// pkg.dirName. Rules BAN, METRICS, HOOK, IDENTITY, and STOPGRACE read
// pkg.files only; rule IMPORT reads pkg.files and pkg.testFiles.
//
// A ruleIMPORT entry in contractAllowlist is all-or-nothing: it lifts the
// orchestrator ban, the sibling-adapter ban, and the package's own
// contractPackageBannedImports prefixes together, in that package's
// non-test and test files alike. There is no narrower exemption; a case
// that needs only one of the three lifted requires a new mechanism,
// decided when it appears rather than pre-built here.
func checkAdapterContractPackage(fset *token.FileSet, pkg contractPackage) []contractViolation {
	var violations []contractViolation

	if !contractExempt(pkg.dirName, ruleBAN) {
		for _, file := range pkg.files {
			violations = append(violations, checkContractBan(fset, file)...)
		}
	}

	registers, usedMeta, hasHook, hasBlockerSource, blockerSourceIsPerIssue, factsPos := contractRegistrationFacts(fset, pkg.files)

	if !contractExempt(pkg.dirName, ruleMETRICS) {
		for _, file := range pkg.files {
			violations = append(violations, checkContractMetrics(fset, file, registers)...)
		}
	}

	if registers && !contractExempt(pkg.dirName, ruleHOOK) {
		if !usedMeta || !hasHook {
			violations = append(violations, contractViolation{
				pos:  factsPos,
				text: "tracker kind registers no config validation hook",
			})
		}
	}

	if registers && !contractExempt(pkg.dirName, ruleBLOCKER) {
		if !usedMeta || !hasBlockerSource {
			violations = append(violations, contractViolation{
				pos:  factsPos,
				text: "tracker kind registers no declared blocker source",
			})
		}
		if blockerSourceIsPerIssue && !packageReferencesIdentifier(pkg.files, "BlockersUnresolved") {
			violations = append(violations, contractViolation{
				pos:  factsPos,
				text: "tracker kind declares BlockersPerIssue but never references BlockersUnresolved",
			})
		}
	}

	if !contractExempt(pkg.dirName, ruleIMPORT) {
		for _, file := range pkg.files {
			violations = append(violations, checkContractImports(fset, file, pkg)...)
		}
		for _, file := range pkg.testFiles {
			violations = append(violations, checkContractImports(fset, file, pkg)...)
		}
	}

	if contractPathIsUnder(pkg.importPath, contractAgentFamilyPath) && !contractExempt(pkg.dirName, ruleIDENTITY) {
		violations = append(violations, checkContractIdentity(fset, pkg)...)
	}

	if contractPathIsUnder(pkg.importPath, contractAgentFamilyPath) && !contractExempt(pkg.dirName, ruleSTOPGRACE) {
		for _, file := range pkg.files {
			violations = append(violations, checkContractStopGrace(fset, file)...)
		}
	}

	return violations
}

// contractWalkRoot walks the Go files under dir, excluding testdata,
// grouping them into packages keyed by directory, and returns those
// packages ordered ascending by directory path, plus whether any parsed
// file under dir imports the registry package. Both returns are scoped to
// this one root; a caller that walks more than one root merges them
// itself.
func contractWalkRoot(t *testing.T, fset *token.FileSet, dir, importPath string) ([]contractWalkedPackage, bool) {
	t.Helper()

	packages := map[string]*contractPackage{}
	var dirOrder []string
	registryImported := false
	parsed := 0

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		fileDir := filepath.Dir(path)
		pkg, seen := packages[fileDir]
		if !seen {
			pkg = &contractPackage{
				dirName:    filepath.Base(fileDir),
				importPath: contractPackageImportPath(dir, importPath, fileDir),
			}
			packages[fileDir] = pkg
			dirOrder = append(dirOrder, fileDir)
		}

		isTestFile := strings.HasSuffix(path, "_test.go")
		mode := parser.SkipObjectResolution
		if isTestFile {
			mode |= parser.ImportsOnly
		}
		file, parseErr := parser.ParseFile(fset, path, nil, mode)
		if parseErr != nil {
			t.Errorf("parse %s: %v", path, parseErr)
			return nil
		}

		if isTestFile {
			pkg.testFiles = append(pkg.testFiles, file)
		} else {
			pkg.files = append(pkg.files, file)
		}
		parsed++

		if !registryImported && resolveContractImportName(file, contractRegistryImportPath) != "" {
			registryImported = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	if parsed == 0 {
		t.Fatalf("root %s yielded no parsed Go files, want at least one", dir)
	}

	walked := make([]contractWalkedPackage, 0, len(dirOrder))
	for _, dirPath := range dirOrder {
		walked = append(walked, contractWalkedPackage{dir: dirPath, pkg: *packages[dirPath]})
	}
	sort.Slice(walked, func(i, j int) bool { return walked[i].dir < walked[j].dir })

	return walked, registryImported
}

// TestCheckAdapterContract walks the Go files under internal/tracker,
// internal/scm, internal/agent, and internal/notify, excluding testdata,
// and fails when any package breaks the shared-decision invariant this
// work establishes: a re-declared reimplementation of a name the ban
// table names, a domain.TrackerAdapter method that does not record
// through trackermetrics.Track, a direct call to IncTrackerRequests, a
// tracker-registering package supplying no config validation hook, or an
// import rule IMPORT rejects.
func TestCheckAdapterContract(t *testing.T) {
	fset := token.NewFileSet()

	roots := []struct {
		dir        string
		importPath string
	}{
		{filepath.Join("..", "tracker"), contractTrackerFamilyPath},
		{filepath.Join("..", "scm"), contractSCMFamilyPath},
		{filepath.Join("..", "agent"), contractAgentFamilyPath},
		{filepath.Join("..", "notify"), contractNotifyFamilyPath},
	}

	var walked []contractWalkedPackage
	for _, root := range roots {
		rootWalked, rootRegistryImported := contractWalkRoot(t, fset, root.dir, root.importPath)
		if !rootRegistryImported {
			t.Fatalf("no parsed file under %s imports %s, want at least one", root.importPath, contractRegistryImportPath)
		}
		walked = append(walked, rootWalked...)
	}

	sort.Slice(walked, func(i, j int) bool { return walked[i].dir < walked[j].dir })
	for _, w := range walked {
		for _, v := range checkAdapterContractPackage(fset, w.pkg) {
			t.Errorf("%s: %s", v.pos, v.text)
		}
	}
}

// TestCheckOrchestratorContract walks the Go files under internal/orchestrator,
// excluding testdata, and fails when a file imports a package under an
// adapter family root that the core-import rule rejects: an ordinary
// import must resolve the adapter kind through the registry instead, and
// a blank import outside a test file belongs in cmd/sortie, not here.
func TestCheckOrchestratorContract(t *testing.T) {
	fset := token.NewFileSet()

	walked, _ := contractWalkRoot(t, fset, filepath.Join("..", "orchestrator"), contractOrchestratorPath)

	hasNonTestFile := false
	hasTestFile := false
	for _, w := range walked {
		if len(w.pkg.files) > 0 {
			hasNonTestFile = true
		}
		if len(w.pkg.testFiles) > 0 {
			hasTestFile = true
		}
	}
	if !hasNonTestFile {
		t.Fatalf("root %s yielded no non-test Go file, want at least one", contractOrchestratorPath)
	}
	if !hasTestFile {
		t.Fatalf("root %s yielded no test Go file, want at least one", contractOrchestratorPath)
	}

	for _, w := range walked {
		for _, v := range checkCoreContractPackage(fset, w.pkg) {
			t.Errorf("%s: %s", v.pos, v.text)
		}
	}
}

// contractCaptureTeardownRoots names the two roots rules CAPTURE and
// TEARDOWN walk, the same module-wide scope contractWideIdentityRoots
// names for rule IDENTITY: every launch site has to reach
// procutil.RunCapture, procutil.StartCapture, procutil.SetGroupCancel,
// or procutil.SetGroupKill, whichever family or layer it lives in.
var contractCaptureTeardownRoots = []struct {
	dir        string
	importPath string
}{
	{filepath.Join("..", "..", "cmd"), "github.com/sortie-ai/sortie/cmd"},
	{filepath.Join("..", "..", "internal"), "github.com/sortie-ai/sortie/internal"},
}

// contractWalkCaptureAndTeardown walks both contractCaptureTeardownRoots
// through contractWalkRoot, grouping files by directory, and merges the
// two roots' packages into one dir-ordered slice. The caller builds rule
// CAPTURE's index from the merged slice via contractBuildModuleCmdIndex,
// so a constructor call spanning two of the walked packages resolves the
// same way a same-package call does.
func contractWalkCaptureAndTeardown(t *testing.T, fset *token.FileSet) []contractWalkedPackage {
	t.Helper()
	var walked []contractWalkedPackage
	for _, root := range contractCaptureTeardownRoots {
		rootWalked, _ := contractWalkRoot(t, fset, root.dir, root.importPath)
		walked = append(walked, rootWalked...)
	}
	sort.Slice(walked, func(i, j int) bool { return walked[i].dir < walked[j].dir })
	return walked
}

// TestContractCaptureAndTeardown walks every non-test Go file under
// cmd/ and internal/, excluding testdata, and fails when a file starts
// a process, waits on one, or wires an exec.Cmd's output or
// cancellation directly, outside procutil and the named test-support
// packages (rule CAPTURE); passes a CaptureParams.Stdout or
// CaptureParams.Stderr that does not resolve to nil or a type
// contractBoundedSinkTypes admits (rule SINK); assigns an exec.Cmd
// teardown field by hand (rule TEARDOWN); or calls
// procutil.StartReaper with a nil logger or a fresh slog.Default()
// call rather than a logger the call site already holds (rule
// REAPER).
func TestContractCaptureAndTeardown(t *testing.T) {
	fset := token.NewFileSet()
	walked := contractWalkCaptureAndTeardown(t, fset)
	idx, fields := contractBuildModuleCmdIndex(walked)

	for _, w := range walked {
		if !contractExempt(w.pkg.dirName, ruleCAPTURE) {
			for _, v := range checkContractCapture(fset, w.pkg, idx, fields) {
				t.Errorf("%s: %s", v.pos, v.text)
			}
		}
		if !contractExempt(w.pkg.dirName, ruleTEARDOWN) {
			for _, file := range w.pkg.files {
				for _, v := range checkContractTeardown(fset, file) {
					t.Errorf("%s: %s", v.pos, v.text)
				}
			}
		}
		if !contractExempt(w.pkg.dirName, ruleREAPER) {
			for _, file := range w.pkg.files {
				for _, v := range checkContractReaperLogger(fset, file) {
					t.Errorf("%s: %s", v.pos, v.text)
				}
			}
		}
	}
}

// TestCheckAdapterContract_DetectsViolations pins the checker's own logic
// against inline source fixtures, independent of the current state of
// any adapter package, so a regression in a rule is caught even when
// every real adapter happens to comply. Each fixture is parsed as the
// single file of a one-file package named by dirName.
// TestCheckAdapterContract_DetectsSSHRemoteCommandHelpers pins the two
// ban-table entries for the retired per-adapter SSH prefix helpers,
// against an inline fixture rather than the current state of any real
// package, so a regression is caught even when every real adapter
// happens to comply.
func TestCheckAdapterContract_DetectsSSHRemoteCommandHelpers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		src        string
		wantSubstr string
	}{
		{
			name: "buildSSHRemoteCmd",
			src: `package fixture

func buildSSHRemoteCmd(cmd, key string) string {
	return cmd
}
`,
			wantSubstr: "call registry.AgentMeta.CredentialEnv",
		},
		{
			name: "buildSSHRemoteCommand",
			src: `package fixture

func buildSSHRemoteCommand(cmd string, env map[string]string) string {
	return cmd
}
`,
			wantSubstr: "call agentcore.LaunchTarget.SSHOptions",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", tt.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parser.ParseFile: %v", err)
			}

			pkg := contractPackage{dirName: "fixture", importPath: "github.com/sortie-ai/sortie/internal/agent/fixture", files: []*ast.File{file}}
			got := checkAdapterContractPackage(fset, pkg)
			if !slices.ContainsFunc(got, func(v contractViolation) bool {
				return strings.Contains(v.text, tt.wantSubstr)
			}) {
				t.Errorf("checkAdapterContractPackage() violations = %+v, want one containing %q", got, tt.wantSubstr)
			}
		})
	}
}

func TestCheckAdapterContract_DetectsViolations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		dirName    string
		importPath string
		src        string
		inTestFile bool
		wantCount  int

		// wantSubstr, when non-empty, must appear in the text of at
		// least one returned violation, pinning the violation to the
		// expected rule rather than accepting any violation of the
		// right count.
		wantSubstr string
	}{
		{
			name:       "a non-test file referencing procutil.DefaultStopGrace directly is rejected",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import (
	"time"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

func grace() time.Duration {
	return procutil.DefaultStopGrace
}
`,
			wantCount:  1,
			wantSubstr: "call procutil.StopGrace",
		},
		{
			// The file also shadows the name and names it on an
			// unrelated package. Reporting the import rather than every
			// identifier is what keeps those from counting: an
			// identifier walk cannot tell them apart without type
			// information, so it would report three violations here
			// instead of one.
			name:       "a non-test file dot-importing procutil is rejected once, on the import",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import (
	"time"

	. "github.com/sortie-ai/sortie/internal/agent/procutil"
	other "github.com/sortie-ai/sortie/internal/agent/agentcore"
)

func grace(ms int) time.Duration {
	DefaultStopGrace := StopGrace(ms)
	_ = other.DefaultStopGrace
	return DefaultStopGrace
}

func plain() time.Duration {
	return DefaultStopGrace
}
`,
			wantCount:  1,
			wantSubstr: "dot-imports procutil",
		},
		{
			name:       "a shadowing local named DefaultStopGrace is not a violation",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import (
	"time"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

func grace(ms int) time.Duration {
	DefaultStopGrace := procutil.StopGrace(ms)
	return DefaultStopGrace
}
`,
			wantCount: 0,
		},
		{
			name:       "an unrelated package's DefaultStopGrace is not a violation",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import (
	"time"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
	other "github.com/sortie-ai/sortie/internal/agent/agentcore"
)

func grace(ms int) time.Duration {
	_ = other.DefaultStopGrace
	return procutil.StopGrace(ms)
}
`,
			wantCount: 0,
		},
		{
			name:       "a package resolving its grace through procutil.StopGrace is accepted",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import (
	"time"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

func grace(ms int) time.Duration {
	return procutil.StopGrace(ms)
}
`,
			wantCount: 0,
		},
		{
			name:       "a re-declared ban-table name is rejected",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/tracker/fixture",
			src: `package fixture

func withRetry() error { return nil }
`,
			wantCount: 1,
		},
		{
			name:       "a re-declared release name is rejected",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

func release() {}
`,
			wantCount:  1,
			wantSubstr: "call procutil.StartOutputRelease",
		},
		{
			name:       "a tracker-registering package's method with no trackermetrics.Track call is rejected",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/tracker/fixture",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Trackers.RegisterWithMeta("fixture", newFixtureAdapter, registry.TrackerMeta{
		ValidateTrackerConfig: validateConfig,
		BlockerSource:         registry.BlockersFromCandidates,
	})
}

type fixtureAdapter struct{}

func (a *fixtureAdapter) FetchIssueByID(ctx int, id string) (int, error) {
	return 0, nil
}
`,
			wantCount: 1,
		},
		{
			name:       "a direct call to IncTrackerRequests is rejected",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/tracker/fixture",
			src: `package fixture

func record(metrics Metrics) {
	metrics.IncTrackerRequests("fetch_candidates", "success")
}
`,
			wantCount: 1,
		},
		{
			// No meta literal at all means no declared blocker source
			// either, so this fixture is caught by both HOOK and BLOCKER.
			name:       "a tracker kind registered through plain Register is rejected",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/tracker/fixture",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Trackers.Register("fixture", newFixtureAdapter)
}
`,
			wantCount: 2,
		},
		{
			name:       "a RegisterWithMeta literal omitting ValidateTrackerConfig is rejected",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/tracker/fixture",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Trackers.RegisterWithMeta("fixture", newFixtureAdapter, registry.TrackerMeta{
		RequiresProject: true,
		BlockerSource:   registry.BlockersFromCandidates,
	})
}
`,
			wantCount: 1,
		},
		{
			name:       "a fully compliant tracker-registering package is accepted",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/tracker/fixture",
			src: `package fixture

import (
	"github.com/sortie-ai/sortie/internal/registry"
	"github.com/sortie-ai/sortie/internal/trackermetrics"
)

func init() {
	registry.Trackers.RegisterWithMeta("fixture", newFixtureAdapter, registry.TrackerMeta{
		ValidateTrackerConfig: validateConfig,
		BlockerSource:         registry.BlockersFromCandidates,
	})
}

type fixtureAdapter struct{}

func (a *fixtureAdapter) FetchIssueByID(ctx int, id string) (int, error) {
	return 0, trackermetrics.Track(a.metrics, "fetch_issue", func() error { return nil })
}
`,
			wantCount: 0,
		},
		{
			// The fixture registers through the plain Register form, which
			// carries no meta literal at all, so it is also caught by rule
			// BLOCKER (no package can declare a blocker source without a
			// meta literal to carry it); dirName "file" is allowlisted for
			// HOOK only, so both BAN and BLOCKER fire here.
			name:       "a package allowlisted for HOOK stays subject to BAN",
			dirName:    "file",
			importPath: "github.com/sortie-ai/sortie/internal/tracker/file",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Trackers.Register("fixture", newFixtureAdapter)
}

func withRetry() error { return nil }
`,
			wantCount: 2,
		},
		{
			name:       "the github SCM package importing a sibling SCM adapter package is rejected",
			dirName:    "github",
			importPath: "github.com/sortie-ai/sortie/internal/scm/github",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/scm/gitlab"
`,
			wantCount: 1,
		},
		{
			name:       "the gitea SCM package importing a sibling SCM adapter package is rejected",
			dirName:    "gitea",
			importPath: "github.com/sortie-ai/sortie/internal/scm/gitea",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/scm/gitlab"
`,
			wantCount: 1,
		},
		{
			name:       "the gitlab SCM package importing a sibling SCM adapter package is rejected",
			dirName:    "gitlab",
			importPath: "github.com/sortie-ai/sortie/internal/scm/gitlab",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/scm/gitea"
`,
			wantCount: 1,
		},
		{
			name:       "a tracker package importing a sibling tracker adapter package is rejected",
			dirName:    "jira",
			importPath: "github.com/sortie-ai/sortie/internal/tracker/jira",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/tracker/linear"
`,
			wantCount: 1,
		},
		{
			name:       "an adapter package importing the orchestrator is rejected",
			dirName:    "gitea",
			importPath: "github.com/sortie-ai/sortie/internal/scm/gitea",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/orchestrator"
`,
			wantCount: 1,
		},
		{
			name:       "an adapter package importing a shared family package is accepted",
			dirName:    "gitea",
			importPath: "github.com/sortie-ai/sortie/internal/scm/gitea",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/scm/scmcore"
`,
			wantCount: 0,
		},
		{
			name:       "an adapter package importing its own path is accepted",
			dirName:    "gitea",
			importPath: "github.com/sortie-ai/sortie/internal/scm/gitea",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/scm/gitea"
`,
			inTestFile: true,
			wantCount:  0,
		},
		{
			name:       "rule IMPORT reaches a package's test files",
			dirName:    "gitlab",
			importPath: "github.com/sortie-ai/sortie/internal/scm/gitlab",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/scm/github"
`,
			inTestFile: true,
			wantCount:  1,
		},
		{
			name:       "gitea importing the banned vendor SDK is rejected",
			dirName:    "gitea",
			importPath: "github.com/sortie-ai/sortie/internal/scm/gitea",
			src: `package fixture

import "code.gitea.io/sdk/gitea"
`,
			wantCount: 1,
		},
		{
			name:       "the per-package banned import table does not extend to a package it does not name",
			dirName:    "gitlab",
			importPath: "github.com/sortie-ai/sortie/internal/scm/gitlab",
			src: `package fixture

import "code.gitea.io/sdk/gitea"
`,
			wantCount: 0,
		},
		{
			name:       "a blank import of a sibling adapter package is rejected the same as a named import",
			dirName:    "github",
			importPath: "github.com/sortie-ai/sortie/internal/scm/github",
			src: `package fixture

import _ "github.com/sortie-ai/sortie/internal/scm/gitea"
`,
			wantCount: 1,
		},
		{
			name:       "the HOOK allowlist entry for file does not exempt it from IMPORT",
			dirName:    "file",
			importPath: "github.com/sortie-ai/sortie/internal/tracker/file",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/tracker/jira"
`,
			wantCount: 1,
		},
		{
			name:       "a per_issue package that never references BlockersUnresolved is rejected",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/tracker/fixture",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Trackers.RegisterWithMeta("fixture", newFixtureAdapter, registry.TrackerMeta{
		ValidateTrackerConfig: validateConfig,
		BlockerSource:         registry.BlockersPerIssue,
	})
}
`,
			wantCount: 1,
		},
		{
			name:       "a per_issue package that references BlockersUnresolved is accepted",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/tracker/fixture",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Trackers.RegisterWithMeta("fixture", newFixtureAdapter, registry.TrackerMeta{
		ValidateTrackerConfig: validateConfig,
		BlockerSource:         registry.BlockersPerIssue,
	})
}

func markUnresolved(issue *domain.Issue) {
	issue.BlockersUnresolved = true
}
`,
			wantCount: 0,
		},
		{
			name:       "a notifier backend importing a sibling notifier backend is rejected",
			dirName:    "slack",
			importPath: "github.com/sortie-ai/sortie/internal/notify/slack",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/notify/webhook"
`,
			wantCount: 1,
		},
		{
			name:       "a notifier backend importing the orchestrator is rejected",
			dirName:    "webhook",
			importPath: "github.com/sortie-ai/sortie/internal/notify/webhook",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/orchestrator"
`,
			wantCount: 1,
		},
		{
			name:       "an agent kind package importing a sibling agent kind package is rejected",
			dirName:    "codex",
			importPath: "github.com/sortie-ai/sortie/internal/agent/codex",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/agent/claude"
`,
			wantCount: 1,
		},
		{
			name:       "an agent kind package importing the permitted shared packages is accepted",
			dirName:    "codex",
			importPath: "github.com/sortie-ai/sortie/internal/agent/codex",
			src: `package fixture

import (
	"github.com/sortie-ai/sortie/internal/agent/agentcore"
	"github.com/sortie-ai/sortie/internal/agent/agenttest"
	"github.com/sortie-ai/sortie/internal/agent/agenttest/dispositiontest"
)
`,
			inTestFile: true,
			wantCount:  0,
		},
		{
			name:       "a package naming a foreign runtime in a single-word string literal is rejected",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

const backend = "gemini"
`,
			wantCount:  1,
			wantSubstr: `names agent identity token "gemini"`,
		},
		{
			// "claude-code" also carries the single-word floor token
			// "claude" as a prefix run, so this fixture reports two
			// violations; wantSubstr pins that one of them names the
			// multi-word token itself, which is the case plain word
			// equality (matching only "claude") would miss.
			name:       "a package naming a foreign multi-word kind string is rejected",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

const backend = "claude-code"
`,
			wantCount:  2,
			wantSubstr: `names agent identity token "claude-code"`,
		},
		{
			name:       "a package switching on agentInfo.Name is rejected",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

func check(agentInfo Info) bool {
	switch agentInfo.Name {
	case "special-mode":
		return true
	}
	return false
}
`,
			wantCount:  1,
			wantSubstr: "switch tag is agentInfo",
		},
		{
			// AgentInfo, capitalized, is the wire type's own field
			// name; the recorded session field is a different
			// identifier from the lowercase agentInfo the second arm
			// bans, so deciding presence from it once is not a
			// violation.
			name:       "a package deciding presence once from the wire type's own AgentInfo field is accepted",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

func present(resp InitializeResponse) bool {
	return resp.AgentInfo != nil
}
`,
			wantCount: 0,
		},
		{
			name:       "a package naming the runtime of its own directory is accepted",
			dirName:    "claude",
			importPath: "github.com/sortie-ai/sortie/internal/agent/claude",
			src: `package fixture

const kind = "claude-code"
`,
			wantCount: 0,
		},
		{
			// subpkg sits under the real claude kind package's own
			// import path, so the subtree exclusion covers every
			// package nested under a kind package's import path,
			// excluding both "claude" and "claude-code" for it too.
			name:       "a package under a kind package's own path naming that kind's registered string is accepted",
			dirName:    "subpkg",
			importPath: "github.com/sortie-ai/sortie/internal/agent/claude/subpkg",
			src: `package fixture

const kind = "claude-code"
`,
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", tt.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parser.ParseFile: %v", err)
			}

			pkg := contractPackage{dirName: tt.dirName, importPath: tt.importPath}
			if tt.inTestFile {
				pkg.testFiles = []*ast.File{file}
			} else {
				pkg.files = []*ast.File{file}
			}
			got := checkAdapterContractPackage(fset, pkg)
			if len(got) != tt.wantCount {
				t.Errorf("checkAdapterContractPackage() returned %d violations, want %d: %+v", len(got), tt.wantCount, got)
			}
			if tt.wantSubstr != "" && !slices.ContainsFunc(got, func(v contractViolation) bool {
				return strings.Contains(v.text, tt.wantSubstr)
			}) {
				t.Errorf("checkAdapterContractPackage() violations = %+v, want one containing %q", got, tt.wantSubstr)
			}
		})
	}
}

// TestCheckContractCapture_DetectsViolations pins rule CAPTURE's, rule
// SINK's, and rule TEARDOWN's own logic against inline source
// fixtures, independent of the current state of any package under
// cmd/ or internal/, so a regression is caught even when every real
// launch site happens to comply. Each fixture is parsed as the single
// non-test file of a one-file package named by dirName; every case
// runs through both checkContractCapture and checkContractTeardown.
// Rule CAPTURE's own Stdout/Stderr check targets a direct assignment
// to exec.Cmd.Stdout or exec.Cmd.Stderr; rule SINK's targets a field of
// that name inside a procutil.CaptureParams composite literal instead,
// so the two never match the same syntax, and TEARDOWN's Cancel and
// WaitDelay checks overlap with neither.
func TestCheckContractCapture_DetectsViolations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		dirName    string
		importPath string
		src        string
		wantCount  int
		wantSubstr string
	}{
		{
			name:       "a chained CombinedOutput call is rejected",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import (
	"context"
	"os/exec"
)

func run(ctx context.Context) ([]byte, error) {
	return exec.CommandContext(ctx, "git", "status").CombinedOutput()
}
`,
			wantCount:  1,
			wantSubstr: "call procutil.RunCapture or procutil.StartCapture",
		},
		{
			name:       "a declared-then-called Run is rejected",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import "os/exec"

func run() error {
	cmd := exec.Command("git", "status")
	return cmd.Run()
}
`,
			wantCount:  1,
			wantSubstr: "waits on an exec.Cmd directly",
		},
		{
			name:       "a var-declared cmd's Output call is rejected",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import "os/exec"

func run() ([]byte, error) {
	var cmd *exec.Cmd
	cmd = exec.Command("git", "status")
	return cmd.Output()
}
`,
			wantCount:  1,
			wantSubstr: "waits on an exec.Cmd directly",
		},
		{
			name:       "a cmd parameter's Wait call is rejected",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import "os/exec"

func run(cmd *exec.Cmd) error {
	return cmd.Wait()
}
`,
			wantCount:  1,
			wantSubstr: "waits on an exec.Cmd directly",
		},
		{
			name:       "a struct field cmd's Start call is rejected",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import "os/exec"

type runner struct {
	cmd *exec.Cmd
}

func (s *runner) run() error {
	return s.cmd.Start()
}
`,
			wantCount:  1,
			wantSubstr: "waits on an exec.Cmd directly",
		},
		{
			name:       "an indexed function's returned cmd is rejected, assigned and chained",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import "os/exec"

func newGitCmd(dir string) *exec.Cmd {
	cmd := exec.Command("git", "status")
	cmd.Dir = dir
	return cmd
}

func runAssigned(dir string) error {
	cmd := newGitCmd(dir)
	return cmd.Run()
}

func runChained(dir string) error {
	return newGitCmd(dir).Start()
}
`,
			wantCount: 2,
		},
		{
			name:       "an os.Process field's Wait call is rejected",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import "os/exec"

func run(c *exec.Cmd) error {
	return c.Process.Wait()
}
`,
			wantCount:  1,
			wantSubstr: "waits on an os.Process directly",
		},
		{
			name:       "os.StartProcess and a later Wait on its result are both rejected",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import "os"

func run() error {
	p, _ := os.StartProcess("/bin/true", nil, &os.ProcAttr{})
	return p.Wait()
}
`,
			wantCount: 2,
		},
		{
			name:       "syscall.ForkExec is rejected",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import "syscall"

func run() (int, error) {
	return syscall.ForkExec("/bin/true", nil, nil)
}
`,
			wantCount:  1,
			wantSubstr: "starts a process outside os/exec",
		},
		{
			name:       "a direct Stdout assignment is rejected",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import (
	"io"
	"os/exec"
)

func run(cmd *exec.Cmd) {
	cmd.Stdout = io.Discard
}
`,
			wantCount:  1,
			wantSubstr: "assigns exec.Cmd.Stdout directly",
		},
		{
			name:       "a dot-import of os/exec is rejected once, on the import",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import . "os/exec"

func run() (*Cmd, error) {
	return Command("git", "status"), nil
}
`,
			wantCount:  1,
			wantSubstr: "dot-imports os/exec",
		},
		{
			name:       "a dot-import of os is rejected once, on the import",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import . "os"

func run() string {
	return Getenv("PATH")
}
`,
			wantCount:  1,
			wantSubstr: "dot-imports os",
		},
		{
			// A dot-imported procutil turns "CaptureParams{...}" into a
			// bare composite literal contractIsCaptureParamsLit cannot
			// recognize (it looks for a "procutil." selector), so
			// without the general dot-import ban this Stdout field,
			// resolving to os.Stdout rather than a bounded sink, would
			// evade rule SINK entirely instead of being reported.
			name:       "a dot-import of procutil hiding an unbounded sink is rejected once, on the import",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import (
	"os"
	"os/exec"

	. "github.com/sortie-ai/sortie/internal/agent/procutil"
)

func run(cmd *exec.Cmd) error {
	_, err := StartCapture(cmd, CaptureParams{Stdout: os.Stdout})
	return err
}
`,
			wantCount:  1,
			wantSubstr: "dot-imports github.com/sortie-ai/sortie/internal/agent/procutil",
		},
		{
			name:       "a sync.WaitGroup Wait in a file importing os/exec is accepted",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import (
	"os/exec"
	"sync"
)

func run(wg *sync.WaitGroup) {
	_ = exec.Command
	wg.Wait()
}
`,
			wantCount: 0,
		},
		{
			name:       "Wait on a *procutil.Capture is accepted",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/agent/procutil"

func run(c *procutil.Capture) {
	c.Wait()
}
`,
			wantCount: 0,
		},
		{
			name:       "Output on an unrelated type in a file that never imports os/exec is accepted",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

type buffer struct{}

func (b *buffer) Output() []byte { return nil }

func run(x *buffer) []byte {
	return x.Output()
}
`,
			wantCount: 0,
		},
		{
			name:       "StdinPipe is accepted",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import "os/exec"

func run(cmd *exec.Cmd) error {
	_, err := cmd.StdinPipe()
	return err
}
`,
			wantCount: 0,
		},
		{
			name:       "Signal on an *os.Process from os.FindProcess is accepted",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import (
	"os"
	"syscall"
)

func run(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(syscall.Signal(0))
}
`,
			wantCount: 0,
		},
		{
			name:       "a hand-wired exec.Cmd cancellation is rejected",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import "os/exec"

func launch(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return nil }
}
`,
			wantCount:  1,
			wantSubstr: "call procutil.SetGroupCancel or procutil.SetGroupKill",
		},
		{
			name:       "a hand-wired exec.Cmd wait delay is rejected",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import (
	"os/exec"
	"time"
)

func launch(cmd *exec.Cmd) {
	cmd.WaitDelay = 5 * time.Second
}
`,
			wantCount:  1,
			wantSubstr: "call procutil.SetGroupCancel or procutil.SetGroupKill",
		},
		{
			name:       "a same-named field in a package that never touches os/exec is accepted",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

type request struct {
	Cancel    func() error
	WaitDelay int
}

func configure(r *request) {
	r.Cancel = func() error { return nil }
	r.WaitDelay = 5
}
`,
			wantCount: 0,
		},
		{
			name:       "a hand-wired exec.Cmd cancellation under internal/workspace is rejected",
			dirName:    "workspace",
			importPath: "github.com/sortie-ai/sortie/internal/workspace",
			src: `package workspace

import "os/exec"

func launch(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return nil }
}
`,
			wantCount:  1,
			wantSubstr: "call procutil.SetGroupCancel or procutil.SetGroupKill",
		},
		{
			// The negative control: an os.Pipe write end is exactly the
			// sink [CaptureParams] warns against, since a reader that
			// stops draining it blocks Write and holds seal, and so
			// Wait, open indefinitely. Reproduces StartCapture's real
			// call shape rather than a synthetic type.
			name:       "an os.Pipe write end passed as a capture sink is rejected",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import (
	"os"
	"os/exec"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

func run(cmd *exec.Cmd) error {
	_, w, err := os.Pipe()
	if err != nil {
		return err
	}
	_, startErr := procutil.StartCapture(cmd, procutil.CaptureParams{Stdout: w})
	return startErr
}
`,
			wantCount:  1,
			wantSubstr: "CaptureParams.Stdout passes a writer not accepted as bounded",
		},
		{
			name:       "os.Stdout passed directly as a capture sink is rejected",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import (
	"os"
	"os/exec"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

func run(cmd *exec.Cmd) error {
	_, err := procutil.RunCapture(cmd, procutil.DefaultStopGrace, procutil.CaptureParams{Stdout: os.Stdout, Stderr: os.Stderr})
	return err
}
`,
			wantCount: 2,
		},
		{
			// The positive control, reproducing the shape every real
			// call site outside procutil uses: a local bytes.Buffer,
			// addressed and shared by both streams.
			name:       "a bytes.Buffer capture sink is accepted",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import (
	"bytes"
	"os/exec"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

func run(cmd *exec.Cmd) error {
	var combined bytes.Buffer
	_, startErr := procutil.StartCapture(cmd, procutil.CaptureParams{Stdout: &combined, Stderr: &combined})
	return startErr
}
`,
			wantCount: 0,
		},
		{
			// Reproduces workspace.RunHook's real shape: a package-local
			// bounded writer built with "&T{...}" and shared by both
			// streams, admitted through contractBoundedSinkTypes by name
			// rather than by structural inspection of its Write method.
			name:       "a locally-declared bounded writer capture sink is accepted",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import (
	"os/exec"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

type limitedBuffer struct{ max int }

func (lb *limitedBuffer) Write(p []byte) (int, error) { return len(p), nil }

func run(cmd *exec.Cmd) error {
	buf := &limitedBuffer{max: 1024}
	_, startErr := procutil.StartCapture(cmd, procutil.CaptureParams{Stdout: buf, Stderr: buf})
	return startErr
}
`,
			wantCount: 0,
		},
		{
			name:       "an explicit nil capture sink is accepted",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import (
	"os/exec"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

func run(cmd *exec.Cmd) error {
	_, startErr := procutil.StartCapture(cmd, procutil.CaptureParams{Stdout: nil})
	return startErr
}
`,
			wantCount: 0,
		},
		{
			name:       "a CaptureParams literal naming neither Stdout nor Stderr is accepted",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/agent/fixture",
			src: `package fixture

import (
	"os/exec"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

func run(cmd *exec.Cmd) error {
	_, startErr := procutil.RunCapture(cmd, procutil.DefaultStopGrace, procutil.CaptureParams{})
	return startErr
}
`,
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", tt.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parser.ParseFile: %v", err)
			}

			pkg := contractPackage{dirName: tt.dirName, importPath: tt.importPath, files: []*ast.File{file}}
			idx, fields := contractBuildModuleCmdIndex([]contractWalkedPackage{{pkg: pkg}})
			got := checkContractCapture(fset, pkg, idx, fields)
			got = append(got, checkContractTeardown(fset, file)...)

			if len(got) != tt.wantCount {
				t.Errorf("checkContractCapture()+checkContractTeardown() returned %d violations, want %d: %+v", len(got), tt.wantCount, got)
			}
			if tt.wantSubstr != "" && !slices.ContainsFunc(got, func(v contractViolation) bool {
				return strings.Contains(v.text, tt.wantSubstr)
			}) {
				t.Errorf("violations = %+v, want one containing %q", got, tt.wantSubstr)
			}
		})
	}
}

// TestCheckContractReaperLogger_DetectsViolations pins rule REAPER's
// own logic against inline source fixtures, independent of the current
// state of any package under cmd/ or internal/, so a regression is
// caught even when every real launch site happens to comply. The
// clean fixtures are the negative control: a call that already passes
// a logger the call site holds, whether in a local variable or a
// struct field, or through a chained slog.Default().With(...) already
// bound to a local before the call, must report no violation, so the
// rule is proven not to fire on the shape every real call site uses.
func TestCheckContractReaperLogger_DetectsViolations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		src        string
		wantCount  int
		wantSubstr string
	}{
		{
			name: "a nil logger is rejected",
			src: `package fixture

import (
	"os/exec"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

func run(cmd *exec.Cmd) {
	procutil.StartReaper(cmd, nil)
}
`,
			wantCount:  1,
			wantSubstr: "nil logger",
		},
		{
			name: "a bare slog.Default() call is rejected",
			src: `package fixture

import (
	"log/slog"
	"os/exec"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

func run(cmd *exec.Cmd) {
	procutil.StartReaper(cmd, slog.Default())
}
`,
			wantCount:  1,
			wantSubstr: "slog.Default()",
		},
		{
			name: "a chained slog.Default().With(...) call is rejected",
			src: `package fixture

import (
	"log/slog"
	"os/exec"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

func run(cmd *exec.Cmd) {
	procutil.StartReaper(cmd, slog.Default().With(slog.String("component", "fixture-adapter")))
}
`,
			wantCount:  1,
			wantSubstr: "slog.Default()",
		},
		{
			name: "a renamed procutil import does not evade the nil check",
			src: `package fixture

import (
	"os/exec"

	proc "github.com/sortie-ai/sortie/internal/agent/procutil"
)

func run(cmd *exec.Cmd) {
	proc.StartReaper(cmd, nil)
}
`,
			wantCount:  1,
			wantSubstr: "nil logger",
		},
		{
			name: "a dot-imported procutil is rejected outright",
			src: `package fixture

import (
	"os/exec"

	. "github.com/sortie-ai/sortie/internal/agent/procutil"
)

func run(cmd *exec.Cmd) {
	StartReaper(cmd, nil)
}
`,
			wantCount:  1,
			wantSubstr: "dot-imports procutil",
		},
		{
			name: "a local variable logger is accepted",
			src: `package fixture

import (
	"log/slog"
	"os/exec"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

func run(cmd *exec.Cmd, logger *slog.Logger) {
	procutil.StartReaper(cmd, logger)
}
`,
			wantCount: 0,
		},
		{
			name: "a struct field logger is accepted",
			src: `package fixture

import (
	"os/exec"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

type session struct {
	logger any
}

func (s *session) run(cmd *exec.Cmd) {
	procutil.StartReaper(cmd, s.logger)
}
`,
			wantCount: 0,
		},
		{
			name: "slog.Default() bound to a local before the call is accepted",
			src: `package fixture

import (
	"log/slog"
	"os/exec"

	"github.com/sortie-ai/sortie/internal/agent/procutil"
)

func run(cmd *exec.Cmd) {
	logger := slog.Default().With(slog.String("component", "fixture-adapter"))
	procutil.StartReaper(cmd, logger)
}
`,
			wantCount: 0,
		},
		{
			name: "a file that never imports procutil is untouched",
			src: `package fixture

func run() int { return 1 }
`,
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", tt.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parser.ParseFile: %v", err)
			}

			got := checkContractReaperLogger(fset, file)

			if len(got) != tt.wantCount {
				t.Errorf("checkContractReaperLogger() returned %d violations, want %d: %+v", len(got), tt.wantCount, got)
			}
			if tt.wantSubstr != "" && !slices.ContainsFunc(got, func(v contractViolation) bool {
				return strings.Contains(v.text, tt.wantSubstr)
			}) {
				t.Errorf("violations = %+v, want one containing %q", got, tt.wantSubstr)
			}
		})
	}
}

// TestCheckContractCapture_DetectsCrossPackageConstructorViolations pins
// that rule CAPTURE binds a call to a constructor declared in another
// walked package, not only one declared in the caller's own package.
// The producer fixture reproduces workspace.GitCommand's real shape - a
// context- and dir-taking function returning *exec.Cmd, built on
// exec.CommandContext - and the consumer fixture reproduces
// internal/orchestrator's real call shape, assigning its result to a
// local and hand-wiring Wait directly instead of going through
// procutil.RunCapture or procutil.StartCapture. idx and fields are
// built module-wide via contractBuildModuleCmdIndex across both fixture
// packages, the way TestContractCaptureAndTeardown builds them across
// the real walk; an idx built from the consumer package alone - the
// defect this test guards against - indexes no function under
// GitCommand's own import path and misses the call.
func TestCheckContractCapture_DetectsCrossPackageConstructorViolations(t *testing.T) {
	t.Parallel()

	const producerSrc = `package workspace

import (
	"context"
	"os/exec"
)

func GitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	return cmd
}
`

	const consumerSrc = `package orchestrator

import (
	"context"
	"os/exec"

	"github.com/sortie-ai/sortie/internal/workspace"
)

func runVerification(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", command)
}

func runGitDiff(ctx context.Context, workspacePath string, args ...string) error {
	cmd := workspace.GitCommand(ctx, workspacePath, args...)
	return cmd.Wait()
}
`

	fset := token.NewFileSet()
	producerFile, err := parser.ParseFile(fset, "workspace.go", producerSrc, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parser.ParseFile(producer): %v", err)
	}
	consumerFile, err := parser.ParseFile(fset, "orchestrator.go", consumerSrc, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parser.ParseFile(consumer): %v", err)
	}

	producerPkg := contractPackage{
		dirName:    "workspace",
		importPath: "github.com/sortie-ai/sortie/internal/workspace",
		files:      []*ast.File{producerFile},
	}
	consumerPkg := contractPackage{
		dirName:    "orchestrator",
		importPath: contractOrchestratorPath,
		files:      []*ast.File{consumerFile},
	}

	walked := []contractWalkedPackage{{pkg: producerPkg}, {pkg: consumerPkg}}
	idx, fields := contractBuildModuleCmdIndex(walked)

	got := checkContractCapture(fset, consumerPkg, idx, fields)
	if len(got) != 1 {
		t.Fatalf("checkContractCapture() on a caller of a cross-package constructor returned %d violations, want 1: %+v", len(got), got)
	}
	const wantSubstr = "waits on an exec.Cmd directly"
	if !strings.Contains(got[0].text, wantSubstr) {
		t.Errorf("violations = %+v, want one containing %q", got, wantSubstr)
	}

	producerGot := checkContractCapture(fset, producerPkg, idx, fields)
	if len(producerGot) != 0 {
		t.Errorf("checkContractCapture() on the constructor's own package returned %d violations, want 0: %+v", len(producerGot), producerGot)
	}
}

// TestCheckContractCapture_DetectsDotImportedConstructorViolations is the
// negative control for the hole an audit of
// TestCheckContractCapture_DetectsCrossPackageConstructorViolations
// found: contractFileImportAliases omits a dot import, since a dot
// import binds no name a selector could qualify, so a call reached
// through one - GitCommand written bare instead of workspace.GitCommand
// - resolved against nothing and the hand-wired Wait beneath it passed
// uncaught. The consumer here is the same shape as that test's, a dot
// import substituted for the named one; before the general dot-import
// ban in checkContractCaptureFile this returned zero violations, which
// an overlay probe against a pre-fix copy confirmed.
func TestCheckContractCapture_DetectsDotImportedConstructorViolations(t *testing.T) {
	t.Parallel()

	const producerSrc = `package workspace

import (
	"context"
	"os/exec"
)

func GitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	return cmd
}
`

	const consumerSrc = `package orchestrator

import (
	"context"

	. "github.com/sortie-ai/sortie/internal/workspace"
)

func runGitDiff(ctx context.Context, workspacePath string, args ...string) error {
	cmd := GitCommand(ctx, workspacePath, args...)
	return cmd.Wait()
}
`

	fset := token.NewFileSet()
	producerFile, err := parser.ParseFile(fset, "workspace.go", producerSrc, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parser.ParseFile(producer): %v", err)
	}
	consumerFile, err := parser.ParseFile(fset, "orchestrator.go", consumerSrc, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parser.ParseFile(consumer): %v", err)
	}

	producerPkg := contractPackage{
		dirName:    "workspace",
		importPath: "github.com/sortie-ai/sortie/internal/workspace",
		files:      []*ast.File{producerFile},
	}
	consumerPkg := contractPackage{
		dirName:    "orchestrator",
		importPath: contractOrchestratorPath,
		files:      []*ast.File{consumerFile},
	}

	walked := []contractWalkedPackage{{pkg: producerPkg}, {pkg: consumerPkg}}
	idx, fields := contractBuildModuleCmdIndex(walked)

	got := checkContractCapture(fset, consumerPkg, idx, fields)
	if len(got) != 1 {
		t.Fatalf("checkContractCapture() on a dot-importing caller of a cross-package constructor returned %d violations, want 1: %+v", len(got), got)
	}
	const wantSubstr = "dot-imports github.com/sortie-ai/sortie/internal/workspace"
	if !strings.Contains(got[0].text, wantSubstr) {
		t.Errorf("violations = %+v, want one containing %q", got, wantSubstr)
	}
}

// TestCheckContractCapture_ResolvesRenamedImportConstructorCalls pins
// that a renamed import - unlike a dot import - does not share the hole
// TestCheckContractCapture_DetectsDotImportedConstructorViolations
// closes: contractFileImportAliases keys aliasPaths from each import's
// own local identifier, alias or not, so ws.GitCommand resolves exactly
// as the unaliased workspace.GitCommand does and the hand-wired Wait
// stays caught. The consumer keeps runVerification and its direct
// os/exec import from TestCheckContractCapture_DetectsCrossPackageConstructorViolations's
// fixture: every real caller of a workspace constructor, such as
// internal/orchestrator/self_review.go, imports os/exec directly in
// the same file, and dropping that import here would exercise this
// function's early-return guard instead of the alias resolution this
// test targets.
func TestCheckContractCapture_ResolvesRenamedImportConstructorCalls(t *testing.T) {
	t.Parallel()

	const producerSrc = `package workspace

import (
	"context"
	"os/exec"
)

func GitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	return cmd
}
`

	const consumerSrc = `package orchestrator

import (
	"context"
	"os/exec"

	ws "github.com/sortie-ai/sortie/internal/workspace"
)

func runVerification(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", command)
}

func runGitDiff(ctx context.Context, workspacePath string, args ...string) error {
	cmd := ws.GitCommand(ctx, workspacePath, args...)
	return cmd.Wait()
}
`

	fset := token.NewFileSet()
	producerFile, err := parser.ParseFile(fset, "workspace.go", producerSrc, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parser.ParseFile(producer): %v", err)
	}
	consumerFile, err := parser.ParseFile(fset, "orchestrator.go", consumerSrc, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parser.ParseFile(consumer): %v", err)
	}

	producerPkg := contractPackage{
		dirName:    "workspace",
		importPath: "github.com/sortie-ai/sortie/internal/workspace",
		files:      []*ast.File{producerFile},
	}
	consumerPkg := contractPackage{
		dirName:    "orchestrator",
		importPath: contractOrchestratorPath,
		files:      []*ast.File{consumerFile},
	}

	walked := []contractWalkedPackage{{pkg: producerPkg}, {pkg: consumerPkg}}
	idx, fields := contractBuildModuleCmdIndex(walked)

	got := checkContractCapture(fset, consumerPkg, idx, fields)
	if len(got) != 1 {
		t.Fatalf("checkContractCapture() on a renamed-import caller of a cross-package constructor returned %d violations, want 1: %+v", len(got), got)
	}
	const wantSubstr = "waits on an exec.Cmd directly"
	if !strings.Contains(got[0].text, wantSubstr) {
		t.Errorf("violations = %+v, want one containing %q", got, wantSubstr)
	}
}

// TestCheckContractCapture_DetectsProducerOnlyImportViolations is the
// negative control for the hole checkContractCaptureFile's early return
// left open: a file that reaches a command through a cross-package
// constructor never has to import os/exec, os, syscall, windows, or
// procutil itself, so naming none of those five was wrongly treated as
// proof the file could not violate rule CAPTURE. The consumer here
// drops every import the sibling constructor tests keep around it -
// os/exec included - leaving only the producer's own import, and waits
// on the result directly the same way those tests' consumers do.
// Before the producer-path check joined the early return's condition,
// an overlay run against a pre-fix copy of this file confirmed this
// case reported zero violations.
func TestCheckContractCapture_DetectsProducerOnlyImportViolations(t *testing.T) {
	t.Parallel()

	const producerSrc = `package workspace

import (
	"context"
	"os/exec"
)

func GitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	return cmd
}
`

	const consumerSrc = `package orchestrator

import (
	"context"

	"github.com/sortie-ai/sortie/internal/workspace"
)

func runGitDiff(ctx context.Context, workspacePath string, args ...string) error {
	cmd := workspace.GitCommand(ctx, workspacePath, args...)
	return cmd.Wait()
}
`

	fset := token.NewFileSet()
	producerFile, err := parser.ParseFile(fset, "workspace.go", producerSrc, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parser.ParseFile(producer): %v", err)
	}
	consumerFile, err := parser.ParseFile(fset, "orchestrator.go", consumerSrc, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parser.ParseFile(consumer): %v", err)
	}

	producerPkg := contractPackage{
		dirName:    "workspace",
		importPath: "github.com/sortie-ai/sortie/internal/workspace",
		files:      []*ast.File{producerFile},
	}
	consumerPkg := contractPackage{
		dirName:    "orchestrator",
		importPath: contractOrchestratorPath,
		files:      []*ast.File{consumerFile},
	}

	walked := []contractWalkedPackage{{pkg: producerPkg}, {pkg: consumerPkg}}
	idx, fields := contractBuildModuleCmdIndex(walked)

	got := checkContractCapture(fset, consumerPkg, idx, fields)
	if len(got) != 1 {
		t.Fatalf("checkContractCapture() on a producer-only-import caller of a cross-package constructor returned %d violations, want 1: %+v", len(got), got)
	}
	const wantSubstr = "waits on an exec.Cmd directly"
	if !strings.Contains(got[0].text, wantSubstr) {
		t.Errorf("violations = %+v, want one containing %q", got, wantSubstr)
	}

	producerGot := checkContractCapture(fset, producerPkg, idx, fields)
	if len(producerGot) != 0 {
		t.Errorf("checkContractCapture() on the constructor's own package returned %d violations, want 0: %+v", len(producerGot), producerGot)
	}
}

// TestContractAllowlist_BlockerRuleHasNoExemptions pins that no package
// carries a contractAllowlist entry for ruleBLOCKER, so every
// tracker-registering package, including file, is subject to it.
func TestContractAllowlist_BlockerRuleHasNoExemptions(t *testing.T) {
	t.Parallel()

	for dirName, reasons := range contractAllowlist {
		if _, exempt := reasons[ruleBLOCKER]; exempt {
			t.Errorf("contractAllowlist[%q] exempts %q, want no exemption from that rule", dirName, ruleBLOCKER)
		}
	}
}

// contractIdentityReporter is the reporting surface the two identity-rule
// staleness checks below need. *testing.T satisfies it, and so does the
// fake the guard test drives them with, so both tests exercise one
// implementation of each check rather than a copy of it.
type contractIdentityReporter interface {
	Errorf(format string, args ...any)
}

// contractCheckIdentityEvaluated reports when rule IDENTITY was
// evaluated for no package under the agent family root.
func contractCheckIdentityEvaluated(r contractIdentityReporter, walked []contractWalkedPackage) {
	evaluated := 0
	for _, w := range walked {
		if contractPathIsUnder(w.pkg.importPath, contractAgentFamilyPath) && !contractExempt(w.pkg.dirName, ruleIDENTITY) {
			evaluated++
		}
	}
	if evaluated == 0 {
		r.Errorf("rule %s was evaluated for no package under %s, want at least one", ruleIDENTITY, contractAgentFamilyPath)
	}
}

// contractCheckIdentityAllowlist reports each contractAllowlist entry
// that exempts rule IDENTITY for a directory the walk did not find.
func contractCheckIdentityAllowlist(r contractIdentityReporter, found map[string]bool) {
	for dirName, reasons := range contractAllowlist {
		if _, exempt := reasons[ruleIDENTITY]; !exempt {
			continue
		}
		if !found[dirName] {
			r.Errorf("contractAllowlist[%q] exempts %s, but the walk under %s did not find a directory named %q", dirName, ruleIDENTITY, contractAgentFamilyPath, dirName)
		}
	}
}

// contractCheckStopGraceEvaluated reports when rule STOPGRACE was
// evaluated for no package under the agent family root.
func contractCheckStopGraceEvaluated(r contractIdentityReporter, walked []contractWalkedPackage) {
	evaluated := 0
	for _, w := range walked {
		if contractPathIsUnder(w.pkg.importPath, contractAgentFamilyPath) && !contractExempt(w.pkg.dirName, ruleSTOPGRACE) {
			evaluated++
		}
	}
	if evaluated == 0 {
		r.Errorf("rule %s was evaluated for no package under %s, want at least one", ruleSTOPGRACE, contractAgentFamilyPath)
	}
}

// contractCheckStopGraceAllowlist reports each contractAllowlist entry
// that exempts rule STOPGRACE for a directory the walk did not find.
func contractCheckStopGraceAllowlist(r contractIdentityReporter, found map[string]bool) {
	for dirName, reasons := range contractAllowlist {
		if _, exempt := reasons[ruleSTOPGRACE]; !exempt {
			continue
		}
		if !found[dirName] {
			r.Errorf("contractAllowlist[%q] exempts %s, but the walk under %s did not find a directory named %q", dirName, ruleSTOPGRACE, contractAgentFamilyPath, dirName)
		}
	}
}

// contractCaptureTeardownEvaluationRoots names the three roots the
// CAPTURE, SINK, and TEARDOWN staleness guards each require at least
// one evaluated, non-exempt file under.
var contractCaptureTeardownEvaluationRoots = []string{
	"github.com/sortie-ai/sortie/internal/agent",
	"github.com/sortie-ai/sortie/internal/orchestrator",
	"github.com/sortie-ai/sortie/internal/workspace",
}

// contractCheckWideRuleEvaluated reports, for each of
// contractCaptureTeardownEvaluationRoots, when rule was evaluated for
// no non-exempt package carrying a non-test file under it.
func contractCheckWideRuleEvaluated(r contractIdentityReporter, rule contractRule, walked []contractWalkedPackage) {
	for _, root := range contractCaptureTeardownEvaluationRoots {
		evaluated := false
		for _, w := range walked {
			if contractPathIsUnder(w.pkg.importPath, root) && len(w.pkg.files) > 0 && !contractExempt(w.pkg.dirName, rule) {
				evaluated = true
				break
			}
		}
		if !evaluated {
			r.Errorf("rule %s was evaluated for no file under %s, want at least one", rule, root)
		}
	}
}

// contractCheckWideRuleAllowlist reports each contractAllowlist entry
// that exempts rule for a directory the walk did not find.
func contractCheckWideRuleAllowlist(r contractIdentityReporter, rule contractRule, found map[string]bool) {
	for dirName, reasons := range contractAllowlist {
		if _, exempt := reasons[rule]; !exempt {
			continue
		}
		if !found[dirName] {
			r.Errorf("contractAllowlist[%q] exempts %s, but the walk did not find a directory named %q", dirName, rule, dirName)
		}
	}
}

// TestContractIdentityRule_AppliesAndStaysCurrent guards rule IDENTITY
// against going stale: it fails when the rule was evaluated for no
// package under the agent family root, when the rule's own token
// extraction reported an error, or when a contractAllowlist entry
// naming ruleIDENTITY names a directory the walk did not find.
func TestContractIdentityRule_AppliesAndStaysCurrent(t *testing.T) {
	fset := token.NewFileSet()
	walked, _ := contractWalkRoot(t, fset, filepath.Join("..", "agent"), contractAgentFamilyPath)

	found := map[string]bool{}
	for _, w := range walked {
		found[w.pkg.dirName] = true
	}

	contractCheckIdentityEvaluated(t, walked)

	for _, v := range contractAgentIdentitySnapshotData().extractionErrors {
		t.Errorf("%s: %s", v.pos, v.text)
	}

	contractCheckIdentityAllowlist(t, found)
}

// contractStalenessFakeReporter records Errorf calls instead of failing
// the enclosing test, so TestContractIdentityRule_StalenessGuardCatchesRealBreaks
// can drive TestContractIdentityRule_AppliesAndStaysCurrent's own checks
// against deliberately-broken synthetic input without reddening this
// test file's own run.
type contractStalenessFakeReporter struct {
	errors []string
}

func (f *contractStalenessFakeReporter) Errorf(format string, _ ...any) {
	f.errors = append(f.errors, format)
}

// TestContractIdentityRule_StalenessGuardCatchesRealBreaks proves the two
// checks TestContractIdentityRule_AppliesAndStaysCurrent performs are
// themselves capable of failing, not merely capable of passing against
// the current tree: fed a walk that evaluated rule IDENTITY for no
// package under the agent family root, or an allowlist naming a
// directory that walk did not find, each check must record a failure.
// Neither subtest runs in parallel: the second temporarily replaces the
// package-level contractAllowlist, which a concurrently-running fixture
// test also reads.
func TestContractIdentityRule_StalenessGuardCatchesRealBreaks(t *testing.T) {
	t.Run("zero packages evaluated under the agent family root", func(t *testing.T) {
		walked := []contractWalkedPackage{
			{pkg: contractPackage{dirName: "fixture", importPath: "github.com/sortie-ai/sortie/internal/tracker/fixture"}},
		}

		reporter := &contractStalenessFakeReporter{}
		contractCheckIdentityEvaluated(reporter, walked)

		if len(reporter.errors) == 0 {
			t.Fatalf("staleness guard recorded no failure for a walk carrying no package under %s, want at least one", contractAgentFamilyPath)
		}
	})

	t.Run("an allowlist entry names a directory the walk did not find", func(t *testing.T) {
		original := contractAllowlist
		contractAllowlist = map[string]map[contractRule]string{
			"ghost-adapter": {ruleIDENTITY: "does not exist on disk"},
		}
		t.Cleanup(func() { contractAllowlist = original })

		found := map[string]bool{"claude": true, "codex": true, "mock": true}

		reporter := &contractStalenessFakeReporter{}
		contractCheckIdentityAllowlist(reporter, found)

		if len(reporter.errors) == 0 {
			t.Fatalf("staleness guard recorded no failure for an allowlist entry naming a directory absent from the walk, want at least one")
		}
	})
}

// TestContractStopGraceRule_AppliesAndStaysCurrent guards rule
// STOPGRACE against going stale: it fails when the rule was evaluated
// for no package under the agent family root, or when a
// contractAllowlist entry naming ruleSTOPGRACE names a directory the
// walk did not find.
func TestContractStopGraceRule_AppliesAndStaysCurrent(t *testing.T) {
	fset := token.NewFileSet()
	walked, _ := contractWalkRoot(t, fset, filepath.Join("..", "agent"), contractAgentFamilyPath)

	found := map[string]bool{}
	for _, w := range walked {
		found[w.pkg.dirName] = true
	}

	contractCheckStopGraceEvaluated(t, walked)
	contractCheckStopGraceAllowlist(t, found)
}

// TestContractStopGraceRule_StalenessGuardCatchesRealBreaks proves the
// two checks TestContractStopGraceRule_AppliesAndStaysCurrent performs
// are themselves capable of failing, not merely capable of passing
// against the current tree: fed a walk that evaluated rule STOPGRACE
// for no package under the agent family root, or an allowlist naming a
// directory that walk did not find, each check must record a failure.
// The second subtest temporarily replaces the package-level
// contractAllowlist, which a concurrently-running fixture test also
// reads, so neither subtest runs in parallel.
func TestContractStopGraceRule_StalenessGuardCatchesRealBreaks(t *testing.T) {
	t.Run("zero packages evaluated under the agent family root", func(t *testing.T) {
		walked := []contractWalkedPackage{
			{pkg: contractPackage{dirName: "fixture", importPath: "github.com/sortie-ai/sortie/internal/tracker/fixture"}},
		}

		reporter := &contractStalenessFakeReporter{}
		contractCheckStopGraceEvaluated(reporter, walked)

		if len(reporter.errors) == 0 {
			t.Fatalf("staleness guard recorded no failure for a walk carrying no package under %s, want at least one", contractAgentFamilyPath)
		}
	})

	t.Run("an allowlist entry names a directory the walk did not find", func(t *testing.T) {
		original := contractAllowlist
		contractAllowlist = map[string]map[contractRule]string{
			"ghost-adapter": {ruleSTOPGRACE: "does not exist on disk"},
		}
		t.Cleanup(func() { contractAllowlist = original })

		found := map[string]bool{"claude": true, "codex": true, "mock": true}

		reporter := &contractStalenessFakeReporter{}
		contractCheckStopGraceAllowlist(reporter, found)

		if len(reporter.errors) == 0 {
			t.Fatalf("staleness guard recorded no failure for an allowlist entry naming a directory absent from the walk, want at least one")
		}
	})
}

// TestContractCaptureAndTeardownRule_AppliesAndStaysCurrent guards
// rules CAPTURE, SINK, TEARDOWN, and REAPER against going stale: each
// fails when it was evaluated for no non-exempt file under
// internal/agent, internal/orchestrator, or internal/workspace, or
// when a contractAllowlist entry naming it names a directory the walk
// did not find.
func TestContractCaptureAndTeardownRule_AppliesAndStaysCurrent(t *testing.T) {
	fset := token.NewFileSet()
	walked := contractWalkCaptureAndTeardown(t, fset)

	found := map[string]bool{}
	for _, w := range walked {
		found[w.pkg.dirName] = true
	}

	contractCheckWideRuleEvaluated(t, ruleCAPTURE, walked)
	contractCheckWideRuleAllowlist(t, ruleCAPTURE, found)
	contractCheckWideRuleEvaluated(t, ruleSINK, walked)
	contractCheckWideRuleAllowlist(t, ruleSINK, found)
	contractCheckWideRuleEvaluated(t, ruleTEARDOWN, walked)
	contractCheckWideRuleAllowlist(t, ruleTEARDOWN, found)
	contractCheckWideRuleEvaluated(t, ruleREAPER, walked)
	contractCheckWideRuleAllowlist(t, ruleREAPER, found)
}

// TestContractCaptureAndTeardownRule_StalenessGuardCatchesRealBreaks
// proves the checks TestContractCaptureAndTeardownRule_AppliesAndStaysCurrent
// performs are themselves capable of failing, not merely capable of
// passing against the current tree: fed a walk that evaluated the rule
// for no file under any of the three required roots, or an allowlist
// naming a directory that walk did not find, each check must record a
// failure. The second subtest temporarily replaces the package-level
// contractAllowlist, which a concurrently-running fixture test also
// reads, so neither subtest runs in parallel.
func TestContractCaptureAndTeardownRule_StalenessGuardCatchesRealBreaks(t *testing.T) {
	for _, rule := range []contractRule{ruleCAPTURE, ruleSINK, ruleTEARDOWN, ruleREAPER} {
		t.Run(string(rule)+": zero files evaluated under a required root", func(t *testing.T) {
			walked := []contractWalkedPackage{
				{pkg: contractPackage{dirName: "fixture", importPath: "github.com/sortie-ai/sortie/internal/tracker/fixture"}},
			}

			reporter := &contractStalenessFakeReporter{}
			contractCheckWideRuleEvaluated(reporter, rule, walked)

			if len(reporter.errors) == 0 {
				t.Fatalf("staleness guard recorded no failure for a walk carrying no file under any required root, want at least one")
			}
		})

		t.Run(string(rule)+": an allowlist entry names a directory the walk did not find", func(t *testing.T) {
			original := contractAllowlist
			contractAllowlist = map[string]map[contractRule]string{
				"ghost-adapter": {rule: "does not exist on disk"},
			}
			t.Cleanup(func() { contractAllowlist = original })

			found := map[string]bool{"procutil": true}

			reporter := &contractStalenessFakeReporter{}
			contractCheckWideRuleAllowlist(reporter, rule, found)

			if len(reporter.errors) == 0 {
				t.Fatalf("staleness guard recorded no failure for an allowlist entry naming a directory absent from the walk, want at least one")
			}
		})
	}
}

// TestResolveContractImportName pins that the qualifier is read from the
// file's own import declaration, including an aliased import, rather
// than assumed to be the literal identifier "registry".
func TestResolveContractImportName(t *testing.T) {
	t.Parallel()

	const src = `package fixture

import r "github.com/sortie-ai/sortie/internal/registry"

var _ = r.Trackers
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parser.ParseFile: %v", err)
	}

	got := resolveContractImportName(file, contractRegistryImportPath)
	if got != "r" {
		t.Errorf("resolveContractImportName() = %q, want %q (the aliased local name)", got, "r")
	}

	gotAbsent := resolveContractImportName(file, contractTrackermetricsImportPath)
	if gotAbsent != "" {
		t.Errorf("resolveContractImportName() for an unimported path = %q, want empty", gotAbsent)
	}
}

// TestContractSharedFamilyPackages_RegisterNoKind guards
// contractSharedFamilyPackages against going stale: it fails when an
// entry carries an empty reason, names a key contractSharedPackageDir
// cannot resolve, names a directory with no parsable non-test Go file,
// names a package that contractPackageRegistersKind now reports true
// for, or carries coreImportable true for a directory whose non-test
// files import testing. It enumerates each named directory with
// os.ReadDir alone, never descending into a subdirectory, so a permitted
// package's verdict never depends on a subpackage the map does not name.
func TestContractSharedFamilyPackages_RegisterNoKind(t *testing.T) {
	t.Parallel()

	for importPath, pkg := range contractSharedFamilyPackages {
		if pkg.reason == "" {
			t.Errorf("contractSharedFamilyPackages[%q] carries an empty reason", importPath)
		}

		dir, err := contractSharedPackageDir(importPath)
		if err != nil {
			t.Errorf("contractSharedPackageDir(%q): %v", importPath, err)
			continue
		}

		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Errorf("os.ReadDir(%q) for %q: %v", dir, importPath, err)
			continue
		}

		fset := token.NewFileSet()
		var files []*ast.File
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if parseErr != nil {
				t.Errorf("parse %s: %v", path, parseErr)
				continue
			}
			files = append(files, file)
		}
		if len(files) == 0 {
			t.Errorf("%q names directory %s, which yielded no parsable non-test Go file", importPath, dir)
			continue
		}

		if contractPackageRegistersKind(files) {
			t.Errorf("%q is a permitted shared package but registers a kind", importPath)
		}

		if !pkg.coreImportable {
			continue
		}
		for _, file := range files {
			if resolveContractImportName(file, "testing") == "" {
				continue
			}
			t.Errorf("%q carries coreImportable true but %s imports testing", importPath, fset.Position(file.Pos()).Filename)
		}
	}
}

// TestCheckOrchestratorContract_DetectsViolations pins the core-import
// decision table against inline source fixtures, independent of the
// current state of internal/orchestrator, so a regression in the rule is
// caught even when every real file happens to comply. Each fixture is
// parsed as the single file of a one-file package.
func TestCheckOrchestratorContract_DetectsViolations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		dirName    string
		importPath string
		src        string
		inTestFile bool
		wantCount  int
		wantReason string
	}{
		{
			name:       "a named import of a kind package is rejected with the registry reason",
			dirName:    "orchestrator",
			importPath: contractOrchestratorPath,
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/scm/github"
`,
			wantCount:  1,
			wantReason: contractCoreRegistryReason,
		},
		{
			name:       "a blank import of a kind package outside a test file is rejected with the registration reason",
			dirName:    "orchestrator",
			importPath: contractOrchestratorPath,
			src: `package fixture

import _ "github.com/sortie-ai/sortie/internal/scm/github"
`,
			wantCount:  1,
			wantReason: contractCoreRegistrationReason,
		},
		{
			name:       "a blank import of a kind package inside a test file is accepted",
			dirName:    "orchestrator",
			importPath: contractOrchestratorPath,
			src: `package fixture

import _ "github.com/sortie-ai/sortie/internal/scm/github"
`,
			inTestFile: true,
			wantCount:  0,
		},
		{
			name:       "a named import of a kind package inside a test file is still rejected",
			dirName:    "orchestrator",
			importPath: contractOrchestratorPath,
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/scm/github"
`,
			inTestFile: true,
			wantCount:  1,
			wantReason: contractCoreRegistryReason,
		},
		{
			name:       "a blank import of the agent mock kind package inside a test file is accepted",
			dirName:    "orchestrator",
			importPath: contractOrchestratorPath,
			src: `package fixture

import _ "github.com/sortie-ai/sortie/internal/agent/mock"
`,
			inTestFile: true,
			wantCount:  0,
		},
		{
			name:       "a named import of the permitted scmcore package is accepted",
			dirName:    "orchestrator",
			importPath: contractOrchestratorPath,
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/scm/scmcore"
`,
			wantCount: 0,
		},
		{
			name:       "a named import of the permitted procutil package is accepted",
			dirName:    "orchestrator",
			importPath: contractOrchestratorPath,
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/agent/procutil"
`,
			wantCount: 0,
		},
		{
			name:       "a named import of an agent kind package is rejected",
			dirName:    "orchestrator",
			importPath: contractOrchestratorPath,
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/agent/claude"
`,
			wantCount:  1,
			wantReason: contractCoreRegistryReason,
		},
		{
			name:       "a named import of a notify kind package is rejected",
			dirName:    "orchestrator",
			importPath: contractOrchestratorPath,
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/notify/slack"
`,
			wantCount:  1,
			wantReason: contractCoreRegistryReason,
		},
		{
			name:       "a named import of a tracker kind package is rejected",
			dirName:    "orchestrator",
			importPath: contractOrchestratorPath,
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/tracker/jira"
`,
			wantCount:  1,
			wantReason: contractCoreRegistryReason,
		},
		{
			name:       "imports outside every family root are accepted",
			dirName:    "orchestrator",
			importPath: contractOrchestratorPath,
			src: `package fixture

import (
	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/registry"
)
`,
			wantCount: 0,
		},
		{
			name:       "a standard-library import is accepted",
			dirName:    "orchestrator",
			importPath: contractOrchestratorPath,
			src: `package fixture

import "net/http"
`,
			wantCount: 0,
		},
		{
			name:       "a named import of a family root itself is rejected",
			dirName:    "orchestrator",
			importPath: contractOrchestratorPath,
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/scm"
`,
			wantCount:  1,
			wantReason: contractCoreRegistryReason,
		},
		{
			name:       "a subpath of a permitted package is rejected, because the permit map matches exactly",
			dirName:    "orchestrator",
			importPath: contractOrchestratorPath,
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/scm/scmcore/inner"
`,
			wantCount:  1,
			wantReason: contractCoreRegistryReason,
		},
		{
			name:       "a path that merely shares a prefix segment with a family root is accepted",
			dirName:    "orchestrator",
			importPath: contractOrchestratorPath,
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/scmwatch"
`,
			wantCount: 0,
		},
		{
			name:       "a non-test orchestrator file importing the shared agent test-support package is rejected",
			dirName:    "orchestrator",
			importPath: contractOrchestratorPath,
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/agent/agenttest"
`,
			wantCount:  1,
			wantReason: contractCoreTestSupportReason,
		},
		{
			name:       "an orchestrator test file importing the shared agent test-support package by name is accepted",
			dirName:    "orchestrator",
			importPath: contractOrchestratorPath,
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/agent/agenttest"
`,
			inTestFile: true,
			wantCount:  0,
		},
		{
			name:       "a non-test orchestrator file importing the permitted agentcore package is accepted",
			dirName:    "orchestrator",
			importPath: contractOrchestratorPath,
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/agent/agentcore"
`,
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", tt.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parser.ParseFile: %v", err)
			}

			pkg := contractPackage{dirName: tt.dirName, importPath: tt.importPath}
			if tt.inTestFile {
				pkg.testFiles = []*ast.File{file}
			} else {
				pkg.files = []*ast.File{file}
			}
			got := checkCoreContractPackage(fset, pkg)
			if len(got) != tt.wantCount {
				t.Fatalf("checkCoreContractPackage() returned %d violations, want %d: %+v", len(got), tt.wantCount, got)
			}
			if tt.wantCount == 0 {
				return
			}
			if !strings.HasSuffix(got[0].text, tt.wantReason) {
				t.Errorf("checkCoreContractPackage() violation text = %q, want suffix %q", got[0].text, tt.wantReason)
			}
		})
	}
}

// TestContractPackageRegistersKind pins the generalized registration
// predicate against inline fixtures, so it stays true for every registry
// namespace and for a qualifier resolved through an import alias, not
// only the literal identifier "registry".
func TestContractPackageRegistersKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		src  string
		want bool
	}{
		{
			name: "a package registering through registry.Agents.Register",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Agents.Register("fixture", newFixtureAgent)
}
`,
			want: true,
		},
		{
			name: "a package registering through registry.Notifiers.RegisterWithMeta",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Notifiers.RegisterWithMeta("fixture", newFixtureNotifier, struct{}{})
}
`,
			want: true,
		},
		{
			name: "a package registering through an aliased registry import",
			src: `package fixture

import reg "github.com/sortie-ai/sortie/internal/registry"

func init() {
	reg.SCMAdapters.Register("fixture", newFixtureSCMAdapter)
}
`,
			want: true,
		},
		{
			name: "a package that imports the registry and calls neither method",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

var _ = registry.TrackerMeta{}
`,
			want: false,
		},
		{
			name: "a package that does not import the registry at all",
			src: `package fixture

func doWork() {}
`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "fixture.go", tt.src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parser.ParseFile: %v", err)
			}

			got := contractPackageRegistersKind([]*ast.File{file})
			if got != tt.want {
				t.Errorf("contractPackageRegistersKind() = %v, want %v", got, tt.want)
			}
		})
	}
}
