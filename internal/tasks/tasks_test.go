package tasks

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func openStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())

	if _, err := s.Add(ctx, "deploy", nil, ""); err == nil {
		t.Fatal("unknown kind accepted")
	}
	id, err := s.Add(ctx, "review", []string{"acme/shop", "7"}, "")
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.Claim(ctx)
	if err != nil || task == nil || task.ID != id || task.Status != Running || task.StartedAt == nil {
		t.Fatalf("Claim = %+v, %v", task, err)
	}
	if next, _ := s.Claim(ctx); next != nil {
		t.Fatalf("claimed a running task again: %+v", next)
	}
	if err := s.Finish(ctx, id, Succeeded, "Review of #7", ""); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, id)
	if got.Status != Succeeded || got.Summary != "Review of #7" || got.Title() != "review acme/shop 7" {
		t.Fatalf("Get = %+v", got)
	}

	retry, err := s.Retry(ctx, id)
	if err != nil || retry == id {
		t.Fatalf("Retry = %d, %v", retry, err)
	}
	if err := s.Cancel(ctx, retry); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.Get(ctx, retry); c.Status != Canceled {
		t.Fatalf("queued task not canceled: %s", c.Status)
	}
	if err := s.Cancel(ctx, retry); err == nil {
		t.Fatal("canceling a finished task succeeded")
	}
	list, _ := s.List(ctx, 10)
	if len(list) != 2 || list[0].ID != retry {
		t.Fatalf("List = %+v", list)
	}
}

func TestClaimIsExclusiveAcrossStores(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	a, b := openStore(t, dir), openStore(t, dir)
	const n = 20
	for i := 0; i < n; i++ {
		a.Add(ctx, "triage", []string{"acme/shop"}, "")
	}
	var mu sync.Mutex
	seen := map[int64]int{}
	var wg sync.WaitGroup
	for _, s := range []*Store{a, b, a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				task, err := s.Claim(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if task == nil {
					return
				}
				mu.Lock()
				seen[task.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != n {
		t.Fatalf("claimed %d distinct tasks, want %d", len(seen), n)
	}
	for id, c := range seen {
		if c != 1 {
			t.Errorf("task %d claimed %d times", id, c)
		}
	}
}

func TestRecoverOrphans(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, t.TempDir())
	id, _ := s.Add(ctx, "review", []string{"a/b", "1"}, "")
	s.Claim(ctx)
	s.RecoverOrphans(ctx, func(int) bool { return false })
	if got, _ := s.Get(ctx, id); got.Status != Failed {
		t.Fatalf("orphan status = %s", got.Status)
	}
}

func TestSplitJoin(t *testing.T) {
	cases := map[string][]string{
		`implement -plan p.json -test "go test ./..."`: {"implement", "-plan", "p.json", "-test", "go test ./..."},
		`review  a/b   12`:     {"review", "a/b", "12"},
		`plan 'it''s' x\ y ""`: {"plan", "its", "x y", ""},
	}
	for in, want := range cases {
		got, err := Split(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("Split(%q) = %q, %v; want %q", in, got, err, want)
		}
		if back, _ := Split(Join(want)); !reflect.DeepEqual(back, want) {
			t.Errorf("Join round trip of %q gave %q", want, back)
		}
	}
	if _, err := Split(`plan "open`); err == nil {
		t.Error("unterminated quote accepted")
	}
	if kind, args, err := Parse("devloop review a/b 3"); err != nil || kind != "review" || len(args) != 2 {
		t.Errorf("Parse = %q %q %v", kind, args, err)
	}
	if _, _, err := Parse("rm -rf /"); err == nil {
		t.Error("unknown command accepted")
	}
}

// fakeDevloop writes a script standing in for the devloop binary.
func fakeDevloop(t *testing.T) string {
	bin := filepath.Join(t.TempDir(), "devloop")
	script := `#!/bin/sh
case "$2" in
  ok)    echo "## Review of #1 — approve"; echo "details"; echo "progress" >&2; pwd > cwd ;;
  fail)  echo "error: GitHub said 401" >&2; exit 1 ;;
  slow)  echo "started"; sleep 30 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func waitFor(t *testing.T, s *Store, id int64, done func(Status) bool) *Task {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		task, _ := s.Get(context.Background(), id)
		if task != nil && done(task.Status) {
			return task
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("task %d did not reach the expected state", id)
	return nil
}

func TestWorkerRunsTasks(t *testing.T) {
	s := openStore(t, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &Worker{Store: s, Executable: fakeDevloop(t), Concurrency: 2, Poll: 50 * time.Millisecond}
	go w.Run(ctx)

	workDir := t.TempDir()
	ok, _ := s.Add(ctx, "review", []string{"ok"}, workDir)
	bad, _ := s.Add(ctx, "review", []string{"fail"}, "")
	slow, _ := s.Add(ctx, "review", []string{"slow"}, "")

	if task := waitFor(t, s, ok, Status.Done); task.Status != Succeeded || task.Summary != "Review of #1 — approve" {
		t.Errorf("ok task = %+v", task)
	}
	if task := waitFor(t, s, bad, Status.Done); task.Status != Failed || task.Error != "GitHub said 401" {
		t.Errorf("failing task = %+v", task)
	}
	if cwd, _ := os.ReadFile(filepath.Join(workDir, "cwd")); strings.TrimSpace(string(cwd)) != workDir {
		t.Errorf("task ran in %q, want %q", cwd, workDir)
	}
	log, _ := os.ReadFile(s.LogPath(ok))
	if !strings.Contains(string(log), "progress") || !strings.Contains(string(log), "details") {
		t.Errorf("log missing output: %q", log)
	}

	waitFor(t, s, slow, func(st Status) bool { return st == Running })
	if err := s.Cancel(ctx, slow); err != nil {
		t.Fatal(err)
	}
	if task := waitFor(t, s, slow, Status.Done); task.Status != Canceled {
		t.Errorf("slow task = %s, want canceled", task.Status)
	}
}
