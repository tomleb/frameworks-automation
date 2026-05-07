package reconcile

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/rancher/release-automation/internal/cascade"
	ghclient "github.com/rancher/release-automation/internal/github"
)

// passCascade walks every open cascade tracker and advances its state machine:
//
//   - Polls the current stage's bump PRs; updates state.
//   - When all current-stage bumps have merged AND the stage has tag prompts,
//     the cascade is parked waiting for tags (handled by pass1Dispatch
//     claiming dispatched tags into the cascade's TagPrompts).
//   - When all current-stage tag prompts are satisfied, advances CurrentStage,
//     fills the next stage's bump versions from the recorded tag versions,
//     and opens those bumps.
//   - When all current-stage bumps merged AND no tag prompts (final stage),
//     closes the cascade.
//
// One cascade's failure doesn't stop the others — log and continue.
func (r *Reconciler) passCascade(ctx context.Context) error {
	cascades, err := r.gh.ListOpenIssues(ctx, r.settings.AutomationRepo, []string{cascade.LabelOp, cascade.ConfigLabel(r.configName)})
	if err != nil {
		return fmt.Errorf("list cascades: %w", err)
	}
	for _, c := range cascades {
		if err := r.advanceCascade(ctx, c); err != nil {
			log.Printf("passCascade: cascade #%d: %v", c.Number, err)
		}
	}
	return nil
}

func (r *Reconciler) advanceCascade(ctx context.Context, issue *ghclient.Issue) error {
	leafRepo, leafBranch := leafFromLabels(issue.Labels)
	if leafRepo == "" || leafBranch == "" {
		return fmt.Errorf("no leaf:* label")
	}
	st, err := cascade.Envelope.Extract(issue.Body)
	if err != nil {
		return fmt.Errorf("extract state: %w", err)
	}
	op := cascade.Op{
		LeafRepo:     leafRepo,
		LeafBranch:   leafBranch,
		Sources:      st.Sources,
		Stages:       st.Stages,
		CurrentStage: st.CurrentStage,
		TriggeredBy:  st.TriggeredBy,
	}

	if _, err := r.pollCascadeBumps(ctx, &op); err != nil {
		return err
	}

	// Heal any current-stage bumps left with empty dep versions — happens
	// when a prior version of the advance logic only folded in the
	// just-completed stage's tags, leaving cross-layer deps (e.g. a layer-1
	// repo that's also a direct dep of the leaf in a layer-3 bump) unfilled.
	// Idempotent on healed cascades.
	fillBumpDepsFromPriorTags(&op, op.CurrentStage)
	if _, err := r.openCascadeStageBumps(ctx, &op, op.CurrentStage, issue.Number, issue.URL); err != nil {
		return err
	}

	for {
		advanced, err := r.maybeAdvanceCascadeStage(ctx, &op, issue.Number, issue.URL)
		if err != nil {
			return err
		}
		if !advanced {
			break
		}
	}

	// Always rewrite the body. It's a regenerated view (Last reconciled
	// timestamp, render-only fixes), so gating on state mutation would
	// strand cosmetic changes until the next real transition.
	if err := cascade.UpdateBody(ctx, r.gh, r.settings.AutomationRepo, issue.Number, op); err != nil {
		return fmt.Errorf("update body: %w", err)
	}

	if cascadeComplete(op) {
		log.Printf("passCascade: cascade #%d complete, closing", issue.Number)
		if err := r.gh.CloseIssue(ctx, r.settings.AutomationRepo, issue.Number, "Cascade complete. Closing tracker."); err != nil {
			return fmt.Errorf("close cascade: %w", err)
		}
	}
	return nil
}

// pollCascadeBumps refreshes PR state for every bump in the current stage.
// Returns true when at least one bump's State changed.
func (r *Reconciler) pollCascadeBumps(ctx context.Context, op *cascade.Op) (bool, error) {
	if op.CurrentStage >= len(op.Stages) {
		return false, nil
	}
	mutated := false
	stage := &op.Stages[op.CurrentStage]
	for i := range stage.Bumps {
		bp := &stage.Bumps[i]
		if bp.PR == 0 || ghclient.IsTerminalPRState(bp.State) {
			continue
		}
		downstream, ok := r.cfg.Repos[bp.Repo]
		if !ok {
			log.Printf("passCascade: bump target %s vanished from config", bp.Repo)
			continue
		}
		ghRepo, err := downstream.GitHubRepo()
		if err != nil {
			return mutated, fmt.Errorf("downstream %s: %w", bp.Repo, err)
		}
		pr, err := r.gh.GetPR(ctx, ghRepo, bp.PR)
		if err != nil {
			return mutated, err
		}
		newState := derivePRState(pr)
		if newState != bp.State {
			log.Printf("passCascade: %s %s PR #%d: %q -> %q", bp.Repo, bp.Branch, bp.PR, displayState(bp.State), newState)
			bp.State = newState
			mutated = true
			if newState == "merged" && strings.HasPrefix(pr.HeadRef, "automation/") {
				if err := r.gh.DeleteBranch(ctx, ghRepo, pr.HeadRef); err != nil {
					log.Printf("passCascade: delete branch %s: %v", pr.HeadRef, err)
				}
			}
		}
	}
	return mutated, nil
}

// maybeAdvanceCascadeStage attempts to move the cascade forward one step:
//
//   - Final stage with all bumps terminal → cascadeComplete (no advance, but
//     the close happens in advanceCascade after this returns).
//   - Non-final stage: if all bumps merged AND all tag prompts satisfied,
//     advance CurrentStage, fill next-stage bump versions from tag versions,
//     and open the new stage's bumps.
//   - Otherwise: no advance.
func (r *Reconciler) maybeAdvanceCascadeStage(ctx context.Context, op *cascade.Op, issueNum int, trackerURL string) (bool, error) {
	if op.CurrentStage >= len(op.Stages) {
		return false, nil
	}
	stage := op.Stages[op.CurrentStage]
	if !allBumpsMerged(stage) {
		return false, nil
	}
	if op.CurrentStage == len(op.Stages)-1 {
		// Final stage merged — no advance, caller handles closing.
		return false, nil
	}
	if !allTagsSatisfied(stage) {
		return false, nil
	}

	op.CurrentStage++
	fillBumpDepsFromPriorTags(op, op.CurrentStage)
	log.Printf("passCascade: advanced to stage %d/%d (%s %s)",
		op.CurrentStage+1, len(op.Stages), op.LeafRepo, op.LeafBranch)
	if _, err := r.openCascadeStageBumps(ctx, op, op.CurrentStage, issueNum, trackerURL); err != nil {
		return true, err
	}
	return true, nil
}

// allBumpsMerged reports whether every bump in `stage` has reached state
// "merged". A "closed" bump (PR rejected) does NOT advance the cascade —
// the operator must intervene (close the cascade tracker manually).
func allBumpsMerged(stage cascade.Stage) bool {
	if len(stage.Bumps) == 0 {
		return true
	}
	for _, b := range stage.Bumps {
		if b.State != "merged" {
			return false
		}
	}
	return true
}

// allTagsSatisfied reports whether every TagPrompt in `stage` has Tagged=true
// AND a non-empty Version. Empty tag list (final stage) returns true.
func allTagsSatisfied(stage cascade.Stage) bool {
	for _, t := range stage.Tags {
		if !t.Tagged || t.Version == "" {
			return false
		}
	}
	return true
}

// cascadeComplete reports whether the cascade is at the final stage and
// every bump there has merged.
func cascadeComplete(op cascade.Op) bool {
	if op.CurrentStage != len(op.Stages)-1 {
		return false
	}
	return allBumpsMerged(op.Stages[op.CurrentStage])
}

// fillBumpDepsFromPriorTags fills empty Dep.Version entries in stage `idx`
// from the recorded tags of every stage strictly before `idx`. A leaf bump
// (e.g. rancher in the final stage) directly depends on layer-1 repos
// like steve, so its bundle must see steve's stage-1 tag — not just the
// just-completed prior stage's tags.
//
// Idempotent: only fills entries with Version=="", so paired-latest /
// explicit-source pre-fills are preserved.
func fillBumpDepsFromPriorTags(op *cascade.Op, idx int) {
	if idx <= 0 || idx >= len(op.Stages) {
		return
	}
	taggedVersion := map[string]string{}
	for i := 0; i < idx; i++ {
		for _, tg := range op.Stages[i].Tags {
			if tg.Tagged && tg.Version != "" {
				taggedVersion[tg.Repo] = tg.Version
			}
		}
	}
	stage := &op.Stages[idx]
	for i := range stage.Bumps {
		for j := range stage.Bumps[i].Deps {
			d := &stage.Bumps[i].Deps[j]
			if d.Version != "" {
				continue
			}
			if v, ok := taggedVersion[d.Dep]; ok {
				d.Version = v
			}
		}
	}
}

// tryClaimCascadeTag offers a just-emitted tag to every open cascade. If any
// cascade is in the awaiting-tags phase of its current stage (all bumps
// merged) and has an unclaimed TagPrompt for `dep`, it records (version,
// Tagged) on the prompt, persists the body, and returns true.
//
// Only the current stage is scanned. Future stages must not pre-claim tags:
// the cascade state machine requires bumps to merge before the corresponding
// tag is accepted, and a tag emitted while an earlier stage is still open
// must not short-circuit the ordering constraint.
//
// Returns (false, nil) when no cascade is waiting on this dep — caller
// proceeds with the regular bump path.
func (r *Reconciler) tryClaimCascadeTag(ctx context.Context, dep, version string) (bool, error) {
	cascades, err := r.gh.ListOpenIssues(ctx, r.settings.AutomationRepo, []string{cascade.LabelOp, cascade.ConfigLabel(r.configName)})
	if err != nil {
		return false, err
	}
	for _, issue := range cascades {
		st, err := cascade.Envelope.Extract(issue.Body)
		if err != nil {
			log.Printf("passCascade: claim: cascade #%d unreadable state: %v", issue.Number, err)
			continue
		}
		if st.CurrentStage >= len(st.Stages) {
			continue
		}
		// Only claim when all bumps in the current stage have merged — the
		// cascade is parked waiting for tags, not still waiting for PRs.
		if !allBumpsMerged(st.Stages[st.CurrentStage]) {
			continue
		}
		claimed := false
		for j := range st.Stages[st.CurrentStage].Tags {
			tg := &st.Stages[st.CurrentStage].Tags[j]
			if tg.Repo != dep || tg.Tagged {
				continue
			}
			tg.Version = version
			tg.Tagged = true
			claimed = true
			break
		}
		if !claimed {
			continue
		}
		leafRepo, leafBranch := leafFromLabels(issue.Labels)
		op := cascade.Op{
			LeafRepo:     leafRepo,
			LeafBranch:   leafBranch,
			Sources:      st.Sources,
			Stages:       st.Stages,
			CurrentStage: st.CurrentStage,
			TriggeredBy:  st.TriggeredBy,
		}
		if err := cascade.UpdateBody(ctx, r.gh, r.settings.AutomationRepo, issue.Number, op); err != nil {
			return false, fmt.Errorf("update cascade #%d after claim: %w", issue.Number, err)
		}
		log.Printf("passCascade: cascade #%d claimed %s %s", issue.Number, dep, version)
		return true, nil
	}
	return false, nil
}

// leafFromLabels parses the first leaf:<repo>:<branch> label encountered.
// Returns ("","") if absent. Branch may itself contain colons in theory
// (e.g. "release/v2.13" → no colon, but be defensive); we split on the
// first two colons only, leaving the remainder as branch.
func leafFromLabels(labels []string) (string, string) {
	const prefix = "leaf:"
	for _, l := range labels {
		if len(l) <= len(prefix) || l[:len(prefix)] != prefix {
			continue
		}
		rest := l[len(prefix):]
		// "repo:branch" — split on first ':'.
		for i := 0; i < len(rest); i++ {
			if rest[i] == ':' {
				return rest[:i], rest[i+1:]
			}
		}
	}
	return "", ""
}
