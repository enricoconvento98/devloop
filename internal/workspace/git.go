// Package workspace manages the isolated git worktree a change is made in.
package workspace

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Worktree is a git worktree on its own branch.
type Worktree struct {
	Dir    string
	Branch string
	Base   string // the ref the branch was created from
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitRaw(ctx, dir, args...)
	return strings.TrimSpace(out), err
}

// gitRaw is git without trimming, for output whose leading whitespace matters.
func gitRaw(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// ResolveBase picks the ref to branch from: origin/<base> when the remote has it,
// then <base> itself; with base empty, origin's default branch, then HEAD.
// It fetches origin first (best effort) so the branch starts from fresh code.
func ResolveBase(ctx context.Context, repoDir, base string) (string, error) {
	if _, err := git(ctx, repoDir, "remote", "get-url", "origin"); err == nil {
		if _, err := git(ctx, repoDir, "fetch", "--quiet", "origin"); err != nil {
			fmt.Fprintf(os.Stderr, "warning: fetch origin failed, using local refs: %v\n", err)
		}
	}
	var candidates []string
	if base != "" {
		candidates = []string{"origin/" + base, base}
	} else {
		if ref, err := git(ctx, repoDir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
			candidates = append(candidates, ref)
		}
		candidates = append(candidates, "HEAD")
	}
	for _, c := range candidates {
		if _, err := git(ctx, repoDir, "rev-parse", "--verify", "--quiet", c+"^{commit}"); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("base ref %q not found in %s", base, repoDir)
}

// Create adds a worktree at dir on a new branch started from base.
func Create(ctx context.Context, repoDir, dir, branch, base string) (*Worktree, error) {
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("%s already exists; remove it with `git worktree remove %s` or pick another branch", dir, dir)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}
	if _, err := git(ctx, repoDir, "worktree", "add", "-b", branch, dir, base); err != nil {
		return nil, err
	}
	return &Worktree{Dir: dir, Branch: branch, Base: base}, nil
}

// Changes stages everything and returns `git diff --stat` against the base;
// empty means nothing changed.
func (w *Worktree) Changes(ctx context.Context) (string, error) {
	if _, err := git(ctx, w.Dir, "add", "--all"); err != nil {
		return "", err
	}
	stat, err := gitRaw(ctx, w.Dir, "diff", "--cached", "--stat", w.Base)
	return strings.TrimRight(stat, "\n"), err
}

// Commit commits the staged changes and returns the commit SHA.
func (w *Worktree) Commit(ctx context.Context, message string) (string, error) {
	if _, err := git(ctx, w.Dir, "commit", "--quiet", "-m", message); err != nil {
		return "", err
	}
	return git(ctx, w.Dir, "rev-parse", "HEAD")
}

// Push pushes the branch to origin and sets its upstream.
func (w *Worktree) Push(ctx context.Context) error {
	_, err := git(ctx, w.Dir, "push", "--quiet", "-u", "origin", w.Branch)
	return err
}

// RunCommand runs a shell command in the worktree and returns its combined output.
func (w *Worktree) RunCommand(ctx context.Context, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = w.Dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}
