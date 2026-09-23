package main

import (
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func mkRelease(tag string, draft, prerelease bool, publishedAt string, assets ...releaseAsset) release {
	return release{TagName: tag, Draft: draft, Prerelease: prerelease, PublishedAt: publishedAt, Assets: assets}
}

func pinAssetsAsReleaseAssets(pin recordedPin) []releaseAsset {
	out := make([]releaseAsset, 0, len(pin.Assets))
	for _, a := range pin.Assets {
		out = append(out, releaseAsset{Name: a.Name, Digest: digestPtr(a.SHA256)})
	}
	return out
}

func TestClassifyRelease(t *testing.T) {
	t.Parallel()

	pin := testPin()

	tests := []struct {
		name    string
		release release
		want    string
	}{
		{"stable release newer than the pin", mkRelease("schema-v1.22.0", false, false, "2026-09-01T00:00:00Z"), "stable"},
		{"the pinned release itself is stable", mkRelease(pin.Tag, false, false, "2026-08-20T00:00:00Z"), "stable"},
		{
			"the pinned tag under a prerelease flag classifies as pinned, not stable",
			mkRelease(pin.Tag, false, true, "2026-08-20T00:00:00Z"),
			"pinned",
		},
		{
			"a non-stable tag carrying the consumed assets after the pin classifies as unrecognized",
			mkRelease("v2.0.0-custom", false, false, "2026-09-01T00:00:00Z", pinAssetsAsReleaseAssets(pin)...),
			"unrecognized",
		},
		{"a draft release classifies as neither", mkRelease("schema-v1.22.0", true, false, "2026-09-01T00:00:00Z"), ""},
		{
			"a stable-form tag published as a prerelease classifies as neither",
			mkRelease("schema-v2.0.0-alpha.5", false, true, "2026-09-01T00:00:00Z", pinAssetsAsReleaseAssets(pin)...),
			"",
		},
		{"a crate release with no assets classifies as neither", mkRelease("v1.9.1", false, false, "2026-09-01T00:00:00Z"), ""},
		{
			"a non-stable tag missing a consumed asset classifies as neither",
			mkRelease("v2.0.0-custom", false, false, "2026-09-01T00:00:00Z", releaseAsset{Name: "meta.json", Digest: digestPtr(pin.Assets[0].SHA256)}),
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyRelease(pin, tt.release); got != tt.want {
				t.Errorf("classifyRelease(%+v) = %q, want %q", tt.release, got, tt.want)
			}
		})
	}
}

var fixedNow = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

func mustComputeDrift(t *testing.T, pin recordedPin, releases []release, refs []gitRef, tagObject *gitTag, now time.Time) drift {
	t.Helper()
	d, err := computeDrift(pin, releases, refs, tagObject, now)
	if err != nil {
		t.Fatalf("computeDrift(...) returned error: %v", err)
	}
	return d
}

func matchingRef(pin recordedPin) gitRef {
	return gitRef{Ref: "refs/tags/" + pin.Tag, Object: gitObject{Type: "commit", SHA: pin.Commit}}
}

func TestComputeDriftNewerReleases(t *testing.T) {
	t.Parallel()

	pin := testPin()
	refs := []gitRef{matchingRef(pin)}

	t.Run("unchanged, changed, and not-published asset statuses", func(t *testing.T) {
		t.Parallel()

		releases := []release{
			mkRelease(pin.Tag, false, false, "2026-08-20T00:00:00Z", pinAssetsAsReleaseAssets(pin)...),
			mkRelease("schema-v1.22.0", false, false, "2026-09-01T00:00:00Z",
				releaseAsset{Name: "meta.json", Digest: digestPtr(pin.Assets[0].SHA256)},
				releaseAsset{Name: "schema.json", Digest: digestPtr(strings.Repeat("9", 64))},
			),
			mkRelease("schema-v2.0.0", false, false, "2026-08-01T00:00:00Z",
				releaseAsset{Name: "meta.json", Digest: digestPtr(pin.Assets[0].SHA256)},
			),
		}

		d := mustComputeDrift(t, pin, releases, refs, nil, fixedNow)

		if len(d.Newer) != 2 {
			t.Fatalf("computeDrift(...).Newer = %+v, want 2 entries", d.Newer)
		}
		if d.Newer[0].Tag != "schema-v1.22.0" || d.Newer[1].Tag != "schema-v2.0.0" {
			t.Fatalf("computeDrift(...).Newer tags = [%s, %s], want ascending by version", d.Newer[0].Tag, d.Newer[1].Tag)
		}
		if !d.Newer[1].NewMajor {
			t.Errorf("computeDrift(...).Newer[schema-v2.0.0].NewMajor = false, want true")
		}

		byName := func(nr newerRelease, name string) string {
			for _, a := range nr.Assets {
				if a.Name == name {
					return a.Status
				}
			}
			t.Fatalf("newerRelease %+v carries no asset comparison named %q", nr, name)
			return ""
		}
		if got := byName(d.Newer[0], "meta.json"); got != "unchanged" {
			t.Errorf("schema-v1.22.0 meta.json status = %q, want %q", got, "unchanged")
		}
		if got := byName(d.Newer[0], "schema.json"); got != "changed" {
			t.Errorf("schema-v1.22.0 schema.json status = %q, want %q", got, "changed")
		}
		if got := byName(d.Newer[1], "schema.json"); got != "not published" {
			t.Errorf("schema-v2.0.0 schema.json status = %q, want %q", got, "not published")
		}
	})

	t.Run("a release missing an asset within the one-hour grace window is excluded", func(t *testing.T) {
		t.Parallel()

		releases := []release{
			mkRelease(pin.Tag, false, false, "2026-08-20T00:00:00Z", pinAssetsAsReleaseAssets(pin)...),
			mkRelease("schema-v1.22.0", false, false, fixedNow.Add(-30*time.Minute).Format(time.RFC3339),
				releaseAsset{Name: "meta.json", Digest: digestPtr(pin.Assets[0].SHA256)}),
		}

		d := mustComputeDrift(t, pin, releases, refs, nil, fixedNow)
		if len(d.Newer) != 0 {
			t.Errorf("computeDrift(...).Newer = %+v, want empty: the release is within its one-hour upload grace window", d.Newer)
		}
	})

	t.Run("a release missing an asset past the one-hour grace window is reported", func(t *testing.T) {
		t.Parallel()

		releases := []release{
			mkRelease(pin.Tag, false, false, "2026-08-20T00:00:00Z", pinAssetsAsReleaseAssets(pin)...),
			mkRelease("schema-v1.22.0", false, false, fixedNow.Add(-2*time.Hour).Format(time.RFC3339),
				releaseAsset{Name: "meta.json", Digest: digestPtr(pin.Assets[0].SHA256)}),
		}

		d := mustComputeDrift(t, pin, releases, refs, nil, fixedNow)
		if len(d.Newer) != 1 {
			t.Fatalf("computeDrift(...).Newer = %+v, want 1 entry past the grace window", d.Newer)
		}
	})
}

func TestComputeDriftIntegrityFindings(t *testing.T) {
	t.Parallel()

	pin := testPin()
	pinnedReleaseWithAllAssets := mkRelease(pin.Tag, false, false, "2026-08-20T00:00:00Z", pinAssetsAsReleaseAssets(pin)...)

	tests := []struct {
		name     string
		releases []release
		refs     []gitRef
		tagObj   *gitTag
		wantKind string
	}{
		{"tag_absent when no ref names the pinned tag", []release{pinnedReleaseWithAllAssets}, nil, nil, "tag_absent"},
		{
			"tag_moved when the exact ref resolves to a different commit",
			[]release{pinnedReleaseWithAllAssets},
			[]gitRef{{Ref: "refs/tags/" + pin.Tag, Object: gitObject{Type: "commit", SHA: strings.Repeat("f", 40)}}},
			nil,
			"tag_moved",
		},
		{
			"release_absent when the publisher lists no release for the pinned tag",
			[]release{mkRelease("schema-v1.22.0", false, false, "2026-09-01T00:00:00Z", pinAssetsAsReleaseAssets(pin)...)},
			[]gitRef{matchingRef(pin)},
			nil,
			"release_absent",
		},
		{
			"asset_absent when the pinned release lacks a consumed asset",
			[]release{mkRelease(pin.Tag, false, false, "2026-08-20T00:00:00Z", releaseAsset{Name: "meta.json", Digest: digestPtr(pin.Assets[0].SHA256)})},
			[]gitRef{matchingRef(pin)},
			nil,
			"asset_absent",
		},
		{
			"asset_replaced when the pinned release's digest differs from the recorded one",
			[]release{mkRelease(pin.Tag, false, false, "2026-08-20T00:00:00Z",
				releaseAsset{Name: "meta.json", Digest: digestPtr(pin.Assets[0].SHA256)},
				releaseAsset{Name: "schema.json", Digest: digestPtr(strings.Repeat("9", 64))},
			)},
			[]gitRef{matchingRef(pin)},
			nil,
			"asset_replaced",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			d := mustComputeDrift(t, pin, tt.releases, tt.refs, tt.tagObj, fixedNow)
			var kinds []string
			for _, f := range d.Integrity {
				kinds = append(kinds, f.Kind)
			}
			if !slices.Contains(kinds, tt.wantKind) {
				t.Errorf("computeDrift(...).Integrity kinds = %v, want %q among them", kinds, tt.wantKind)
			}
		})
	}

	t.Run("an integrity finding is reported even when no newer release exists", func(t *testing.T) {
		t.Parallel()

		releases := []release{mkRelease(pin.Tag, false, false, "2026-08-20T00:00:00Z", releaseAsset{Name: "meta.json", Digest: digestPtr(pin.Assets[0].SHA256)})}
		d := mustComputeDrift(t, pin, releases, []gitRef{matchingRef(pin)}, nil, fixedNow)
		if len(d.Newer) != 0 {
			t.Fatalf("computeDrift(...).Newer = %+v, want empty", d.Newer)
		}
		if len(d.Integrity) == 0 {
			t.Error("computeDrift(...).Integrity = empty, want the asset_absent finding")
		}
	})

	t.Run("an annotated tag resolves through the tag object", func(t *testing.T) {
		t.Parallel()

		ref := gitRef{Ref: "refs/tags/" + pin.Tag, Object: gitObject{Type: "tag", SHA: strings.Repeat("e", 40)}}
		tagObj := &gitTag{Object: gitObject{Type: "commit", SHA: pin.Commit}}
		d := mustComputeDrift(t, pin, []release{pinnedReleaseWithAllAssets}, []gitRef{ref}, tagObj, fixedNow)
		for _, f := range d.Integrity {
			if f.Kind == "tag_moved" || f.Kind == "tag_absent" {
				t.Errorf("computeDrift(...).Integrity = %+v, want no tag finding: the tag object resolves to the recorded commit", d.Integrity)
			}
		}
	})
}

func TestComputeDriftFaultsOnMalformedInput(t *testing.T) {
	t.Parallel()

	pin := testPin()
	refs := []gitRef{matchingRef(pin)}
	pinnedReleaseWithAllAssets := mkRelease(pin.Tag, false, false, "2026-08-20T00:00:00Z", pinAssetsAsReleaseAssets(pin)...)

	tests := []struct {
		name     string
		releases []release
		refs     []gitRef
		tagObj   *gitTag
	}{
		{"empty stable set", nil, refs, nil},
		{"a stable-form tag exists only as a prerelease", []release{mkRelease("schema-v1.22.0", false, true, "2026-09-01T00:00:00Z")}, refs, nil},
		{
			"tag ref of type tag with no tag object supplied",
			[]release{pinnedReleaseWithAllAssets},
			[]gitRef{{Ref: "refs/tags/" + pin.Tag, Object: gitObject{Type: "tag", SHA: strings.Repeat("e", 40)}}},
			nil,
		},
		{
			"tag ref of an unrecognized object type",
			[]release{pinnedReleaseWithAllAssets},
			[]gitRef{{Ref: "refs/tags/" + pin.Tag, Object: gitObject{Type: "blob", SHA: strings.Repeat("e", 40)}}},
			nil,
		},
		{
			"malformed commit SHA",
			[]release{pinnedReleaseWithAllAssets},
			[]gitRef{{Ref: "refs/tags/" + pin.Tag, Object: gitObject{Type: "commit", SHA: "not-a-sha"}}},
			nil,
		},
		{
			"malformed published_at on a stable release",
			[]release{mkRelease("schema-v1.22.0", false, false, "not-a-date")},
			refs,
			nil,
		},
		{
			"malformed pinned-asset digest",
			[]release{mkRelease(pin.Tag, false, false, "2026-08-20T00:00:00Z", releaseAsset{Name: "meta.json", Digest: digestPtr(pin.Assets[0].SHA256)}, releaseAsset{Name: "schema.json", Digest: nil})},
			refs,
			nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, err := computeDrift(pin, tt.releases, tt.refs, tt.tagObj, fixedNow); err == nil {
				t.Errorf("computeDrift(%s) error = nil, want a fault", tt.name)
			}
		})
	}
}

func TestComputeDriftDeterministicUnderPermutation(t *testing.T) {
	t.Parallel()

	pin := testPin()
	refs := []gitRef{matchingRef(pin)}
	releases := []release{
		mkRelease(pin.Tag, false, false, "2026-08-20T00:00:00Z",
			releaseAsset{Name: "meta.json", Digest: digestPtr(pin.Assets[0].SHA256)},
			releaseAsset{Name: "schema.json", Digest: digestPtr(pin.Assets[1].SHA256)},
		),
		mkRelease("schema-v1.22.0", false, false, "2026-09-01T00:00:00Z",
			releaseAsset{Name: "schema.json", Digest: digestPtr(strings.Repeat("9", 64))},
			releaseAsset{Name: "meta.json", Digest: digestPtr(pin.Assets[0].SHA256)},
		),
		mkRelease("v1.9.1", false, false, "2026-08-25T00:00:00Z"),
	}

	want := mustComputeDrift(t, pin, releases, refs, nil, fixedNow)

	rng := rand.New(rand.NewPCG(1, 2))
	for range 5 {
		shuffled := slices.Clone(releases)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		for i, r := range shuffled {
			assets := slices.Clone(r.Assets)
			rng.Shuffle(len(assets), func(a, b int) { assets[a], assets[b] = assets[b], assets[a] })
			shuffled[i].Assets = assets
		}

		got := mustComputeDrift(t, pin, shuffled, refs, nil, fixedNow)
		if !reflect.DeepEqual(got.Newer, want.Newer) {
			t.Errorf("computeDrift(permuted releases).Newer = %+v, want %+v", got.Newer, want.Newer)
		}
		if !reflect.DeepEqual(got.Integrity, want.Integrity) {
			t.Errorf("computeDrift(permuted releases).Integrity = %+v, want %+v", got.Integrity, want.Integrity)
		}
		if !reflect.DeepEqual(got.Unrecognized, want.Unrecognized) {
			t.Errorf("computeDrift(permuted releases).Unrecognized = %+v, want %+v", got.Unrecognized, want.Unrecognized)
		}
	}
}
