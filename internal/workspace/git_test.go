package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newRepo creates a git repository with one commit.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
	run("add", "-A")
	run("commit", "-qm", "init")
	return dir
}

func TestWorktreeLifecycle(t *testing.T) {
	ctx := context.Background()
	repo := newRepo(t)

	base, err := ResolveBase(ctx, repo, "")
	if err != nil || base != "HEAD" {
		t.Fatalf("ResolveBase = %q, %v; want HEAD (no origin)", base, err)
	}
	if b, err := ResolveBase(ctx, repo, "main"); err != nil || b != "main" {
		t.Fatalf("ResolveBase(main) = %q, %v", b, err)
	}
	if _, err := ResolveBase(ctx, repo, "nope"); err == nil {
		t.Fatal("missing base accepted")
	}

	wt, err := Create(ctx, repo, filepath.Join(t.TempDir(), "wt"), "feature", base)
	if err != nil {
		t.Fatal(err)
	}
	if stat, err := wt.Changes(ctx); err != nil || stat != "" {
		t.Fatalf("fresh worktree has changes: %q, %v", stat, err)
	}

	os.WriteFile(filepath.Join(wt.Dir, "b.txt"), []byte("b\n"), 0o644)
	stat, err := wt.Changes(ctx)
	if err != nil || !strings.Contains(stat, "b.txt") || !strings.HasPrefix(stat, " ") {
		t.Fatalf("Changes = %q, %v", stat, err)
	}
	if _, err := wt.Commit(ctx, "add b"); err != nil {
		t.Fatal(err)
	}
	if out, err := wt.RunCommand(ctx, "git log --oneline | wc -l"); err != nil || strings.TrimSpace(out) != "2" {
		t.Fatalf("commit count = %q, %v", out, err)
	}
	// The original checkout is untouched.
	if _, err := os.Stat(filepath.Join(repo, "b.txt")); !os.IsNotExist(err) {
		t.Error("change leaked into the main checkout")
	}
	if _, err := Create(ctx, repo, wt.Dir, "other", base); err == nil {
		t.Error("existing worktree dir reused")
	}
}
