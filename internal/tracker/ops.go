package tracker

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/mod/semver"

	ghclient "github.com/rancher/release-automation/internal/github"
)

// Issue is a thin wrapper carrying both the rendered body and the GitHub
// issue identity. Returned by FindOrCreate/Render-then-Update so callers
// can update the body without re-querying.
type Issue struct {
	Number int
	Title  string
	URL    string
	Body   string // current rendered body, including embedded metadata
}

// API is the slice of the GitHub client this package needs. *ghclient.Client
// satisfies it via duck typing; tests can supply an in-memory fake.
type API interface {
	ListOpenIssues(ctx context.Context, repo string, labels []string) ([]*ghclient.Issue, error)
	CreateIssue(ctx context.Context, repo, title, body string, labels, assignees []string) (*ghclient.Issue, error)
	UpdateIssueBody(ctx context.Context, repo string, num int, body string) error
	CloseIssue(ctx context.Context, repo string, num int, comment string) error
	ClosePR(ctx context.Context, repo string, num int, comment string) error
}

// FindOrCreate looks up the open tracker for (config, op.Dep, op.Version,
// op.LeafRepo, op.LeafBranch) in the automation repo. If absent, creates
// one rendered from `op`. If present, merges any state already stored in
// its metadata block back into `op` so the caller sees existing PR links.
//
// Lookup: query `bump-op + config:<name> + dep:<name> + leaf:<repo>:<branch>`
// and filter by version parsed from the title. The label quad alone usually
// returns one issue (older versions for the same leaf get superseded); the
// version filter handles the brief overlap window.
func FindOrCreate(ctx context.Context, gh API, automationRepo, config string, op *Op) (*Issue, error) {
	labels := Labels(config, op.Dep, op.LeafRepo, op.LeafBranch)
	candidates, err := gh.ListOpenIssues(ctx, automationRepo, labels)
	if err != nil {
		return nil, fmt.Errorf("find tracker for %s %s on %s %s: %w", op.Dep, op.Version, op.LeafRepo, op.LeafBranch, err)
	}
	for _, existing := range candidates {
		if ParseVersionFromTitle(existing.Title, op.Dep) != op.Version {
			continue
		}
		st, err := Envelope.Extract(existing.Body)
		if err != nil {
			return nil, fmt.Errorf("read state from tracker #%d: %w", existing.Number, err)
		}
		mergeState(op, st)
		return &Issue{Number: existing.Number, Title: existing.Title, URL: existing.URL, Body: existing.Body}, nil
	}

	body, err := renderForCreate(*op)
	if err != nil {
		return nil, err
	}
	created, err := gh.CreateIssue(ctx, automationRepo, Title(config, op.Dep, op.Version, op.LeafRepo, op.LeafBranch), body, labels, nil)
	if err != nil {
		return nil, fmt.Errorf("create tracker for %s %s on %s %s: %w", op.Dep, op.Version, op.LeafRepo, op.LeafBranch, err)
	}
	return &Issue{Number: created.Number, Title: created.Title, URL: created.URL, Body: body}, nil
}

// UpdateBody re-renders `op` and pushes the new body to the tracker issue.
// Call after mutating op.Targets (e.g. after opening a PR).
func UpdateBody(ctx context.Context, gh API, automationRepo string, issueNum int, op Op) error {
	body, err := renderForCreate(op)
	if err != nil {
		return err
	}
	return gh.UpdateIssueBody(ctx, automationRepo, issueNum, body)
}

// Supersede closes any open tracker for `dep` on this same leaf branch
// (within `config`) whose version is older than `newVersion`, including all
// open PRs linked from that tracker. Each closed PR + tracker gets a comment
// pointing at `newURL` for traceability.
//
// Scoped to (config, dep, leafRepo, leafBranch): newer wrangler v0.5.2 → main
// in config A supersedes older wrangler v0.5.1 → main in config A, but does
// NOT touch wrangler v0.5.1 → release/v2.13 (independent in-flight op on a
// different line) or wrangler v0.5.1 → main in config B (different config).
//
// "Older" is a strict semver comparison — equal versions don't supersede
// (FindOrCreate handles the dedupe for the same version).
func Supersede(ctx context.Context, gh API, automationRepo, config string, dep, leafRepo, leafBranch, newVersion string, newTrackerURL string) error {
	open, err := gh.ListOpenIssues(ctx, automationRepo, Labels(config, dep, leafRepo, leafBranch))
	if err != nil {
		return fmt.Errorf("scan trackers for config:%s dep:%s leaf:%s:%s: %w", config, dep, leafRepo, leafBranch, err)
	}
	for _, issue := range open {
		ver := ParseVersionFromTitle(issue.Title, dep)
		if ver == "" || ver == newVersion {
			continue
		}
		if semver.Compare(ver, newVersion) >= 0 {
			continue // not older
		}
		st, err := Envelope.Extract(issue.Body)
		if err != nil {
			return fmt.Errorf("read state from tracker #%d: %w", issue.Number, err)
		}
		// Close every still-open PR linked from the older tracker. Use
		// per-target repo names to find owner/name — we stored "config-key"
		// not "owner/name" in the metadata, so we rely on the label-derived
		// dep + the downstream's owner being known. For pilot 1 the
		// downstream is always rancher/<name>; we cheat and look up via the
		// PR's repo from the URL stored alongside.
		for _, t := range st.Targets {
			if t.PR == 0 || t.PRURL == "" {
				continue
			}
			repo, err := repoFromPRURL(t.PRURL)
			if err != nil {
				return fmt.Errorf("tracker #%d target %s: %w", issue.Number, t.Repo, err)
			}
			comment := fmt.Sprintf("Superseded by %s.", newTrackerURL)
			if err := gh.ClosePR(ctx, repo, t.PR, comment); err != nil {
				// Best-effort: log via wrapping but keep going so one stuck
				// PR doesn't strand the whole supersede.
				return fmt.Errorf("close PR %s#%d: %w", repo, t.PR, err)
			}
		}
		comment := fmt.Sprintf("Superseded by %s.", newTrackerURL)
		if err := gh.CloseIssue(ctx, automationRepo, issue.Number, comment); err != nil {
			return fmt.Errorf("close tracker #%d: %w", issue.Number, err)
		}
	}
	return nil
}

// mergeState reconciles op.Targets with what's already stored in the tracker.
// Union semantics: PR/state from `st` overlays matching op.Targets entries,
// AND stored targets not present in op.Targets are appended.
//
// The append path matters for manual `Bump dep` runs (RunBumpDep): the caller
// builds an Op with one target — the manual one — and we need to keep the
// auto-bumped targets that an earlier dispatch wrote. Without the append, a
// manual bump would clobber the tracker.
func mergeState(op *Op, st Persistent) {
	inOp := make(map[string]int, len(op.Targets))
	for i, t := range op.Targets {
		inOp[t.Repo+"|"+t.Branch] = i
	}
	for _, k := range st.Targets {
		if i, ok := inOp[k.Repo+"|"+k.Branch]; ok {
			op.Targets[i].PR = k.PR
			op.Targets[i].PRURL = k.PRURL
			op.Targets[i].State = k.State
			continue
		}
		op.Targets = append(op.Targets, k)
	}
}

// renderForCreate is Render but uses a fixed "now" placeholder when the
// caller hasn't provided one yet. Pass-through to Render with time.Now() —
// kept as a separate hook so tests can pin the timestamp.
var renderForCreate = func(op Op) (string, error) {
	return Render(op, nowFn())
}

// repoFromPRURL extracts "owner/name" from a PR HTML URL like
// https://github.com/rancher/rancher/pull/1234.
func repoFromPRURL(u string) (string, error) {
	const prefix = "https://github.com/"
	if !strings.HasPrefix(u, prefix) {
		return "", fmt.Errorf("not a github PR URL: %q", u)
	}
	rest := strings.TrimPrefix(u, prefix)
	parts := strings.SplitN(rest, "/", 4)
	if len(parts) < 3 {
		return "", fmt.Errorf("malformed PR URL: %q", u)
	}
	return parts[0] + "/" + parts[1], nil
}
