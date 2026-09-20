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
// "registry" qualifier from per file, so an aliased import cannot evade
// the check.
const contractRegistryImportPath = "github.com/sortie-ai/sortie/internal/registry"

// contractTrackermetricsImportPath is resolved per file so a Track call is
// caught regardless of the local import alias.
const contractTrackermetricsImportPath = "github.com/sortie-ai/sortie/internal/trackermetrics"

// contractProcutilImportPath is resolved per file so an aliased import
// cannot evade rule STOPGRACE.
const contractProcutilImportPath = "github.com/sortie-ai/sortie/internal/agent/procutil"

// contractBanTable maps a name extracted into a shared package to its
// owner; a top-level function re-declaring one is a violation. The owner
// records where the extraction landed rather than a universal
// requirement, so a package in another family may satisfy the rule with a
// different name instead. The agent-family entries name a single owner
// because persistent JSON-RPC framing has no alternative.
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

// contractTrackerAdapterMethods are the tracker operation methods rule
// METRICS requires a trackermetrics.Track call inside when the package
// registers a tracker kind. FetchIssueBlockers is a domain.BlockerReader
// method; the rest are domain.TrackerAdapter methods.
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

const (
	contractTrackerFamilyPath = "github.com/sortie-ai/sortie/internal/tracker"
	contractSCMFamilyPath     = "github.com/sortie-ai/sortie/internal/scm"
	contractOrchestratorPath  = "github.com/sortie-ai/sortie/internal/orchestrator"
)

const (
	contractAgentFamilyPath  = "github.com/sortie-ai/sortie/internal/agent"
	contractNotifyFamilyPath = "github.com/sortie-ai/sortie/internal/notify"
	contractInternalPrefix   = "github.com/sortie-ai/sortie/internal/"
)

// contractFamilyRoots is the ban surface both the adapter-to-adapter arm
// of contractImportBanReason and the core-import rule match against.
var contractFamilyRoots = []string{
	contractTrackerFamilyPath,
	contractSCMFamilyPath,
	contractAgentFamilyPath,
	contractNotifyFamilyPath,
}

// contractSharedPackage records why a package under a family root holds no
// adapter and whether the orchestrator's production code may import it.
// Rule IMPORT consults presence alone; the core-import rule also consults
// coreImportable.
type contractSharedPackage struct {
	reason         string
	coreImportable bool
}

// contractSharedFamilyPackages names each adapter-less package under a
// family root, so it may be imported by a package under a family root,
// and states why. Keys match exactly, so a subpackage needs its own
// entry. A package absent here may be imported only by itself and
// packages under its own path. An entry whose coreImportable is false
// stays importable across family roots but not by the orchestrator.
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

// contractAllowlist exempts a package (by its directory base name) from
// one rule and states why; it stays subject to every other rule.
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

// The outcomes checkContractCoreImports renders as
// "imports " + path + "; " + reason.
const (
	contractCoreRegistryReason     = "the orchestrator resolves an adapter kind through the registry rather than importing its package"
	contractCoreRegistrationReason = "cmd/sortie owns the blank imports that trigger kind registration"
	contractCoreTestSupportReason  = "the orchestrator's production code must not import a test-support package"
)

type contractViolation struct {
	pos  token.Position
	text string
}

// contractWalkedPackage pairs a package with the directory it was found
// at, because contractPackage's base name cannot order packages across
// roots.
type contractWalkedPackage struct {
	dir string
	pkg contractPackage
}

// contractPackage carries one package's identity and parsed files.
// Non-test files feed rules BAN, METRICS, and HOOK; test files are parsed
// imports-only and feed rule IMPORT alone.
type contractPackage struct {
	dirName    string
	importPath string
	files      []*ast.File
	testFiles  []*ast.File
}

// resolveContractImportName returns the local identifier a file binds to
// importPath, or "" when absent. It reads the file's own import
// declarations, so an aliased import cannot evade the check.
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
// through a leading address-of so both value and pointer literals are
// recognized.
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

// compositeLitKeyValue returns the value of the field keyed by key in the
// composite literal expr denotes, or nil when absent.
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

func compositeLitHasKey(expr ast.Expr, key string) bool {
	return compositeLitKeyValue(expr, key) != nil
}

// packageReferencesIdentifier reports whether any file references the bare
// identifier name, catching both a plain reference and a selector's
// trailing field name (e.g. issue.BlockersUnresolved).
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

// contractPackageRegistersKind reports whether any file calls Register or
// RegisterWithMeta on any selector of the registry qualifier, resolved per
// file so an aliased import cannot evade it. Unlike contractRegistrationFacts
// it does not require the middle selector to be Trackers.
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

type contractSharedPackageDirError string

func (e contractSharedPackageDirError) Error() string { return string(e) }

// contractSharedPackageDir maps a contractSharedFamilyPackages key to its
// directory relative to this package, joining with filepath.Join so the
// result carries the platform separator.
func contractSharedPackageDir(importPath string) (string, error) {
	if !strings.HasPrefix(importPath, contractInternalPrefix) {
		return "", contractSharedPackageDirError("permit-map key " + importPath + " does not start with " + contractInternalPrefix)
	}
	segments := strings.Split(strings.TrimPrefix(importPath, contractInternalPrefix), "/")
	return filepath.Join(append([]string{".."}, segments...)...), nil
}

// checkContractBan reports a violation for every top-level function whose
// name is a contractBanTable entry.
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
// wire by hand; either one rebuilds the process-group teardown
// contractTeardownOwner owns. Setting one and forgetting the other keeps
// the os/exec default, so a cancelled context force-kills the direct child
// alone and its descendants outlive the cancellation.
var contractTeardownFields = map[string]bool{
	"Cancel":    true,
	"WaitDelay": true,
}

const contractTeardownOwner = "procutil.SetGroupCancel or procutil.SetGroupKill"

// checkContractTeardown reports a violation for every assignment to an
// exec.Cmd teardown field, only when file imports os/exec so a same-named
// field on an unrelated type is never matched.
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

const contractCaptureOwner = "procutil.RunCapture or procutil.StartCapture"

// contractCaptureDotImportReason is the text rules CAPTURE and SINK give
// for any dot import: it binds no name a qualifier can key to, so a call,
// constant, or sink type reached through it is unresolvable and cannot be
// told apart from one hiding a process launch or an unbounded sink.
const contractCaptureDotImportReason = "which this rule cannot resolve a bound identifier, capture sink, or command constructor through; import it by name"

// contractBoundedSinkTypes names the sink types [procutil.CaptureParams]
// admits for Stdout and Stderr. [procutil.Capture.Wait] copies into them
// and [sinkWriter.seal] shares a Write's lock, so a blocking Write holds
// Wait open past every bound. Each entry returns from Write immediately
// (discards, caps, or grows in memory); a type absent here is presumed to
// block until rule SINK is extended to admit it.
var contractBoundedSinkTypes = map[string]string{
	"bytes.Buffer":  "grows in memory and never blocks on Write",
	"limitedBuffer": "drops the earliest bytes once its cap is exceeded",
	"cappedWriter":  "discards bytes past its cap and always reports success",
}

// contractSinkTypeOwner is the map rule SINK directs a caller to extend
// when a new bounded sink type needs admitting.
const contractSinkTypeOwner = "contractBoundedSinkTypes"

// contractCmdIndex records every top-level function and method returning
// *exec.Cmd or exec.Cmd in a package's non-test files: a function keyed by
// "importPath.Name", a method by its bare name (a call site names a method
// without a receiver qualifier). producerPaths holds each indexed
// function's import path, letting a caller recognize a file that reaches a
// command through a constructor it names only by import.
type contractCmdIndex struct {
	funcs         map[string]bool
	methods       map[string]bool
	producerPaths map[string]bool
}

// contractCmdFields records every struct field typed *exec.Cmd or exec.Cmd
// in a package's non-test files.
type contractCmdFields map[string]bool

// contractTypeIsExecCmd reports whether expr names exec.Cmd or *exec.Cmd
// under file's own import name for os/exec.
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

// contractBuildCmdIndex adds every exec.Cmd-returning function and method
// in file to idx, and every exec.Cmd-typed struct field to fields.
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

// contractFileImportAliases maps each import to the identifier a selector
// qualifies it with (explicit alias, else last path segment), so a
// package-qualified call like workspace.GitCommand(...) resolves to the
// import path contractBuildCmdIndex keyed its funcs entry under, treating
// a call into another walked package like a same-package call. Blank and
// dot imports are omitted: neither binds a name a selector could qualify.
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

// contractProcessBoundExpr reports whether expr is a selector naming field
// Process, or a call to os.StartProcess or os.FindProcess.
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

// contractCollectBoundNames walks fn once and returns the local
// identifiers (parameters, var declarations, assignment targets) bound to
// an exec.Cmd and those bound to an *os.Process.
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

// contractExprIsCmdBound reports whether expr, a Start/Run/Wait receiver,
// is bound to an exec.Cmd: a direct exec.Command(Context) call, a call to
// an indexed function or method, a local name contractCollectBoundNames
// marked, or a selector whose field fields declares as exec.Cmd.
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
// expression names ("bytes.Buffer" for the file's bytes import, else the
// bare identifier), or "" for an unrecognized type, which is then treated
// as unbounded.
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

// contractSinkTypeFromValue returns the contractBoundedSinkTypes key for a
// value that constructs a sink directly, looking through a leading
// address-of so both "T{}" and "&T{}" resolve to T. It returns "" when
// expr is not a composite literal.
func contractSinkTypeFromValue(bytesName string, expr ast.Expr) string {
	lit, ok := unwrapCompositeLit(expr)
	if !ok {
		return ""
	}
	return contractSinkTypeName(bytesName, lit.Type)
}

// contractCollectSinkVarTypes maps every local variable fn binds to a
// recognized sink type (by "var x T", "var x T = ...", "x := T{...}", or
// "x := &T{...}") to that type's contractBoundedSinkTypes key.
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
// resolves to via sinkTypes, and whether expr is the literal nil (which
// [procutil.CaptureParams] accepts). An expression it cannot resolve
// reports "", false and is treated as unbounded.
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
// procName.CaptureParams.
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

// checkContractCaptureSinkFields reports a rule SINK violation for each of
// lit's Stdout and Stderr fields that is set and resolves, via sinkTypes,
// to neither nil nor a contractBoundedSinkTypes type.
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
// violation in file, using idx and fields built across the whole package.
// Rule SINK is skipped when dirName is exempt.
//
// Any dot import is itself a rule CAPTURE violation: a call, constant, or
// sink type reached through it binds no local identifier, so the rest of
// this function cannot see through it. Reporting it once keeps a file
// hiding a constructor or sink behind a dot import from passing.
//
// The early return below stays open when idx.producerPaths names one of
// file's own imports, because a file importing none of os/exec, os,
// syscall, windows, or procutil can still reach an exec.Cmd through
// another walked package's constructor.
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
// struct-field index from every non-test file across walked, so a caller
// binding an exec.Cmd from a constructor declared in another package is
// indexed like a same-package call site. It is one more pass over files
// already in memory, not a second tree walk.
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

// checkContractCapture applies rules CAPTURE and SINK to every non-test
// file in pkg, using idx and fields the caller built with
// contractBuildModuleCmdIndex (from pkg alone for an isolated check, or
// from the whole tree so cross-package call sites resolve). A caller that
// skips a CAPTURE-exempt package skips SINK for it too.
func checkContractCapture(fset *token.FileSet, pkg contractPackage, idx *contractCmdIndex, fields contractCmdFields) []contractViolation {
	var violations []contractViolation
	for _, file := range pkg.files {
		violations = append(violations, checkContractCaptureFile(fset, file, pkg.importPath, pkg.dirName, idx, fields)...)
	}
	return violations
}

// contractStopGraceOwner is the helper rule STOPGRACE directs a family to,
// instead of reading [procutil.DefaultStopGrace] directly.
const contractStopGraceOwner = "procutil.StopGrace"

// importPos returns the position of file's import of importPath, or the
// file start when absent, so a violation always carries an openable
// position.
func importPos(file *ast.File, importPath string) token.Pos {
	for _, imp := range file.Imports {
		if path, err := strconv.Unquote(imp.Path.Value); err == nil && path == importPath {
			return imp.Pos()
		}
	}
	return file.Pos()
}

// checkContractStopGrace reports a violation for every reference to
// procutil.DefaultStopGrace in file, the qualifier resolved from file's
// own imports so an aliased import cannot evade it.
func checkContractStopGrace(fset *token.FileSet, file *ast.File) []contractViolation {
	procutilIdent := resolveContractImportName(file, contractProcutilImportPath)
	if procutilIdent == "" {
		return nil
	}
	var violations []contractViolation
	// A dot import binds the constant to a bare identifier with no
	// selector node to match, so the rule reports the import itself:
	// chasing bare identifiers would also flag a shadowing local and an
	// unrelated other.DefaultStopGrace, which need type information to
	// tell apart.
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

// contractReaperLoggerHint is what checkContractReaperLogger tells a
// caller to pass instead of the two forbidden shapes.
const contractReaperLoggerHint = "a logger the call site already holds, in a local variable or a struct field"

// contractCallsSlogDefault reports whether expr is a call to
// slogIdent.Default, or a call chained onto one (e.g.
// slog.Default().With(...)).
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
// procutil.StartReaper call whose second argument is nil or slog.Default()
// rather than a logger the call site holds. Either shape routes the one
// CaptureCleanupWarning record away from the caller's own logger to
// StartReaper's package-level default.
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

// contractPathIsUnder reports whether importPath is prefix or a path
// below it, matching on segments not a bare substring so a vanity path
// embedding prefix does not match.
func contractPathIsUnder(importPath, prefix string) bool {
	if importPath == prefix {
		return true
	}
	return strings.HasPrefix(importPath, prefix+"/")
}

// contractPackageImportPath maps dir to its full import path, given the
// path root resolves to, normalizing separators for cross-platform CI.
func contractPackageImportPath(root, rootImportPath, dir string) string {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." {
		return rootImportPath
	}
	return rootImportPath + "/" + filepath.ToSlash(rel)
}

// contractImportBanReason returns the reason importPath is banned for pkg,
// or "" when allowed. First match wins, so one import yields one reason.
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
// a core file, or "" when allowed. First match wins.
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

// checkCoreContractPackage applies the core-import rule to pkg.files
// (inTestFile false) and pkg.testFiles (inTestFile true). It consults no
// allowlist: the orchestrator root holds one package, so any exemption
// would disable the rule.
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

// contractAgentIdentityFloorTable names the vendor and runtime names the
// identity rule always treats as identity tokens, in addition to the kind
// strings extracted from the tree. It is a floor, not a closed world.
var contractAgentIdentityFloorTable = []string{
	"claude", "codex", "copilot", "kiro", "opencode", "mock",
	"gemini", "amp", "goose", "zed", "cursor", "aider", "qwen", "crush",
}

// contractAgentIdentitySnapshot is the identity rule's token set, gathered
// once from internal/agent and the runtime profiles: every floor and
// extracted token, and each registering package's kinds keyed by its
// import path. profileDeclaredTokens marks a token sourced from a
// profile's identity_tokens, which makes an unanchored one wide-scoped.
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

// contractAgentIdentitySnapshotData returns the identity rule's token set,
// built from disk on first use and cached. One shared snapshot is what
// lets a package be caught for naming a kind another package registers.
func contractAgentIdentitySnapshotData() contractAgentIdentitySnapshot {
	contractAgentIdentityOnce.Do(func() {
		contractAgentIdentityData = buildContractAgentIdentitySnapshot()
	})
	return contractAgentIdentityData
}

// buildContractAgentIdentitySnapshot walks internal/agent from disk,
// collecting the first argument of every registry.Agents.Register and
// RegisterWithMeta call in non-test files, resolving the registry
// qualifier per file. A non-string-literal kind argument contributes an
// extraction error rather than being skipped, so a kind the checker
// cannot read cannot go unreported.
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

// contractRuntimeProfilesGlob matches every runtime profile document. A
// package variable, not an inline literal, so a staleness-guard test can
// point it at a scratch fixture directory.
var contractRuntimeProfilesGlob = filepath.Join("..", "qualification", "profiles", "*.json")

// contractProfileDeclaredTokens reads every profile matching pattern via
// qualification.ReadRuntimeProfileFile, unioning each profile's
// identity_tokens into tokenSet and returning the lowercased
// profile-declared subset. A profile that fails to decode contributes an
// extraction error rather than being skipped.
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

// contractIdentityWordsFromIdent splits name into lowercase words on every
// case transition and underscore.
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
// every non-letter, non-digit character.
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
// consecutive run inside valueWords. Both are already lowercased.
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
// kindPackageImportPath or under it, backing the identity rule's subtree
// exclusion: a package under a kind package's path may name that kind.
func contractPackageUnderKindPackage(checkedImportPath, kindPackageImportPath string) bool {
	return contractPathIsUnder(checkedImportPath, kindPackageImportPath)
}

// contractIdentityExcludedTokens returns the tokens the identity rule does
// not enforce against pkg: its own directory name, and the directory name
// and registered kinds of any kind package that is pkg's import path or an
// ancestor. The ancestor case lets a package nested under a kind package
// name that kind.
func contractIdentityExcludedTokens(pkg contractPackage, kindImportPaths map[string][]string) map[string]bool {
	excluded := map[string]bool{strings.ToLower(pkg.dirName): true}
	if contractIdentitySeamImportPath(pkg.importPath) {
		for _, tok := range contractWideScopedTokens(contractAgentIdentitySnapshotData()) {
			excluded[strings.ToLower(tok)] = true
		}
	}
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
// identifier outside an import declaration whose words carry a token from
// tokens that excluded does not exempt. Import declarations are skipped
// because rule IMPORT governs import paths; this arm targets identity
// branching in prose and identifiers.
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

// exprContainsAgentInfo reports whether expr's tree contains an identifier
// spelled exactly agentInfo, bare or in a selector chain.
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

// contractIdentityArm2Violations reports every equality/inequality
// comparison, switch tag, case expression, and map index whose operand
// contains agentInfo: the recorded identity may be logged, but nothing
// may compare, switch on, or key a map by it to decide behavior.
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

// checkContractIdentity evaluates both arms of the identity rule against
// pkg.files: the token arm (using the shared snapshot minus pkg's own
// exclusions) and the agentInfo branch arm. Test files are exempt.
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

// contractTokenIsKindAnchored reports whether some kindImportPaths entry
// anchors token (its base directory name or a registered kind). That is
// the set contractIdentityExcludedTokens can exempt, so an unanchored
// token has no directory where its name is legal.
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

// contractWideScopedTokens returns the profile-declared tokens no kind
// package anchors, the set the wide scope applies to. Every other token
// keeps the narrow scope of internal/agent alone.
func contractWideScopedTokens(snapshot contractAgentIdentitySnapshot) []string {
	var wide []string
	for _, tok := range snapshot.tokens {
		if snapshot.profileDeclaredTokens[tok] && !contractTokenIsKindAnchored(tok, snapshot.kindImportPaths) {
			wide = append(wide, tok)
		}
	}
	return wide
}

// contractWideIdentityRoots are the roots the wide scope walks beyond the
// per-family walks the two contract tests already cover.
var contractWideIdentityRoots = []string{filepath.Join("..", "..", "cmd"), filepath.Join("..", "..", "internal")}

// contractIdentityMeasurementSeam is the one directory where a
// profile-declared, unanchored token is legal: the transport's
// measurement sources, which read a runtime's own output format and are
// vendor-specific by construction. Every other directory under the wide
// roots stays bound by the rule.
var contractIdentityMeasurementSeam = filepath.Join("internal", "agent", "clientprotocol", "usagesource")

// contractIdentitySeamImportPath reports whether importPath is the
// measurement seam's own package.
func contractIdentitySeamImportPath(importPath string) bool {
	return strings.HasSuffix(importPath, "/"+filepath.ToSlash(contractIdentityMeasurementSeam))
}

// contractIdentitySeamDir reports whether walkPath is the measurement
// seam, under whatever root the walk started from.
func contractIdentitySeamDir(walkPath string) bool {
	clean := filepath.Clean(walkPath)
	return clean == contractIdentityMeasurementSeam ||
		strings.HasSuffix(clean, string(filepath.Separator)+contractIdentityMeasurementSeam)
}

// contractWideIdentityNonTestViolations walks every non-test .go file
// under root (excluding testdata) and applies contractIdentityArm1Violations
// for the wide-scoped tokens with no exclusion. It returns the violations
// and the files scanned, so the caller can confirm the walk covered
// something.
func contractWideIdentityNonTestViolations(t *testing.T, fset *token.FileSet, root string, tokens []string) (violations []contractViolation, scanned []string) {
	t.Helper()

	err := filepath.WalkDir(root, func(walkPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == "testdata" || contractIdentitySeamDir(walkPath) {
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

// contractWideIdentityTestViolations walks every _test.go file under root
// (excluding testdata), parsing in full because the identifier-only arm
// needs to see past imports, and applies
// contractIdentityTestFileIdentViolations for the wide-scoped tokens.
func contractWideIdentityTestViolations(t *testing.T, fset *token.FileSet, root string, tokens []string) (violations []contractViolation, scanned []string) {
	t.Helper()

	err := filepath.WalkDir(root, func(walkPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == "testdata" || contractIdentitySeamDir(walkPath) {
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

// TestContractIdentitySeamIsNarrow holds the one exempted directory to
// exactly what it is exempted for: removing the exemption must produce
// violations, and every one of them must sit inside the seam.
func TestContractIdentitySeamIsNarrow(t *testing.T) {
	info, err := os.Stat(filepath.Join("..", "..", contractIdentityMeasurementSeam))
	if err != nil || !info.IsDir() {
		t.Fatalf("the exempted seam %s is not a directory: %v", contractIdentityMeasurementSeam, err)
	}

	fset := token.NewFileSet()
	wideTokens := contractWideScopedTokens(contractAgentIdentitySnapshotData())

	var unexempted []contractViolation
	for _, root := range contractWideIdentityRoots {
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
			if !strings.HasSuffix(walkPath, ".go") {
				return nil
			}
			file, parseErr := parser.ParseFile(fset, walkPath, nil, parser.SkipObjectResolution)
			if parseErr != nil {
				t.Fatalf("parse %s: %v", walkPath, parseErr)
			}
			if strings.HasSuffix(walkPath, "_test.go") {
				unexempted = append(unexempted, contractIdentityTestFileIdentViolations(fset, file, wideTokens)...)
				return nil
			}
			unexempted = append(unexempted, contractIdentityArm1Violations(fset, file, wideTokens, map[string]bool{})...)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	if len(unexempted) == 0 {
		t.Fatal("removing the seam exemption produced no violation, so the exemption is dead and this check proves nothing")
	}
	for _, v := range unexempted {
		if !contractIdentitySeamDir(filepath.Dir(v.pos.Filename)) {
			t.Errorf("%s: %s, outside the one exempted seam", v.pos, v.text)
		}
	}
}

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

// TestContractIdentityWideScope_StalenessGuardCatchesRealBreaks proves the
// mechanisms TestContractIdentityWideScope relies on can fail, not just
// pass against the current tree: a profile that fails to decode is
// reported, and dropping either the anchoring clause or the
// profile-source clause reddens against a real collision (kiro, cursor).
// The last two subtests confirm today's snapshot keeps both green.
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

func contractExempt(dirName string, rule contractRule) bool {
	reasons, ok := contractAllowlist[dirName]
	if !ok {
		return false
	}
	_, ok = reasons[rule]
	return ok
}

// checkAdapterContractPackage evaluates rules BAN, METRICS, HOOK, IMPORT,
// and (for an agent-family package) IDENTITY and STOPGRACE against pkg,
// honoring pkg.dirName's allowlist entries. Only IMPORT reads test files.
//
// A ruleIMPORT allowlist entry is all-or-nothing: it lifts the
// orchestrator ban, the sibling-adapter ban, and the package's own
// contractPackageBannedImports together, in non-test and test files
// alike. A case needing only one lifted requires a new mechanism.
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

// contractWalkRoot walks the Go files under dir (excluding testdata),
// groups them into packages keyed by directory ordered ascending, and
// reports whether any parsed file imports the registry package. Both
// returns are scoped to this one root.
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

// TestCheckAdapterContract walks internal/tracker, internal/scm,
// internal/agent, and internal/notify and fails when any package breaks
// rules BAN, METRICS, HOOK, IMPORT, IDENTITY, or STOPGRACE.
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

// TestCheckOrchestratorContract walks internal/orchestrator and fails when
// a file imports an adapter-family package the core-import rule rejects.
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

// contractCaptureTeardownRoots names the roots rules CAPTURE and TEARDOWN
// walk: every launch site, whatever family or layer, must reach
// procutil.RunCapture/StartCapture/SetGroupCancel/SetGroupKill.
var contractCaptureTeardownRoots = []struct {
	dir        string
	importPath string
}{
	{filepath.Join("..", "..", "cmd"), "github.com/sortie-ai/sortie/cmd"},
	{filepath.Join("..", "..", "internal"), "github.com/sortie-ai/sortie/internal"},
}

// contractWalkCaptureAndTeardown walks both roots through contractWalkRoot
// and merges their packages into one dir-ordered slice, so a constructor
// call spanning two walked packages resolves like a same-package call.
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

// TestContractCaptureAndTeardown walks every non-test Go file under cmd/
// and internal/ and fails on rules CAPTURE, SINK, TEARDOWN, and REAPER.
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

// TestCheckAdapterContract_DetectsSSHRemoteCommandHelpers pins the two
// ban-table entries for the retired SSH prefix helpers against an inline
// fixture, so a regression is caught even when every real adapter
// complies.
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

		// wantSubstr, when non-empty, must appear in a returned violation,
		// pinning it to the expected rule rather than any violation.
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
			// No meta literal means no declared blocker source either, so
			// both HOOK and BLOCKER fire.
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
			// Plain Register carries no meta literal, so BLOCKER also
			// fires; "file" is allowlisted for HOOK only, so BAN and
			// BLOCKER both fire.
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
			// "claude-code" also carries the floor token "claude", so two
			// violations; wantSubstr pins the multi-word one plain word
			// equality would miss.
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
			// Capitalized AgentInfo is the wire type's field, a different
			// identifier from the banned lowercase agentInfo, so deciding
			// presence from it once is legal.
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
			// subpkg sits under the real claude kind package's path, so the
			// subtree exclusion covers it too.
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

// TestCheckContractCapture_DetectsViolations pins rules CAPTURE, SINK, and
// TEARDOWN against inline fixtures, so a regression is caught even when
// every real launch site complies. Each fixture is the single non-test
// file of a one-file package run through both checkContractCapture and
// checkContractTeardown. CAPTURE targets a direct exec.Cmd.Stdout/Stderr
// assignment, SINK targets those fields inside a procutil.CaptureParams
// literal, and TEARDOWN targets Cancel and WaitDelay, so no two match the
// same syntax.
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
			// A dot-imported procutil makes "CaptureParams{...}" a bare
			// literal contractIsCaptureParamsLit cannot recognize, so
			// without the dot-import ban this os.Stdout sink would evade
			// rule SINK.
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
			// An os.Pipe write end is exactly the sink CaptureParams warns
			// against: a reader that stops draining it blocks Write and
			// holds Wait open indefinitely.
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
			// A bounded writer is admitted through contractBoundedSinkTypes
			// by name, not by structural inspection of its Write method.
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

// TestCheckContractReaperLogger_DetectsViolations pins rule REAPER against
// inline fixtures, so a regression is caught even when every real launch
// site complies. The clean fixtures are the negative control: a call
// passing a logger the site holds (local, struct field, or a chained
// slog.Default().With bound to a local first) must not fire.
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
// walked package, not only the caller's own. idx and fields are built
// module-wide across both fixture packages; an idx built from the consumer
// alone (the defect this guards against) would miss the call.
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

// TestCheckContractCapture_DetectsDotImportedConstructorViolations guards
// the hole where a constructor reached through a dot import bound no name
// a selector could qualify, so the hand-wired Wait beneath it passed
// uncaught. Before the general dot-import ban this returned zero
// violations.
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

// TestCheckContractCapture_ResolvesRenamedImportConstructorCalls pins that
// a renamed import, unlike a dot import, does not share the hole the
// previous test closes: contractFileImportAliases keys off each import's
// local identifier, so ws.GitCommand resolves like workspace.GitCommand
// and the hand-wired Wait stays caught. The consumer keeps a direct
// os/exec import so the alias resolution, not the early-return guard, is
// exercised.
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

// TestCheckContractCapture_DetectsProducerOnlyImportViolations guards the
// hole in checkContractCaptureFile's early return: a file reaching a
// command through a cross-package constructor need not import os/exec
// itself, so naming none of the five imports was wrongly treated as proof
// it could not violate rule CAPTURE. Before the producer-path check joined
// the early-return condition this reported zero violations.
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

// contractIdentityReporter is the reporting surface the identity-rule
// staleness checks need. Both *testing.T and the guard test's fake satisfy
// it, so each check runs one implementation rather than a copy.
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
// against going stale: it fails when the rule was evaluated for no agent
// package, when token extraction reported an error, or when a ruleIDENTITY
// allowlist entry names a directory the walk did not find.
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

// contractStalenessFakeReporter records Errorf calls instead of failing,
// so the guard test can drive the real checks against broken synthetic
// input without reddening this file's run.
type contractStalenessFakeReporter struct {
	errors []string
}

func (f *contractStalenessFakeReporter) Errorf(format string, _ ...any) {
	f.errors = append(f.errors, format)
}

// TestContractIdentityRule_StalenessGuardCatchesRealBreaks proves the
// checks TestContractIdentityRule_AppliesAndStaysCurrent performs can
// fail, not just pass against the current tree. Neither subtest runs in
// parallel: the second replaces the package-level contractAllowlist a
// concurrent fixture test also reads.
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

// TestContractStopGraceRule_AppliesAndStaysCurrent guards rule STOPGRACE
// against going stale: it fails when the rule was evaluated for no agent
// package, or when a ruleSTOPGRACE allowlist entry names a directory the
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
// checks TestContractStopGraceRule_AppliesAndStaysCurrent performs can
// fail, not just pass. The second subtest replaces the package-level
// contractAllowlist a concurrent fixture test reads, so neither runs in
// parallel.
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

// TestContractCaptureAndTeardownRule_AppliesAndStaysCurrent guards rules
// CAPTURE, SINK, TEARDOWN, and REAPER against going stale: each fails when
// evaluated for no non-exempt file under internal/agent,
// internal/orchestrator, or internal/workspace, or when an allowlist entry
// names a directory the walk did not find.
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
// performs can fail, not just pass. The second subtest replaces the
// package-level contractAllowlist a concurrent fixture test reads, so
// neither runs in parallel.
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
// file's own import declaration, including an aliased import.
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
// contractSharedFamilyPackages against going stale: it fails on an empty
// reason, a key contractSharedPackageDir cannot resolve, a directory with
// no parsable non-test Go file, a package contractPackageRegistersKind now
// reports true for, or coreImportable true for a directory whose non-test
// files import testing. It enumerates each named directory alone, never
// descending, so a verdict never depends on an unnamed subpackage.
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
// decision table against inline fixtures, so a regression is caught even
// when every real file complies. Each fixture is a one-file package.
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
// predicate against inline fixtures, covering every registry namespace and
// a qualifier resolved through an import alias.
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
