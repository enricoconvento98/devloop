// Package coder delegates code changes to Claude Code running headless
// (`claude -p`), which brings its own file-editing tools and agent loop.
package coder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type Options struct {
	Binary      string  // path to the claude CLI; default "claude"
	Model       string  // empty: Claude Code's default
	MaxBudget   float64 // USD cap for the run; 0 for none
	TestCommand string  // the one shell command Claude may run; empty for none
	Bare        bool    // skip the host's and repo's hooks, plugins, MCP servers and CLAUDE.md
	Verbose     bool
}

// Result is the subset of `claude -p --output-format json` that devloop uses.
type Result struct {
	Subtype   string   `json:"subtype"`
	IsError   bool     `json:"is_error"`
	Result    string   `json:"result"`
	NumTurns  int      `json:"num_turns"`
	CostUSD   float64  `json:"total_cost_usd"`
	SessionID string   `json:"session_id"`
	Errors    []string `json:"errors"`
}

// AllowedTools is the tool allowlist for a run: file reads and edits, plus the test command.
func AllowedTools(testCommand string) []string {
	tools := []string{"Read", "Edit", "Write", "Glob", "Grep"}
	if testCommand != "" {
		// The exact command, and the same command with extra arguments (e.g. one package).
		tools = append(tools, fmt.Sprintf("Bash(%s)", testCommand), fmt.Sprintf("Bash(%s *)", testCommand))
	}
	return tools
}

// Args builds the CLI arguments for a run.
func Args(prompt string, opts Options) []string {
	args := []string{"-p", prompt,
		"--output-format", "json",
		// Anything outside the allowlist is denied rather than prompting.
		"--permission-mode", "dontAsk",
		"--allowedTools", strings.Join(AllowedTools(opts.TestCommand), ","),
	}
	if opts.Bare {
		args = append(args, "--bare")
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.MaxBudget > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(opts.MaxBudget, 'f', 2, 64))
	}
	return args
}

// Run runs Claude Code in dir with prompt and returns its final result.
func Run(ctx context.Context, dir, prompt string, opts Options) (*Result, error) {
	bin := opts.Binary
	if bin == "" {
		bin = "claude"
	}
	cmd := exec.CommandContext(ctx, bin, Args(prompt, opts)...)
	cmd.Dir = dir
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if opts.Verbose {
		cmd.Stderr = os.Stderr
	}
	runErr := cmd.Run()

	var res Result
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &res); err != nil {
		if runErr != nil {
			return nil, fmt.Errorf("claude: %w: %.500s", runErr, stdout.String())
		}
		return nil, fmt.Errorf("claude: unreadable output: %w: %.500s", err, stdout.String())
	}
	if res.IsError || res.Subtype != "success" {
		msg := res.Result
		if msg == "" {
			msg = strings.Join(res.Errors, "; ")
		}
		return &res, errors.New("claude run failed (" + res.Subtype + "): " + msg)
	}
	return &res, nil
}
