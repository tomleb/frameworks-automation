package reconcile

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/rancher/release-automation/internal/cascade"
	"github.com/rancher/release-automation/internal/config"
	"github.com/rancher/release-automation/internal/pr"
)

// TestRunCascade_Fixtures walks every subdirectory under
// testdata/runcascade/, drives RunCascade against an in-memory fake GitHub
// client and a recording fake bumper, and compares the resulting cascade
// plan (extracted from the persisted issue body) plus the captured
// side-effects against the case's expected.yaml.
//
// Add a new scenario by creating a new directory with three files:
// dependencies.yaml (config), state.yaml (initial fake state + trigger),
// expected.yaml (expected plan + side-effects). No new Go required.
func TestRunCascade_Fixtures(t *testing.T) {
	root := "testdata/runcascade"
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			runFixture(t, filepath.Join(root, e.Name()))
		})
	}
}

func runFixture(t *testing.T, dir string) {
	t.Helper()
	cfg := loadConfigFixture(t, filepath.Join(dir, "dependencies.yaml"))
	state := loadStateFixture(t, filepath.Join(dir, "state.yaml"))
	expected := loadExpectedFixture(t, filepath.Join(dir, "expected.yaml"))

	for repo, mods := range state.Modules {
		if cfg.Modules == nil {
			cfg.Modules = map[string][]string{}
		}
		cfg.Modules[repo] = mods
	}

	gh := newFakeGH(state.repoStates())
	bumper := newFakeBumper(gh)

	settings := Settings{
		AutomationRepo: state.AutomationRepo,
		GitHubToken:    "fake-token",
		GitHubActor:    state.GitHubActor,
	}
	r := newWithDeps(testConfigName, cfg, settings, gh, bumper)

	if err := r.RunCascade(context.Background(), state.Trigger.LeafBranch, state.Trigger.Independents); err != nil {
		t.Fatalf("RunCascade: %v", err)
	}

	issues := gh.snapshotIssues()
	if len(issues) != 1 {
		t.Fatalf("want exactly 1 tracker issue, got %d: %+v", len(issues), issues)
	}
	tracker := issues[0]

	persisted, err := cascade.Envelope.Extract(tracker.Body)
	if err != nil {
		t.Fatalf("extract cascade state: %v", err)
	}

	assertSources(t, expected.Sources, persisted.Sources)
	assertStages(t, expected.Stages, persisted.Stages)
	assertIssue(t, expected.Issue, tracker)
	assertPRsOpened(t, expected.PRsOpened, bumper.snapshotCalls())
}

// --- fixture types ----------------------------------------------------------

type stateFixture struct {
	AutomationRepo string                       `yaml:"automation_repo"`
	GitHubActor    string                       `yaml:"github_actor"`
	Trigger        triggerFixture               `yaml:"trigger"`
	Modules        map[string][]string          `yaml:"modules"`
	Repos          map[string]repoStateFixture  `yaml:"repos"`
}

type triggerFixture struct {
	LeafBranch   string            `yaml:"leaf_branch"`
	Independents map[string]string `yaml:"independents"`
}

type repoStateFixture struct {
	GHRepo        string                          `yaml:"gh_repo"`
	DefaultBranch string                          `yaml:"default_branch"`
	Tags          []string                        `yaml:"tags"`
	Branches      map[string]branchStateFixture   `yaml:"branches"`
	TagFiles      map[string]map[string]string    `yaml:"tag_files"`
}

type branchStateFixture struct {
	Files   map[string]string `yaml:"files"`
	AheadOf map[string]int    `yaml:"ahead_of"`
}

type expectedFixture struct {
	Sources   []expectedSource `yaml:"sources"`
	Stages    []expectedStage  `yaml:"stages"`
	Issue     expectedIssue    `yaml:"issue"`
	PRsOpened []expectedPR     `yaml:"prs_opened"`
}

type expectedSource struct {
	Name     string `yaml:"name"`
	Version  string `yaml:"version"`
	Explicit bool   `yaml:"explicit"`
}

type expectedStage struct {
	Layer int            `yaml:"layer"`
	Bumps []expectedBump `yaml:"bumps"`
	Tags  []expectedTag  `yaml:"tags"`
}

type expectedBump struct {
	Repo   string        `yaml:"repo"`
	Branch string        `yaml:"branch"`
	Deps   []expectedDep `yaml:"deps"`
}

type expectedDep struct {
	Dep      string          `yaml:"dep"`
	Module   string          `yaml:"module"`
	Version  string          `yaml:"version"`
	Strategy config.Strategy `yaml:"strategy"`
}

type expectedTag struct {
	Repo     string `yaml:"repo"`
	Branch   string `yaml:"branch"`
	Expected string `yaml:"expected,omitempty"`
}

type expectedIssue struct {
	Title  string   `yaml:"title"`
	Labels []string `yaml:"labels"`
}

type expectedPR struct {
	Repo    string             `yaml:"repo"`
	Base    string             `yaml:"base"`
	Modules []expectedPRModule `yaml:"modules"`
}

type expectedPRModule struct {
	Path    string `yaml:"path"`
	Version string `yaml:"version"`
}

func (s stateFixture) repoStates() map[string]*fakeRepoState {
	out := map[string]*fakeRepoState{}
	for _, repo := range s.Repos {
		state := &fakeRepoState{
			DefaultBranch: repo.DefaultBranch,
			Tags:          append([]string(nil), repo.Tags...),
			Branches:      map[string]fakeBranchState{},
			TagFiles:      map[string]map[string]string{},
		}
		if state.DefaultBranch == "" {
			for name := range repo.Branches {
				state.DefaultBranch = name
				break
			}
		}
		for name, br := range repo.Branches {
			state.Branches[name] = fakeBranchState{
				Files:   copyStrMap(br.Files),
				AheadOf: copyIntMap(br.AheadOf),
			}
		}
		for tag, files := range repo.TagFiles {
			state.TagFiles[tag] = copyStrMap(files)
		}
		out[repo.GHRepo] = state
	}
	return out
}

func copyStrMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyIntMap(in map[string]int) map[string]int {
	if in == nil {
		return nil
	}
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// --- loaders ----------------------------------------------------------------

func loadConfigFixture(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return cfg
}

func loadStateFixture(t *testing.T, path string) stateFixture {
	t.Helper()
	var f stateFixture
	mustReadYAML(t, path, &f)
	return f
}

func loadExpectedFixture(t *testing.T, path string) expectedFixture {
	t.Helper()
	var f expectedFixture
	mustReadYAML(t, path, &f)
	return f
}

func mustReadYAML(t *testing.T, path string, into any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := yaml.Unmarshal(b, into); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

// --- assertions -------------------------------------------------------------

func assertSources(t *testing.T, want []expectedSource, got []cascade.Source) {
	t.Helper()
	var gotNorm []expectedSource
	if len(got) > 0 {
		gotNorm = make([]expectedSource, len(got))
		for i, s := range got {
			gotNorm[i] = expectedSource{Name: s.Name, Version: s.Version, Explicit: s.Explicit}
		}
		sort.Slice(gotNorm, func(i, j int) bool { return gotNorm[i].Name < gotNorm[j].Name })
	}
	var wantNorm []expectedSource
	if len(want) > 0 {
		wantNorm = make([]expectedSource, len(want))
		copy(wantNorm, want)
		sort.Slice(wantNorm, func(i, j int) bool { return wantNorm[i].Name < wantNorm[j].Name })
	}
	if !reflect.DeepEqual(wantNorm, gotNorm) {
		t.Errorf("sources mismatch:\n  got:  %+v\n  want: %+v", gotNorm, wantNorm)
	}
}

func assertStages(t *testing.T, want []expectedStage, got []cascade.Stage) {
	t.Helper()
	gotNorm := normalizeStages(got)
	wantNorm := normalizeExpectedStages(want)
	// `expected` (next-tag hint) is opt-in: a fixture that omits it
	// shouldn't fail when fillTagPromptHints computes one. Clear got's
	// Expected for any tag whose want-pair didn't set it.
	for i := range gotNorm {
		if i >= len(wantNorm) {
			break
		}
		for j := range gotNorm[i].Tags {
			if j >= len(wantNorm[i].Tags) {
				break
			}
			if wantNorm[i].Tags[j].Expected == "" {
				gotNorm[i].Tags[j].Expected = ""
			}
		}
	}
	if !reflect.DeepEqual(wantNorm, gotNorm) {
		t.Errorf("stages mismatch:\n  got:  %s\n  want: %s",
			yamlDump(t, gotNorm), yamlDump(t, wantNorm))
	}
}

func assertIssue(t *testing.T, want expectedIssue, got ghclientIssue) {
	t.Helper()
	if want.Title != "" && got.Title != want.Title {
		t.Errorf("issue title: got %q want %q", got.Title, want.Title)
	}
	if len(want.Labels) > 0 {
		gotLabels := append([]string(nil), got.Labels...)
		sort.Strings(gotLabels)
		wantLabels := append([]string(nil), want.Labels...)
		sort.Strings(wantLabels)
		if !reflect.DeepEqual(gotLabels, wantLabels) {
			t.Errorf("issue labels:\n  got:  %v\n  want: %v", gotLabels, wantLabels)
		}
	}
}

func assertPRsOpened(t *testing.T, want []expectedPR, got []pr.Request) {
	t.Helper()
	var gotNorm []expectedPR
	if len(got) > 0 {
		gotNorm = make([]expectedPR, len(got))
		for i, c := range got {
			var mods []expectedPRModule
			if len(c.Modules) > 0 {
				mods = make([]expectedPRModule, len(c.Modules))
				for j, m := range c.Modules {
					mods[j] = expectedPRModule{Path: m.Path, Version: m.Version}
				}
				sort.Slice(mods, func(a, b int) bool { return mods[a].Path < mods[b].Path })
			}
			gotNorm[i] = expectedPR{Repo: c.Repo, Base: c.BaseBranch, Modules: mods}
		}
		sort.Slice(gotNorm, func(i, j int) bool {
			if gotNorm[i].Repo != gotNorm[j].Repo {
				return gotNorm[i].Repo < gotNorm[j].Repo
			}
			return gotNorm[i].Base < gotNorm[j].Base
		})
	}
	var wantNorm []expectedPR
	if len(want) > 0 {
		wantNorm = make([]expectedPR, len(want))
		for i, p := range want {
			var mods []expectedPRModule
			if len(p.Modules) > 0 {
				mods = make([]expectedPRModule, len(p.Modules))
				copy(mods, p.Modules)
				sort.Slice(mods, func(a, b int) bool { return mods[a].Path < mods[b].Path })
			}
			wantNorm[i] = expectedPR{Repo: p.Repo, Base: p.Base, Modules: mods}
		}
		sort.Slice(wantNorm, func(i, j int) bool {
			if wantNorm[i].Repo != wantNorm[j].Repo {
				return wantNorm[i].Repo < wantNorm[j].Repo
			}
			return wantNorm[i].Base < wantNorm[j].Base
		})
	}
	if !reflect.DeepEqual(wantNorm, gotNorm) {
		t.Errorf("PRs opened mismatch:\n  got:  %s\n  want: %s",
			yamlDump(t, gotNorm), yamlDump(t, wantNorm))
	}
}

// ghclientIssue is a tiny alias to keep the assertion signature short.
type ghclientIssue = struct {
	Number int
	Title  string
	Body   string
	State  string
	Labels []string
	URL    string
}

func normalizeStages(stages []cascade.Stage) []expectedStage {
	out := make([]expectedStage, len(stages))
	for i, st := range stages {
		var bumps []expectedBump
		if len(st.Bumps) > 0 {
			bumps = make([]expectedBump, len(st.Bumps))
			for j, bp := range st.Bumps {
				var deps []expectedDep
				if len(bp.Deps) > 0 {
					deps = make([]expectedDep, len(bp.Deps))
					for k, d := range bp.Deps {
						deps[k] = expectedDep{Dep: d.Dep, Module: d.Module, Version: d.Version, Strategy: d.Strategy}
					}
					sort.Slice(deps, func(a, b int) bool { return deps[a].Dep < deps[b].Dep })
				}
				bumps[j] = expectedBump{Repo: bp.Repo, Branch: bp.Branch, Deps: deps}
			}
			sort.Slice(bumps, func(a, b int) bool {
				if bumps[a].Repo != bumps[b].Repo {
					return bumps[a].Repo < bumps[b].Repo
				}
				return bumps[a].Branch < bumps[b].Branch
			})
		}
		var tags []expectedTag
		if len(st.Tags) > 0 {
			tags = make([]expectedTag, len(st.Tags))
			for j, tg := range st.Tags {
				tags[j] = expectedTag{Repo: tg.Repo, Branch: tg.Branch, Expected: tg.Expected}
			}
			sort.Slice(tags, func(a, b int) bool {
				if tags[a].Repo != tags[b].Repo {
					return tags[a].Repo < tags[b].Repo
				}
				return tags[a].Branch < tags[b].Branch
			})
		}
		out[i] = expectedStage{Layer: st.Layer, Bumps: bumps, Tags: tags}
	}
	return out
}

func normalizeExpectedStages(stages []expectedStage) []expectedStage {
	out := make([]expectedStage, len(stages))
	for i, st := range stages {
		var bumps []expectedBump
		if len(st.Bumps) > 0 {
			bumps = make([]expectedBump, len(st.Bumps))
			for j, bp := range st.Bumps {
				var deps []expectedDep
				if len(bp.Deps) > 0 {
					deps = make([]expectedDep, len(bp.Deps))
					for k, d := range bp.Deps {
						deps[k] = d
						if deps[k].Strategy == "" {
							deps[k].Strategy = config.StrategyGoGet
						}
					}
					sort.Slice(deps, func(a, b int) bool { return deps[a].Dep < deps[b].Dep })
				}
				bumps[j] = expectedBump{Repo: bp.Repo, Branch: bp.Branch, Deps: deps}
			}
			sort.Slice(bumps, func(a, b int) bool {
				if bumps[a].Repo != bumps[b].Repo {
					return bumps[a].Repo < bumps[b].Repo
				}
				return bumps[a].Branch < bumps[b].Branch
			})
		}
		var tags []expectedTag
		if len(st.Tags) > 0 {
			tags = make([]expectedTag, len(st.Tags))
			for j, tg := range st.Tags {
				tags[j] = tg
			}
			sort.Slice(tags, func(a, b int) bool {
				if tags[a].Repo != tags[b].Repo {
					return tags[a].Repo < tags[b].Repo
				}
				return tags[a].Branch < tags[b].Branch
			})
		}
		out[i] = expectedStage{Layer: st.Layer, Bumps: bumps, Tags: tags}
	}
	return out
}

func yamlDump(t *testing.T, v any) string {
	t.Helper()
	b, err := yaml.Marshal(v)
	if err != nil {
		t.Fatalf("yaml dump: %v", err)
	}
	return "\n" + string(b)
}
