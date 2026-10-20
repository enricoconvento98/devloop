package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/enricoconvento98/devloop/internal/agent"
	"github.com/enricoconvento98/devloop/internal/ghclient"
)

var severityIcon = map[string]string{"blocker": "🔴", "major": "🟠", "minor": "🟡", "nit": "⚪"}

func renderReview(pr int, r *agent.Review, findings []agent.Finding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Review of #%d — %s\n\n%s\n\n", pr, strings.ReplaceAll(r.Verdict, "_", " "), r.Summary)
	if len(r.Findings) == 0 {
		b.WriteString("No findings.\n")
	}
	for _, f := range findings {
		where := f.File
		if f.Line > 0 {
			where = fmt.Sprintf("%s:%d", f.File, f.Line)
		}
		fmt.Fprintf(&b, "### %s %s\n`%s` · %s\n\n%s\n\n", severityIcon[f.Severity], f.Title, where, f.Severity, f.Body)
	}
	return strings.TrimRight(b.String(), "\n")
}

var (
	priorityOrder = []string{"high", "medium", "low"}
	statusOrder   = []string{"ci_failing", "merge_conflict", "changes_requested", "needs_review", "ready_to_merge", "stale", "draft"}
)

func renderTriage(repo ghclient.Repo, items []agent.TriageItem) string {
	sorted := slices.Clone(items)
	slices.SortStableFunc(sorted, func(a, b agent.TriageItem) int {
		if d := slices.Index(priorityOrder, a.Priority) - slices.Index(priorityOrder, b.Priority); d != 0 {
			return d
		}
		return slices.Index(statusOrder, a.Status) - slices.Index(statusOrder, b.Status)
	})
	esc := strings.NewReplacer("|", `\|`, "\n", " ").Replace

	var b strings.Builder
	fmt.Fprintf(&b, "## Open PRs in %s (%d)\n\n", repo, len(items))
	b.WriteString("| PR | Priority | Status | Author | Summary | Next action |\n|---|---|---|---|---|---|\n")
	for _, i := range sorted {
		fmt.Fprintf(&b, "| #%d %s | %s | %s | %s | %s | %s |\n", i.Number, esc(i.Title), i.Priority,
			strings.ReplaceAll(i.Status, "_", " "), i.Author, esc(i.Summary), esc(i.NextAction))
	}
	return strings.TrimRight(b.String(), "\n")
}
