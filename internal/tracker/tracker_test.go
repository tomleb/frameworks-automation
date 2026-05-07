package tracker

import (
	"strings"
	"testing"
	"time"
)

func TestRender_BodyContainsTargetsAndState(t *testing.T) {
	op := Op{
		Dep:     "steve",
		Version: "v0.7.5",
		Targets: []Target{
			{Repo: "rancher", Branch: "release/v2.13", PR: 1234, PRURL: "https://github.com/rancher/rancher/pull/1234", State: "open"},
			{Repo: "rancher", Branch: "main"},
		},
	}
	body, err := Render(op, time.Date(2026, 4, 21, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(body, "[bump]") || strings.Contains(body, "[bump]") && !strings.Contains(body, "steve v0.7.5") {
		// Title isn't in the body — only check trigger line.
		if !strings.Contains(body, "steve v0.7.5 released") {
			t.Errorf("body missing trigger: %s", body)
		}
	}
	if !strings.Contains(body, "[#1234](") || !strings.Contains(body, "(open · [checks](") {
		t.Errorf("body missing linked PR ref: %s", body)
	}
	if !strings.Contains(body, "_pending_") {
		t.Errorf("body missing pending placeholder: %s", body)
	}
	if !strings.Contains(body, "Last reconciled: 2026-04-21T10:00:00Z") {
		t.Errorf("body missing reconciled timestamp: %s", body)
	}
	st, err := Envelope.Extract(body)
	if err != nil {
		t.Fatalf("extract from rendered: %v", err)
	}
	if len(st.Targets) != 2 {
		t.Errorf("expected 2 targets in state, got %d", len(st.Targets))
	}
}

func TestMergeState_PreservesOpTargetsAndOverlaysPR(t *testing.T) {
	op := Op{
		Dep:     "steve",
		Version: "v0.7.5",
		Targets: []Target{
			{Repo: "rancher", Branch: "main"},                  // newly added
			{Repo: "rancher", Branch: "release/v2.13"},         // existing
		},
	}
	stored := Persistent{
		Targets: []Target{
			{Repo: "rancher", Branch: "release/v2.13", PR: 1235, State: "approved"},
		},
	}
	mergeState(&op, stored)
	if op.Targets[0].PR != 0 {
		t.Errorf("new target should have no PR, got %+v", op.Targets[0])
	}
	if op.Targets[1].PR != 1235 || op.Targets[1].State != "approved" {
		t.Errorf("existing target not merged: %+v", op.Targets[1])
	}
}

// TestMergeState_AppendsStoredOnlyTargets covers the manual-bump path: the
// caller's Op has only the manual target, but the tracker already knows about
// auto-bumped targets from an earlier dispatch. Those must survive.
func TestMergeState_AppendsStoredOnlyTargets(t *testing.T) {
	op := Op{
		Dep:     "wrangler",
		Version: "v0.5.1",
		Targets: []Target{
			// Caller (RunBumpDep) only knows about the one branch the user
			// asked for.
			{Repo: "rancher", Branch: "release/v2.13"},
		},
	}
	stored := Persistent{
		Targets: []Target{
			{Repo: "rancher", Branch: "main", PR: 10, PRURL: "https://github.com/x/y/pull/10", State: "open"},
			{Repo: "steve", Branch: "main", PR: 11, PRURL: "https://github.com/x/y/pull/11", State: "merged"},
		},
	}
	mergeState(&op, stored)
	if len(op.Targets) != 3 {
		t.Fatalf("want 3 targets after union merge, got %d: %+v", len(op.Targets), op.Targets)
	}
	// Caller-supplied target stays first, no PR on it yet.
	if op.Targets[0].Repo != "rancher" || op.Targets[0].Branch != "release/v2.13" || op.Targets[0].PR != 0 {
		t.Errorf("caller target not preserved: %+v", op.Targets[0])
	}
	// Stored-only targets appended.
	got := map[string]Target{
		op.Targets[1].Repo + "|" + op.Targets[1].Branch: op.Targets[1],
		op.Targets[2].Repo + "|" + op.Targets[2].Branch: op.Targets[2],
	}
	if got["rancher|main"].PR != 10 || got["steve|main"].State != "merged" {
		t.Errorf("appended targets wrong: %+v", op.Targets[1:])
	}
}

func TestRenderRef(t *testing.T) {
	cases := []struct {
		name string
		t    Target
		want string
	}{
		{"no PR yet", Target{}, "_pending_"},
		{"open with URL", Target{PR: 42, PRURL: "https://github.com/rancher/steve/pull/42", State: "open"},
			"[#42](https://github.com/rancher/steve/pull/42) (open · [checks](https://github.com/rancher/steve/pull/42/checks))"},
		{"empty state defaults to open", Target{PR: 42, PRURL: "https://x/pull/42"},
			"[#42](https://x/pull/42) (open · [checks](https://x/pull/42/checks))"},
		{"ci-failing links to checks tab", Target{PR: 88, PRURL: "https://github.com/rancher/apiserver/pull/88", State: "ci-failing"},
			"[#88](https://github.com/rancher/apiserver/pull/88) ([ci-failing](https://github.com/rancher/apiserver/pull/88/checks) · [checks](https://github.com/rancher/apiserver/pull/88/checks))"},
		{"merged terminal", Target{PR: 15, PRURL: "https://x/pull/15", State: "merged"},
			"[#15](https://x/pull/15) (merged)"},
		{"closed terminal", Target{PR: 16, PRURL: "https://x/pull/16", State: "closed"},
			"[#16](https://x/pull/16) (closed)"},
		{"missing URL falls back to plain text", Target{PR: 7, State: "open"},
			"#7 (open)"},
	}
	for _, c := range cases {
		if got := renderRef(c.t); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestParseVersionFromTitle(t *testing.T) {
	cases := []struct {
		title, dep, want string
	}{
		{"[bump:rancher-chart-webhook] wrangler v0.5.1 → rancher main", "wrangler", "v0.5.1"},
		{"[bump:full] steve v0.7.5-rc1 → rancher release/v2.13", "steve", "v0.7.5-rc1"},
		{"[bump:full] wrangler v0.5.1 → rancher main", "steve", ""},                                // wrong dep
		{"[bump:full] wrangler v0.5.1", "wrangler", ""},                                            // missing arrow
		{"random title", "wrangler", ""},                                                           // wrong shape
		{"[bump:full] wranglerv0.5.1 → rancher main", "wrangler", ""},                              // missing space
		{"[bump] wrangler v0.5.1 → rancher main", "wrangler", ""},                                  // legacy single-config format no longer parses
		{Title("rancher", "apiserver", "v0.10.0", "rancher", "release/v2.13"), "apiserver", "v0.10.0"}, // round-trip
	}
	for _, c := range cases {
		if got := ParseVersionFromTitle(c.title, c.dep); got != c.want {
			t.Errorf("ParseVersionFromTitle(%q, %q) = %q, want %q", c.title, c.dep, got, c.want)
		}
	}
}

func TestLabelsContainsConfigLeafAndDep(t *testing.T) {
	got := Labels("rancher-chart-webhook", "wrangler", "rancher", "release/v2.13")
	want := map[string]bool{
		"bump-op":                            true,
		"config:rancher-chart-webhook":       true,
		"dep:wrangler":                       true,
		"leaf:rancher:release/v2.13":         true,
	}
	for _, l := range got {
		if strings.HasPrefix(l, "version:") {
			t.Errorf("Labels should not include a version: label, got %v", got)
		}
		delete(want, l)
	}
	if len(want) != 0 {
		t.Errorf("Labels missing entries: %v (got %v)", want, got)
	}
}

func TestTitleAndLeafLabel(t *testing.T) {
	if got, want := Title("rancher-chart-webhook", "wrangler", "v0.5.1", "rancher", "main"),
		"[bump:rancher-chart-webhook] wrangler v0.5.1 → rancher main"; got != want {
		t.Errorf("Title: got %q want %q", got, want)
	}
	if got, want := ConfigLabel("rancher-chart-webhook"), "config:rancher-chart-webhook"; got != want {
		t.Errorf("ConfigLabel: got %q want %q", got, want)
	}
	if got, want := LeafLabel("rancher", "release/v2.13"), "leaf:rancher:release/v2.13"; got != want {
		t.Errorf("LeafLabel: got %q want %q", got, want)
	}
}

func TestRepoFromPRURL(t *testing.T) {
	got, err := repoFromPRURL("https://github.com/rancher/rancher/pull/1234")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "rancher/rancher" {
		t.Errorf("got %q want rancher/rancher", got)
	}
	if _, err := repoFromPRURL("not a url"); err == nil {
		t.Error("expected err for non-url")
	}
}
