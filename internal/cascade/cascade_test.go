package cascade

import (
	"strings"
	"testing"
	"time"
)

func TestRender_BodyContainsStagesAndState(t *testing.T) {
	op := Op{
		LeafRepo:     "rancher",
		LeafBranch:   "main",
		CurrentStage: 0,
		Sources: []Source{
			{Name: "wrangler", Version: "v0.5.1", Explicit: true},
		},
		Stages: []Stage{
			{Layer: 1,
				Bumps: []Bump{{Repo: "steve", Branch: "main", PR: 42, PRURL: "https://github.com/x/y/pull/42", State: "open",
					Deps: []DepBump{{Dep: "wrangler", Module: "github.com/x/wrangler", Version: "v0.5.1"}}}},
				Tags: []TagPrompt{{Repo: "steve", Branch: "main"}},
			},
			{Layer: 2,
				Bumps: []Bump{{Repo: "rancher", Branch: "main",
					Deps: []DepBump{{Dep: "steve", Module: "github.com/x/steve"}}}},
			},
		},
	}
	body, err := Render(op, time.Date(2026, 4, 21, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(body, "rancher main") {
		t.Errorf("body missing leaf line: %s", body)
	}
	if !strings.Contains(body, "wrangler v0.5.1 (explicit)") {
		t.Errorf("body missing explicit source line: %s", body)
	}
	if !strings.Contains(body, "Stage 1: bump → tag (current)") {
		t.Errorf("body missing stage 1 marker: %s", body)
	}
	if !strings.Contains(body, "Stage 2: bump (final)") {
		t.Errorf("body missing stage 2 final marker: %s", body)
	}
	if !strings.Contains(body, "[#42](https://github.com/x/y/pull/42) (open · [checks](https://github.com/x/y/pull/42/checks))") {
		t.Errorf("body missing linked PR ref: %s", body)
	}
	if !strings.Contains(body, "(steve@_pending_)") {
		t.Errorf("body missing pending version placeholder: %s", body)
	}
	if !strings.Contains(body, "Last reconciled: 2026-04-21T10:00:00Z") {
		t.Errorf("body missing reconciled timestamp: %s", body)
	}
	st, err := Envelope.Extract(body)
	if err != nil {
		t.Fatalf("extract from rendered: %v", err)
	}
	if len(st.Stages) != 2 {
		t.Errorf("expected 2 stages in state, got %d", len(st.Stages))
	}
	if len(st.Sources) != 1 || st.Sources[0].Name != "wrangler" || !st.Sources[0].Explicit {
		t.Errorf("expected sources persisted, got %+v", st.Sources)
	}
}

func TestRender_PairedLatestSource(t *testing.T) {
	op := Op{
		LeafRepo:   "rancher",
		LeafBranch: "main",
		Sources: []Source{
			{Name: "steve", Version: "v0.7.5"}, // implicit paired-latest
		},
		Stages: []Stage{
			{Layer: 1,
				Bumps: []Bump{{Repo: "rancher", Branch: "main",
					Deps: []DepBump{{Dep: "steve", Module: "github.com/x/steve", Version: "v0.7.5"}}}},
			},
		},
	}
	body, err := Render(op, time.Date(2026, 4, 21, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(body, "steve v0.7.5 (paired-latest)") {
		t.Errorf("body missing paired-latest source line: %s", body)
	}
}

func TestRender_MermaidDiagram(t *testing.T) {
	op := Op{
		LeafRepo:     "rancher",
		LeafBranch:   "main",
		CurrentStage: 1,
		Sources:      []Source{{Name: "wrangler", Version: "v0.5.1", Explicit: true}},
		Stages: []Stage{
			{
				Layer: 1,
				Bumps: []Bump{{Repo: "steve", Branch: "main", State: "merged",
					Deps: []DepBump{{Dep: "wrangler", Module: "github.com/x/wrangler", Version: "v0.5.1"}}}},
				Tags: []TagPrompt{{Repo: "steve", Branch: "main", Tagged: true, Version: "v0.7.6"}},
			},
			{
				Layer: 2,
				Bumps: []Bump{{Repo: "rancher", Branch: "main",
					Deps: []DepBump{{Dep: "steve", Module: "github.com/x/steve"}}}},
				Tags: []TagPrompt{{Repo: "rancher", Branch: "main"}},
			},
		},
	}
	body, err := Render(op, time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	// Diagram block appears between Sources and the first ## Stage header.
	sourcesIdx := strings.Index(body, "## Sources")
	diagramIdx := strings.Index(body, "## Diagram")
	stageIdx := strings.Index(body, "## Stage")
	if sourcesIdx < 0 || diagramIdx < 0 || stageIdx < 0 {
		t.Fatalf("missing expected sections in body:\n%s", body)
	}
	if !(sourcesIdx < diagramIdx && diagramIdx < stageIdx) {
		t.Errorf("section order wrong: Sources=%d Diagram=%d Stage=%d", sourcesIdx, diagramIdx, stageIdx)
	}

	// init config and flowchart direction.
	if !strings.Contains(body, "'fontSize': '12px'") || !strings.Contains(body, "'nodeSpacing': 15") {
		t.Errorf("missing fontSize/nodeSpacing init config in body:\n%s", body)
	}
	if !strings.Contains(body, "flowchart TD") {
		t.Errorf("missing 'flowchart TD' in body:\n%s", body)
	}

	// Merged bump is :::done; unmerged bump is :::pending.
	if !strings.Contains(body, `"bump steve@main"]:::done`) {
		t.Errorf("merged bump should be :::done:\n%s", body)
	}
	if !strings.Contains(body, `"bump rancher@main"]:::pending`) {
		t.Errorf("unmerged bump should be :::pending:\n%s", body)
	}

	// Tagged tag is :::done; untagged tag is :::pending.
	if !strings.Contains(body, `"tag steve@main"]:::done`) {
		t.Errorf("tagged tag should be :::done:\n%s", body)
	}
	if !strings.Contains(body, `"tag rancher@main"]:::pending`) {
		t.Errorf("untagged tag should be :::pending:\n%s", body)
	}

	// Stage subgraph classes: i=0 < CurrentStage=1 → done; i=1 == CurrentStage → current.
	if !strings.Contains(body, "class S0 done") {
		t.Errorf("stage 0 should be class done:\n%s", body)
	}
	if !strings.Contains(body, "class S1 current") {
		t.Errorf("stage 1 should be class current:\n%s", body)
	}

	// Inter-stage edge.
	if !strings.Contains(body, "S0 --> S1") {
		t.Errorf("missing inter-stage edge S0 --> S1:\n%s", body)
	}

	// Existing assertions still pass: state block round-trips.
	st, err := Envelope.Extract(body)
	if err != nil {
		t.Fatalf("extract state: %v", err)
	}
	if len(st.Stages) != 2 {
		t.Errorf("expected 2 stages in state, got %d", len(st.Stages))
	}
}

func TestRenderTagRef(t *testing.T) {
	cases := []struct {
		name string
		t    TagPrompt
		want string
	}{
		{"pending no hints", TagPrompt{}, "_pending_"},
		{"pending with expected only", TagPrompt{Expected: "v0.7.6"}, "expected v0.7.6"},
		{"pending with url only", TagPrompt{WorkflowURL: "https://x/actions"}, "_pending_ ([run Release workflow](https://x/actions))"},
		{"pending with expected + url", TagPrompt{Expected: "v0.7.6", WorkflowURL: "https://x/actions"},
			"expected v0.7.6 ([run Release workflow](https://x/actions))"},
		{"tagged collapses to version", TagPrompt{Tagged: true, Version: "v0.7.6", Expected: "v0.7.6", WorkflowURL: "https://x"}, "v0.7.6"},
		{"tagged but no version", TagPrompt{Tagged: true}, "_tagged_"},
	}
	for _, c := range cases {
		if got := renderTagRef(c.t); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestRenderBumpRef(t *testing.T) {
	cases := []struct {
		name string
		b    Bump
		want string
	}{
		{"no PR yet", Bump{}, "_pending_"},
		{"no-op already at target", Bump{State: "merged"}, "already up to date"},
		{"open with URL", Bump{PR: 42, PRURL: "https://x/pull/42", State: "open"}, "[#42](https://x/pull/42) (open · [checks](https://x/pull/42/checks))"},
		{"empty state defaults to open", Bump{PR: 42, PRURL: "https://x/pull/42"}, "[#42](https://x/pull/42) (open · [checks](https://x/pull/42/checks))"},
		{"ci-failing links to checks", Bump{PR: 88, PRURL: "https://x/pull/88", State: "ci-failing"}, "[#88](https://x/pull/88) ([ci-failing](https://x/pull/88/checks) · [checks](https://x/pull/88/checks))"},
		{"merged terminal", Bump{PR: 15, PRURL: "https://x/pull/15", State: "merged"}, "[#15](https://x/pull/15) (merged)"},
		{"closed terminal", Bump{PR: 16, PRURL: "https://x/pull/16", State: "closed"}, "[#16](https://x/pull/16) (closed)"},
		{"missing URL falls back to plain", Bump{PR: 7, State: "open"}, "#7 (open)"},
	}
	for _, c := range cases {
		if got := renderBumpRef(c.b); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestTitleAndLabelsAndLeafLabel(t *testing.T) {
	if got, want := Title("rancher-chart-webhook", "rancher", "main"),
		"[cascade:rancher-chart-webhook] rancher main"; got != want {
		t.Errorf("Title: got %q want %q", got, want)
	}
	got := Labels("rancher-chart-webhook", "rancher", "release/v2.13")
	want := []string{"cascade-op", "config:rancher-chart-webhook", "leaf:rancher:release/v2.13"}
	if len(got) != len(want) {
		t.Fatalf("Labels len: got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Labels[%d]: got %q want %q", i, got[i], want[i])
		}
	}
	if got, want := ConfigLabel("rancher-chart-webhook"), "config:rancher-chart-webhook"; got != want {
		t.Errorf("ConfigLabel: got %q want %q", got, want)
	}
	if got, want := LeafLabel("rancher", "release/v2.13"), "leaf:rancher:release/v2.13"; got != want {
		t.Errorf("LeafLabel: got %q want %q", got, want)
	}
}

func TestSameExplicitSources(t *testing.T) {
	cases := []struct {
		name string
		a, b []Source
		want bool
	}{
		{"both empty", nil, nil, true},
		{"both empty paired-only on both",
			[]Source{{Name: "steve", Version: "v0.7.5"}},
			[]Source{{Name: "steve", Version: "v0.7.6"}}, // paired-latest can drift
			true},
		{"matching explicit",
			[]Source{{Name: "wrangler", Version: "v0.5.1", Explicit: true}},
			[]Source{{Name: "wrangler", Version: "v0.5.1", Explicit: true}},
			true},
		{"different explicit version",
			[]Source{{Name: "wrangler", Version: "v0.5.1", Explicit: true}},
			[]Source{{Name: "wrangler", Version: "v0.5.2", Explicit: true}},
			false},
		{"different explicit set",
			[]Source{{Name: "wrangler", Version: "v0.5.1", Explicit: true}},
			[]Source{{Name: "lasso", Version: "v1.0.0", Explicit: true}},
			false},
		{"one has explicit, other doesn't",
			[]Source{{Name: "wrangler", Version: "v0.5.1", Explicit: true}},
			[]Source{{Name: "wrangler", Version: "v0.5.1"}}, // implicit
			false},
		{"order doesn't matter",
			[]Source{{Name: "wrangler", Version: "v0.5.1", Explicit: true}, {Name: "lasso", Version: "v1.0.0", Explicit: true}},
			[]Source{{Name: "lasso", Version: "v1.0.0", Explicit: true}, {Name: "wrangler", Version: "v0.5.1", Explicit: true}},
			true},
	}
	for _, c := range cases {
		if got := SameExplicitSources(c.a, c.b); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
