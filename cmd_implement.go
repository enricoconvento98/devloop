package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/enricoconvento98/devloop/internal/agent"
	"github.com/enricoconvento98/devloop/internal/coder"
	"github.com/enricoconvento98/devloop/internal/ghclient"
	"github.com/enricoconvento98/devloop/internal/workspace"
	"github.com/enricoconvento98/devloop/prompts"
)

var implementTmpl = template.Must(template.New("implement").Funcs(template.FuncMap{
	"inc":  func(i int) int { return i + 1 },
	"join": strings.Join,
}).Parse(prompts.Implement))

type implementInput struct {
	*agent.PlanFile
	TestCommand string
}

func renderImplementPrompt(pf *agent.PlanFile, testCommand string) (string, error) {
	var b strings.Builder
	err := implementTmpl.Execute(&b, implementInput{pf, testCommand})
	return b.String(), err
}

func implementCmd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("implement", flag.ExitOnError)
	planPath := fs.String("plan", "", "plan JSON written by `devloop plan` (required)")
	repoDir := fs.String("repo-dir", ".", "local clone of the repository")
	testCmd := fs.String("test", "", "test command; Claude may run it, and devloop re-runs it before committing")
	base := fs.String("base", "", "branch to start from (default: the plan's base, else origin's default)")
	branch := fs.String("branch", "", "branch to create (default devloop/issue-<n>)")
	worktreeDir := fs.String("worktree-dir", "", "where to create the worktree (default: user cache dir)")
	model := fs.String("model", "", "model for Claude Code (default: Claude Code's default)")
	budget := fs.Float64("max-budget", 5, "USD cap for the Claude Code run (0 for none)")
	bare := fs.Bool("bare", os.Getenv("ANTHROPIC_API_KEY") != "", "run Claude Code with --bare (ignores the repo's and your hooks, plugins, MCP servers and CLAUDE.md; needs ANTHROPIC_API_KEY)")
	claudeBin := fs.String("claude", envOr("DEVLOOP_CLAUDE", "claude"), "path to the claude CLI")
	timeout := fs.Duration("timeout", 45*time.Minute, "overall time limit")
	push := fs.Bool("push", false, "push the branch and open a draft PR (asks for confirmation)")
	yes := fs.Bool("yes", false, "with -push, don't ask for confirmation")
	verbose := fs.Bool("v", false, "show Claude Code's stderr")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: devloop implement [flags] -plan <plan.json>")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if *planPath == "" || fs.NArg() != 0 {
		fs.Usage()
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	pf, err := loadPlan(*planPath)
	if err != nil {
		return err
	}
	if len(pf.Plan.OpenQuestions) > 0 {
		fmt.Fprintf(os.Stderr, "note: the plan has %d open question(s); Claude will pick and report its choices\n", len(pf.Plan.OpenQuestions))
	}
	repoAbs, err := filepath.Abs(*repoDir)
	if err != nil {
		return err
	}
	if *base == "" {
		*base = pf.Base
	}
	if *branch == "" {
		*branch = fmt.Sprintf("devloop/issue-%d", pf.Issue.Number)
	}
	if *worktreeDir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return err
		}
		*worktreeDir = filepath.Join(cache, "devloop", "worktrees", filepath.Base(repoAbs)+"-"+slug(*branch))
	}

	baseRef, err := workspace.ResolveBase(ctx, repoAbs, *base)
	if err != nil {
		return err
	}
	wt, err := workspace.Create(ctx, repoAbs, *worktreeDir, *branch, baseRef)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "worktree %s on %s (from %s)\n", wt.Dir, wt.Branch, baseRef)

	prompt, err := renderImplementPrompt(pf, *testCmd)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "running Claude Code…")
	res, err := coder.Run(ctx, wt.Dir, prompt, coder.Options{
		Binary: *claudeBin, Model: *model, MaxBudget: *budget, TestCommand: *testCmd, Bare: *bare, Verbose: *verbose,
	})
	if err != nil {
		return fmt.Errorf("%w\nworktree kept at %s", err, wt.Dir)
	}

	stat, err := wt.Changes(ctx)
	if err != nil {
		return err
	}
	if stat == "" {
		return fmt.Errorf("Claude Code made no changes. Its summary:\n%s\nworktree kept at %s", res.Result, wt.Dir)
	}

	testSummary := "not run (no -test given)"
	var testOutput string
	if *testCmd != "" {
		fmt.Fprintf(os.Stderr, "verifying: %s\n", *testCmd)
		testOutput, err = wt.RunCommand(ctx, *testCmd)
		if err != nil {
			return fmt.Errorf("tests fail after Claude's changes (%v):\n%s\nnothing committed; worktree kept at %s", err, tail(testOutput, 40), wt.Dir)
		}
		testSummary = "passed: `" + *testCmd + "`"
	}

	title := fmt.Sprintf("%s (#%d)", pf.Issue.Title, pf.Issue.Number)
	sha, err := wt.Commit(ctx, fmt.Sprintf("%s\n\n%s\n\nImplemented by devloop with Claude Code.", title, pf.Plan.Summary))
	if err != nil {
		return err
	}

	fmt.Printf("## Implemented #%d — %s\n\n", pf.Issue.Number, pf.Issue.Title)
	fmt.Printf("Branch `%s` · commit `%.10s` · worktree `%s`\n\n", wt.Branch, sha, wt.Dir)
	fmt.Printf("```\n%s\n```\n\n**Tests:** %s\n\n**Claude Code:**\n\n%s\n\n_%d turns · ~$%.2f_\n",
		stat, testSummary, strings.TrimSpace(res.Result), res.NumTurns, res.CostUSD)

	if !*push {
		fmt.Fprintf(os.Stderr, "\nnot pushed. Inspect with: git -C %s show --stat\nthen re-run with -push, or push yourself: git -C %s push -u origin %s\n", wt.Dir, wt.Dir, wt.Branch)
		return nil
	}
	if !*yes && !confirm(fmt.Sprintf("Push %s and open a draft PR in %s?", wt.Branch, pf.Issue.Repo)) {
		fmt.Fprintln(os.Stderr, "not pushed.")
		return nil
	}

	repo, err := ghclient.ParseRepo(pf.Issue.Repo)
	if err != nil {
		return err
	}
	gh, err := ghclient.New(repo)
	if err != nil {
		return err
	}
	if err := wt.Push(ctx); err != nil {
		return err
	}
	body := fmt.Sprintf("Closes #%d\n\n%s\n\n**Tests:** %s\n\n### Notes from Claude Code\n\n%s\n\n---\n_Generated by devloop (Claude). Review before merging._",
		pf.Issue.Number, pf.Plan.Summary, testSummary, strings.TrimSpace(res.Result))
	url, err := gh.CreateDraftPR(ctx, wt.Branch, strings.TrimPrefix(baseRef, "origin/"), title, body)
	if err != nil {
		return fmt.Errorf("branch pushed but opening the PR failed: %w", err)
	}
	fmt.Fprintln(os.Stderr, "opened draft PR:", url)
	return nil
}

func loadPlan(path string) (*agent.PlanFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pf agent.PlanFile
	if err := json.Unmarshal(b, &pf); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if pf.Issue.Number == 0 || pf.Issue.Repo == "" || len(pf.Plan.Steps) == 0 {
		return nil, fmt.Errorf("%s: not a devloop plan, or the plan has no steps", path)
	}
	return &pf, nil
}

var nonSlug = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func slug(s string) string { return nonSlug.ReplaceAllString(s, "-") }

func tail(s string, lines int) string {
	parts := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}

func confirm(question string) bool {
	fmt.Fprintf(os.Stderr, "%s [y/N] ", question)
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
