package main

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"time"
)

type drift struct {
	Pin          recordedPin
	Newer        []newerRelease
	Integrity    []integrityFinding
	Unrecognized []unrecognizedRelease
}

type newerRelease struct {
	Tag       string
	Published string
	NewMajor  bool
	Assets    []assetComparison
}

type assetComparison struct {
	Name   string
	Status string
}

type integrityFinding struct {
	Kind      string
	Subject   string
	Recorded  string
	Published string
}

type unrecognizedRelease struct {
	Tag       string
	Published string
}

var sha256DigestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func carriesConsumedAssets(pin recordedPin, r release) bool {
	names := make(map[string]bool, len(r.Assets))
	for _, a := range r.Assets {
		names[a.Name] = true
	}
	for _, want := range pin.Assets {
		if !names[want.Name] {
			return false
		}
	}
	return true
}

// classifyRelease favors "stable" over "pinned" when a release matches
// both, keeping it in the stable set rather than dropping it entirely;
// version comparison, not classification, is what excludes it from the
// newer list.
func classifyRelease(pin recordedPin, r release) string {
	switch {
	case !r.Draft && !r.Prerelease && stableTagRE.MatchString(r.TagName):
		return "stable"
	case !r.Draft && r.TagName == pin.Tag:
		return "pinned"
	case !r.Draft && !r.Prerelease && !stableTagRE.MatchString(r.TagName) && carriesConsumedAssets(pin, r):
		return "unrecognized"
	default:
		return ""
	}
}

func findPinnedRelease(pin recordedPin, releases []release) (release, bool) {
	for _, r := range releases {
		if !r.Draft && r.TagName == pin.Tag {
			return r, true
		}
	}
	return release{}, false
}

func parseStableVersion(tag string) (v [3]int, ok bool) {
	m := stableTagRE.FindStringSubmatch(tag)
	if m == nil {
		return v, false
	}
	for i, group := range m[1:] {
		n, err := strconv.Atoi(group)
		if err != nil {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

func compareVersions(a, b [3]int) int {
	for i := range a {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return 0
}

func assetStatus(want pinnedAsset, digest *string) string {
	if digest == nil || !sha256DigestRE.MatchString(*digest) {
		return "not published"
	}
	if *digest == "sha256:"+want.SHA256 {
		return "unchanged"
	}
	return "changed"
}

func assetComparisons(pin recordedPin, r release) []assetComparison {
	digests := make(map[string]*string, len(r.Assets))
	for _, a := range r.Assets {
		digests[a.Name] = a.Digest
	}
	out := make([]assetComparison, 0, len(pin.Assets))
	for _, want := range pin.Assets {
		out = append(out, assetComparison{Name: want.Name, Status: assetStatus(want, digests[want.Name])})
	}
	return out
}

func parsePublishedAt(tag, value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("release %q: published_at %q does not parse: %w", tag, value, err)
	}
	return t, nil
}

func computeDrift(pin recordedPin, releases []release, refs []gitRef, tagObject *gitTag, now time.Time) (drift, error) {
	pinnedVersion, ok := parseStableVersion(pin.Tag)
	if !ok {
		return drift{}, fmt.Errorf("pin tag %q does not parse as a stable schema version", pin.Tag)
	}

	type stableEntry struct {
		release     release
		version     [3]int
		publishedAt time.Time
	}

	var stable []stableEntry
	for _, r := range releases {
		if classifyRelease(pin, r) != "stable" {
			continue
		}
		v, ok := parseStableVersion(r.TagName)
		if !ok {
			return drift{}, fmt.Errorf("stable release %q does not parse as a stable schema version", r.TagName)
		}
		publishedAt, err := parsePublishedAt(r.TagName, r.PublishedAt)
		if err != nil {
			return drift{}, err
		}
		stable = append(stable, stableEntry{release: r, version: v, publishedAt: publishedAt})
	}
	if len(stable) == 0 {
		return drift{}, errors.New("the publisher lists no stable schema release")
	}

	var newerEntries []stableEntry
	for _, e := range stable {
		if compareVersions(e.version, pinnedVersion) <= 0 {
			continue
		}
		if !carriesConsumedAssets(pin, e.release) && now.Sub(e.publishedAt) < time.Hour {
			continue
		}
		newerEntries = append(newerEntries, e)
	}
	sort.Slice(newerEntries, func(i, j int) bool {
		return compareVersions(newerEntries[i].version, newerEntries[j].version) < 0
	})

	newer := make([]newerRelease, 0, len(newerEntries))
	for _, e := range newerEntries {
		newer = append(newer, newerRelease{
			Tag:       e.release.TagName,
			Published: e.publishedAt.UTC().Format("2006-01-02"),
			NewMajor:  e.version[0] != pinnedVersion[0],
			Assets:    assetComparisons(pin, e.release),
		})
	}

	integrity, err := integrityFindings(pin, releases, refs, tagObject)
	if err != nil {
		return drift{}, err
	}

	pinnedRelease, pinnedFound := findPinnedRelease(pin, releases)
	var pinnedPublished time.Time
	if pinnedFound {
		pinnedPublished, err = parsePublishedAt(pin.Tag, pinnedRelease.PublishedAt)
		if err != nil {
			return drift{}, err
		}
	}

	var unrecognized []unrecognizedRelease
	for _, r := range releases {
		if classifyRelease(pin, r) != "unrecognized" {
			continue
		}
		publishedAt, err := parsePublishedAt(r.TagName, r.PublishedAt)
		if err != nil {
			return drift{}, err
		}
		if !pinnedFound || !publishedAt.After(pinnedPublished) {
			continue
		}
		unrecognized = append(unrecognized, unrecognizedRelease{Tag: r.TagName, Published: publishedAt.UTC().Format("2006-01-02")})
	}
	sort.Slice(unrecognized, func(i, j int) bool {
		if unrecognized[i].Published != unrecognized[j].Published {
			return unrecognized[i].Published < unrecognized[j].Published
		}
		return unrecognized[i].Tag < unrecognized[j].Tag
	})

	return drift{Pin: pin, Newer: newer, Integrity: integrity, Unrecognized: unrecognized}, nil
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

func resolvePinnedCommit(pin recordedPin, refs []gitRef, tagObject *gitTag) (commit string, found bool, err error) {
	wantRef := "refs/tags/" + pin.Tag
	var exact *gitObject
	for _, r := range refs {
		if r.Ref == wantRef {
			obj := r.Object
			exact = &obj
			break
		}
	}
	if exact == nil {
		return "", false, nil
	}

	switch exact.Type {
	case "commit":
		if !shaRE.MatchString(exact.SHA) {
			return "", false, fmt.Errorf("tag %q resolves to a malformed commit SHA %q", pin.Tag, exact.SHA)
		}
		return exact.SHA, true, nil
	case "tag":
		if tagObject == nil {
			return "", false, fmt.Errorf("tag %q is an annotated tag but no tag object was supplied", pin.Tag)
		}
		if tagObject.Object.Type != "commit" {
			return "", false, fmt.Errorf("tag %q's tag object targets a %q, want a commit", pin.Tag, tagObject.Object.Type)
		}
		if !shaRE.MatchString(tagObject.Object.SHA) {
			return "", false, fmt.Errorf("tag %q's tag object targets a malformed commit SHA %q", pin.Tag, tagObject.Object.SHA)
		}
		return tagObject.Object.SHA, true, nil
	default:
		return "", false, fmt.Errorf("tag %q's ref object has type %q, want commit or tag", pin.Tag, exact.Type)
	}
}

// integrityFindings returns findings in a fixed kind order regardless of
// releases' own order, so the result stays stable across runs.
func integrityFindings(pin recordedPin, releases []release, refs []gitRef, tagObject *gitTag) ([]integrityFinding, error) {
	var findings []integrityFinding

	resolvedCommit, tagFound, err := resolvePinnedCommit(pin, refs, tagObject)
	if err != nil {
		return nil, err
	}
	switch {
	case !tagFound:
		findings = append(findings, integrityFinding{Kind: "tag_absent", Recorded: pin.Tag})
	case resolvedCommit != pin.Commit:
		findings = append(findings, integrityFinding{Kind: "tag_moved", Recorded: pin.Commit, Published: resolvedCommit})
	}

	pinnedRelease, pinnedFound := findPinnedRelease(pin, releases)
	if !pinnedFound {
		findings = append(findings, integrityFinding{Kind: "release_absent", Recorded: pin.Tag})
		return findings, nil
	}

	digests := make(map[string]*string, len(pinnedRelease.Assets))
	for _, a := range pinnedRelease.Assets {
		digests[a.Name] = a.Digest
	}

	assets := append([]pinnedAsset(nil), pin.Assets...)
	sort.Slice(assets, func(i, j int) bool { return assets[i].Name < assets[j].Name })

	var absent, replaced []integrityFinding
	for _, a := range assets {
		digest, present := digests[a.Name]
		if !present {
			absent = append(absent, integrityFinding{Kind: "asset_absent", Subject: a.Name, Recorded: "sha256:" + a.SHA256})
			continue
		}
		if digest == nil || !sha256DigestRE.MatchString(*digest) {
			return nil, fmt.Errorf("pinned release %q: asset %q has a malformed digest", pin.Tag, a.Name)
		}
		if *digest != "sha256:"+a.SHA256 {
			replaced = append(replaced, integrityFinding{Kind: "asset_replaced", Subject: a.Name, Recorded: "sha256:" + a.SHA256, Published: *digest})
		}
	}
	findings = append(findings, absent...)
	findings = append(findings, replaced...)

	return findings, nil
}
