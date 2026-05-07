package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/rancher/release-automation/internal/cascade"
	"github.com/rancher/release-automation/internal/config"
)

func TestAllBumpsMerged(t *testing.T) {
	cases := []struct {
		name string
		st   cascade.Stage
		want bool
	}{
		{"empty", cascade.Stage{}, true},
		{"all merged", cascade.Stage{Bumps: []cascade.Bump{{State: "merged"}, {State: "merged"}}}, true},
		{"one open", cascade.Stage{Bumps: []cascade.Bump{{State: "merged"}, {State: "open"}}}, false},
		{"one closed (not merged)", cascade.Stage{Bumps: []cascade.Bump{{State: "merged"}, {State: "closed"}}}, false},
	}
	for _, c := range cases {
		if got := allBumpsMerged(c.st); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestAllTagsSatisfied(t *testing.T) {
	cases := []struct {
		name string
		st   cascade.Stage
		want bool
	}{
		{"no tags (final)", cascade.Stage{}, true},
		{"all tagged", cascade.Stage{Tags: []cascade.TagPrompt{{Tagged: true, Version: "v1"}, {Tagged: true, Version: "v2"}}}, true},
		{"one untagged", cascade.Stage{Tags: []cascade.TagPrompt{{Tagged: true, Version: "v1"}, {Tagged: false}}}, false},
		{"tagged but no version", cascade.Stage{Tags: []cascade.TagPrompt{{Tagged: true}}}, false},
	}
	for _, c := range cases {
		if got := allTagsSatisfied(c.st); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestCascadeComplete(t *testing.T) {
	mid := cascade.Op{
		CurrentStage: 0,
		Stages: []cascade.Stage{
			{Bumps: []cascade.Bump{{State: "merged"}}},
			{Bumps: []cascade.Bump{{State: "open"}}},
		},
	}
	if cascadeComplete(mid) {
		t.Error("not on final stage — should not be complete")
	}
	finalNotMerged := cascade.Op{
		CurrentStage: 1,
		Stages: []cascade.Stage{
			{Bumps: []cascade.Bump{{State: "merged"}}},
			{Bumps: []cascade.Bump{{State: "open"}}},
		},
	}
	if cascadeComplete(finalNotMerged) {
		t.Error("final stage open — should not be complete")
	}
	done := cascade.Op{
		CurrentStage: 1,
		Stages: []cascade.Stage{
			{Bumps: []cascade.Bump{{State: "merged"}}},
			{Bumps: []cascade.Bump{{State: "merged"}}},
		},
	}
	if !cascadeComplete(done) {
		t.Error("final stage merged — should be complete")
	}
}

func TestLeafFromLabels(t *testing.T) {
	cases := []struct {
		name              string
		labels            []string
		wantRepo, wantBr  string
	}{
		{"main", []string{"cascade-op", "dep:wrangler", "leaf:rancher:main"}, "rancher", "main"},
		{"release branch", []string{"leaf:rancher:release/v2.13"}, "rancher", "release/v2.13"},
		{"missing", []string{"cascade-op", "dep:wrangler"}, "", ""},
	}
	for _, c := range cases {
		repo, br := leafFromLabels(c.labels)
		if repo != c.wantRepo || br != c.wantBr {
			t.Errorf("%s: got (%q,%q) want (%q,%q)", c.name, repo, br, c.wantRepo, c.wantBr)
		}
	}
}

// TestTryClaimCascadeTag_ScopedByConfig verifies that a webhook-tag dispatch
// processed under config "test" doesn't claim a cascade owned by config
// "other" — the label-scoped cascade query is what isolates per-config
// state from the symmetric multi-config dispatch fan-out.
func TestTryClaimCascadeTag_ScopedByConfig(t *testing.T) {
	cfg := &config.Config{Repos: map[string]config.Repo{
		"rancher": {Kind: config.KindLeaf, Repo: "x/rancher"},
		"webhook": {Kind: config.KindPaired, Repo: "x/webhook"},
	}}
	gh := newFakeGH(nil)

	// Seed two cascades on the same leaf branch, one per config. Both are
	// awaiting a webhook tag (current-stage bumps merged, TagPrompt for
	// webhook on main is unclaimed).
	awaitingOp := func() cascade.Op {
		return cascade.Op{
			LeafRepo:   "rancher",
			LeafBranch: "main",
			Stages: []cascade.Stage{
				{
					Layer: 1,
					Bumps: []cascade.Bump{{Repo: "webhook", Branch: "main", State: "merged"}},
					Tags:  []cascade.TagPrompt{{Repo: "webhook", Branch: "main"}},
				},
			},
		}
	}
	body := func(op cascade.Op) string {
		b, err := cascade.Render(op, time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatalf("render: %v", err)
		}
		return b
	}
	testOp := awaitingOp()
	otherOp := awaitingOp()
	gh.CreateIssue(context.Background(), "owner/auto",
		cascade.Title("test", "rancher", "main"), body(testOp),
		cascade.Labels("test", "rancher", "main"), nil)
	gh.CreateIssue(context.Background(), "owner/auto",
		cascade.Title("other", "rancher", "main"), body(otherOp),
		cascade.Labels("other", "rancher", "main"), nil)

	r := newWithDeps("test", cfg, Settings{AutomationRepo: "owner/auto", GitHubToken: "x"}, gh, newFakeBumper(gh))

	claimed, err := r.tryClaimCascadeTag(context.Background(), "webhook", "v0.7.0")
	if err != nil {
		t.Fatalf("tryClaimCascadeTag: %v", err)
	}
	if !claimed {
		t.Fatalf("test-config cascade should have claimed the webhook tag")
	}

	// Inspect both issues — the test-config one should now record the tag,
	// the other-config one must be unchanged.
	for _, issue := range gh.snapshotIssues() {
		st, err := cascade.Envelope.Extract(issue.Body)
		if err != nil {
			t.Fatalf("extract %s: %v", issue.Title, err)
		}
		if len(st.Stages) == 0 || len(st.Stages[0].Tags) != 1 {
			t.Fatalf("unexpected shape on %s: %+v", issue.Title, st)
		}
		tg := st.Stages[0].Tags[0]
		switch {
		case has(issue.Labels, cascade.ConfigLabel("test")):
			if !tg.Tagged || tg.Version != "v0.7.0" {
				t.Errorf("test-config cascade should have claimed the tag, got %+v", tg)
			}
		case has(issue.Labels, cascade.ConfigLabel("other")):
			if tg.Tagged || tg.Version != "" {
				t.Errorf("other-config cascade must NOT be touched, got %+v", tg)
			}
		default:
			t.Errorf("issue %s missing config label: %v", issue.Title, issue.Labels)
		}
	}
}

func has(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

func TestCascadeBumpBranchName(t *testing.T) {
	cases := []struct {
		name         string
		issue        int
		bumpRepo     string
		bumpBranch   string
		want         string
	}{
		{"main", 42, "rancher", "main", "automation/cascade-42-bump-rancher-main"},
		{"release branch slashes flattened", 99, "steve", "release/v0.7", "automation/cascade-99-bump-steve-release-v0.7"},
	}
	for _, c := range cases {
		if got := cascadeBumpBranchName(c.issue, c.bumpRepo, c.bumpBranch); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
