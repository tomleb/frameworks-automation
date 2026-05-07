// Package tracker owns the lifecycle of bump-op tracker issues. The issue
// body doubles as the state store: a fenced metadata block holds the
// per-target PR number and last-known PR state.
//
// Tracker identity is (config, dep, version, leaf-branch) — one tracker per
// bump landing on a specific leaf branch within one config. Lookup is by
// labels: `bump-op`, `config:<name>`, `dep:<name>`, `leaf:<leaf-repo>:<leaf-branch>`.
// The config dimension keeps each `dependencies/<name>.yaml` file's bump-ops
// fully isolated from every other config's.
package tracker

import (
	"fmt"
	"strings"
	"time"

	ghclient "github.com/rancher/release-automation/internal/github"
	"github.com/rancher/release-automation/internal/issuestate"
)

const (
	LabelOp        = "bump-op"
	LabelConfigFmt = "config:%s"     // e.g. config:rancher-chart-webhook
	LabelDepFmt    = "dep:%s"        // e.g. dep:steve
	LabelLeafFmt   = "leaf:%s:%s"    // e.g. leaf:rancher:main, leaf:rancher:release/v2.13
)

// Envelope is the issue-state envelope for bump-op trackers — the marker
// pair that wraps the YAML metadata block in the issue body. Use
// Envelope.Extract / Envelope.Embed to round-trip Persistent.
var Envelope = issuestate.Marker[Persistent]{
	Open:  "<!-- bump-op-state v1",
	Close: "-->",
}

// titlePrefixFmt matches Title(): "[bump:{config}] {dep} {version} → {leafRepo} {leafBranch}".
// Stable — ParseVersionFromTitle relies on the prefix and the " → " separator.
const (
	titlePrefixFmt = "[bump:%s] %s "
	titleArrow     = " → "
)

// Op is a single bump operation: a (dep, version) landing on one leaf branch
// (e.g. rancher main, rancher release/v2.13). Targets are the per-downstream
// branches that ship against that leaf branch — ordered for stable rendering
// (keep them sorted by Repo,Branch).
type Op struct {
	Dep        string
	Version    string
	LeafRepo   string // e.g. "rancher" — the leaf this bump targets
	LeafBranch string // e.g. "main", "release/v2.13"
	Targets    []Target
}

// Target is one downstream branch. PR is 0 until a bump PR is opened.
// State is the last PR-state we observed (used by pass 2 to diff & post).
// Values: "" (none yet) | "open" | "ci-failing" | "approved" | "merged" | "closed".
type Target struct {
	Repo   string `yaml:"repo"`
	Branch string `yaml:"branch"`
	PR     int    `yaml:"pr,omitempty"`
	PRURL  string `yaml:"pr_url,omitempty"`
	State  string `yaml:"state,omitempty"`
}

// Persistent is what survives between reconciler runs. Lives in the metadata
// block. Keep YAML tags stable: older runs must read newer files (additive
// only, never rename or remove a field).
type Persistent struct {
	Targets      []Target `yaml:"targets"`
	SupersededBy *int     `yaml:"superseded_by,omitempty"` // tracker issue number
}

// Title is the canonical issue title for an op. The version is parsed back
// out of the title (see ParseVersionFromTitle) — keep this format stable.
//
// Format: "[bump:{config}] {dep} {version} → {leafRepo} {leafBranch}".
// Encoding the leaf in the title makes trackers immediately distinguishable
// in lists (you can tell wrangler v0.5.1→main from wrangler v0.5.1→release/v2.13
// at a glance), encoding the config disambiguates the same (dep, version,
// leaf) coexisting under multiple specialized configs, and the " → " separator
// is what ParseVersionFromTitle splits on.
func Title(config, dep, version, leafRepo, leafBranch string) string {
	return fmt.Sprintf(titlePrefixFmt+"%s"+titleArrow+"%s %s", config, dep, version, leafRepo, leafBranch)
}

// Labels returns the canonical label set for a tracker. The version is NOT a
// label (it would proliferate one new label per release); it lives in the
// title and is parsed by ParseVersionFromTitle. The leaf label tags each
// tracker with its (leafRepo, leafBranch) for human discovery in the GitHub
// issue list — proliferation is bounded by leaf branches (a handful). The
// config label scopes every label-query so config A's reconciler never sees
// config B's trackers.
func Labels(config, dep, leafRepo, leafBranch string) []string {
	return []string{
		LabelOp,
		fmt.Sprintf(LabelConfigFmt, config),
		fmt.Sprintf(LabelDepFmt, dep),
		fmt.Sprintf(LabelLeafFmt, leafRepo, leafBranch),
	}
}

// ConfigLabel returns the single config-axis label for `config`. Used by
// pass2 / passcascade to scope the broad label query (just LabelOp +
// ConfigLabel) without needing a specific dep / leaf-branch.
func ConfigLabel(config string) string {
	return fmt.Sprintf(LabelConfigFmt, config)
}

// LeafLabel returns the single leaf-axis label for a (leafRepo, leafBranch).
func LeafLabel(leafRepo, leafBranch string) string {
	return fmt.Sprintf(LabelLeafFmt, leafRepo, leafBranch)
}

// ParseVersionFromTitle returns the version embedded in `title` for `dep`,
// or "" if `title` doesn't match the canonical format. Used during
// FindOrCreate (filter candidates returned by the dep-label query) and
// Supersede (compare versions across open trackers for the same dep+leaf).
//
// The config segment in the prefix is treated as a wildcard here — callers
// already scope by the config: label, so the title parser only needs to
// confirm "looks like one of our titles for this dep" and pull the version.
func ParseVersionFromTitle(title, dep string) string {
	const open = "[bump:"
	if !strings.HasPrefix(title, open) {
		return ""
	}
	close := strings.Index(title, "] ")
	if close < 0 {
		return ""
	}
	depPrefix := dep + " "
	rest := title[close+len("] "):]
	if !strings.HasPrefix(rest, depPrefix) {
		return ""
	}
	rest = rest[len(depPrefix):]
	i := strings.Index(rest, titleArrow)
	if i < 0 {
		return ""
	}
	return rest[:i]
}

// Render produces the issue body markdown for `op`, with the metadata block
// already embedded. Idempotent: rendering the same Op twice (same time aside)
// produces the same body.
func Render(op Op, now time.Time) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "## Trigger\n%s %s released\n\n", op.Dep, op.Version)

	b.WriteString("## Targets\n")
	if len(op.Targets) == 0 {
		b.WriteString("_no targets — nothing to bump_\n")
	}
	for _, t := range op.Targets {
		check := " "
		if t.State == "merged" {
			check = "x"
		}
		fmt.Fprintf(&b, "- [%s] `%s` `%s` — %s\n", check, t.Repo, t.Branch, renderRef(t))
	}

	fmt.Fprintf(&b, "\n## Status\nLast reconciled: %s\n\n", now.UTC().Format(time.RFC3339))

	body, err := Envelope.Embed(b.String(), Persistent{Targets: op.Targets})
	if err != nil {
		return "", err
	}
	return body, nil
}

func displayState(s string) string {
	if s == "" {
		return "open"
	}
	return s
}

// renderRef builds the per-target trailing markdown: PR autolink + a state
// label that's itself a link when there's a useful destination. Non-terminal
// targets also get a trailing checks link so debugging an in-flight bump is
// one click away. Examples:
//
//	_pending_                                                       (no PR yet)
//	[#42](url) (open · [checks](url/checks))                        (PR open)
//	[#88](url) ([ci-failing](url/checks) · [checks](url/checks))    (failing)
//	[#15](url) (merged)                                             (terminal)
//
// /pull/N/checks works for both GHA and third-party check-runs, so it's a
// useful target without requiring an extra API call.
func renderRef(t Target) string {
	if t.PR == 0 {
		return "_pending_"
	}
	state := displayState(t.State)
	if t.State == "ci-failing" && t.PRURL != "" {
		state = fmt.Sprintf("[%s](%s/checks)", state, t.PRURL)
	}
	if t.PRURL != "" && !ghclient.IsTerminalPRState(t.State) {
		state = fmt.Sprintf("%s · [checks](%s/checks)", state, t.PRURL)
	}
	if t.PRURL == "" {
		return fmt.Sprintf("#%d (%s)", t.PR, state)
	}
	return fmt.Sprintf("[#%d](%s) (%s)", t.PR, t.PRURL, state)
}

