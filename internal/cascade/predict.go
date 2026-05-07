package cascade

import (
	"fmt"
	"strings"

	"golang.org/x/mod/semver"
)

// PredictNextPatch picks the highest patch matching `minor` (e.g. "v0.7")
// and returns minor + "." + (patch+1). Returns minor + ".0" when no prior
// release matches — this is the first patch on this minor. Pre-release
// suffixes on existing tags still bump the implied patch.
func PredictNextPatch(tags []string, minor string) string {
	highest := -1
	for _, t := range tags {
		patch, ok := patchForMinor(t, minor)
		if !ok {
			continue
		}
		if patch > highest {
			highest = patch
		}
	}
	if highest < 0 {
		return minor + ".0"
	}
	return fmt.Sprintf("%s.%d", minor, highest+1)
}

// PredictNextRC suggests the next rc tag on `minor`. Picks the highest
// semver-ordered release on the minor: if it has an rc.N suffix, returns
// the same major.minor.patch with rc.(N+1); if it's a GA, returns the next
// patch with rc.1; if no prior release exists, returns minor + ".0-rc.1".
func PredictNextRC(tags []string, minor string) string {
	var top string
	for _, t := range tags {
		if _, ok := patchForMinor(t, minor); !ok {
			continue
		}
		if top == "" || semver.Compare(t, top) > 0 {
			top = t
		}
	}
	if top == "" {
		return minor + ".0-rc.1"
	}
	base, rc, hasRC := SplitRC(top)
	if hasRC {
		return fmt.Sprintf("%s-rc.%d", base, rc+1)
	}
	patch, _ := patchForMinor(top, minor)
	return fmt.Sprintf("%s.%d-rc.1", minor, patch+1)
}

// PredictUnRC suggests the GA tag for an in-flight rc on `minor`. Picks
// the highest semver-ordered release on the minor: if it has an rc.N
// suffix, returns the suffix-stripped base (v0.9.0-rc.4 → v0.9.0). When
// the highest existing tag is already GA — or when no prior rc exists on
// the minor — returns "" because there is nothing to unRC; callers treat
// the empty string as "no hint available."
func PredictUnRC(tags []string, minor string) string {
	var top string
	for _, t := range tags {
		if _, ok := patchForMinor(t, minor); !ok {
			continue
		}
		if top == "" || semver.Compare(t, top) > 0 {
			top = t
		}
	}
	if top == "" {
		return ""
	}
	base, _, hasRC := SplitRC(top)
	if !hasRC {
		return ""
	}
	return base
}

// SplitRC parses a tag like "v0.7.5-rc.2" into ("v0.7.5", 2, true). For
// tags without an "-rc.N" suffix, returns (tag, 0, false).
func SplitRC(tag string) (string, int, bool) {
	i := strings.Index(tag, "-rc.")
	if i < 0 {
		return tag, 0, false
	}
	var n int
	if _, err := fmt.Sscanf(tag[i+len("-rc."):], "%d", &n); err != nil {
		return tag, 0, false
	}
	return tag[:i], n, true
}

// patchForMinor returns the patch number of `tag` when it belongs to
// `minor` (e.g. "v0.7"). Pre-release suffixes are tolerated — the patch
// is the integer between the second dot and the suffix. Returns (0,
// false) when the tag is invalid semver, doesn't match the minor, or has
// no parseable patch.
func patchForMinor(tag, minor string) (int, bool) {
	if !semver.IsValid(tag) || semver.MajorMinor(tag) != minor {
		return 0, false
	}
	rest := strings.TrimPrefix(tag, minor+".")
	if rest == "" {
		return 0, false
	}
	patchStr := rest
	if i := strings.IndexAny(rest, "-+"); i >= 0 {
		patchStr = rest[:i]
	}
	var patch int
	if _, err := fmt.Sscanf(patchStr, "%d", &patch); err != nil {
		return 0, false
	}
	return patch, true
}
