package reconcile

import (
	"context"
	"fmt"
	"log"
	"strings"

	ghclient "github.com/rancher/release-automation/internal/github"
	"github.com/rancher/release-automation/internal/tracker"
)

// pass2PollPRs walks every open bump-op tracker, refreshes the state of each
// linked PR, updates the tracker body if any changed, and closes the tracker
// when every target reaches a terminal state.
//
// One tracker's failure doesn't stop the others — pass 2 is the catch-up
// loop, so we'd rather keep moving and retry next tick than abort the sweep.
func (r *Reconciler) pass2PollPRs(ctx context.Context) error {
	trackers, err := r.gh.ListOpenIssues(ctx, r.settings.AutomationRepo, []string{tracker.LabelOp, tracker.ConfigLabel(r.configName)})
	if err != nil {
		return fmt.Errorf("list trackers: %w", err)
	}
	for _, t := range trackers {
		if err := r.pollTracker(ctx, t); err != nil {
			log.Printf("pass2: tracker #%d: %v", t.Number, err)
		}
	}
	return nil
}

func (r *Reconciler) pollTracker(ctx context.Context, issue *ghclient.Issue) error {
	dep := depFromLabels(issue.Labels)
	if dep == "" {
		return fmt.Errorf("no dep:* label")
	}
	version := tracker.ParseVersionFromTitle(issue.Title, dep)
	if version == "" {
		return fmt.Errorf("title %q has no parseable version for dep %q", issue.Title, dep)
	}
	st, err := tracker.Envelope.Extract(issue.Body)
	if err != nil {
		return fmt.Errorf("extract state: %w", err)
	}
	op := tracker.Op{Dep: dep, Version: version, Targets: st.Targets}

	mutated := false
	for i, t := range op.Targets {
		if t.PR == 0 || ghclient.IsTerminalPRState(t.State) {
			continue
		}
		downstream, ok := r.cfg.Repos[t.Repo]
		if !ok {
			log.Printf("pass2: tracker #%d target %s vanished from config", issue.Number, t.Repo)
			continue
		}
		ghRepo, err := downstream.GitHubRepo()
		if err != nil {
			return fmt.Errorf("downstream %s: %w", t.Repo, err)
		}
		pr, err := r.gh.GetPR(ctx, ghRepo, t.PR)
		if err != nil {
			return err
		}
		newState := derivePRState(pr)
		if newState != t.State {
			log.Printf("pass2: tracker #%d %s %s PR #%d: %q -> %q", issue.Number, t.Repo, t.Branch, t.PR, displayState(t.State), newState)
			op.Targets[i].State = newState
			mutated = true
			if newState == "merged" && strings.HasPrefix(pr.HeadRef, "automation/") {
				if err := r.gh.DeleteBranch(ctx, ghRepo, pr.HeadRef); err != nil {
					log.Printf("pass2: tracker #%d delete branch %s: %v", issue.Number, pr.HeadRef, err)
				}
			}
		}
	}

	if mutated {
		if err := tracker.UpdateBody(ctx, r.gh, r.settings.AutomationRepo, issue.Number, op); err != nil {
			return fmt.Errorf("update body: %w", err)
		}
	}

	if allTerminal(op.Targets) {
		log.Printf("pass2: tracker #%d all targets terminal, closing", issue.Number)
		if err := r.gh.CloseIssue(ctx, r.settings.AutomationRepo, issue.Number, "All targets reached a terminal state. Closing tracker."); err != nil {
			return fmt.Errorf("close tracker: %w", err)
		}
	}
	return nil
}

// depFromLabels returns the value of the first `dep:*` label, or "".
func depFromLabels(labels []string) string {
	const prefix = "dep:"
	for _, l := range labels {
		if strings.HasPrefix(l, prefix) {
			return strings.TrimPrefix(l, prefix)
		}
	}
	return ""
}

// derivePRState collapses a PR's state into the tracker vocabulary. We only
// distinguish merged / closed / open here; CI and review signals are a
// follow-up (need ListCheckRuns + ListReviews and aren't in the demo flow).
func derivePRState(pr *ghclient.PR) string {
	if pr.Merged {
		return "merged"
	}
	if pr.State == "closed" {
		return "closed"
	}
	return "open"
}

// allTerminal returns true only when every target has a PR and is terminal.
// Targets without a PR (PR == 0) are pending bump opens — never terminal.
func allTerminal(targets []tracker.Target) bool {
	if len(targets) == 0 {
		return false
	}
	for _, t := range targets {
		if t.PR == 0 || !ghclient.IsTerminalPRState(t.State) {
			return false
		}
	}
	return true
}

// displayState mirrors tracker.displayState — empty means "open" for logs.
func displayState(s string) string {
	if s == "" {
		return "open"
	}
	return s
}
