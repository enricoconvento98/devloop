# devloop

A Claude-powered agent harness for the development cycle, written in Go. It's built on the official [Anthropic Go SDK](https://github.com/anthropics/anthropic-sdk-go) and its tool runner.

Today it does **PR review** and **PR triage**. The [roadmap](#roadmap) extends it to the rest of the cycle: planning, implementation, CI fixes, deploys and monitoring.

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

## Usage

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

Flags shared by both commands:

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
main.go ──► agent.RunReview / RunTriage
               │  system prompt: prompts/*.md (embedded into the binary)
               │  loop: client.Beta.Messages.NewToolRunner(...)
               │
               ├── read tools ──► internal/ghclient (go-github)
               │     list_open_prs, get_pr, get_pr_checks, get_pr_reviews, get_file
               ├── local tools (only with -repo-dir)
               │     read_local_file, grep_local, list_local_files
               └── submit_review / submit_triage  ◄── the agent's structured result
                                │
main.go renders it as Markdown or JSON, or posts it (-post)
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
| Plan | `plan <issue>` | Issues, Asana, Slack; repo reads | – |
| Implement | `implement <issue>` | Edit, build and test in a git worktree; open a PR | Code, PR |
| Fix CI | `ci-fix <pr>` | Job logs; edit, test, push | Code |
| Deploy | `deploy <pr>` | Deploy CLI (e.g. `statwolf bundle`) | Servers |
| Monitor | `watch` | Logs, metrics, Slack alerts | – |

The coding stages (implement, fix CI) will hand the editing work to Claude Code in headless mode (`claude -p … --output-format json`) inside an isolated worktree. devloop orchestrates, gathers context, and keeps a human approval step before anything is pushed or deployed.

## Development

```bash
go vet ./...
go test ./...   # includes an end-to-end tool-loop test against a fake Claude API
```
