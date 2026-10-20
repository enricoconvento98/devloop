package agent

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/enricoconvento98/devloop/internal/ghclient"
	"github.com/enricoconvento98/devloop/prompts"
)

// Step is one unit of work in a plan.
type Step struct {
	Title   string   `json:"title" jsonschema:"required"`
	Files   []string `json:"files" jsonschema:"required,description=Repo-relative paths this step creates or changes"`
	Details string   `json:"details" jsonschema:"required,description=What to change and why, precise enough for another engineer to implement"`
}

// Plan is the structured result of the planning agent.
type Plan struct {
	Summary       string   `json:"summary" jsonschema:"required,description=1-3 sentences: what will change and why"`
	Approach      string   `json:"approach" jsonschema:"required,description=The chosen design and the main alternative rejected, with the reason"`
	Steps         []Step   `json:"steps" jsonschema:"required"`
	TestPlan      string   `json:"test_plan" jsonschema:"required,description=Tests to add or update and how to verify the change"`
	Risks         []string `json:"risks" jsonschema:"required,description=What could break; empty if none"`
	OpenQuestions []string `json:"open_questions" jsonschema:"required,description=Decisions only the issue author can make; empty if none"`
}

// IssueRef identifies the issue a plan is for.
type IssueRef struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	URL    string `json:"url"`
}

// PlanFile is what `devloop plan` writes and `devloop implement` reads.
type PlanFile struct {
	Issue IssueRef `json:"issue"`
	Base  string   `json:"base"`
	Plan  Plan     `json:"plan"`
}

type issueInput struct {
	Number int `json:"number" jsonschema:"required"`
}

func RunPlan(ctx context.Context, client anthropic.Client, gh ghclient.API, repo ghclient.Repo, issue int, opts Options) (*PlanFile, Stats, error) {
	var fetched *ghclient.Issue
	tools := []anthropic.BetaTool{
		newTool(opts.Verbose, "get_issue", "Get an issue's title, body, labels, comments and the repository's default branch.",
			func(ctx context.Context, in issueInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				is, err := gh.GetIssue(ctx, in.Number)
				if err == nil && in.Number == issue {
					fetched = is
				}
				return asJSON(is, err)
			}),
		newTool(opts.Verbose, "get_file", "Get the full contents of a file at a git ref (e.g. the default branch).",
			func(ctx context.Context, in fileInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				content, err := gh.GetFile(ctx, in.Path, in.Ref)
				return text(content), err
			}),
	}
	task := fmt.Sprintf("Write an implementation plan for issue #%d in %s.", issue, repo)
	if opts.RepoDir != "" {
		local, err := localTools(opts.RepoDir, opts.Verbose)
		if err != nil {
			return nil, Stats{}, fmt.Errorf("-repo-dir: %w", err)
		}
		tools = append(tools, local...)
		task += " A local checkout of the repository is available through the *_local tools; prefer it for exploring the code."
	}
	submit, sub := submitTool[Plan](opts.Verbose, "submit_plan", "Submit the implementation plan. Call exactly once, at the end.")
	tools = append(tools, submit)

	stats, err := run(ctx, client, prompts.Plan, task, tools, opts)
	if err == nil && !sub.ok {
		err = ErrNoResult
	}
	if err != nil {
		return nil, stats, err
	}
	if fetched == nil {
		// The agent should always read the issue; fetch it so the plan file is complete either way.
		if fetched, err = gh.GetIssue(ctx, issue); err != nil {
			return nil, stats, err
		}
	}
	return &PlanFile{
		Issue: IssueRef{Repo: repo.String(), Number: fetched.Number, Title: fetched.Title, URL: fetched.URL},
		Base:  fetched.DefaultBranch,
		Plan:  sub.value,
	}, stats, nil
}
