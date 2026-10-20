package coder

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeClaude writes a script that records its arguments and prints output.
func fakeClaude(t *testing.T, output string) (bin, argsFile string) {
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	bin = filepath.Join(dir, "claude")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\ncat <<'JSON'\n" + output + "\nJSON\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, argsFile
}

func TestArgsLockDownPermissions(t *testing.T) {
	args := Args("do it", Options{TestCommand: "go test ./...", Bare: true, MaxBudget: 3})
	joined := strings.Join(args, " ")
	for _, want := range []string{"-p do it", "--permission-mode dontAsk", "--bare", "--max-budget-usd 3.00",
		"Bash(go test ./...),Bash(go test ./... *)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %s", want, joined)
		}
	}
	if slices.Contains(AllowedTools(""), "Bash") {
		t.Error("bare Bash allowed")
	}
	for _, tool := range AllowedTools("") {
		if strings.HasPrefix(tool, "Bash") {
			t.Errorf("Bash allowed without a test command: %s", tool)
		}
	}
}

func TestRunParsesResult(t *testing.T) {
	bin, argsFile := fakeClaude(t, `{"type":"result","subtype":"success","is_error":false,"result":"Fixed it.","num_turns":4,"total_cost_usd":0.12,"session_id":"s1"}`)
	res, err := Run(context.Background(), t.TempDir(), "fix the bug", Options{Binary: bin})
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != "Fixed it." || res.NumTurns != 4 || res.CostUSD != 0.12 {
		t.Fatalf("unexpected result: %+v", res)
	}
	got, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(got), "fix the bug") {
		t.Errorf("prompt not passed: %s", got)
	}
}

func TestRunReportsFailure(t *testing.T) {
	bin, _ := fakeClaude(t, `{"type":"result","subtype":"error_max_budget_usd","is_error":true,"errors":["budget exceeded"]}`)
	_, err := Run(context.Background(), t.TempDir(), "x", Options{Binary: bin})
	if err == nil || !strings.Contains(err.Error(), "budget exceeded") {
		t.Fatalf("err = %v", err)
	}
}
