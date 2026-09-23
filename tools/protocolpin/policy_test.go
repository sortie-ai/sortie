package main

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
)

func TestFingerprintDeterministic(t *testing.T) {
	t.Parallel()

	pin := testPin()
	refs := []gitRef{matchingRef(pin)}
	releases := []release{
		mkRelease(pin.Tag, false, false, "2026-08-20T00:00:00Z", pinAssetsAsReleaseAssets(pin)...),
		mkRelease("schema-v1.22.0", false, false, "2026-09-01T00:00:00Z",
			releaseAsset{Name: "meta.json", Digest: digestPtr(pin.Assets[0].SHA256)},
			releaseAsset{Name: "schema.json", Digest: digestPtr(strings.Repeat("9", 64))},
		),
	}

	base := mustComputeDrift(t, pin, releases, refs, nil, fixedNow)
	baseFP, err := fingerprint(base)
	if err != nil {
		t.Fatalf("fingerprint(base) returned error: %v", err)
	}

	rng := rand.New(rand.NewPCG(3, 4))
	for range 5 {
		shuffled := slices.Clone(releases)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		for i, r := range shuffled {
			assets := slices.Clone(r.Assets)
			rng.Shuffle(len(assets), func(a, b int) { assets[a], assets[b] = assets[b], assets[a] })
			shuffled[i].Assets = assets
		}

		d := mustComputeDrift(t, pin, shuffled, refs, nil, fixedNow)
		fp, err := fingerprint(d)
		if err != nil {
			t.Fatalf("fingerprint(permuted) returned error: %v", err)
		}
		if fp != baseFP {
			t.Errorf("fingerprint(permuted releases) = %q, want %q (unchanged under permutation)", fp, baseFP)
		}
	}

	t.Run("changing any field changes the fingerprint", func(t *testing.T) {
		t.Parallel()

		mutated := base
		mutated.Pin.Tag = "schema-v9.9.9"
		mutatedFP, err := fingerprint(mutated)
		if err != nil {
			t.Fatalf("fingerprint(mutated) returned error: %v", err)
		}
		if mutatedFP == baseFP {
			t.Error("fingerprint(mutated Pin.Tag) equals fingerprint(base), want a different digest")
		}
	})
}

func TestExtractFingerprintLastOccurrence(t *testing.T) {
	t.Parallel()

	first := "sha256:" + strings.Repeat("1", 64)
	last := "sha256:" + strings.Repeat("2", 64)

	tests := []struct {
		name      string
		body      string
		wantFP    string
		wantFound bool
	}{
		{"no marker", "just some text", "", false},
		{
			"a single marker",
			"body text\n<!-- protocol-pin-fingerprint: " + first + " -->",
			first, true,
		},
		{
			"the last of two markers wins",
			"<!-- protocol-pin-fingerprint: " + first + " -->\nmore text\n<!-- protocol-pin-fingerprint: " + last + " -->",
			last, true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotFP, gotFound := extractFingerprint(tt.body)
			if gotFound != tt.wantFound {
				t.Fatalf("extractFingerprint(%q) found = %v, want %v", tt.body, gotFound, tt.wantFound)
			}
			if gotFP != tt.wantFP {
				t.Errorf("extractFingerprint(%q) = %q, want %q", tt.body, gotFP, tt.wantFP)
			}
		})
	}
}

func TestDecideActionTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                   string
		driftPresent           bool
		reportState            string
		bodyFingerprintMatches bool
		want                   string
	}{
		{"absent drift, absent report", false, "absent", false, "none"},
		{"absent drift, open report", false, "open", false, "close"},
		{"absent drift, open report, marker happens to match", false, "open", true, "close"},
		{"absent drift, closed report", false, "closed", false, "none"},
		{"present drift, absent report", true, "absent", false, "open"},
		{"present drift, open report, marker matches", true, "open", true, "none"},
		{"present drift, open report, marker does not match", true, "open", false, "update"},
		{"present drift, closed report, marker matches", true, "closed", true, "none"},
		{"present drift, closed report, marker does not match", true, "closed", false, "reopen"},
		{"present drift, closed report, no marker at all counts as not matching", true, "closed", false, "reopen"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := decideAction(tt.driftPresent, tt.reportState, tt.bodyFingerprintMatches); got != tt.want {
				t.Errorf("decideAction(%v, %q, %v) = %q, want %q", tt.driftPresent, tt.reportState, tt.bodyFingerprintMatches, got, tt.want)
			}
		})
	}
}

func TestDecideActionNoRepetition(t *testing.T) {
	t.Parallel()

	pin := testPin()
	refs := []gitRef{matchingRef(pin)}
	releases := []release{
		mkRelease(pin.Tag, false, false, "2026-08-20T00:00:00Z", pinAssetsAsReleaseAssets(pin)...),
		mkRelease("schema-v1.22.0", false, false, "2026-09-01T00:00:00Z",
			releaseAsset{Name: "meta.json", Digest: digestPtr(pin.Assets[0].SHA256)},
			releaseAsset{Name: "schema.json", Digest: digestPtr(strings.Repeat("9", 64))},
		),
	}
	d := mustComputeDrift(t, pin, releases, refs, nil, fixedNow)
	fp, err := fingerprint(d)
	if err != nil {
		t.Fatalf("fingerprint(d) returned error: %v", err)
	}

	openBody := renderBody("open", d, fp)
	bodyFP, hadMarker := extractFingerprint(openBody)
	if !hadMarker || bodyFP != fp {
		t.Fatalf("extractFingerprint(renderBody(open, ...)) = (%q, %v), want (%q, true)", bodyFP, hadMarker, fp)
	}

	if got := decideAction(true, "open", hadMarker && bodyFP == fp); got != "none" {
		t.Errorf("decideAction fed the report's own open decision = %q, want %q", got, "none")
	}
	if got := decideAction(true, "closed", hadMarker && bodyFP == fp); got != "none" {
		t.Errorf("decideAction fed the same body on a closed report = %q, want %q", got, "none")
	}

	closeBody := renderBody("close", drift{Pin: pin}, "")
	closeFP, closeHadMarker := extractFingerprint(closeBody)
	if closeHadMarker {
		t.Fatalf("extractFingerprint(renderBody(close, ...)) found a marker, want none")
	}
	if got := decideAction(true, "closed", closeHadMarker && closeFP == fp); got != "reopen" {
		t.Errorf("decideAction fed a closed report's marker-free close body, with drift present, = %q, want %q", got, "reopen")
	}
}
