# devloop

A Claude-powered agent harness for the development cycle, written in Go. It's built on the official [Anthropic Go SDK](https://github.com/anthropics/anthropic-sdk-go) and its tool runner.

It takes work from issue to merge-ready PR:

```
devloop plan       issue  ──►  structured implementation plan (plan-N.json)
devloop implement  plan   ──►  code + tests in an isolated worktree ──► (draft PR)
devloop review     PR     ──►  severity-ranked findings
devloop triage     repo   ──►  prioritised table of open PRs
```

The [roadmap](#roadmap) covers what's next: CI fixes, deploys and monitoring.

## Install

```bash
go install github.com/enricoconvento98/devloop@latest
# or, from a clone:
go build -o devloop .
```

Set your credentials:

```bash
export GITHUB_TOKEN=ghp_...         # read access to the repo; pull-request write access for -post
export ANTHROPIC_API_KEY=sk-ant-... # or log in with `ant auth login`
```

Requirements: Go 1.25+, `git`, and for `implement` the [Claude Code](https://code.claude.com) CLI (`claude`) on your PATH.

## Usage

### Plan and implement an issue

```bash
# 1. Plan: reads the issue and the code, writes plan-42.json (and prints it)
devloop plan -repo-dir . owner/repo 42
devloop plan -repo-dir . -post owner/repo 42      # also comment the plan on the issue

# 2. Implement: Claude Code writes the change in a separate worktree,
#    devloop re-runs your tests and commits on branch devloop/issue-42
devloop implement -plan plan-42.json -repo-dir . -test "go test ./..."

# 3. When it looks right, push and open a draft PR (asks first; -yes skips the question)
devloop implement -plan plan-42.json -repo-dir . -test "go test ./..." -push
```

You can edit `plan-42.json` between steps 1 and 2. That's the moment to correct the approach before any code is written.

What `implement` does, and what it won't do:

1. Creates a **git worktree** on a new branch from the fresh base (origin's default branch, or `-base`). Your checkout is never touched.
2. Runs **Claude Code headless** (`claude -p`) in the worktree with the plan as its task. It can only read and edit files and run **your test command**. Every other shell command is denied (`--permission-mode dontAsk`). The run is capped at `-max-budget` USD (default 5).
3. **Re-runs the tests itself.** If they fail, nothing is committed and the worktree is kept for you to inspect.
4. **Commits** on the branch. It pushes and opens a **draft** PR (`Closes #42`) only with `-push`, and only after you confirm.

With `ANTHROPIC_API_KEY` set, Claude Code runs with `--bare`. In that mode it ignores hooks, plugins, MCP servers and CLAUDE.md, both yours and the repo's, so every machine gets the same result. Without a key it uses your normal Claude Code login and configuration.

### Review and triage PRs

```bash
# Review a PR (printed as Markdown)
devloop review owner/repo 123

# Let the agent read a local checkout for extra context (read-only, confined to that directory)
devloop review -repo-dir ~/src/repo owner/repo 123

# Post the review on the PR: findings with a line become inline comments.
# It always posts as COMMENT; it never approves or requests changes.
devloop review -post owner/repo 123

# Triage open PRs into a prioritised table
devloop triage -limit 20 -stale-days 10 owner/repo
```

Flags shared by `plan`, `review` and `triage`:

| Flag | Default | Meaning |
|---|---|---|
| `-model` | `claude-opus-5-5` | Claude model ID (`claude-sonnet-5-5` is cheaper) |
| `-effort` | `high` | `low` … `max`: how hard the model thinks |
| `-max-turns` | 40 / 80 | Cap on API calls per run |
| `-json` | off | Print the structured result as JSON |
| `-v` | off | Log every tool call to stderr |

Every run prints its turn count and token usage.

## How it works

```
main.go ──► agent.RunPlan / RunReview / RunTriage
               │  system prompt: prompts/*.md (embedded into the binary)
               │  loop: client.Beta.Messages.NewToolRunner(...)
               │
               ├── read tools ──► internal/ghclient (go-github)
               │     get_issue, list_open_prs, get_pr, get_pr_checks, get_pr_reviews, get_file
               ├── local tools (only with -repo-dir)
               │     read_local_file, grep_local, list_local_files
               └── submit_plan / submit_review / submit_triage  ◄── structured result
                                │
main.go renders it as Markdown or JSON, or posts it (-post)

cmd_implement.go ──► internal/workspace (git worktree, commit, push)
                 ──► internal/coder     (claude -p, locked-down tool allowlist)
                 ──► re-run tests ──► commit ──► (push + draft PR)
```

The design choices that matter:

- **The agent can only read.** Every tool it gets is read-only. Local file access is confined to `-repo-dir`, and symlinks or `..` can't escape it. Writes, such as posting a review, are done by the CLI after the run, and only when you ask for them with a flag.
- **Structured output through a tool.** The agent finishes by calling `submit_*`. Go struct tags define that tool's JSON schema (`jsonschema:"required,enum=..."`), so the result is typed data, not text you have to parse.
- **Prompts are plain Markdown.** Edit `prompts/review.md` or `prompts/triage.md` to change the agent's behaviour, then rebuild.
- **Reliability defaults.** The conversation prefix is cached between turns, which saves cost on long loops. Server-side refusal fallback is on, and runs fail clearly on `max_tokens` or a refusal.

## Adding a command

A command is a **prompt**, a **set of tools** and a **submit tool**:

1. Write `prompts/<name>.md` and embed it in `prompts/prompts.go`.
2. Define the result type with `jsonschema` tags in `internal/agent/tools.go`.
3. Add `RunX` in `internal/agent/agent.go`: pick the tools, add `submitTool[YourType]`, and call `run(...)`.
4. Add the subcommand in `main.go` and a renderer in `render.go`.

New tools are just Go functions:

```go
newTool(verbose, "get_job_logs", "Get the log of a failed CI job.",
	func(ctx context.Context, in jobInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
		return asJSON(gh.GetJobLogs(ctx, in.JobID))
	})
```

## Roadmap

| Stage | Command | Needs | Writes? |
|---|---|---|---|
| Review | `review` ✅ | GitHub reads | Optional comment (`-post`) |
| Triage | `triage` ✅ | GitHub reads | – |
| Plan | `plan` ✅ | Issue + repo reads | Optional comment (`-post`) |
| Implement | `implement` ✅ | Claude Code in a worktree; your test command | Commit; draft PR with `-push` |
| Fix CI | `ci-fix <pr>` | Job logs; edit, test, push | Code |
| Deploy | `deploy <pr>` | Deploy CLI (e.g. `statwolf bundle`) | Servers |
| Monitor | `watch` | Logs, metrics, Slack alerts | – |

`ci-fix` will reuse the `implement` machinery (worktree, Claude Code, re-verification), with the failing job's logs as the task. Next on the list: pulling issues from Asana and Slack in `plan`.

## Development

```bash
go vet ./...
go test ./...   # offline: fake Claude API, fake claude CLI, real git
```
