package cascade

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"

	"github.com/rancher/release-automation/internal/config"
)

// RepoFactory hands out a per-repo RepoView. PlanStages calls Repo for
// each managed repo it needs to query (paired-latest resolution, stale
// detection). Implementations typically wrap the GH client + config so
// each RepoView is scoped to one (repoKey, ghRepo) pair.
type RepoFactory interface {
	Repo(name string) (RepoView, error)
}

// PlanStages plans a multi-source cascade end-to-end: ComputeStages →
// detect-stale + force-unRC promotions → ComputeStages again with the
// promoted set. Owns the choreography that callers previously had to
// orchestrate manually (compute, then run staleness detection, then
// recompute).
//
// `factory` is used for both paired-latest resolution (looking up the
// highest released tag on each paired dep's leaf-paired branch) and
// staleness detection (CommitsAheadOf + go.mod fetches).
//
// Returns the same (sources, stages) shape as ComputeStages.
func PlanStages(
	ctx context.Context,
	cfg *config.Config,
	independents map[string]string,
	leafRepo, leafBranch string,
	leafTable *config.VersionTable,
	pairedTables map[string]*config.VersionTable,
	factory RepoFactory,
) ([]Source, []Stage, error) {
	resolveLatest := func(name, branch string) (string, error) {
		return resolveLatestForBranch(ctx, cfg, factory, name, branch)
	}

	sources, stages, err := ComputeStages(cfg, independents, leafRepo, leafBranch, leafTable, pairedTables, resolveLatest, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("compute cascade stages: %w", err)
	}

	leafMinor := leafTable.LookupMinor(leafBranch)
	promote, err := detectStalePairedRepos(ctx, cfg, sources, leafMinor, pairedTables, factory)
	if err != nil {
		log.Printf("cascade: stale detection error (continuing): %v", err)
	}
	if promote == nil {
		promote = map[string]bool{}
	}
	for name := range forceUnRCPromotions(cfg, sources) {
		promote[name] = true
	}
	if len(promote) == 0 {
		return sources, stages, nil
	}

	log.Printf("cascade: promoting paired repos into propagation: %v", sortedKeys(promote))
	sources, stages, err = ComputeStages(cfg, independents, leafRepo, leafBranch, leafTable, pairedTables, resolveLatest, promote)
	if err != nil {
		return nil, nil, fmt.Errorf("compute cascade stages (with promoted repos): %w", err)
	}
	return sources, stages, nil
}

// resolveLatestForBranch returns the highest existing release tag on
// `repoName`'s `branch` (matched by VERSION.md minor or branch-template
// extraction). Used by ComputeStages to pin paired-latest sources at
// cascade creation. "" with no error means the branch has no published
// release yet.
func resolveLatestForBranch(ctx context.Context, cfg *config.Config, factory RepoFactory, repoName, branch string) (string, error) {
	repoCfg, ok := cfg.Repos[repoName]
	if !ok {
		return "", fmt.Errorf("repo %q not in config", repoName)
	}
	view, err := factory.Repo(repoName)
	if err != nil {
		return "", err
	}

	var minor string
	if repoCfg.BranchTemplate != "" {
		// Branch-template repos (rancher/charts) carry the rancher minor
		// in the branch name itself — VERSION.md isn't required (and
		// isn't available). Extract by reversing the template.
		before, after, ok := strings.Cut(repoCfg.BranchTemplate, "{rancher-minor}")
		if !ok {
			return "", fmt.Errorf("repo %q: branch-template %q lacks {rancher-minor} placeholder", repoName, repoCfg.BranchTemplate)
		}
		if !strings.HasPrefix(branch, before) || !strings.HasSuffix(branch, after) {
			return "", fmt.Errorf("repo %q: branch %q does not match template %q", repoName, branch, repoCfg.BranchTemplate)
		}
		minor = strings.TrimSuffix(strings.TrimPrefix(branch, before), after)
	} else {
		m, err := view.Minor(ctx, branch)
		if err != nil {
			return "", fmt.Errorf("fetch %s VERSION.md: %w", repoName, err)
		}
		if m == "" {
			return "", fmt.Errorf("branch %q not in %s VERSION.md", branch, repoName)
		}
		minor = m
	}

	tags, err := view.ListReleaseTags(ctx)
	if err != nil {
		return "", err
	}
	var best string
	for _, t := range tags {
		if !semver.IsValid(t) || semver.MajorMinor(t) != minor {
			continue
		}
		if best == "" || semver.Compare(t, best) > 0 {
			best = t
		}
	}
	return best, nil
}

// LeafBranchForDepVersion returns the leaf branch this `dep`@`version`
// auto-bump targets. Inverse of branchForRepo: where branchForRepo asks
// "given the leaf's minor, which branch on this paired dep matches?",
// LeafBranchForDepVersion asks "given a dep's version, which leaf
// branch does it flow into?".
//
//	independent → "main" (older release/* lines require a manual
//	              `Bump <dep>` workflow run; the auto path doesn't
//	              infer them since there's no version-pair to consult).
//	paired      → dep.VERSION.md row whose Minor == version's minor
//	              gives Pair (= leaf.minor); leaf.VERSION.md row whose
//	              Minor == that pair gives leaf.branch.
//
// `depTable` may be nil for independents (it's not consulted). Returns
// "" with nil error when the chain exists but the leaf hasn't cut the
// matching branch yet.
func LeafBranchForDepVersion(cfg *config.Config, dep, version string, leafTable, depTable *config.VersionTable) (string, error) {
	depCfg, ok := cfg.Repos[dep]
	if !ok {
		return "", fmt.Errorf("dep %q not in config", dep)
	}
	if depCfg.Kind == config.KindIndependent {
		return "main", nil
	}
	if depTable == nil {
		return "", fmt.Errorf("dep %q: depTable required for paired kind", dep)
	}
	if leafTable == nil {
		return "", fmt.Errorf("dep %q: leafTable required for paired kind", dep)
	}
	minor := semver.MajorMinor(version)
	if minor == "" {
		return "", fmt.Errorf("invalid semver %q", version)
	}
	pair := depTable.LookupPair(minor)
	if pair == "" {
		return "", fmt.Errorf("dep %s minor %s not in VERSION.md", dep, minor)
	}
	return leafTable.BranchForMinor(pair), nil
}

// branchForRepo returns the branch of `repoName` that corresponds to
// `leafMinor`. Handles independent (always "main"), branch-template
// paired (template substitution) and VERSION.md paired (table lookup).
func branchForRepo(cfg *config.Config, repoName, leafMinor string, pairedTables map[string]*config.VersionTable) (string, error) {
	repoCfg, ok := cfg.Repos[repoName]
	if !ok {
		return "", fmt.Errorf("repo %q not in config", repoName)
	}
	switch repoCfg.Kind {
	case config.KindIndependent:
		return "main", nil
	case config.KindPaired:
		br, err := repoCfg.ResolveBranch(leafMinor, pairedTables[repoName])
		if err != nil {
			return "", fmt.Errorf("resolve branch for %s: %w", repoName, err)
		}
		return br, nil
	}
	return "", fmt.Errorf("repo %q: unsupported kind %q", repoName, repoCfg.Kind)
}

// forceUnRCPromotions returns the paired-latest sources that need a
// bump→tag stage purely because their strategy is unrc. Unlike
// detectStalePairedRepos, no branch-ahead or pin-drift signal is
// consulted — the unrc workflow is "tag this rc'd commit as GA", and
// the same commit already carrying an rc tag is precisely what makes
// the re-tag necessary.
//
// Promotion is gated on the pinned version actually carrying an -rc.N
// suffix: when the latest tag is already a GA, there is nothing to
// unRC and the cascade falls back to propagating the existing GA as
// paired-latest.
func forceUnRCPromotions(cfg *config.Config, sources []Source) map[string]bool {
	out := map[string]bool{}
	for _, src := range sources {
		if src.Explicit {
			continue
		}
		repo, ok := cfg.Repos[src.Name]
		if !ok {
			continue
		}
		if repo.NextTagStrategy != config.NextTagUnRC {
			continue
		}
		if _, _, hasRC := SplitRC(src.Version); !hasRC {
			continue
		}
		out[src.Name] = true
	}
	return out
}

// detectStalePairedRepos scans paired-latest sources (and their managed
// paired deps transitively) for two flavors of staleness, both of which
// require promoting the affected repo into the cascade's propagation
// set so it gets a proper bump→tag stage:
//
//  1. Branch-ahead: the repo's branch HEAD has unreleased commits past
//     its latest tag. The next release will be from HEAD, so a re-cut
//     is needed.
//  2. Pin-drift: the repo's go.mod (at its latest tag) pins one of its
//     paired deps at a version BELOW that dep's own latest tag. Without
//     a re-cut, downstream consumers picking up this repo at
//     paired-latest would inherit the stale upstream pin.
//
// The scan starts from each paired-latest source and follows go.mod
// deps one level at a time. Independent deps are skipped — their
// release cycle is separate and managed via explicit-independent
// cascades.
func detectStalePairedRepos(
	ctx context.Context,
	cfg *config.Config,
	sources []Source,
	leafMinor string,
	pairedTables map[string]*config.VersionTable,
	factory RepoFactory,
) (map[string]bool, error) {
	moduleToRepo := cfg.ModuleToRepo()

	// depLatest caches per-dep latest-tag lookups so multiple parents
	// pinning the same dep don't trigger duplicate ListReleaseTags
	// calls. An empty string is a valid cached value (means "no
	// published release on this branch") and short-circuits the
	// pin-drift comparison.
	depLatest := map[string]string{}
	resolveDepLatest := func(depName string) (string, error) {
		if v, ok := depLatest[depName]; ok {
			return v, nil
		}
		br, err := branchForRepo(cfg, depName, leafMinor, pairedTables)
		if err != nil || br == "" {
			depLatest[depName] = ""
			return "", err
		}
		tag, err := resolveLatestForBranch(ctx, cfg, factory, depName, br)
		if err != nil {
			return "", err
		}
		depLatest[depName] = tag
		return tag, nil
	}

	stale := map[string]bool{}
	queue := map[string]bool{}
	for _, src := range sources {
		if !src.Explicit {
			queue[src.Name] = true
		}
	}

	checked := map[string]bool{}
	for len(queue) > 0 {
		var name string
		for n := range queue {
			name = n
			break
		}
		delete(queue, name)
		if checked[name] {
			continue
		}
		checked[name] = true

		if _, ok := cfg.Repos[name]; !ok {
			continue
		}
		view, err := factory.Repo(name)
		if err != nil {
			continue
		}
		branch, err := branchForRepo(cfg, name, leafMinor, pairedTables)
		if err != nil || branch == "" {
			log.Printf("cascade stale: %s branch lookup: %v", name, err)
			continue
		}

		// Baseline is the dep's own latest release tag — never an
		// upstream's go.mod pin. The pin lags real releases, so it
		// would false-positive any dep that has tagged since the
		// upstream's last release.
		latestTag, err := resolveLatestForBranch(ctx, cfg, factory, name, branch)
		if err != nil {
			log.Printf("cascade stale: %s resolve latest tag on %s: %v", name, branch, err)
			continue
		}
		if latestTag == "" {
			log.Printf("cascade stale: %s has no released tag on %s — skipping", name, branch)
			continue
		}
		depLatest[name] = latestTag

		ahead, err := view.CommitsAheadOf(ctx, latestTag, branch)
		if err != nil {
			log.Printf("cascade stale: %s ahead check: %v", name, err)
			continue
		}
		if ahead > 0 {
			log.Printf("cascade: %s branch %s is %d commit(s) ahead of %s — promoting into cascade stages", name, branch, ahead, latestTag)
			stale[name] = true
		}

		gomod, err := view.FetchGoMod(ctx, latestTag)
		if err != nil {
			log.Printf("cascade stale: fetch %s@%s go.mod: %v", name, latestTag, err)
			continue
		}
		mf, err := modfile.Parse("go.mod", []byte(gomod), nil)
		if err != nil {
			continue
		}
		for _, req := range mf.Require {
			depName, ok := moduleToRepo[req.Mod.Path]
			if !ok {
				continue
			}
			depCfg, ok := cfg.Repos[depName]
			if !ok || depCfg.Kind != config.KindPaired {
				continue
			}

			// Pin-drift: the parent's released go.mod pins this dep at
			// a version below the dep's own latest tag. Without a
			// re-cut, any downstream picking the parent up at
			// paired-latest inherits the stale upstream pin. Mark the
			// PARENT (`name`) stale.
			if pinVer := req.Mod.Version; semver.IsValid(pinVer) {
				if depTag, err := resolveDepLatest(depName); err != nil {
					log.Printf("cascade stale: %s pin-drift check for %s: %v", name, depName, err)
				} else if depTag != "" && semver.Compare(pinVer, depTag) < 0 && !stale[name] {
					log.Printf("cascade: %s pins %s %s but %s latest tag is %s — promoting %s into cascade stages",
						name, depName, pinVer, depName, depTag, name)
					stale[name] = true
				}
			}

			if !checked[depName] {
				queue[depName] = true
			}
		}
	}
	return stale, nil
}

// sortedKeys returns the keys of m in sorted order, for deterministic
// logging.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
