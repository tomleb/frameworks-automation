package reconcile

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"

	"github.com/rancher/release-automation/internal/cascade"
	"github.com/rancher/release-automation/internal/config"
	"github.com/rancher/release-automation/internal/pr"
)

// RunCascade is the cascade entrypoint. The .github/workflows/cascade.yaml
// dispatches the reconciler with -mode=cascade to walk a multi-source
// cascade up the DAG to a leaf branch, opening one stage of bump PRs at a
// time and prompting a re-tag at each intermediate layer.
//
// `independents` is the user-supplied source set: a map of independent dep
// name to target version. Empty means "no explicit independents — just
// pick up paired-latest into leaf". Paired deps are always picked up at
// the highest existing tag on the leaf-paired branch (paired-latest); the
// user doesn't (and shouldn't) supply versions for paired components.
//
// Pipeline:
//
//  1. Validate inputs; resolve leaf repo; assert each independent's version
//     is a published release.
//  2. Load VERSION.md tables (leaf + every paired in cfg).
//  3. cascade.ComputeStages → planned stages + sources (explicit + paired-latest).
//  4. FindOrCreate cascade tracker (per-leaf-branch identity); supersedes any
//     open cascade on the same leaf with a different explicit-source set.
//  5. Open stage 1 bump PRs; subsequent stages open as prior tags arrive
//     (handled in passCascade).
//  6. Persist state; run later passes so in-flight ops keep moving.
func (r *Reconciler) RunCascade(ctx context.Context, leafBranch string, independents map[string]string) error {
	if leafBranch == "" {
		return fmt.Errorf("leaf branch is required")
	}
	leaves := r.cfg.LeafRepos()
	if len(leaves) != 1 {
		return fmt.Errorf("expected exactly one leaf repo, found %d: %v", len(leaves), leaves)
	}
	leafRepo := leaves[0]

	for name, version := range independents {
		if version == "" {
			return fmt.Errorf("source %q: version is required (omit the input to skip)", name)
		}
		if !semver.IsValid(version) {
			return fmt.Errorf("source %q: invalid version %q (not semver)", name, version)
		}
		repoCfg, ok := r.cfg.Repos[name]
		if !ok {
			return fmt.Errorf("source %q not in config", name)
		}
		if repoCfg.Kind != config.KindIndependent {
			return fmt.Errorf("source %q is kind=%s; only independents may be cascade inputs", name, repoCfg.Kind)
		}
		if err := r.assertReleaseExists(ctx, repoCfg, name, version); err != nil {
			return err
		}
	}

	leafTable, err := r.fetchVersionTable(ctx, leafRepo)
	if err != nil {
		return fmt.Errorf("load leaf VERSION.md: %w", err)
	}
	pairedTables := make(map[string]*config.VersionTable)
	for name, repo := range r.cfg.Repos {
		if repo.Kind != config.KindPaired {
			continue
		}
		// Branch-template paired repos resolve their branch from the leaf
		// rancher minor — no VERSION.md fetch required.
		if repo.BranchTemplate != "" {
			continue
		}
		tbl, err := r.fetchVersionTable(ctx, name)
		if err != nil {
			return fmt.Errorf("load %s VERSION.md: %w", name, err)
		}
		pairedTables[name] = tbl
	}

	resolver := func(name, branch string) (string, error) {
		return r.resolveLatestForBranch(ctx, name, branch)
	}

	sources, stages, err := cascade.ComputeStages(r.cfg, independents, leafRepo, leafBranch, leafTable, pairedTables, resolver, nil)
	if err != nil {
		return fmt.Errorf("compute cascade stages: %w", err)
	}

	// Decide which paired-latest sources need their own bump→tag stage instead
	// of being consumed at their current tag. Two reasons trigger promotion:
	//
	//   - branch-ahead / pin-drift (detectStalePairedRepos): the dep has
	//     unreleased commits or pins a stale upstream — the next release has
	//     to come from HEAD.
	//   - unRC (forceUnRCPromotions): the dep's strategy is unrc and its
	//     latest tag is still an rc — the operator wants the same commit
	//     re-tagged as GA, which the staleness check would never flag (no
	//     branch-ahead, no pin-drift) but is the whole point of the unrc
	//     workflow.
	//
	// Both flavors merge into one promote set; ComputeStages re-runs once
	// with the union.
	leafMinor := leafTable.LookupMinor(leafBranch)
	promote, err := r.detectStalePairedRepos(ctx, sources, leafMinor, pairedTables)
	if err != nil {
		log.Printf("cascade: stale detection error (continuing): %v", err)
	}
	if promote == nil {
		promote = map[string]bool{}
	}
	for name := range r.forceUnRCPromotions(sources) {
		promote[name] = true
	}
	if len(promote) > 0 {
		log.Printf("cascade: promoting paired repos into propagation: %v", sortedKeys(promote))
		sources, stages, err = cascade.ComputeStages(r.cfg, independents, leafRepo, leafBranch, leafTable, pairedTables, resolver, promote)
		if err != nil {
			return fmt.Errorf("compute cascade stages (with promoted repos): %w", err)
		}
	}

	if err := r.fillTagPromptHints(ctx, stages, leafTable, pairedTables); err != nil {
		// Hints are advisory — log and continue with a barer prompt.
		log.Printf("cascade: fill tag prompt hints: %v", err)
	}

	op := cascade.Op{
		LeafRepo:   leafRepo,
		LeafBranch: leafBranch,
		Sources:    sources,
		Stages:     stages,
	}

	issue, err := cascade.FindOrCreate(ctx, r.gh, r.settings.AutomationRepo, r.configName, &op, r.supersedeCascade, r.settings.GitHubActor)
	if err != nil {
		return err
	}
	log.Printf("cascade[%s]: tracker for %s %s -> %s", r.configName, leafRepo, leafBranch, issue.URL)

	mutated, err := r.openCascadeStageBumps(ctx, &op, op.CurrentStage, issue.Number, issue.URL)
	if err != nil {
		return err
	}
	if mutated {
		if err := cascade.UpdateBody(ctx, r.gh, r.settings.AutomationRepo, issue.Number, op); err != nil {
			return fmt.Errorf("update cascade #%d body: %w", issue.Number, err)
		}
	}
	return r.passesAfter1(ctx)
}

// supersedeCascade closes an existing cascade whose explicit-source set has
// been replaced by a re-trigger. Closes any open bump PRs first so the
// supersede comment appears in the timeline before the close marker, then
// closes the issue itself.
func (r *Reconciler) supersedeCascade(ctx context.Context, old *cascade.Issue) error {
	log.Printf("cascade: superseding cascade #%d (explicit-source set changed)", old.Number)
	st, err := cascade.Envelope.Extract(old.Body)
	if err != nil {
		log.Printf("cascade: supersede #%d: extract state: %v", old.Number, err)
	} else {
		for _, stage := range st.Stages {
			for _, bp := range stage.Bumps {
				if bp.PR == 0 || bp.State == "merged" || bp.State == "closed" {
					continue
				}
				downstream, ok := r.cfg.Repos[bp.Repo]
				if !ok {
					continue
				}
				ghRepo, err := downstream.GitHubRepo()
				if err != nil {
					log.Printf("cascade: supersede #%d %s: %v", old.Number, bp.Repo, err)
					continue
				}
				comment := fmt.Sprintf("Superseded by a new cascade on %s with a different source set.", old.Title)
				if err := r.gh.ClosePR(ctx, ghRepo, bp.PR, comment); err != nil {
					log.Printf("cascade: supersede #%d close PR %s#%d: %v", old.Number, ghRepo, bp.PR, err)
				}
			}
		}
	}
	return r.gh.CloseIssue(ctx, r.settings.AutomationRepo, old.Number,
		"Superseded by a new cascade with a different explicit-source set.")
}

// openCascadeStageBumps opens one bump PR per Bump in `op.Stages[stage]`,
// bundling every Dep in the Bump into a single PR. Mutates op.Stages in
// place. Returns true when at least one Bump changed.
//
// A Bump is skipped if it already has a PR, or if any of its Deps still has
// Version=="" (we wait until every dep in the bundle is resolved before
// opening — bundling means we can't issue a partial PR and patch in the
// missing deps later).
func (r *Reconciler) openCascadeStageBumps(ctx context.Context, op *cascade.Op, stage, issueNum int, trackerURL string) (bool, error) {
	if stage < 0 || stage >= len(op.Stages) {
		return false, nil
	}
	mutated := false
	for i := range op.Stages[stage].Bumps {
		bp := &op.Stages[stage].Bumps[i]
		if bp.PR != 0 || !bumpReady(bp) {
			continue
		}
		downstream, ok := r.cfg.Repos[bp.Repo]
		if !ok {
			return mutated, fmt.Errorf("cascade target repo %q vanished from config", bp.Repo)
		}
		downstreamGH, err := downstream.GitHubRepo()
		if err != nil {
			return mutated, fmt.Errorf("cascade downstream %s: %w", bp.Repo, err)
		}
		req := pr.Request{
			Repo:       downstreamGH,
			Fork:       downstream.Fork,
			BaseBranch: bp.Branch,
			HeadBranch: cascadeBumpBranchName(issueNum, bp.Repo, bp.Branch),
			Modules:    bumpModules(bp),
			TrackerURL: trackerURL,
			Assignees:  actorAssignees(op.TriggeredBy),
		}
		log.Printf("cascade: opening stage %d %s %s -> %s base=%s head=%s",
			op.Stages[stage].Layer, bp.Repo, bp.Branch, req.Repo, req.BaseBranch, req.HeadBranch)
		res, err := r.bumper.Open(ctx, req)
		if err != nil {
			return mutated, fmt.Errorf("cascade bump %s on %s %s: %w", bp.Repo, req.Repo, req.BaseBranch, err)
		}
		log.Printf("cascade: %s", res.Notes)
		switch {
		case res.NoOp:
			bp.State = "merged"
			mutated = true
			// Branch is already at the target. If the latest published tag
			// on this minor also has every dep at target, no NEW tag is
			// needed — claim the existing tag for this stage's prompt and
			// let the cascade auto-advance.
			if claimed, err := r.maybeClaimExistingTag(ctx, op, stage, bp); err != nil {
				log.Printf("cascade: existing-tag check %s %s: %v", bp.Repo, bp.Branch, err)
			} else if claimed != "" {
				log.Printf("cascade: %s %s already at target via existing tag %s", bp.Repo, bp.Branch, claimed)
			}
		case res.PR != nil:
			bp.PR = res.PR.Number
			bp.PRURL = res.PR.URL
			bp.State = "open"
			mutated = true
		}
	}
	return mutated, nil
}

// maybeClaimExistingTag handles the "branch was already at target before we
// touched it" case. When the latest published tag on bp's branch lineage has
// every dep in bp pinned at its target version, that existing tag satisfies
// the cascade-mid prompt — no new release is required. Sets the matching
// TagPrompt's Version+Tagged and returns the claimed tag; returns "" when
// no satisfying tag was found.
func (r *Reconciler) maybeClaimExistingTag(ctx context.Context, op *cascade.Op, stage int, bp *cascade.Bump) (string, error) {
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
	tag, err := r.findExistingTagForBump(ctx, bp)
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
	// Final stage has no Tags — that's fine, no claim to make.
	return "", nil
}

// findExistingTagForBump returns the highest published release tag on bp's
// branch lineage (matched by minor) where go.mod already pins every dep in
// bp at its target version. Returns "" when no satisfying tag is found.
func (r *Reconciler) findExistingTagForBump(ctx context.Context, bp *cascade.Bump) (string, error) {
	repo, ok := r.cfg.Repos[bp.Repo]
	if !ok {
		return "", fmt.Errorf("repo %q not in config", bp.Repo)
	}
	ghRepo, err := repo.GitHubRepo()
	if err != nil {
		return "", err
	}
	tbl, err := r.fetchVersionTable(ctx, bp.Repo)
	if err != nil {
		return "", fmt.Errorf("fetch %s VERSION.md: %w", bp.Repo, err)
	}
	minor := tbl.LookupMinor(bp.Branch)
	if minor == "" {
		return "", nil
	}
	tags, err := r.gh.ListReleaseTags(ctx, ghRepo)
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
		ok, err := r.tagSatisfiesBump(ctx, ghRepo, tag, bp.Deps)
		if err != nil {
			log.Printf("cascade: check %s@%s: %v", ghRepo, tag, err)
			continue
		}
		if !ok {
			continue
		}
		// Reject the tag if the branch has advanced past it: unreleased commits
		// mean the tag doesn't represent the full current state of the branch and
		// a new release is required.
		ahead, err := r.gh.CommitsAheadOf(ctx, ghRepo, tag, bp.Branch)
		if err != nil {
			log.Printf("cascade: ahead-of check %s %s...%s: %v", ghRepo, tag, bp.Branch, err)
			continue
		}
		if ahead > 0 {
			return "", nil
		}
		return tag, nil
	}
	return "", nil
}

// tagSatisfiesBump returns true when go.mod at `tag` requires every dep in
// `deps` at its target version (exact match).
func (r *Reconciler) tagSatisfiesBump(ctx context.Context, ghRepo, tag string, deps []cascade.DepBump) (bool, error) {
	gomod, err := r.gh.FetchFile(ctx, ghRepo, tag, "go.mod")
	if err != nil {
		return false, err
	}
	mf, err := modfile.Parse("go.mod", []byte(gomod), nil)
	if err != nil {
		return false, fmt.Errorf("parse go.mod at %s@%s: %w", ghRepo, tag, err)
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

// bumpReady reports whether every Dep in `bp` has a non-empty Version. We
// only open a Bump's PR once the whole bundle is resolved, since cascades
// can't go back and add deps to an existing PR without rebasing it.
func bumpReady(bp *cascade.Bump) bool {
	if len(bp.Deps) == 0 {
		return false
	}
	for _, d := range bp.Deps {
		if d.Version == "" {
			return false
		}
	}
	return true
}

func bumpModules(bp *cascade.Bump) []pr.Module {
	out := make([]pr.Module, len(bp.Deps))
	for i, d := range bp.Deps {
		out[i] = pr.Module{Path: d.Module, Version: d.Version, Strategy: d.Strategy}
	}
	return out
}

// assertReleaseExists confirms `version` is a published release tag on
// `dep`'s repo. Pre-flight check so a typo (wrong version, wrong dep) fails
// before we create a tracker issue and try to clone downstreams.
//
// Released-tag check (not just any git tag): cascade is for finished
// releases — the per-repo Release workflow is what produces these tags, so
// "is there a Release with this tag" is the right question.
func (r *Reconciler) assertReleaseExists(ctx context.Context, depRepo config.Repo, dep, version string) error {
	ghRepo, err := depRepo.GitHubRepo()
	if err != nil {
		return fmt.Errorf("dep %s: %w", dep, err)
	}
	tags, err := r.gh.ListReleaseTags(ctx, ghRepo)
	if err != nil {
		return fmt.Errorf("list %s releases: %w", dep, err)
	}
	for _, t := range tags {
		if t == version {
			return nil
		}
	}
	return fmt.Errorf("dep %s has no published release %s on %s", dep, version, ghRepo)
}

// resolveLatestForBranch returns the highest existing release tag on
// `repoName`'s `branch` (matched by VERSION.md minor). Used by ComputeStages
// to pin paired-latest sources at cascade creation. "" with no error means
// the branch has no published release yet.
func (r *Reconciler) resolveLatestForBranch(ctx context.Context, repoName, branch string) (string, error) {
	repoCfg, ok := r.cfg.Repos[repoName]
	if !ok {
		return "", fmt.Errorf("repo %q not in config", repoName)
	}
	ghRepo, err := repoCfg.GitHubRepo()
	if err != nil {
		return "", err
	}
	var minor string
	if repoCfg.BranchTemplate != "" {
		// Branch-template repos (rancher/charts) carry the rancher minor in
		// the branch name itself, so VERSION.md isn't required — and isn't
		// available (rancher/charts has no VERSION.md). Extract by reversing
		// the template substitution.
		before, after, ok := strings.Cut(repoCfg.BranchTemplate, "{rancher-minor}")
		if !ok {
			return "", fmt.Errorf("repo %q: branch-template %q lacks {rancher-minor} placeholder", repoName, repoCfg.BranchTemplate)
		}
		if !strings.HasPrefix(branch, before) || !strings.HasSuffix(branch, after) {
			return "", fmt.Errorf("repo %q: branch %q does not match template %q", repoName, branch, repoCfg.BranchTemplate)
		}
		minor = strings.TrimSuffix(strings.TrimPrefix(branch, before), after)
	} else {
		tbl, err := r.fetchVersionTable(ctx, repoName)
		if err != nil {
			return "", fmt.Errorf("fetch %s VERSION.md: %w", repoName, err)
		}
		minor = tbl.LookupMinor(branch)
		if minor == "" {
			return "", fmt.Errorf("branch %q not in %s VERSION.md", branch, repoName)
		}
	}
	tags, err := r.gh.ListReleaseTags(ctx, ghRepo)
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

// fillTagPromptHints populates each TagPrompt's Expected (next-patch
// suggestion) and WorkflowURL by querying the prompt repo's releases. The
// minor used for filtering comes from each repo's own VERSION.md row for
// the prompt's branch — that's the version line the per-repo Release
// workflow validates against, so any future tag matching this minor is the
// correct cascade-mid tag.
//
// Hints are advisory: stale or missing hints don't break the cascade flow
// (the per-repo Release workflow validates the input version anyway).
func (r *Reconciler) fillTagPromptHints(
	ctx context.Context,
	stages []cascade.Stage,
	leafTable *config.VersionTable,
	dependentTables map[string]*config.VersionTable,
) error {
	for i := range stages {
		for j := range stages[i].Tags {
			tg := &stages[i].Tags[j]
			repo, ok := r.cfg.Repos[tg.Repo]
			if !ok {
				continue
			}
			ghRepo, err := repo.GitHubRepo()
			if err != nil {
				return fmt.Errorf("repo %s: %w", tg.Repo, err)
			}
			tg.WorkflowURL = fmt.Sprintf("https://github.com/%s/actions/workflows/cut-release.yaml", ghRepo)

			minor := minorForRepoBranch(tg.Repo, tg.Branch, leafTable, dependentTables)
			if minor == "" {
				continue
			}
			next, err := r.predictNextTag(ctx, ghRepo, minor, repo.NextTagStrategy)
			if err != nil {
				log.Printf("cascade: predict next tag %s %s: %v", tg.Repo, tg.Branch, err)
				continue
			}
			tg.Expected = next
		}
	}
	return nil
}

// minorForRepoBranch returns the VERSION.md minor for `repo`'s `branch`.
// The leaf repo uses leafTable; everything else uses dependentTables.
// Returns "" if the table is unavailable or the branch isn't listed.
func minorForRepoBranch(repo, branch string, leafTable *config.VersionTable, dependentTables map[string]*config.VersionTable) string {
	if tbl := dependentTables[repo]; tbl != nil {
		return tbl.LookupMinor(branch)
	}
	if leafTable != nil {
		return leafTable.LookupMinor(branch)
	}
	return ""
}

// predictNextTag dispatches to the per-repo NextTagStrategy. NextTagPatch
// (the default) bumps the patch number; NextTagRC bumps the rc.N suffix
// when the highest existing release on this minor already carries one,
// otherwise starts a fresh rc cycle on the next patch; NextTagUnRC drops
// the rc.N suffix from the highest existing rc tag, returning empty when
// nothing on the minor still carries one.
func (r *Reconciler) predictNextTag(ctx context.Context, ghRepo, minor string, strategy config.NextTagStrategy) (string, error) {
	tags, err := r.gh.ListReleaseTags(ctx, ghRepo)
	if err != nil {
		return "", err
	}
	switch strategy {
	case config.NextTagRC:
		return predictNextRC(tags, minor), nil
	case config.NextTagUnRC:
		return predictUnRC(tags, minor), nil
	default:
		return predictNextPatch(tags, minor), nil
	}
}

// predictNextPatch picks the highest patch matching `minor` (e.g. "v0.7")
// and returns minor + "." + (patch+1). Returns minor + ".0" when no prior
// release matches — this is the first patch on this minor. Pre-release
// suffixes on existing tags still bump the implied patch.
func predictNextPatch(tags []string, minor string) string {
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

// predictNextRC suggests the next rc tag on `minor`. Picks the highest
// semver-ordered release on the minor: if it has an rc.N suffix, returns
// the same major.minor.patch with rc.(N+1); if it's a GA, returns the
// next patch with rc.1; if no prior release exists, returns minor + ".0-rc.1".
func predictNextRC(tags []string, minor string) string {
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
	base, rc, hasRC := splitRC(top)
	if hasRC {
		return fmt.Sprintf("%s-rc.%d", base, rc+1)
	}
	patch, _ := patchForMinor(top, minor)
	return fmt.Sprintf("%s.%d-rc.1", minor, patch+1)
}

// predictUnRC suggests the GA tag for an in-flight rc on `minor`. Picks the
// highest semver-ordered release on the minor: if it has an rc.N suffix,
// returns the suffix-stripped base (v0.9.0-rc.4 → v0.9.0). When the highest
// existing tag is already GA — or when no prior rc exists on the minor —
// returns "" because there is nothing to unRC; fillTagPromptHints leaves
// Expected blank in that case (hints are advisory).
func predictUnRC(tags []string, minor string) string {
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
	base, _, hasRC := splitRC(top)
	if !hasRC {
		return ""
	}
	return base
}

// patchForMinor returns the patch number of `tag` when it belongs to
// `minor` (e.g. "v0.7"). Pre-release suffixes are tolerated — the patch is
// the integer between the second dot and the suffix. Returns (0, false)
// when the tag is invalid semver, doesn't match the minor, or has no
// parseable patch.
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

// splitRC parses a tag like "v0.7.5-rc.2" into ("v0.7.5", 2, true). For
// tags without an "-rc.N" suffix, returns (tag, 0, false).
func splitRC(tag string) (string, int, bool) {
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

// forceUnRCPromotions returns the paired-latest sources that need a bump→tag
// stage purely because their strategy is unrc. Unlike detectStalePairedRepos,
// no branch-ahead or pin-drift signal is consulted — the unrc workflow is
// "tag this rc'd commit as GA", and the same commit already carrying an rc
// tag is precisely what makes the re-tag necessary, not a reason to skip it.
//
// Promotion is gated on the pinned version actually carrying an -rc.N
// suffix: when the latest tag is already a GA, there is nothing to unRC and
// the cascade falls back to propagating the existing GA as paired-latest.
func (r *Reconciler) forceUnRCPromotions(sources []cascade.Source) map[string]bool {
	out := map[string]bool{}
	for _, src := range sources {
		if src.Explicit {
			continue
		}
		repo, ok := r.cfg.Repos[src.Name]
		if !ok {
			continue
		}
		if repo.NextTagStrategy != config.NextTagUnRC {
			continue
		}
		if _, _, hasRC := splitRC(src.Version); !hasRC {
			continue
		}
		out[src.Name] = true
	}
	return out
}

// detectStalePairedRepos scans paired-latest sources (and their managed paired
// deps transitively) for two flavors of staleness, both of which require
// promoting the affected repo into the cascade's propagation set so it gets a
// proper bump→tag stage:
//
//  1. Branch-ahead: the repo's branch HEAD has unreleased commits past its
//     latest tag. The next release will be from HEAD, so a re-cut is needed.
//  2. Pin-drift: the repo's go.mod (at its latest tag) pins one of its paired
//     deps at a version BELOW that dep's own latest tag. Without a re-cut,
//     downstream consumers picking up this repo at paired-latest would inherit
//     the stale upstream pin.
//
// The scan starts from each paired-latest source and follows go.mod deps one
// level at a time. Independent deps are skipped — their release cycle is
// separate and managed via explicit-independent cascades.
func (r *Reconciler) detectStalePairedRepos(
	ctx context.Context,
	sources []cascade.Source,
	leafMinor string,
	pairedTables map[string]*config.VersionTable,
) (map[string]bool, error) {
	moduleToRepo := r.cfg.ModuleToRepo()

	// depLatest caches per-dep latest-tag lookups so multiple parents pinning
	// the same dep don't trigger duplicate ListReleaseTags calls. An empty
	// string is a valid cached value (means "no published release on this
	// branch") and short-circuits the pin-drift comparison.
	depLatest := map[string]string{}
	resolveDepLatest := func(depName string) (string, error) {
		if v, ok := depLatest[depName]; ok {
			return v, nil
		}
		br, err := r.branchForRepo(depName, leafMinor, pairedTables)
		if err != nil || br == "" {
			depLatest[depName] = ""
			return "", err
		}
		tag, err := r.resolveLatestForBranch(ctx, depName, br)
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

		repoCfg, ok := r.cfg.Repos[name]
		if !ok {
			continue
		}
		ghRepo, err := repoCfg.GitHubRepo()
		if err != nil {
			continue
		}
		branch, err := r.branchForRepo(name, leafMinor, pairedTables)
		if err != nil || branch == "" {
			log.Printf("cascade stale: %s branch lookup: %v", name, err)
			continue
		}

		// Baseline is the dep's own latest release tag — never an upstream's
		// go.mod pin. The pin lags real releases, so it would false-positive
		// any dep that has tagged since the upstream's last release.
		latestTag, err := r.resolveLatestForBranch(ctx, name, branch)
		if err != nil {
			log.Printf("cascade stale: %s resolve latest tag on %s: %v", name, branch, err)
			continue
		}
		if latestTag == "" {
			log.Printf("cascade stale: %s has no released tag on %s — skipping", name, branch)
			continue
		}
		depLatest[name] = latestTag

		ahead, err := r.gh.CommitsAheadOf(ctx, ghRepo, latestTag, branch)
		if err != nil {
			log.Printf("cascade stale: %s ahead check: %v", name, err)
			continue
		}
		if ahead > 0 {
			log.Printf("cascade: %s branch %s is %d commit(s) ahead of %s — promoting into cascade stages", name, branch, ahead, latestTag)
			stale[name] = true
		}

		gomod, err := r.gh.FetchFile(ctx, ghRepo, latestTag, "go.mod")
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
			depCfg, ok := r.cfg.Repos[depName]
			if !ok || depCfg.Kind != config.KindPaired {
				continue
			}

			// Pin-drift: the parent's released go.mod pins this dep at a
			// version below the dep's own latest tag. Without a re-cut, any
			// downstream picking the parent up at paired-latest inherits the
			// stale upstream pin. Mark the PARENT (`name`) stale.
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

// sortedKeys returns the keys of m in sorted order, for deterministic logging.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// branchForRepo returns the branch of `repoName` that corresponds to
// `leafMinor`. Handles both VERSION.md paired repos and branch-template repos.
func (r *Reconciler) branchForRepo(repoName, leafMinor string, pairedTables map[string]*config.VersionTable) (string, error) {
	repoCfg, ok := r.cfg.Repos[repoName]
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

// actorAssignees returns a single-element slice for the given actor, or nil
// when actor is empty (cron runs have no actor).
func actorAssignees(actor string) []string {
	if actor == "" {
		return nil
	}
	return []string{actor}
}

// cascadeBumpBranchName is the canonical head-branch name for a cascade bump
// PR. Stable per (cascade issue, bump position) so:
//
//   - Re-runs idempotently dedupe via ListOpenPRsByHead (same branch → same
//     existing PR, not a duplicate).
//   - Different cascades on the same leaf branch (after supersede creates a
//     new issue with new sources) get distinct branch names — no collision
//     with the superseded cascade's now-closed PRs.
//
// The cascade issue number is the disambiguator; including the bump's
// (repo, branch) keeps multi-bump cascades on different head branches inside
// each downstream repo.
func cascadeBumpBranchName(cascadeIssue int, bumpRepo, bumpBranch string) string {
	br := strings.ReplaceAll(bumpBranch, "/", "-")
	return fmt.Sprintf("automation/cascade-%d-bump-%s-%s", cascadeIssue, bumpRepo, br)
}
