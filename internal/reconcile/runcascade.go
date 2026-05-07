package reconcile

import (
	"context"
	"fmt"
	"log"
	"strings"

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

	sources, stages, err := cascade.PlanStages(ctx, r.cfg, independents, leafRepo, leafBranch, leafTable, pairedTables, r.cascadePlanner())
	if err != nil {
		return err
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
		if bp.PR != 0 || !bp.Ready() {
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
			repoView, vErr := r.bumpRepoView(bp)
			if vErr != nil {
				log.Printf("cascade: existing-tag check %s %s: %v", bp.Repo, bp.Branch, vErr)
			} else if claimed, err := cascade.MaybeClaimExistingTag(ctx, op, stage, bp, repoView); err != nil {
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

// bumpRepoView builds a cascade.RepoView scoped to bp's repo. Wraps the
// reconciler's GH client and version-table cache so cascade can look up
// the minor, list release tags, fetch go.mod, and check ahead-of without
// learning about config or VERSION.md directly.
func (r *Reconciler) bumpRepoView(bp *cascade.Bump) (cascade.RepoView, error) {
	repoCfg, ok := r.cfg.Repos[bp.Repo]
	if !ok {
		return nil, fmt.Errorf("repo %q not in config", bp.Repo)
	}
	ghRepo, err := repoCfg.GitHubRepo()
	if err != nil {
		return nil, err
	}
	return &reconcileRepoView{r: r, repoKey: bp.Repo, ghRepo: ghRepo}, nil
}

// reconcileRepoView adapts the Reconciler's GH + config + version-table
// cache to cascade.RepoView for one (repoKey, ghRepo) pair.
type reconcileRepoView struct {
	r       *Reconciler
	repoKey string // config key, used by fetchVersionTable's cache
	ghRepo  string // owner/name
}

func (v *reconcileRepoView) Minor(ctx context.Context, branch string) (string, error) {
	tbl, err := v.r.fetchVersionTable(ctx, v.repoKey)
	if err != nil {
		return "", fmt.Errorf("fetch %s VERSION.md: %w", v.repoKey, err)
	}
	return tbl.LookupMinor(branch), nil
}

func (v *reconcileRepoView) ListReleaseTags(ctx context.Context) ([]string, error) {
	return v.r.gh.ListReleaseTags(ctx, v.ghRepo)
}

func (v *reconcileRepoView) FetchGoMod(ctx context.Context, ref string) (string, error) {
	return v.r.gh.FetchFile(ctx, v.ghRepo, ref, "go.mod")
}

func (v *reconcileRepoView) CommitsAheadOf(ctx context.Context, base, head string) (int, error) {
	return v.r.gh.CommitsAheadOf(ctx, v.ghRepo, base, head)
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

// cascadePlanner returns a cascade.RepoFactory that hands out per-repo
// RepoView adapters wrapping the reconciler's GH client and version-table
// cache. PlanStages calls Repo for each repo it needs to query.
func (r *Reconciler) cascadePlanner() cascade.RepoFactory {
	return &reconcilePlanner{r: r}
}

type reconcilePlanner struct{ r *Reconciler }

func (p *reconcilePlanner) Repo(name string) (cascade.RepoView, error) {
	repoCfg, ok := p.r.cfg.Repos[name]
	if !ok {
		return nil, fmt.Errorf("repo %q not in config", name)
	}
	ghRepo, err := repoCfg.GitHubRepo()
	if err != nil {
		return nil, err
	}
	return &reconcileRepoView{r: p.r, repoKey: name, ghRepo: ghRepo}, nil
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
		return cascade.PredictNextRC(tags, minor), nil
	case config.NextTagUnRC:
		return cascade.PredictUnRC(tags, minor), nil
	default:
		return cascade.PredictNextPatch(tags, minor), nil
	}
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
