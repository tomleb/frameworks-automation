// Package github wraps the go-github SDK with the focused surface the
// reconciler needs: fetching files (VERSION.md, go.mod), tracker-issue CRUD
// keyed by labels, and bump-PR open/close.
//
// All methods take an `owner/name` string for convenience — split internally.
package github

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v66/github"
	"golang.org/x/oauth2"
)

type Client struct {
	gh *gh.Client
}

func NewClient(ctx context.Context, token string) *Client {
	if token == "" {
		return &Client{gh: gh.NewClient(nil)}
	}
	tc := oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token}))
	return &Client{gh: gh.NewClient(tc)}
}

// FetchFile returns the decoded contents of `path` in `repo` at `ref`. `ref`
// may be a branch name, tag, or SHA; empty means the default branch.
func (c *Client) FetchFile(ctx context.Context, repo, ref, path string) (string, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return "", err
	}
	var opt *gh.RepositoryContentGetOptions
	if ref != "" {
		opt = &gh.RepositoryContentGetOptions{Ref: ref}
	}
	f, _, _, err := c.gh.Repositories.GetContents(ctx, owner, name, path, opt)
	if err != nil {
		return "", fmt.Errorf("get %s/%s@%s: %w", repo, path, ref, err)
	}
	if f == nil {
		return "", fmt.Errorf("get %s/%s@%s: not a file", repo, path, ref)
	}
	s, err := f.GetContent()
	if err != nil {
		return "", fmt.Errorf("decode %s/%s@%s: %w", repo, path, ref, err)
	}
	return s, nil
}

// GetLatestReleaseTag returns the tag string of the latest published release
// (excluding drafts and pre-releases — that's what the GitHub API itself
// considers "latest"). Returns ("", nil) if the repo has no releases yet.
func (c *Client) GetLatestReleaseTag(ctx context.Context, repo string) (string, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return "", err
	}
	rel, resp, err := c.gh.Repositories.GetLatestRelease(ctx, owner, name)
	if err != nil {
		if resp != nil && resp.StatusCode == 404 {
			return "", nil
		}
		return "", fmt.Errorf("latest release %s: %w", repo, err)
	}
	return rel.GetTagName(), nil
}

// ListReleaseTags returns the tag names of every published release in `repo`,
// in the order GitHub returns them (newest first). Used by next-patch
// prediction in the cascade tag prompts. Returns an empty slice when the
// repo has no releases yet.
func (c *Client) ListReleaseTags(ctx context.Context, repo string) ([]string, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	rels, _, err := c.gh.Repositories.ListReleases(ctx, owner, name, &gh.ListOptions{PerPage: 100})
	if err != nil {
		return nil, fmt.Errorf("list releases %s: %w", repo, err)
	}
	out := make([]string, 0, len(rels))
	for _, r := range rels {
		if r.GetDraft() {
			continue
		}
		if t := r.GetTagName(); t != "" {
			out = append(out, t)
		}
	}
	return out, nil
}

type Issue struct {
	Number int
	Title  string
	Body   string
	State  string // "open" | "closed"
	Labels []string
	URL    string // HTML URL, for cross-linking
}

// ListOpenIssues returns OPEN non-PR issues in `repo` matching every label
// in `labels`. Used both to find a specific tracker (caller filters by title)
// and to scan for older trackers of the same dep when superseding.
func (c *Client) ListOpenIssues(ctx context.Context, repo string, labels []string) ([]*Issue, error) {
	return c.listIssues(ctx, repo, labels, "open")
}

// ListIssuesAllStates is like ListOpenIssues but includes closed issues. Used
// by pass 1 cron to detect "have we already processed this version" — open
// trackers alone aren't enough, since pass 2 closes them on completion.
func (c *Client) ListIssuesAllStates(ctx context.Context, repo string, labels []string) ([]*Issue, error) {
	return c.listIssues(ctx, repo, labels, "all")
}

func (c *Client) listIssues(ctx context.Context, repo string, labels []string, state string) ([]*Issue, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	issues, _, err := c.gh.Issues.ListByRepo(ctx, owner, name, &gh.IssueListByRepoOptions{
		Labels:      labels,
		State:       state,
		ListOptions: gh.ListOptions{PerPage: 100},
	})
	if err != nil {
		return nil, fmt.Errorf("list issues %s labels=%v state=%s: %w", repo, labels, state, err)
	}
	out := make([]*Issue, 0, len(issues))
	for _, i := range issues {
		// ListByRepo returns PRs too; filter them out.
		if i.IsPullRequest() {
			continue
		}
		out = append(out, toIssue(i))
	}
	return out, nil
}

func (c *Client) CreateIssue(ctx context.Context, repo, title, body string, labels []string, assignees []string) (*Issue, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	if labels == nil {
		labels = []string{}
	}
	if assignees == nil {
		assignees = []string{}
	}
	issue, _, err := c.gh.Issues.Create(ctx, owner, name, &gh.IssueRequest{
		Title:     &title,
		Body:      &body,
		Labels:    &labels,
		Assignees: &assignees,
	})
	if err != nil {
		return nil, fmt.Errorf("create issue in %s: %w", repo, err)
	}
	return toIssue(issue), nil
}

// AddPRAssignees adds assignees to an existing PR. The GitHub API for PR
// creation does not support assignees; use the Issues API after creation.
func (c *Client) AddPRAssignees(ctx context.Context, repo string, number int, assignees []string) error {
	if len(assignees) == 0 {
		return nil
	}
	owner, name, err := splitRepo(repo)
	if err != nil {
		return err
	}
	if _, _, err := c.gh.Issues.AddAssignees(ctx, owner, name, number, assignees); err != nil {
		return fmt.Errorf("add assignees to %s#%d: %w", repo, number, err)
	}
	return nil
}

func (c *Client) UpdateIssueBody(ctx context.Context, repo string, number int, body string) error {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return err
	}
	_, _, err = c.gh.Issues.Edit(ctx, owner, name, number, &gh.IssueRequest{Body: &body})
	if err != nil {
		return fmt.Errorf("edit issue %s#%d: %w", repo, number, err)
	}
	return nil
}

// CloseIssue closes the issue and posts `comment` first if non-empty (so the
// supersede note appears before the closed marker in the timeline).
func (c *Client) CloseIssue(ctx context.Context, repo string, number int, comment string) error {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return err
	}
	if comment != "" {
		if _, _, err := c.gh.Issues.CreateComment(ctx, owner, name, number, &gh.IssueComment{Body: &comment}); err != nil {
			return fmt.Errorf("comment on %s#%d before close: %w", repo, number, err)
		}
	}
	state := "closed"
	if _, _, err := c.gh.Issues.Edit(ctx, owner, name, number, &gh.IssueRequest{State: &state}); err != nil {
		return fmt.Errorf("close issue %s#%d: %w", repo, number, err)
	}
	return nil
}

type PR struct {
	Number  int
	Title   string
	State   string // "open" | "closed"
	Merged  bool
	HeadRef string
	BaseRef string
	URL     string // HTML URL
}

// IsTerminalPRState reports whether a derived PR-state string is final —
// "merged" or "closed". Used by tracker, cascade, and reconcile passes to
// skip further polling and to decide when a tracker can be closed.
func IsTerminalPRState(state string) bool {
	return state == "merged" || state == "closed"
}

// GetPR fetches a single PR's current state. Used by pass 2 to poll
// open trackers' linked PRs without paging the full list.
func (c *Client) GetPR(ctx context.Context, repo string, number int) (*PR, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	pr, _, err := c.gh.PullRequests.Get(ctx, owner, name, number)
	if err != nil {
		return nil, fmt.Errorf("get PR %s#%d: %w", repo, number, err)
	}
	return toPR(pr), nil
}

// ListOpenPRsByHead returns OPEN PRs in `repo` whose head matches `head`.
// Used to dedupe: if a bump branch already has an open PR, don't open another.
//
// `head` must be qualified as "owner:branch". For same-repo bumps, pass
// "<repo-owner>:<branch>". For cross-repo (fork) bumps, pass
// "<fork-owner>:<branch>" so the GitHub API matches the fork head.
func (c *Client) ListOpenPRsByHead(ctx context.Context, repo, head string) ([]*PR, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	prs, _, err := c.gh.PullRequests.List(ctx, owner, name, &gh.PullRequestListOptions{
		State:       "open",
		Head:        head,
		ListOptions: gh.ListOptions{PerPage: 50},
	})
	if err != nil {
		return nil, fmt.Errorf("list PRs %s head=%s: %w", repo, head, err)
	}
	out := make([]*PR, 0, len(prs))
	for _, p := range prs {
		out = append(out, toPR(p))
	}
	return out, nil
}

func (c *Client) CreatePR(ctx context.Context, repo, title, body, head, base string) (*PR, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	pr, _, err := c.gh.PullRequests.Create(ctx, owner, name, &gh.NewPullRequest{
		Title: &title,
		Body:  &body,
		Head:  &head,
		Base:  &base,
	})
	if err != nil {
		return nil, fmt.Errorf("create PR %s %s -> %s: %w", repo, head, base, err)
	}
	return toPR(pr), nil
}

// CommitsAheadOf returns how many commits `head` is ahead of `base` in repo.
// Returns 0 when identical or when base is ahead of head. Used to detect
// unreleased work on an intermediate branch before claiming an existing tag.
func (c *Client) CommitsAheadOf(ctx context.Context, repo, base, head string) (int, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return 0, err
	}
	cmp, _, err := c.gh.Repositories.CompareCommits(ctx, owner, name, base, head, nil)
	if err != nil {
		return 0, fmt.Errorf("compare %s %s...%s: %w", repo, base, head, err)
	}
	return cmp.GetAheadBy(), nil
}

// DeleteBranch deletes a branch in repo. Best-effort: callers log and continue
// on failure so a stuck branch never blocks tracker closure.
func (c *Client) DeleteBranch(ctx context.Context, repo, branch string) error {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return err
	}
	if _, err := c.gh.Git.DeleteRef(ctx, owner, name, "refs/heads/"+branch); err != nil {
		return fmt.Errorf("delete branch %s in %s: %w", branch, repo, err)
	}
	return nil
}

// ClosePR closes the PR (does not merge). Posts `comment` first if provided
// (used for supersede notes).
func (c *Client) ClosePR(ctx context.Context, repo string, number int, comment string) error {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return err
	}
	if comment != "" {
		if _, _, err := c.gh.Issues.CreateComment(ctx, owner, name, number, &gh.IssueComment{Body: &comment}); err != nil {
			return fmt.Errorf("comment on %s#%d before close: %w", repo, number, err)
		}
	}
	state := "closed"
	if _, _, err := c.gh.PullRequests.Edit(ctx, owner, name, number, &gh.PullRequest{State: &state}); err != nil {
		return fmt.Errorf("close PR %s#%d: %w", repo, number, err)
	}
	return nil
}

// GetGoModPaths returns every go.mod path in repo's default branch tree,
// excluding files under any vendor/ directory. Used by config.DiscoverModules
// to populate the module-path index without a local clone.
func (c *Client) GetGoModPaths(ctx context.Context, repo string) ([]string, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return nil, err
	}
	tree, _, err := c.gh.Git.GetTree(ctx, owner, name, "HEAD", true)
	if err != nil {
		return nil, fmt.Errorf("get tree %s: %w", repo, err)
	}
	var out []string
	for _, e := range tree.Entries {
		path := e.GetPath()
		if e.GetType() != "blob" {
			continue
		}
		if path != "go.mod" && !strings.HasSuffix(path, "/go.mod") {
			continue
		}
		// Skip vendor directories.
		if strings.Contains(path, "/vendor/") || strings.HasPrefix(path, "vendor/") {
			continue
		}
		out = append(out, path)
	}
	return out, nil
}

// --- helpers ----------------------------------------------------------------

func splitRepo(repo string) (owner, name string, err error) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid repo %q: want owner/name", repo)
	}
	return parts[0], parts[1], nil
}

func toIssue(i *gh.Issue) *Issue {
	out := &Issue{
		Number: i.GetNumber(),
		Title:  i.GetTitle(),
		Body:   i.GetBody(),
		State:  i.GetState(),
		URL:    i.GetHTMLURL(),
	}
	for _, l := range i.Labels {
		out.Labels = append(out.Labels, l.GetName())
	}
	return out
}

func toPR(p *gh.PullRequest) *PR {
	return &PR{
		Number:  p.GetNumber(),
		Title:   p.GetTitle(),
		State:   p.GetState(),
		Merged:  p.GetMerged(),
		HeadRef: p.GetHead().GetRef(),
		BaseRef: p.GetBase().GetRef(),
		URL:     p.GetHTMLURL(),
	}
}
