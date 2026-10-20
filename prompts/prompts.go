// Package prompts embeds the agents' system prompts, so the binary is self-contained.
package prompts

import _ "embed"

//go:embed review.md
var Review string

//go:embed triage.md
var Triage string
