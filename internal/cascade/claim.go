package cascade

import (
	"context"
	"fmt"
	"sort"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

// RepoView is the narrow read-only window into a single GitHub repo that
// MaybeClaimExistingTag needs. Implementations are scoped to one (repo,
// branch) pair — Minor reads VERSION.md for that repo, ListReleaseTags
// returns its release tags, FetchGoMod reads its go.mod at a tag, and
// CommitsAheadOf compares two refs in it.
type RepoView interface {
	// Minor returns the VERSION.md minor for `branch` (e.g. "v0.7"). Returns
	// "" with nil error when the branch is not listed in the table; non-nil
	// error means the table itself could not be fetched.
	Minor(ctx context.Context, branch string) (string, error)
	// ListReleaseTags returns every published release tag in the repo,
	// newest-first.
	ListReleaseTags(ctx context.Context) ([]string, error)
	// FetchGoMod returns the contents of go.mod at `ref` (a tag, branch, or
	// SHA).
	FetchGoMod(ctx context.Context, ref string) (string, error)
	// CommitsAheadOf reports how many commits `head` is ahead of `base` in
	// the repo. 0 means head equals or is behind base.
	CommitsAheadOf(ctx context.Context, base, head string) (int, error)
}

// MaybeClaimExistingTag handles the "branch was already at target before we
// touched it" case for one cascade Bump. When the highest published tag on
// bp's branch lineage already pins every dep in bp at its target version
// AND the branch hasn't advanced past it, that tag satisfies the
// cascade-mid prompt — no new release is required. Sets the matching
// TagPrompt's Version+Tagged on op.Stages[stage] in place and returns the
// claimed tag, or "" when no satisfying tag was found.
//
// `repo` is scoped to bp's repo — callers build one RepoView per bump.
func MaybeClaimExistingTag(ctx context.Context, op *Op, stage int, bp *Bump, repo RepoView) (string, error) {
	// No tag prompt for this bump → nothing to claim. Skip the VERSION.md
	// fetch and tag scan entirely (chart, for instance, has bump-only
	// stages and no VERSION.md to read).
	hasPrompt := false
	for _, tg := range op.Stages[stage].Tags {
		if tg.Repo == bp.Repo && tg.Branch == bp.Branch {
			hasPrompt = true
			break
		}
	}
	if !hasPrompt {
		return "", nil
	}

	tag, err := findSatisfyingTag(ctx, bp, repo)
	if err != nil {
		return "", err
	}
	if tag == "" {
		return "", nil
	}

	for j := range op.Stages[stage].Tags {
		tg := &op.Stages[stage].Tags[j]
		if tg.Repo == bp.Repo && tg.Branch == bp.Branch && !tg.Tagged {
			tg.Version = tag
			tg.Tagged = true
			return tag, nil
		}
	}
	return "", nil
}

// findSatisfyingTag returns the highest published release tag on bp's
// branch lineage (matched by minor) where go.mod already pins every dep
// in bp at its target version, rejecting any tag the branch has advanced
// past. Returns "" when no satisfying tag is found.
func findSatisfyingTag(ctx context.Context, bp *Bump, repo RepoView) (string, error) {
	minor, err := repo.Minor(ctx, bp.Branch)
	if err != nil {
		return "", fmt.Errorf("fetch minor for %s: %w", bp.Branch, err)
	}
	if minor == "" {
		return "", nil
	}
	tags, err := repo.ListReleaseTags(ctx)
	if err != nil {
		return "", err
	}
	candidates := tags[:0:0]
	for _, t := range tags {
		if semver.IsValid(t) && semver.MajorMinor(t) == minor {
			candidates = append(candidates, t)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return semver.Compare(candidates[i], candidates[j]) > 0
	})
	for _, tag := range candidates {
		gomod, err := repo.FetchGoMod(ctx, tag)
		if err != nil {
			// Best-effort: a single broken tag shouldn't strand the scan.
			continue
		}
		ok, err := tagSatisfiesDeps(gomod, bp.Deps, tag)
		if err != nil {
			continue
		}
		if !ok {
			continue
		}
		// Reject the tag if the branch has advanced past it: unreleased
		// commits mean the tag doesn't represent the full current state of
		// the branch and a new release is required.
		ahead, err := repo.CommitsAheadOf(ctx, tag, bp.Branch)
		if err != nil {
			continue
		}
		if ahead > 0 {
			return "", nil
		}
		return tag, nil
	}
	return "", nil
}

// tagSatisfiesDeps returns true when `gomod` requires every dep in `deps`
// at its target version (exact match). `tag` is used only for error
// messages.
func tagSatisfiesDeps(gomod string, deps []DepBump, tag string) (bool, error) {
	mf, err := modfile.Parse("go.mod", []byte(gomod), nil)
	if err != nil {
		return false, fmt.Errorf("parse go.mod at %s: %w", tag, err)
	}
	have := make(map[string]string, len(mf.Require))
	for _, req := range mf.Require {
		have[req.Mod.Path] = req.Mod.Version
	}
	for _, d := range deps {
		if have[d.Module] != d.Version {
			return false, nil
		}
	}
	return true, nil
}
