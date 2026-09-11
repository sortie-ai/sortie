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
	"github.com/sortie-ai/sortie/internal/agent/procutil":                  {reason: "shared subprocess group handling; registers no kind and holds no adapter", coreImportable: true},
	"github.com/sortie-ai/sortie/internal/agent/agentcore":                 {reason: "shared agent session, event, and disposition core; registers no kind and holds no adapter", coreImportable: true},
	"github.com/sortie-ai/sortie/internal/agent/mcpconfig":                 {reason: "shared MCP configuration parsing; registers no kind and holds no adapter", coreImportable: true},
	"github.com/sortie-ai/sortie/internal/agent/sshutil":                   {reason: "shared SSH invocation helpers; registers no kind and holds no adapter", coreImportable: true},
	"github.com/sortie-ai/sortie/internal/agent/jsonrpc":                   {reason: "shared newline-delimited JSON-RPC framing; registers no kind and holds no adapter", coreImportable: true},
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
		ruleTEARDOWN:  "owns SetGroupCancel, the helper every other launcher calls",
		ruleSTOPGRACE: "owns DefaultStopGrace, the fallback every other family reaches through StopGrace",
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
const contractTeardownOwner = "procutil.SetGroupCancel"

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
		if !contractExempt(pkg.dirName, ruleTEARDOWN) {
			violations = append(violations, checkContractTeardown(fset, file)...)
		}
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

	if !contractExempt(pkg.dirName, ruleTEARDOWN) {
		for _, file := range pkg.files {
			violations = append(violations, checkContractTeardown(fset, file)...)
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

// TestCheckAdapterContract_DetectsViolations pins the checker's own logic
// against inline source fixtures, independent of the current state of
// any adapter package, so a regression in a rule is caught even when
// every real adapter happens to comply. Each fixture is parsed as the
// single file of a one-file package named by dirName.
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
			wantSubstr: "call procutil.SetGroupCancel",
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
			wantSubstr: "call procutil.SetGroupCancel",
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
