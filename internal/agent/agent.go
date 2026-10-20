// Package agent runs Claude in a tool-use loop over read-only GitHub (and
// optionally local-checkout) tools. The agent finishes by calling a submit_*
// tool whose input is its structured result.
package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/enricoconvento98/devloop/internal/ghclient"
	"github.com/enricoconvento98/devloop/prompts"
)

type Options struct {
	Model    string
	Effort   string
	MaxTurns int
	RepoDir  string // local checkout the review agent may read; empty for none
	Verbose  bool
}

// Stats summarises a run's token usage.
type Stats struct {
	Turns            int
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

func (s Stats) String() string {
	return fmt.Sprintf("%d turns · %d in (+%d cached, %d cache-write) / %d out tokens",
		s.Turns, s.InputTokens, s.CacheReadTokens, s.CacheWriteTokens, s.OutputTokens)
}

var ErrNoResult = errors.New("agent finished without submitting a result")

func RunReview(ctx context.Context, client anthropic.Client, gh ghclient.API, repo ghclient.Repo, pr int, opts Options) (*Review, Stats, error) {
	tools := githubTools(gh, opts.Verbose, true)
	task := fmt.Sprintf("Review pull request #%d in %s.", pr, repo)
	if opts.RepoDir != "" {
		local, err := localTools(opts.RepoDir, opts.Verbose)
		if err != nil {
			return nil, Stats{}, fmt.Errorf("--repo-dir: %w", err)
		}
		tools = append(tools, local...)
		task += " A local checkout of the repository is available through the *_local tools; it may not be on the PR's head commit."
	}
	submit, sub := submitTool[Review](opts.Verbose, "submit_review", "Submit your final review. Call exactly once, at the end.")
	tools = append(tools, submit)

	stats, err := run(ctx, client, prompts.Review, task, tools, opts)
	if err == nil && !sub.ok {
		err = ErrNoResult
	}
	if err != nil {
		return nil, stats, err
	}
	return &sub.value, stats, nil
}

func RunTriage(ctx context.Context, client anthropic.Client, gh ghclient.API, repo ghclient.Repo, task string, opts Options) ([]TriageItem, Stats, error) {
	tools := githubTools(gh, opts.Verbose, false)
	submit, sub := submitTool[triageResult](opts.Verbose, "submit_triage", "Submit the triage of all open PRs. Call exactly once, at the end.")
	tools = append(tools, submit)

	stats, err := run(ctx, client, prompts.Triage, task, tools, opts)
	if err == nil && !sub.ok {
		err = ErrNoResult
	}
	if err != nil {
		return nil, stats, err
	}
	return sub.value.Items, stats, nil
}

func run(ctx context.Context, client anthropic.Client, system, task string, tools []anthropic.BetaTool, opts Options) (Stats, error) {
	runner := client.Beta.Messages.NewToolRunner(tools, anthropic.BetaToolRunnerParams{
		BetaMessageNewParams: anthropic.BetaMessageNewParams{
			Model:     anthropic.Model(opts.Model),
			MaxTokens: 16000,
			System:    []anthropic.BetaTextBlockParam{{Text: system}},
			// Cache the growing conversation prefix between loop iterations.
			CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
			OutputConfig: anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(opts.Effort)},
			// If a safety classifier declines, let the API re-route to a suitable model.
			Fallbacks: anthropic.BetaFallbacksParamOfDefault(),
			Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
			Messages: []anthropic.BetaMessageParam{
				anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(task)),
			},
		},
		MaxIterations: opts.MaxTurns,
	})

	var stats Stats
	for msg, err := range runner.All(ctx) {
		if err != nil {
			return stats, err
		}
		stats.Turns++
		stats.InputTokens += msg.Usage.InputTokens
		stats.OutputTokens += msg.Usage.OutputTokens
		stats.CacheReadTokens += msg.Usage.CacheReadInputTokens
		stats.CacheWriteTokens += msg.Usage.CacheCreationInputTokens

		switch msg.StopReason {
		case anthropic.BetaStopReasonRefusal:
			return stats, fmt.Errorf("model declined the request (%s)", msg.StopDetails.Category)
		case anthropic.BetaStopReasonMaxTokens:
			return stats, errors.New("response hit max_tokens")
		}
		if opts.Verbose {
			for _, block := range msg.Content {
				if t, ok := block.AsAny().(anthropic.BetaTextBlock); ok && strings.TrimSpace(t.Text) != "" {
					line, _, _ := strings.Cut(strings.TrimSpace(t.Text), "\n")
					fmt.Fprintf(os.Stderr, "  %.160s\n", line)
				}
			}
		}
	}
	return stats, nil
}
