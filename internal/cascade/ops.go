package cascade

import (
	"context"
	"fmt"
	"time"

	ghclient "github.com/rancher/release-automation/internal/github"
)

// Issue is a thin wrapper carrying both the rendered body and the GitHub
// issue identity.
type Issue struct {
	Number int
	Title  string
	URL    string
	Body   string
}

// IssueAPI is the slice of the GitHub client this package needs. Declared
// here so callers (and tests) can supply any implementation; the concrete
// *ghclient.Client satisfies it via duck typing.
type IssueAPI interface {
	ListOpenIssues(ctx context.Context, repo string, labels []string) ([]*ghclient.Issue, error)
	CreateIssue(ctx context.Context, repo, title, body string, labels, assignees []string) (*ghclient.Issue, error)
	UpdateIssueBody(ctx context.Context, repo string, num int, body string) error
}

// SupersedeFunc is invoked by FindOrCreate for every existing open cascade on
// the same (leafRepo, leafBranch) whose stored explicit-source set differs
// from the new op. Caller decides what supersede means — typically: close
// the cascade's open PRs and close the issue with a comment.
type SupersedeFunc func(ctx context.Context, old *Issue) error

// FindOrCreate looks up the open cascade for (config, op.LeafRepo,
// op.LeafBranch). One cascade per (config, leaf branch) is the invariant —
// other configs may concurrently hold their own cascade for the same leaf
// branch, isolated by the config: label. The explicit-source set determines
// whether re-running matches an existing cascade or replaces it.
//
//   - Match (same explicit sources) → merge stored state into op, return that
//     cascade. Paired-latest stays pinned to whatever was stored at creation
//     time (mergeState's per-DepBump Version overlay handles this for free).
//   - No match → invoke `supersede` for every divergent existing cascade,
//     then create a fresh issue rendered from op.
func FindOrCreate(
	ctx context.Context,
	gh IssueAPI,
	automationRepo, config string,
	op *Op,
	supersede SupersedeFunc,
	actor string,
) (*Issue, error) {
	labels := Labels(config, op.LeafRepo, op.LeafBranch)
	candidates, err := gh.ListOpenIssues(ctx, automationRepo, labels)
	if err != nil {
		return nil, fmt.Errorf("find cascade for %s %s: %w", op.LeafRepo, op.LeafBranch, err)
	}
	for _, existing := range candidates {
		st, err := Envelope.Extract(existing.Body)
		if err != nil {
			return nil, fmt.Errorf("read state from cascade #%d: %w", existing.Number, err)
		}
		if !SameExplicitSources(op.Sources, st.Sources) {
			continue
		}
		mergeState(op, st)
		return &Issue{Number: existing.Number, Title: existing.Title, URL: existing.URL, Body: existing.Body}, nil
	}

	for _, existing := range candidates {
		if supersede == nil {
			continue
		}
		if err := supersede(ctx, &Issue{
			Number: existing.Number,
			Title:  existing.Title,
			URL:    existing.URL,
			Body:   existing.Body,
		}); err != nil {
			return nil, fmt.Errorf("supersede cascade #%d: %w", existing.Number, err)
		}
	}

	if actor != "" {
		op.TriggeredBy = actor
	}
	body, err := renderForCreate(*op)
	if err != nil {
		return nil, err
	}
	var assignees []string
	if actor != "" {
		assignees = []string{actor}
	}
	created, err := gh.CreateIssue(ctx, automationRepo, Title(config, op.LeafRepo, op.LeafBranch), body, labels, assignees)
	if err != nil {
		return nil, fmt.Errorf("create cascade for %s %s: %w", op.LeafRepo, op.LeafBranch, err)
	}
	return &Issue{Number: created.Number, Title: created.Title, URL: created.URL, Body: body}, nil
}

// UpdateBody re-renders `op` and pushes the new body to the cascade issue.
func UpdateBody(ctx context.Context, gh IssueAPI, automationRepo string, issueNum int, op Op) error {
	body, err := renderForCreate(op)
	if err != nil {
		return err
	}
	return gh.UpdateIssueBody(ctx, automationRepo, issueNum, body)
}

// mergeState reconciles op.Stages with what's already stored in the tracker.
// Stage shape (layer, repos, deps) is determined at cascade creation by
// ComputeStages and shouldn't drift across runs — so we overlay PR/version/
// state from `st` onto matching stage/bump positions and adopt CurrentStage
// from `st`. Mismatched shapes are kept from `op` (the recompute) on the
// theory that a config change since cascade creation should be visible.
//
// Source overlay: if stored has a Sources entry for a name in op.Sources,
// take the stored Version. Pins paired-latest to whatever was resolved at
// creation time, even if a newer release has dropped since.
func mergeState(op *Op, st Persistent) {
	op.CurrentStage = st.CurrentStage
	if op.TriggeredBy == "" {
		op.TriggeredBy = st.TriggeredBy
	}

	if len(st.Sources) > 0 {
		storedSources := make(map[string]Source, len(st.Sources))
		for _, s := range st.Sources {
			storedSources[s.Name] = s
		}
		for i := range op.Sources {
			if s, ok := storedSources[op.Sources[i].Name]; ok && s.Version != "" {
				op.Sources[i].Version = s.Version
			}
		}
	}

	for i := range op.Stages {
		if i >= len(st.Stages) {
			break
		}
		stored := st.Stages[i]
		// Bumps overlay by (Repo, Branch) — one Bump per (repo, branch) per
		// stage, so that's the stable identity. Per-dep Version values
		// overlay by Dep name within the matching Bump.
		idx := make(map[string]int, len(stored.Bumps))
		for j, bp := range stored.Bumps {
			idx[bp.Repo+"|"+bp.Branch] = j
		}
		for k := range op.Stages[i].Bumps {
			cur := op.Stages[i].Bumps[k]
			j, ok := idx[cur.Repo+"|"+cur.Branch]
			if !ok {
				continue
			}
			stb := stored.Bumps[j]
			op.Stages[i].Bumps[k].PR = stb.PR
			op.Stages[i].Bumps[k].PRURL = stb.PRURL
			op.Stages[i].Bumps[k].State = stb.State
			depIdx := make(map[string]int, len(stb.Deps))
			for d, dp := range stb.Deps {
				depIdx[dp.Dep] = d
			}
			for d := range op.Stages[i].Bumps[k].Deps {
				curDep := op.Stages[i].Bumps[k].Deps[d]
				if storedAt, ok := depIdx[curDep.Dep]; ok {
					if v := stb.Deps[storedAt].Version; v != "" {
						op.Stages[i].Bumps[k].Deps[d].Version = v
					}
				}
			}
		}
		// Tags overlay by (Repo, Branch) — what we wait on.
		tidx := make(map[string]int, len(stored.Tags))
		for j, tg := range stored.Tags {
			tidx[tg.Repo+"|"+tg.Branch] = j
		}
		for k := range op.Stages[i].Tags {
			cur := op.Stages[i].Tags[k]
			if j, ok := tidx[cur.Repo+"|"+cur.Branch]; ok {
				op.Stages[i].Tags[k].Version = stored.Tags[j].Version
				op.Stages[i].Tags[k].Tagged = stored.Tags[j].Tagged
				if stored.Tags[j].Expected != "" {
					op.Stages[i].Tags[k].Expected = stored.Tags[j].Expected
				}
				if stored.Tags[j].WorkflowURL != "" {
					op.Stages[i].Tags[k].WorkflowURL = stored.Tags[j].WorkflowURL
				}
			}
		}
	}
}

// renderForCreate is Render but uses the package-level clock so tests can
// pin the timestamp.
var renderForCreate = func(op Op) (string, error) {
	return Render(op, nowFn())
}

var nowFn = func() time.Time { return time.Now() }
