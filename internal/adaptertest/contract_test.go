package adaptertest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// contractRegistryImportPath is the import path the checker resolves the
// "registry" package qualifier from, per file, rather than assuming the
// literal identifier "registry".
const contractRegistryImportPath = "github.com/sortie-ai/sortie/internal/registry"

// contractTrackermetricsImportPath is the import path the checker
// resolves the "trackermetrics" package qualifier from, so a call to
// Track is caught regardless of the local import alias.
const contractTrackermetricsImportPath = "github.com/sortie-ai/sortie/internal/trackermetrics"

// contractBanTable maps a reimplementation this work extracted to the
// shared owner a tracker or source-control package must call instead. A
// top-level function re-declaring one of these names is a violation.
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
	ruleBAN     contractRule = "BAN"
	ruleMETRICS contractRule = "METRICS"
	ruleHOOK    contractRule = "HOOK"
	ruleIMPORT  contractRule = "IMPORT"
	ruleBLOCKER contractRule = "BLOCKER"
)

// Family roots and the orchestrator path rule IMPORT matches an import
// path against.
const (
	contractTrackerFamilyPath = "github.com/sortie-ai/sortie/internal/tracker"
	contractSCMFamilyPath     = "github.com/sortie-ai/sortie/internal/scm"
	contractOrchestratorPath  = "github.com/sortie-ai/sortie/internal/orchestrator"
)

// The two remaining family roots that hold kind packages, and the
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

// contractSharedFamilyPackages names each package under a family root that
// holds no adapter, so it may be imported by a package under a family
// root and by the orchestrator, and states why. Keys are matched exactly,
// so a subpackage of a permitted package needs its own entry. A package
// under a family root that is absent from this map may be imported only
// by itself and by packages under its own path.
var contractSharedFamilyPackages = map[string]string{
	"github.com/sortie-ai/sortie/internal/scm/scmcore":    "shared forge decision core; registers no kind and holds no adapter",
	"github.com/sortie-ai/sortie/internal/agent/procutil": "shared subprocess group handling; registers no kind and holds no adapter",
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
}

// The two outcomes checkContractCoreImports renders as
// "imports " + path + "; " + reason.
const (
	contractCoreRegistryReason     = "the orchestrator resolves an adapter kind through the registry rather than importing its package"
	contractCoreRegistrationReason = "cmd/sortie owns the blank imports that trigger kind registration"
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

	if _, shared := contractSharedFamilyPackages[importPath]; shared {
		return ""
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

// checkAdapterContractPackage evaluates rules BAN, METRICS, HOOK, and
// IMPORT against pkg, honoring the allowlist entries for pkg.dirName.
// Rules BAN, METRICS, and HOOK read pkg.files only; rule IMPORT reads
// pkg.files and pkg.testFiles.
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

// TestCheckAdapterContract walks the Go files under internal/tracker and
// internal/scm, excluding testdata, and fails when any package breaks the
// shared-decision invariant this work establishes: a re-declared
// reimplementation of a name the ban table names, a domain.TrackerAdapter
// method that does not record through trackermetrics.Track, a direct
// call to IncTrackerRequests, a tracker-registering package supplying no
// config validation hook, or an import rule IMPORT rejects.
func TestCheckAdapterContract(t *testing.T) {
	fset := token.NewFileSet()

	roots := []struct {
		dir        string
		importPath string
	}{
		{filepath.Join("..", "tracker"), contractTrackerFamilyPath},
		{filepath.Join("..", "scm"), contractSCMFamilyPath},
	}

	var walked []contractWalkedPackage
	registryImported := false
	for _, root := range roots {
		rootWalked, rootRegistryImported := contractWalkRoot(t, fset, root.dir, root.importPath)
		walked = append(walked, rootWalked...)
		registryImported = registryImported || rootRegistryImported
	}

	if !registryImported {
		t.Fatalf("no parsed file under either root imports %s, want at least one", contractRegistryImportPath)
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
	}{
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
// or names a package that contractPackageRegistersKind now reports true
// for. It enumerates each named directory with os.ReadDir alone, never
// descending into a subdirectory, so a permitted package's verdict never
// depends on a subpackage the map does not name.
func TestContractSharedFamilyPackages_RegisterNoKind(t *testing.T) {
	t.Parallel()

	for importPath, reason := range contractSharedFamilyPackages {
		if reason == "" {
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
