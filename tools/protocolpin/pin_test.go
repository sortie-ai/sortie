package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repoRootForProtocolPinTest(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed to report the test file's own path")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

const fixturePinTag = "schema-v9.9.9"

func fixtureGenerateGo(assetsArg string) string {
	return fmt.Sprintf("package clientprotocol\n\n//go:generate go run %s %s wire_gen.go\n", generatorPackagePath, assetsArg)
}

func fixtureProvenanceTxt(tag, commit string, assetLines ...string) string {
	var b strings.Builder
	b.WriteString("Upstream repository: https://github.com/agentclientprotocol/agent-client-protocol\n")
	fmt.Fprintf(&b, "Tag:    %s\n", tag)
	fmt.Fprintf(&b, "Commit: %s\n", commit)
	for _, line := range assetLines {
		b.WriteString(line + "\n")
	}
	return b.String()
}

func fixtureSchemagenGo(tag, commit string) string {
	return fmt.Sprintf("package main\n\nconst (\n\tupstreamTag    = %q\n\tupstreamCommit = %q\n)\n", tag, commit)
}

func defaultFixtureAssetLines() []string {
	return []string{
		"schema.json 10 sha256:" + strings.Repeat("b", 64),
		"meta.json 20 sha256:" + strings.Repeat("c", 64),
	}
}

func buildFixtureRepo(t *testing.T, generateGo, provenanceTxt, schemagenGo string) string {
	t.Helper()
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, adapterPackageDir, "generate.go"), generateGo)
	mustWriteFile(t, filepath.Join(root, adapterPackageDir, "testdata", fixturePinTag, "PROVENANCE.txt"), provenanceTxt)
	mustWriteFile(t, filepath.Join(root, adapterPackageDir, "schemagen", "generate.go"), schemagenGo)
	return root
}

func TestLocatePinReadsCommittedTree(t *testing.T) {
	t.Parallel()

	pin, err := locatePin(repoRootForProtocolPinTest(t))
	if err != nil {
		t.Fatalf("locatePin(committed tree) returned error: %v", err)
	}
	if pin.Repository != "agentclientprotocol/agent-client-protocol" {
		t.Errorf("locatePin(committed tree).Repository = %q, want %q", pin.Repository, "agentclientprotocol/agent-client-protocol")
	}
	names := make(map[string]bool, len(pin.Assets))
	for _, a := range pin.Assets {
		names[a.Name] = true
		if len(a.SHA256) != 64 {
			t.Errorf("locatePin(committed tree) asset %q SHA256 = %q, want 64 hex characters", a.Name, a.SHA256)
		}
	}
	if !names["schema.json"] || !names["meta.json"] {
		t.Errorf("locatePin(committed tree).Assets = %+v, want schema.json and meta.json", pin.Assets)
	}
}

func TestLocatePinFaultsOnGenerateLineShape(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("a", 40)
	provenance := fixtureProvenanceTxt(fixturePinTag, commit, defaultFixtureAssetLines()...)
	schemagen := fixtureSchemagenGo(fixturePinTag, commit)

	tests := []struct {
		name       string
		generateGo string
	}{
		{"zero go:generate lines", "package clientprotocol\n"},
		{
			"several go:generate lines",
			fixtureGenerateGo("testdata/"+fixturePinTag) + fmt.Sprintf("//go:generate go run %s testdata/other wire_gen.go\n", generatorPackagePath),
		},
		{"missing assets-directory argument", fmt.Sprintf("package clientprotocol\n\n//go:generate go run %s\n", generatorPackagePath)},
		{"assets directory escapes the adapter package", fixtureGenerateGo("../../etc")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := buildFixtureRepo(t, tt.generateGo, provenance, schemagen)
			if _, err := locatePin(root); err == nil {
				t.Errorf("locatePin(%s) error = nil, want a fault", tt.name)
			}
		})
	}
}

func TestLocatePinFaultsOnMalformedProvenance(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("a", 40)
	generateGo := fixtureGenerateGo("testdata/" + fixturePinTag)
	schemagen := fixtureSchemagenGo(fixturePinTag, commit)

	tests := []struct {
		name       string
		provenance string
	}{
		{
			"missing header line",
			"Upstream repository: https://github.com/agentclientprotocol/agent-client-protocol\nCommit: " + commit + "\n" + strings.Join(defaultFixtureAssetLines(), "\n") + "\n",
		},
		{
			"repeated asset name",
			fixtureProvenanceTxt(fixturePinTag, commit, "schema.json 10 sha256:"+strings.Repeat("b", 64), "schema.json 11 sha256:"+strings.Repeat("d", 64)),
		},
		{
			"bad byte count leaves no recognizable asset line",
			fixtureProvenanceTxt(fixturePinTag, commit, "schema.json not-a-number sha256:"+strings.Repeat("b", 64)),
		},
		{
			"non-hex digest leaves no recognizable asset line",
			fixtureProvenanceTxt(fixturePinTag, commit, "schema.json 10 sha256:not-hex"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := buildFixtureRepo(t, generateGo, tt.provenance, schemagen)
			if _, err := locatePin(root); err == nil {
				t.Errorf("locatePin(%s) error = nil, want a fault", tt.name)
			}
		})
	}
}

func TestLocatePinFaultsOnTagFormMismatch(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("a", 40)

	t.Run("tag does not match the stable schema form", func(t *testing.T) {
		t.Parallel()
		root := buildFixtureRepoNamed(t, "v9.9.9",
			fixtureGenerateGoNamed("v9.9.9"),
			fixtureProvenanceTxt("v9.9.9", commit, defaultFixtureAssetLines()...),
			fixtureSchemagenGo("v9.9.9", commit))
		if _, err := locatePin(root); err == nil {
			t.Error("locatePin(non-stable tag form) error = nil, want a fault")
		}
	})

	t.Run("tag disagrees with the assets directory name", func(t *testing.T) {
		t.Parallel()
		root := buildFixtureRepo(t,
			fixtureGenerateGo("testdata/"+fixturePinTag),
			fixtureProvenanceTxt("schema-v1.0.0", commit, defaultFixtureAssetLines()...),
			fixtureSchemagenGo("schema-v1.0.0", commit))
		if _, err := locatePin(root); err == nil {
			t.Error("locatePin(tag disagreeing with directory name) error = nil, want a fault")
		}
	})
}

func TestLocatePinFaultsOnConstantMismatch(t *testing.T) {
	t.Parallel()

	commit := strings.Repeat("a", 40)
	generateGo := fixtureGenerateGo("testdata/" + fixturePinTag)
	provenance := fixtureProvenanceTxt(fixturePinTag, commit, defaultFixtureAssetLines()...)

	tests := []struct {
		name      string
		schemagen string
	}{
		{"upstreamTag disagrees", fixtureSchemagenGo("schema-v1.0.0", commit)},
		{"upstreamCommit disagrees", fixtureSchemagenGo(fixturePinTag, strings.Repeat("f", 40))},
		{"constants absent", "package main\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root := buildFixtureRepo(t, generateGo, provenance, tt.schemagen)
			if _, err := locatePin(root); err == nil {
				t.Errorf("locatePin(%s) error = nil, want a fault", tt.name)
			}
		})
	}
}

func testPin() recordedPin {
	return recordedPin{
		Repository: "agentclientprotocol/agent-client-protocol",
		Tag:        "schema-v1.21.0",
		Commit:     strings.Repeat("a", 40),
		Assets: []pinnedAsset{
			{Name: "meta.json", SHA256: strings.Repeat("1", 64)},
			{Name: "schema.json", SHA256: strings.Repeat("2", 64)},
		},
	}
}

func digestPtr(hex string) *string {
	s := "sha256:" + hex
	return &s
}
