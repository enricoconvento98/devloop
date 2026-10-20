// Package prompts embeds the agents' prompts, so the binary is self-contained.
package prompts

import _ "embed"

//go:embed review.md
var Review string

//go:embed triage.md
var Triage string

//go:embed plan.md
var Plan string

// Implement is a text/template for the task given to Claude Code in the implement step.
//
//go:embed implement.md
var Implement string
