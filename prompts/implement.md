Implement GitHub issue #{{.Issue.Number}} ("{{.Issue.Title}}") in this repository, following the plan below.

You are working in a dedicated git worktree on a fresh branch. Edit the files directly. Don't commit, push or create branches: devloop reviews and commits your changes after you finish.

## Plan

**Summary:** {{.Plan.Summary}}

**Approach:** {{.Plan.Approach}}

{{range $i, $s := .Plan.Steps}}### Step {{inc $i}}: {{$s.Title}}
Files: {{join $s.Files ", "}}

{{$s.Details}}

{{end}}**Test plan:** {{.Plan.TestPlan}}
{{if .Plan.Risks}}
**Risks to watch:**
{{range .Plan.Risks}}- {{.}}
{{end}}{{end}}{{if .Plan.OpenQuestions}}
**Open questions:** these weren't settled when the plan was written. Pick the option most consistent with the codebase and the issue, and note your choice in your final message.
{{range .Plan.OpenQuestions}}- {{.}}
{{end}}{{end}}
## Rules

- Read the code around each change before editing, and follow its conventions.
- Keep the change to what the plan and the issue need. If the plan is wrong in a detail, do what the issue actually needs and say why in your final message.
- Add or update tests as the test plan says.
{{if .TestCommand}}- Run the tests with exactly: `{{.TestCommand}}`. Iterate until they pass. Don't skip, disable or weaken tests to make them pass.
{{else}}- No test command was configured, so you can't run the tests. Make sure the code is correct by reading it carefully.
{{end}}
When you're done, reply with a short summary: what you changed, any deviation from the plan and why, and whether the tests pass.
