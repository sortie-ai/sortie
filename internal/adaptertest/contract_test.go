package adaptertest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
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

// contractTrackerAdapterMethods are the domain.TrackerAdapter method
// names rule METRICS requires a trackermetrics.Track call inside, when
// the enclosing package registers a tracker kind.
var contractTrackerAdapterMethods = map[string]bool{
	"FetchCandidateIssues":          true,
	"FetchIssueByID":                true,
	"FetchIssuesByStates":           true,
	"FetchIssueStatesByIDs":         true,
	"FetchIssueStatesByIdentifiers": true,
	"FetchIssueComments":            true,
	"TransitionIssue":               true,
	"CommentIssue":                  true,
	"AddLabel":                      true,
}

// contractRule names one of the three syntactic rules the checker
// enforces.
type contractRule string

const (
	ruleBAN     contractRule = "BAN"
	ruleMETRICS contractRule = "METRICS"
	ruleHOOK    contractRule = "HOOK"
	ruleIMPORT  contractRule = "IMPORT"
)

// Family roots and the orchestrator path rule IMPORT matches an import
// path against.
const (
	contractTrackerFamilyPath = "github.com/sortie-ai/sortie/internal/tracker"
	contractSCMFamilyPath     = "github.com/sortie-ai/sortie/internal/scm"
	contractOrchestratorPath  = "github.com/sortie-ai/sortie/internal/orchestrator"
)

// contractSharedFamilyPackages names each package under a family root that
// holds no adapter, so any package under either root may import it, and
// states why. A package under a family root that is absent from this map
// may be imported only by itself and by packages under its own path.
var contractSharedFamilyPackages = map[string]string{
	"github.com/sortie-ai/sortie/internal/scm/scmcore": "shared forge decision core; registers no kind and holds no adapter",
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

// contractViolation describes one place a file or package breaks the
// adapter contract.
type contractViolation struct {
	pos  token.Position
	text string
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

// compositeLitHasKey reports whether expr is a composite literal
// carrying a field keyed by the given identifier name.
func compositeLitHasKey(expr ast.Expr, key string) bool {
	lit, ok := unwrapCompositeLit(expr)
	if !ok {
		return false
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if ident, ok := kv.Key.(*ast.Ident); ok && ident.Name == key {
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
// literal supplies ValidateTrackerConfig.
func contractRegistrationFacts(fset *token.FileSet, files []*ast.File) (registers bool, usedMeta bool, hasHook bool, pos token.Position) {
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
				}
			}
			return true
		})
	}
	return registers, usedMeta, hasHook, pos
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

	underTrackerFamily := contractPathIsUnder(importPath, contractTrackerFamilyPath)
	underSCMFamily := contractPathIsUnder(importPath, contractSCMFamilyPath)
	if underTrackerFamily || underSCMFamily {
		if !contractPathIsUnder(importPath, pkg.importPath) {
			if _, shared := contractSharedFamilyPackages[importPath]; !shared {
				return "an adapter package must not import a sibling adapter package"
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

	registers, usedMeta, hasHook, hookPos := contractRegistrationFacts(fset, pkg.files)

	if !contractExempt(pkg.dirName, ruleMETRICS) {
		for _, file := range pkg.files {
			violations = append(violations, checkContractMetrics(fset, file, registers)...)
		}
	}

	if registers && !contractExempt(pkg.dirName, ruleHOOK) {
		if !usedMeta || !hasHook {
			violations = append(violations, contractViolation{
				pos:  hookPos,
				text: "tracker kind registers no config validation hook",
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

// TestCheckAdapterContract walks the Go files under internal/tracker and
// internal/scm, excluding testdata, and fails when any package breaks the
// shared-decision invariant this work establishes: a re-declared
// reimplementation of a name the ban table names, a domain.TrackerAdapter
// method that does not record through trackermetrics.Track, a direct
// call to IncTrackerRequests, a tracker-registering package supplying no
// config validation hook, or an import rule IMPORT rejects.
func TestCheckAdapterContract(t *testing.T) {
	fset := token.NewFileSet()
	packages := map[string]*contractPackage{}
	var dirOrder []string
	registryImported := false

	roots := []struct {
		dir        string
		importPath string
	}{
		{filepath.Join("..", "tracker"), contractTrackerFamilyPath},
		{filepath.Join("..", "scm"), contractSCMFamilyPath},
	}

	for _, root := range roots {
		parsed := 0
		err := filepath.WalkDir(root.dir, func(path string, d fs.DirEntry, walkErr error) error {
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

			dir := filepath.Dir(path)
			pkg, seen := packages[dir]
			if !seen {
				pkg = &contractPackage{
					dirName:    filepath.Base(dir),
					importPath: contractPackageImportPath(root.dir, root.importPath, dir),
				}
				packages[dir] = pkg
				dirOrder = append(dirOrder, dir)
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
			t.Fatalf("walk %s: %v", root.dir, err)
		}
		if parsed == 0 {
			t.Fatalf("root %s yielded no parsed Go files, want at least one", root.dir)
		}
	}

	if !registryImported {
		t.Fatalf("no parsed file under either root imports %s, want at least one", contractRegistryImportPath)
	}

	sort.Strings(dirOrder)
	for _, dir := range dirOrder {
		for _, v := range checkAdapterContractPackage(fset, *packages[dir]) {
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
			name:       "a tracker kind registered through plain Register is rejected",
			dirName:    "fixture",
			importPath: "github.com/sortie-ai/sortie/internal/tracker/fixture",
			src: `package fixture

import "github.com/sortie-ai/sortie/internal/registry"

func init() {
	registry.Trackers.Register("fixture", newFixtureAdapter)
}
`,
			wantCount: 1,
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
			wantCount: 1,
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
