// Package reconcile is the orchestration layer: it owns the multi-pass loop
// and dispatches into the focused packages (drift, pr, tracker).
//
// Pass 1 — detect new upstream releases (or react to a dispatch event) and
//          open bump PRs in target downstreams. Materialize tracker issues.
// Pass 2 — poll PR state for every open tracker. Tick checkboxes, close
//          trackers when all targets merged.
// Pass 3 — drift check on notify-only branches (independent libs on
//          release/*).
package reconcile

import (
	"context"
	"fmt"

	"github.com/rancher/release-automation/internal/config"
	ghclient "github.com/rancher/release-automation/internal/github"
	"github.com/rancher/release-automation/internal/pr"
)

type Settings struct {
	AutomationRepo string // owner/name where tracker issues live
	GitHubToken    string
	GitHubActor    string // login of the user who triggered the workflow; empty for cron
}

type DispatchEvent struct {
	Repo string // owner/name, e.g. "rancher/steve"
	Tag  string // e.g. "v0.7.5"
	SHA  string // commit SHA the tag points at
}

// gitHubClient is the slice of *ghclient.Client this package depends on. The
// union covers every method called directly on r.gh across the reconcile
// package plus the methods it forwards to cascade.FindOrCreate / UpdateBody
// (so r.gh structurally satisfies cascade.IssueAPI). *ghclient.Client
// satisfies this interface via duck typing — no method changes on the real
// client.
type gitHubClient interface {
	FetchFile(ctx context.Context, repo, ref, path string) (string, error)
	GetLatestReleaseTag(ctx context.Context, repo string) (string, error)
	ListReleaseTags(ctx context.Context, repo string) ([]string, error)
	CommitsAheadOf(ctx context.Context, repo, base, head string) (int, error)
	GetPR(ctx context.Context, repo string, number int) (*ghclient.PR, error)
	ClosePR(ctx context.Context, repo string, number int, comment string) error
	DeleteBranch(ctx context.Context, repo, branch string) error
	ListOpenIssues(ctx context.Context, repo string, labels []string) ([]*ghclient.Issue, error)
	ListIssuesAllStates(ctx context.Context, repo string, labels []string) ([]*ghclient.Issue, error)
	CreateIssue(ctx context.Context, repo, title, body string, labels, assignees []string) (*ghclient.Issue, error)
	UpdateIssueBody(ctx context.Context, repo string, number int, body string) error
	CloseIssue(ctx context.Context, repo string, number int, comment string) error
}

// Bumper is what the reconciler needs from a PR-bump executor. Production
// uses *pr.Bumper; tests inject a recording fake.
type Bumper interface {
	Open(ctx context.Context, req pr.Request) (*pr.Result, error)
}

type Reconciler struct {
	configName string // e.g. "rancher-chart-webhook" — basename of dependencies/<name>.yaml
	cfg        *config.Config
	settings   Settings
	gh         gitHubClient
	bumper     Bumper

	// versionTables caches VERSION.md tables for the lifetime of this
	// Reconciler instance. main.go creates a fresh Reconciler per
	// invocation (cron tick / dispatch / cascade run), so cache lifetime
	// is one tick — there's no stale-data risk. The reconciler runs
	// single-threaded (workflow concurrency group + no goroutine fanning
	// inside), so no mutex is needed.
	versionTables map[string]*config.VersionTable
}

func New(configName string, cfg *config.Config, s Settings) (*Reconciler, error) {
	if configName == "" {
		return nil, fmt.Errorf("configName is required")
	}
	if cfg == nil {
		return nil, fmt.Errorf("config is required")
	}
	if s.AutomationRepo == "" {
		return nil, fmt.Errorf("AutomationRepo is required")
	}
	if s.GitHubToken == "" {
		return nil, fmt.Errorf("GitHubToken is required")
	}
	gh := ghclient.NewClient(context.Background(), s.GitHubToken)
	return newWithDeps(configName, cfg, s, gh, pr.NewBumper(gh, s.GitHubToken)), nil
}

// newWithDeps wires a Reconciler with caller-supplied collaborators. Used by
// tests that inject in-memory fakes; prod goes through New.
func newWithDeps(configName string, cfg *config.Config, s Settings, gh gitHubClient, bumper Bumper) *Reconciler {
	return &Reconciler{
		configName:    configName,
		cfg:           cfg,
		settings:      s,
		gh:            gh,
		bumper:        bumper,
		versionTables: map[string]*config.VersionTable{},
	}
}

// RunCron is the safety-net path. Walks every upstream looking for new
// releases, then runs the remaining passes.
func (r *Reconciler) RunCron(ctx context.Context) error {
	if err := r.pass1Cron(ctx); err != nil {
		return fmt.Errorf("pass 1 (cron): %w", err)
	}
	return r.passesAfter1(ctx)
}

// RunDispatch is the fast path. Scoped to a single just-emitted tag; skips
// upstream discovery and goes straight to opening PRs for that tag, then
// runs the remaining passes so any in-flight ops keep moving.
func (r *Reconciler) RunDispatch(ctx context.Context, ev DispatchEvent) error {
	if err := r.pass1Dispatch(ctx, ev); err != nil {
		return fmt.Errorf("pass 1 (dispatch %s@%s): %w", ev.Repo, ev.Tag, err)
	}
	return r.passesAfter1(ctx)
}

func (r *Reconciler) passesAfter1(ctx context.Context) error {
	if err := r.pass2(ctx); err != nil {
		return fmt.Errorf("pass 2: %w", err)
	}
	if err := r.pass3(ctx); err != nil {
		return fmt.Errorf("pass 3: %w", err)
	}
	if err := r.passCascade(ctx); err != nil {
		return fmt.Errorf("pass cascade: %w", err)
	}
	return nil
}

func (r *Reconciler) pass2(ctx context.Context) error {
	return r.pass2PollPRs(ctx)
}

func (r *Reconciler) pass3(ctx context.Context) error {
	// TODO: for each independent dep, check go.mod on every release/* of its
	// dependents.
	return nil
}
