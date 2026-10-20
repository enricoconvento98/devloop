package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"

	"github.com/enricoconvento98/devloop/internal/ghclient"
)

// Finding is one problem reported by the review agent.
type Finding struct {
	Severity string `json:"severity" jsonschema:"required,enum=blocker,enum=major,enum=minor,enum=nit"`
	File     string `json:"file" jsonschema:"required,description=Path of the file as in the PR's file list"`
	Line     int    `json:"line,omitempty" jsonschema:"description=Line number in the new version of the file; must be a line touched by the diff. Omit if not applicable."`
	Title    string `json:"title" jsonschema:"required,description=One-line statement of the problem"`
	Body     string `json:"body" jsonschema:"required,description=Why it is a problem: a concrete failure scenario and a suggested fix"`
}

// Review is the structured result of the review agent.
type Review struct {
	Summary  string    `json:"summary" jsonschema:"required,description=2-5 sentence overview of the change and whether it is safe to merge"`
	Verdict  string    `json:"verdict" jsonschema:"required,enum=approve,enum=comment,enum=request_changes"`
	Findings []Finding `json:"findings" jsonschema:"required"`
}

// TriageItem is the triage agent's assessment of one open PR.
type TriageItem struct {
	Number     int    `json:"number" jsonschema:"required"`
	Title      string `json:"title" jsonschema:"required"`
	Author     string `json:"author,omitempty"`
	Status     string `json:"status" jsonschema:"required,enum=ready_to_merge,enum=needs_review,enum=changes_requested,enum=ci_failing,enum=merge_conflict,enum=stale,enum=draft"`
	Priority   string `json:"priority" jsonschema:"required,enum=high,enum=medium,enum=low"`
	Summary    string `json:"summary" jsonschema:"required,description=One sentence on what the PR does"`
	NextAction string `json:"next_action" jsonschema:"required,description=Who should do what next"`
}

type triageResult struct {
	Items []TriageItem `json:"items" jsonschema:"required"`
}

type listInput struct {
	Limit int `json:"limit" jsonschema:"required,minimum=1,maximum=100"`
}
type prInput struct {
	Number int `json:"number" jsonschema:"required"`
}
type shaInput struct {
	SHA string `json:"sha" jsonschema:"required,description=Commit SHA (use the PR's head_sha)"`
}
type fileInput struct {
	Path string `json:"path" jsonschema:"required"`
	Ref  string `json:"ref" jsonschema:"required,description=Branch name or commit SHA"`
}

func text(s string) anthropic.BetaToolResultBlockParamContentUnion {
	return anthropic.BetaToolResultBlockParamContentUnion{OfText: &anthropic.BetaTextBlockParam{Text: s}}
}

func asJSON(v any, err error) (anthropic.BetaToolResultBlockParamContentUnion, error) {
	if err != nil {
		return anthropic.BetaToolResultBlockParamContentUnion{}, err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return anthropic.BetaToolResultBlockParamContentUnion{}, err
	}
	return text(string(b)), nil
}

// newTool builds a tool from a typed handler and, when verbose, logs each call to stderr.
func newTool[T any](verbose bool, name, description string, handler func(context.Context, T) (anthropic.BetaToolResultBlockParamContentUnion, error)) anthropic.BetaTool {
	t, err := toolrunner.NewBetaToolFromJSONSchema(name, description, func(ctx context.Context, in T) (anthropic.BetaToolResultBlockParamContentUnion, error) {
		if verbose {
			b, _ := json.Marshal(in)
			fmt.Fprintf(os.Stderr, "  → %s %.200s\n", name, b)
		}
		return handler(ctx, in)
	})
	if err != nil {
		panic(fmt.Sprintf("tool %s: %v", name, err)) // schema is static; a failure is a programming error
	}
	return t
}

func githubTools(gh ghclient.API, verbose bool, withFile bool) []anthropic.BetaTool {
	tools := []anthropic.BetaTool{
		newTool(verbose, "list_open_prs", "List open pull requests, most recently updated first.",
			func(ctx context.Context, in listInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				return asJSON(gh.ListOpenPRs(ctx, in.Limit))
			}),
		newTool(verbose, "get_pr", "Get a pull request's metadata, description and changed files with diffs. Large patches are truncated; use get_file for full contents.",
			func(ctx context.Context, in prInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				return asJSON(gh.GetPR(ctx, in.Number))
			}),
		newTool(verbose, "get_pr_checks", "Get CI check runs and commit statuses for a commit SHA.",
			func(ctx context.Context, in shaInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				return asJSON(gh.GetChecks(ctx, in.SHA))
			}),
		newTool(verbose, "get_pr_reviews", "Get the reviews and inline review comments already left on a pull request.",
			func(ctx context.Context, in prInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				return asJSON(gh.GetReviews(ctx, in.Number))
			}),
	}
	if withFile {
		tools = append(tools, newTool(verbose, "get_file", "Get the full contents of a file at a git ref.",
			func(ctx context.Context, in fileInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
				content, err := gh.GetFile(ctx, in.Path, in.Ref)
				return text(content), err
			}))
	}
	return tools
}

// submission holds the agent's final result once it calls its submit tool.
type submission[T any] struct {
	value T
	ok    bool
}

// submitTool returns a tool whose input is the agent's final result, and where it is stored.
func submitTool[T any](verbose bool, name, description string) (anthropic.BetaTool, *submission[T]) {
	sub := &submission[T]{}
	t := newTool(verbose, name, description, func(_ context.Context, in T) (anthropic.BetaToolResultBlockParamContentUnion, error) {
		sub.value, sub.ok = in, true
		return text("Received. You are done; do not call any more tools."), nil
	})
	return t, sub
}
