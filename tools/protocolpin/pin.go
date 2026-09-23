package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const adapterPackageDir = "internal/agent/clientprotocol"

const generatorPackagePath = "github.com/sortie-ai/sortie/internal/agent/clientprotocol/schemagen"

type recordedPin struct {
	Repository string
	Tag        string
	Commit     string
	Assets     []pinnedAsset
}

type pinnedAsset struct {
	Name   string
	SHA256 string
}

var goGenerateLineRE = regexp.MustCompile(`^//go:generate\s+(.*)$`)

// findGenerateAssetsDir returns its result relative to adapterPackageDir,
// not repoRoot.
func findGenerateAssetsDir(repoRoot string) (string, error) {
	path := filepath.Join(repoRoot, adapterPackageDir, "generate.go")
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is derived from the caller's own repository root
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	var matches [][]string
	for line := range strings.SplitSeq(string(data), "\n") {
		m := goGenerateLineRE.FindStringSubmatch(strings.TrimRight(line, "\r"))
		if m == nil {
			continue
		}
		fields := strings.Fields(m[1])
		for i, f := range fields {
			if f == generatorPackagePath {
				matches = append(matches, fields[i+1:])
				break
			}
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("%s: found %d //go:generate line(s) running %s, want exactly 1", path, len(matches), generatorPackagePath)
	}

	args := matches[0]
	if len(args) == 0 {
		return "", fmt.Errorf("%s: the //go:generate line running %s names no assets-directory argument", path, generatorPackagePath)
	}
	assetsDir := args[0]

	cleaned := filepath.Clean(filepath.Join(adapterPackageDir, assetsDir))
	if !strings.HasPrefix(cleaned, adapterPackageDir+string(filepath.Separator)) {
		return "", fmt.Errorf("%s: assets directory %q escapes %s", path, assetsDir, adapterPackageDir)
	}

	return assetsDir, nil
}

// provenanceLineRE is declared here, not imported, because its only other
// definition lives in a package that cannot be imported.
var provenanceLineRE = regexp.MustCompile(`^(\S+)\s+(\d+)\s+sha256:([0-9a-f]{64})$`)

var provenanceHeaderRE = regexp.MustCompile(`^(Upstream repository|Tag|Commit):\s*(.*)$`)

var repositoryURLRE = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)$`)

var commitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

func parseProvenanceFile(path string) (recordedPin, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is derived from the caller's own repository root
	if err != nil {
		return recordedPin{}, fmt.Errorf("read %s: %w", path, err)
	}

	headers := map[string]string{}
	headerCounts := map[string]int{}
	seenAssets := map[string]bool{}
	var assets []pinnedAsset

	for line := range strings.SplitSeq(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if m := provenanceHeaderRE.FindStringSubmatch(trimmed); m != nil {
			headerCounts[m[1]]++
			headers[m[1]] = strings.TrimSpace(m[2])
			continue
		}
		if m := provenanceLineRE.FindStringSubmatch(trimmed); m != nil {
			if seenAssets[m[1]] {
				return recordedPin{}, fmt.Errorf("%s: asset %q is recorded more than once", path, m[1])
			}
			seenAssets[m[1]] = true
			assets = append(assets, pinnedAsset{Name: m[1], SHA256: m[3]})
		}
	}

	for _, header := range []string{"Upstream repository", "Tag", "Commit"} {
		if headerCounts[header] != 1 {
			return recordedPin{}, fmt.Errorf("%s: %q header line appears %d time(s), want exactly 1", path, header, headerCounts[header])
		}
	}
	if len(assets) == 0 {
		return recordedPin{}, fmt.Errorf("%s: no asset lines found", path)
	}

	repoMatch := repositoryURLRE.FindStringSubmatch(headers["Upstream repository"])
	if repoMatch == nil {
		return recordedPin{}, fmt.Errorf("%s: %q header value %q is not a well-formed https://github.com/{owner}/{repo} URL", path, "Upstream repository", headers["Upstream repository"])
	}

	commit := headers["Commit"]
	if !commitRE.MatchString(commit) {
		return recordedPin{}, fmt.Errorf("%s: %q header value %q is not 40 lowercase hex characters", path, "Commit", commit)
	}

	sort.Slice(assets, func(i, j int) bool { return assets[i].Name < assets[j].Name })

	return recordedPin{
		Repository: repoMatch[1] + "/" + repoMatch[2],
		Tag:        headers["Tag"],
		Commit:     commit,
		Assets:     assets,
	}, nil
}

var stableTagRE = regexp.MustCompile(`^schema-v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func locatePin(repoRoot string) (recordedPin, error) {
	assetsDir, err := findGenerateAssetsDir(repoRoot)
	if err != nil {
		return recordedPin{}, err
	}

	provenancePath := filepath.Join(repoRoot, adapterPackageDir, assetsDir, "PROVENANCE.txt")
	pin, err := parseProvenanceFile(provenancePath)
	if err != nil {
		return recordedPin{}, err
	}

	if !stableTagRE.MatchString(pin.Tag) {
		return recordedPin{}, fmt.Errorf("%s: tag %q does not match the stable schema tag form", provenancePath, pin.Tag)
	}
	if base := filepath.Base(assetsDir); base != pin.Tag {
		return recordedPin{}, fmt.Errorf("%s: tag %q disagrees with assets directory name %q", provenancePath, pin.Tag, base)
	}

	generatorPath := filepath.Join(repoRoot, adapterPackageDir, "schemagen", "generate.go")
	genTag, genCommit, err := parseGeneratorConstants(generatorPath)
	if err != nil {
		return recordedPin{}, err
	}
	if genTag != pin.Tag {
		return recordedPin{}, fmt.Errorf("%s: upstreamTag %q disagrees with %s's Tag %q", generatorPath, genTag, provenancePath, pin.Tag)
	}
	if genCommit != pin.Commit {
		return recordedPin{}, fmt.Errorf("%s: upstreamCommit %q disagrees with %s's Commit %q", generatorPath, genCommit, provenancePath, pin.Commit)
	}

	return pin, nil
}

// parseGeneratorConstants reads path as Go source rather than importing it,
// because the package it belongs to cannot be imported.
func parseGeneratorConstants(path string) (tag, commit string, err error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return "", "", fmt.Errorf("parse %s: %w", path, err)
	}

	values := map[string]string{}
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.CONST {
			continue
		}
		for _, spec := range genDecl.Specs {
			valueSpec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range valueSpec.Names {
				if name.Name != "upstreamTag" && name.Name != "upstreamCommit" {
					continue
				}
				if i >= len(valueSpec.Values) {
					continue
				}
				lit, ok := valueSpec.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				unquoted, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				values[name.Name] = unquoted
			}
		}
	}

	tag, tagOK := values["upstreamTag"]
	commit, commitOK := values["upstreamCommit"]
	if !tagOK || !commitOK {
		return "", "", fmt.Errorf("%s: does not declare both upstreamTag and upstreamCommit as string constants", path)
	}
	return tag, commit, nil
}
