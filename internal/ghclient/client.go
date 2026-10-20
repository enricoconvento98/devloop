// Package ghclient is a thin wrapper around the GitHub REST API.
// The agent only ever gets the read methods (see internal/agent); writes are
// done by the CLI itself, after the run, and only when --post is passed.
package ghclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/google/go-github/v90/github"
)

const (
	maxPatchCharsPerFile = 8_000
	maxPatchCharsTotal   = 120_000
)

type Repo struct{ Owner, Name string }

func (r Repo) String() string { return r.Owner + "/" + r.Name }

var repoPattern = regexp.MustCompile(`^([\w.-]+)/([\w.-]+)$`)

func ParseRepo(s string) (Repo, error) {
	m := repoPattern.FindStringSubmatch(s)
	if m == nil {
		return Repo{}, fmt.Errorf("expected owner/repo, got %q", s)
	}
	return Repo{m[1], m[2]}, nil
}

// API is what the agent tools and commands need from GitHub; tests can fake it.
type API interface {
	ListOpenPRs(ctx context.Context, limit int) ([]PRSummary, error)
	GetPR(ctx context.Context, number int) (*PRDetail, error)
	GetChecks(ctx context.Context, sha string) (*Checks, error)
	GetReviews(ctx context.Context, number int) (*Reviews, error)
	GetFile(ctx context.Context, path, ref string) (string, error)
	PostReview(ctx context.Context, number int, commitSHA, body string, comments []InlineComment) (string, error)
}

type Client struct {
	gh   *github.Client
	repo Repo
}

func New(repo Repo) (*Client, error) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		token = os.Getenv("GH_TOKEN")
	}
	if token == "" {
		return nil, errors.New("set GITHUB_TOKEN (or GH_TOKEN) to a GitHub token")
	}
	gh, err := github.NewClient(github.WithAuthToken(token))
	if err != nil {
		return nil, err
	}
	return &Client{gh: gh, repo: repo}, nil
}

type PRSummary struct {
	Number             int       `json:"number"`
	Title              string    `json:"title"`
	Author             string    `json:"author"`
	Draft              bool      `json:"draft"`
	Base               string    `json:"base"`
	Head               string    `json:"head"`
	HeadSHA            string    `json:"head_sha"`
	Labels             []string  `json:"labels"`
	RequestedReviewers []string  `json:"requested_reviewers"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

func (c *Client) ListOpenPRs(ctx context.Context, limit int) ([]PRSummary, error) {
	prs, _, err := c.gh.PullRequests.List(ctx, c.repo.Owner, c.repo.Name, &github.PullRequestListOptions{
		State:       "open",
		Sort:        "updated",
		Direction:   "desc",
		ListOptions: github.ListOptions{PerPage: min(limit, 100)},
	})
	if err != nil {
		return nil, err
	}
	out := make([]PRSummary, 0, len(prs))
	for _, pr := range prs {
		s := PRSummary{
			Number:    pr.GetNumber(),
			Title:     pr.GetTitle(),
			Author:    pr.GetUser().GetLogin(),
			Draft:     pr.GetDraft(),
			Base:      pr.GetBase().GetRef(),
			Head:      pr.GetHead().GetRef(),
			HeadSHA:   pr.GetHead().GetSHA(),
			CreatedAt: pr.GetCreatedAt().Time,
			UpdatedAt: pr.GetUpdatedAt().Time,
		}
		for _, l := range pr.Labels {
			s.Labels = append(s.Labels, l.GetName())
		}
		for _, r := range pr.RequestedReviewers {
			s.RequestedReviewers = append(s.RequestedReviewers, r.GetLogin())
		}
		out = append(out, s)
	}
	return out, nil
}

type PRFile struct {
	Filename       string `json:"filename"`
	Status         string `json:"status"`
	Additions      int    `json:"additions"`
	Deletions      int    `json:"deletions"`
	Patch          string `json:"patch,omitempty"`
	PatchTruncated bool   `json:"patch_truncated"`
}

type PRDetail struct {
	Number         int       `json:"number"`
	Title          string    `json:"title"`
	Body           string    `json:"body"`
	Author         string    `json:"author"`
	State          string    `json:"state"`
	Draft          bool      `json:"draft"`
	Base           string    `json:"base"`
	Head           string    `json:"head"`
	HeadSHA        string    `json:"head_sha"`
	MergeableState string    `json:"mergeable_state"`
	Additions      int       `json:"additions"`
	Deletions      int       `json:"deletions"`
	ChangedFiles   int       `json:"changed_files"`
	UpdatedAt      time.Time `json:"updated_at"`
	Files          []PRFile  `json:"files"`
}

func (c *Client) GetPR(ctx context.Context, number int) (*PRDetail, error) {
	pr, _, err := c.gh.PullRequests.Get(ctx, c.repo.Owner, c.repo.Name, number)
	if err != nil {
		return nil, err
	}
	d := &PRDetail{
		Number:         pr.GetNumber(),
		Title:          pr.GetTitle(),
		Body:           pr.GetBody(),
		Author:         pr.GetUser().GetLogin(),
		State:          pr.GetState(),
		Draft:          pr.GetDraft(),
		Base:           pr.GetBase().GetRef(),
		Head:           pr.GetHead().GetRef(),
		HeadSHA:        pr.GetHead().GetSHA(),
		MergeableState: pr.GetMergeableState(),
		Additions:      pr.GetAdditions(),
		Deletions:      pr.GetDeletions(),
		ChangedFiles:   pr.GetChangedFiles(),
		UpdatedAt:      pr.GetUpdatedAt().Time,
	}

	budget := maxPatchCharsTotal
	opts := &github.ListOptions{PerPage: 100}
	for {
		files, resp, err := c.gh.PullRequests.ListFiles(ctx, c.repo.Owner, c.repo.Name, number, opts)
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			patch := f.GetPatch()
			truncated := patch == "" && f.GetChanges() > 0
			if limit := min(maxPatchCharsPerFile, budget); len(patch) > limit {
				patch, truncated = patch[:limit], true
			}
			budget -= len(patch)
			d.Files = append(d.Files, PRFile{
				Filename:       f.GetFilename(),
				Status:         f.GetStatus(),
				Additions:      f.GetAdditions(),
				Deletions:      f.GetDeletions(),
				Patch:          patch,
				PatchTruncated: truncated,
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return d, nil
}

type CheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Summary    string `json:"summary,omitempty"`
}

type Status struct {
	Context     string `json:"context"`
	State       string `json:"state"`
	Description string `json:"description"`
}

type Checks struct {
	CombinedStatus string     `json:"combined_status"`
	Statuses       []Status   `json:"statuses"`
	CheckRuns      []CheckRun `json:"check_runs"`
}

func (c *Client) GetChecks(ctx context.Context, sha string) (*Checks, error) {
	runs, _, err := c.gh.Checks.ListCheckRunsForRef(ctx, c.repo.Owner, c.repo.Name, sha,
		&github.ListCheckRunsOptions{ListOptions: github.ListOptions{PerPage: 100}})
	if err != nil {
		return nil, err
	}
	combined, _, err := c.gh.Repositories.GetCombinedStatus(ctx, c.repo.Owner, c.repo.Name, sha, nil)
	if err != nil {
		return nil, err
	}
	out := &Checks{CombinedStatus: combined.GetState()}
	for _, s := range combined.Statuses {
		out.Statuses = append(out.Statuses, Status{s.GetContext(), s.GetState(), s.GetDescription()})
	}
	for _, r := range runs.CheckRuns {
		out.CheckRuns = append(out.CheckRuns, CheckRun{
			Name:       r.GetName(),
			Status:     r.GetStatus(),
			Conclusion: r.GetConclusion(),
			Summary:    truncate(r.GetOutput().GetSummary(), 1_000),
		})
	}
	return out, nil
}

type Review struct {
	Reviewer    string    `json:"reviewer"`
	State       string    `json:"state"`
	SubmittedAt time.Time `json:"submitted_at"`
	Body        string    `json:"body,omitempty"`
}

type ReviewComment struct {
	Reviewer  string    `json:"reviewer"`
	Path      string    `json:"path"`
	Line      int       `json:"line,omitempty"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

type Reviews struct {
	Reviews        []Review        `json:"reviews"`
	ReviewComments []ReviewComment `json:"review_comments"`
}

func (c *Client) GetReviews(ctx context.Context, number int) (*Reviews, error) {
	reviews, _, err := c.gh.PullRequests.ListReviews(ctx, c.repo.Owner, c.repo.Name, number,
		&github.ListOptions{PerPage: 100})
	if err != nil {
		return nil, err
	}
	comments, _, err := c.gh.PullRequests.ListComments(ctx, c.repo.Owner, c.repo.Name, number,
		&github.PullRequestListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100}})
	if err != nil {
		return nil, err
	}
	out := &Reviews{}
	for _, r := range reviews {
		out.Reviews = append(out.Reviews, Review{
			Reviewer:    r.GetUser().GetLogin(),
			State:       r.GetState(),
			SubmittedAt: r.GetSubmittedAt().Time,
			Body:        truncate(r.GetBody(), 500),
		})
	}
	for _, cm := range comments {
		out.ReviewComments = append(out.ReviewComments, ReviewComment{
			Reviewer:  cm.GetUser().GetLogin(),
			Path:      cm.GetPath(),
			Line:      cm.GetLine(),
			Body:      truncate(cm.GetBody(), 500),
			CreatedAt: cm.GetCreatedAt().Time,
		})
	}
	return out, nil
}

func (c *Client) GetFile(ctx context.Context, path, ref string) (string, error) {
	file, _, _, err := c.gh.Repositories.GetContents(ctx, c.repo.Owner, c.repo.Name, path,
		&github.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		return "", err
	}
	if file == nil {
		return "", fmt.Errorf("%s is a directory at %s", path, ref)
	}
	return file.GetContent()
}

type InlineComment struct {
	Path string
	Line int
	Body string
}

// PostReview posts a COMMENT review (never APPROVE or REQUEST_CHANGES) and returns its URL.
func (c *Client) PostReview(ctx context.Context, number int, commitSHA, body string, comments []InlineComment) (string, error) {
	req := &github.PullRequestReviewRequest{
		CommitID: github.Ptr(commitSHA),
		Body:     github.Ptr(body),
		Event:    github.Ptr("COMMENT"),
	}
	for _, cm := range comments {
		req.Comments = append(req.Comments, &github.DraftReviewComment{
			Path: github.Ptr(cm.Path),
			Line: github.Ptr(cm.Line),
			Side: github.Ptr("RIGHT"),
			Body: github.Ptr(cm.Body),
		})
	}
	review, _, err := c.gh.PullRequests.CreateReview(ctx, c.repo.Owner, c.repo.Name, number, req)
	if err != nil {
		return "", err
	}
	return review.GetHTMLURL(), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
