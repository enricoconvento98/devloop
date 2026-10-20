package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enricoconvento98/devloop/internal/agent"
)

func testPlan() *agent.PlanFile {
	return &agent.PlanFile{
		Issue: agent.IssueRef{Repo: "acme/toy", Number: 1, Title: "Add b"},
		Base:  "main",
		Plan: agent.Plan{
			Summary:       "Add b.txt.",
			Approach:      "Write the file.",
			Steps:         []agent.Step{{Title: "Create b", Files: []string{"b.txt"}, Details: "Write b."}},
			TestPlan:      "test -f b.txt",
			OpenQuestions: []string{"Uppercase or lowercase?"},
		},
	}
}

func TestRenderImplementPrompt(t *testing.T) {
	p, err := renderImplementPrompt(testPlan(), "test -f b.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"issue #1", "### Step 1: Create b", "Files: b.txt", "exactly `test -f b.txt`", "Uppercase or lowercase?"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
	if p, _ := renderImplementPrompt(testPlan(), ""); !strings.Contains(p, "No test command") {
		t.Error("missing no-test-command note")
	}
}

func TestLoadPlanRejectsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.json")
	os.WriteFile(path, []byte(`{"issue":{"repo":"a/b","number":1},"plan":{"steps":[]}}`), 0o644)
	if _, err := loadPlan(path); err == nil {
		t.Fatal("plan without steps accepted")
	}
}

// TestImplementEndToEnd runs the implement command against a real git repo,
// with a fake claude CLI that writes a file.
func TestImplementEndToEnd(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	os.MkdirAll(repo, 0o755)
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "t"},
		{"commit", "-q", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	planPath := filepath.Join(dir, "plan.json")
	b, _ := json.Marshal(testPlan())
	os.WriteFile(planPath, b, 0o644)

	claude := filepath.Join(dir, "claude")
	os.WriteFile(claude, []byte("#!/bin/sh\necho b > b.txt\necho '{\"subtype\":\"success\",\"is_error\":false,\"result\":\"Added b.txt.\",\"num_turns\":2,\"total_cost_usd\":0.01}'\n"), 0o755)

	wtDir := filepath.Join(dir, "wt")
	err := implementCmd(context.Background(), []string{
		"-plan", planPath, "-repo-dir", repo, "-worktree-dir", wtDir, "-claude", claude, "-test", "test -f b.txt",
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", wtDir, "log", "--format=%s", "-1").Output()
	if err != nil || strings.TrimSpace(string(out)) != "Add b (#1)" {
		t.Fatalf("last commit = %q, %v", out, err)
	}

	// A failing verification must not commit.
	err = implementCmd(context.Background(), []string{
		"-plan", planPath, "-repo-dir", repo, "-worktree-dir", filepath.Join(dir, "wt2"), "-branch", "other",
		"-claude", claude, "-test", "test -f missing.txt",
	})
	if err == nil || !strings.Contains(err.Error(), "nothing committed") {
		t.Fatalf("err = %v, want test failure", err)
	}
}

func TestParseFlagsInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	limit := fs.Int("limit", 30, "")
	v := fs.Bool("v", false, "")
	parseFlags(fs, []string{"acme/shop", "-limit", "5", "extra", "-v", "--", "-not-a-flag"})
	if *limit != 5 || !*v {
		t.Errorf("limit=%d v=%v", *limit, *v)
	}
	if got := strings.Join(fs.Args(), " "); got != "acme/shop extra -not-a-flag" {
		t.Errorf("args = %q", got)
	}
}
